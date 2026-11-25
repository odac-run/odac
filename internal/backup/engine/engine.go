// Package engine is the one place ODAC talks to Kopia. Kopia promises no
// stable library API, so everything it exposes stays behind this package's
// own types and every upgrade is absorbed here. It is linked into
// odac-backup alone (cmd/odac-backup/boundary_test.go enforces that).
//
// The wrapper's main job is to never inherit a Kopia zero value that is
// wrong for ODAC: the splitter, compression, cache location and size,
// restore depth and maintenance safety are all set explicitly, and every
// snapshot is pinned so that no Kopia retention rule can delete it. Which
// snapshot goes is decided by ODAC's thinning alone.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/encryption"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/hashing"
	"github.com/kopia/kopia/repo/maintenance"

	"odac/internal/serverid"
)

const (
	// splitter is DYNAMIC-1M-BUZHASH rather than Kopia's 4M default: a
	// 1 MiB edit in a 50 MiB file wrote 2.7 MB instead of 7.5 MB.
	splitter = "DYNAMIC-1M-BUZHASH"
	// compressor applies to file and stream data. Kopia's default is none,
	// and dumps are taken uncompressed precisely so that this layer can
	// compress them without breaking deduplication.
	compressor = "zstd-fastest"
	// userName is the Kopia user every ODAC snapshot is written under; the
	// host is the server id.
	userName = "odac"
	// pin keeps a snapshot out of every Kopia retention pass (checkpoints
	// apply the default policy on their own). Delete ignores pins.
	pin = "odac"

	defaultCacheBytes = 1 << 30
	minPasswordLen    = 16
)

// Errors callers branch on. Kopia's own errors never cross this package.
var (
	ErrNotInitialized = errors.New("engine: no repository at this storage")
	ErrExists         = errors.New("engine: a repository already exists at this storage")
	ErrWrongPassword  = errors.New("engine: wrong repository password")
	ErrNotOwner       = errors.New("engine: maintenance of this repository belongs to another server")
	ErrReadOnly       = errors.New("engine: repository opened read-only")
	ErrNotFound       = errors.New("engine: snapshot not found")
	ErrForeign        = errors.New("engine: snapshot belongs to another server")
	ErrKind           = errors.New("engine: snapshot holds the other kind of data")
	ErrTargetNotEmpty = errors.New("engine: restore target is not empty")
)

// Config says where a repository lives and who is using it.
type Config struct {
	Storage  Storage
	Password string
	// ServerID names this server inside the repository: it is the Kopia
	// host of every snapshot written, the maintenance owner, and the
	// boundary Delete refuses to cross.
	ServerID string
	// WorkDir holds Kopia's connection file for the life of the Repo. That
	// file carries the storage credentials, so it is created 0700 inside
	// WorkDir and removed by Close; empty means the OS temp directory.
	WorkDir string
	// CacheDir is required: Kopia's fallback is the user cache directory,
	// which in a container is a layer that vanishes. Each repository gets
	// its own subdirectory, keyed by the repository's unique id.
	CacheDir string
	// CacheBytes bounds the content and metadata caches together; zero
	// means 1 GiB.
	CacheBytes int64
	// ScratchDir holds the consistent copies of SQLite databases while a
	// directory backup reads them, so it must be on a disk with room for
	// the largest one; empty means a directory inside CacheDir.
	ScratchDir string
	// ReadOnly opens a repository for listing and restore only, which is
	// how another server's data is read: nothing can be written into it.
	ReadOnly bool

	now func() time.Time
}

// Repo is an open repository. Its methods are safe for concurrent use, but
// writes (backup, delete, maintenance) run one at a time: maintenance must
// never overlap a backup in the same repository.
type Repo struct {
	rep     repo.DirectRepository
	cfg     Config
	workDir string
	writeMu sync.Mutex
	fatal   atomic.Pointer[error]
}

func (c *Config) validate() error {
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if len(c.Password) < minPasswordLen {
		return fmt.Errorf("engine: password must be at least %d characters", minPasswordLen)
	}
	if !serverid.Valid(c.ServerID) {
		return errors.New("engine: invalid server id")
	}
	if c.CacheDir == "" || !filepath.IsAbs(c.CacheDir) {
		return errors.New("engine: cache directory must be an absolute path")
	}
	if c.ScratchDir != "" && !filepath.IsAbs(c.ScratchDir) {
		return errors.New("engine: scratch directory must be an absolute path")
	}
	if c.CacheBytes < 0 {
		return errors.New("engine: negative cache size")
	}
	return nil
}

func (c *Config) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Create initializes a new repository and opens it. It never touches a
// repository that already exists (ErrExists), since a second format blob
// over the first would orphan every snapshot in it.
func Create(ctx context.Context, cfg Config) (*Repo, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.ReadOnly {
		return nil, ErrReadOnly
	}
	st, err := cfg.Storage.connect(ctx, true)
	if err != nil {
		return nil, err
	}
	err = repo.Initialize(ctx, st, &repo.NewRepositoryOptions{
		BlockFormat: format.ContentFormat{
			Hash:       hashing.DefaultAlgorithm,
			Encryption: encryption.DefaultAlgorithm,
			// ECC stays off: experimental in Kopia, fixed at creation, and
			// only covers small in-file damage. Two targets are the defense.
			ECC:                  "",
			ECCOverheadPercent:   0,
			EnablePasswordChange: true,
		},
		ObjectFormat: format.ObjectFormat{Splitter: splitter},
	}, cfg.Password)
	st.Close(ctx)
	if errors.Is(err, repo.ErrAlreadyInitialized) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, fmt.Errorf("engine: initialize: %w", err)
	}
	r, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := r.claimMaintenance(ctx); err != nil {
		r.Close(ctx)
		return nil, err
	}
	return r, nil
}

// Open connects to an existing repository.
func Open(ctx context.Context, cfg Config) (*Repo, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	st, err := cfg.Storage.connect(ctx, false)
	if err != nil {
		return nil, err
	}
	uniqueID, err := readUniqueID(ctx, st)
	if err != nil {
		st.Close(ctx)
		return nil, err
	}

	workDir, err := os.MkdirTemp(cfg.WorkDir, "odac-kopia-*")
	if err != nil {
		st.Close(ctx)
		return nil, fmt.Errorf("engine: work dir: %w", err)
	}
	configFile := filepath.Join(workDir, "repository.config")

	err = writeConnection(ctx, configFile, st, cfg, uniqueID)
	st.Close(ctx)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, err
	}

	r := &Repo{cfg: cfg, workDir: workDir}
	rep, err := repo.Open(ctx, configFile, cfg.Password, &repo.Options{
		TimeNowFunc: cfg.clock,
		// Kopia's default exits the process. Record it instead so that the
		// caller reports it like any other failure.
		OnFatalError: func(err error) { r.fatal.Store(&err) },
	})
	if err != nil {
		os.RemoveAll(workDir)
		return nil, mapOpenError(err)
	}
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		rep.Close(ctx)
		os.RemoveAll(workDir)
		return nil, errors.New("engine: not a direct repository")
	}
	r.rep = dr
	return r, nil
}

// Close releases the repository and removes the connection file. The
// cache stays, because it is what makes the next run fast.
func (r *Repo) Close(ctx context.Context) error {
	err := r.rep.Close(ctx)
	if rerr := os.RemoveAll(r.workDir); err == nil {
		err = rerr
	}
	return err
}

func (r *Repo) err() error {
	if p := r.fatal.Load(); p != nil {
		return fmt.Errorf("engine: repository failed: %w", *p)
	}
	return nil
}

func (r *Repo) writable() error {
	if r.cfg.ReadOnly {
		return ErrReadOnly
	}
	return r.err()
}

func mapOpenError(err error) error {
	switch {
	case errors.Is(err, repo.ErrRepositoryNotInitialized):
		return ErrNotInitialized
	case errors.Is(err, repo.ErrInvalidPassword):
		return ErrWrongPassword
	}
	return fmt.Errorf("engine: open: %w", err)
}

// readUniqueID reads the repository's unique id from its format blob so
// the cache directory can be keyed by it, the way Kopia keys its default.
// Two repositories sharing one cache directory would read each other's
// entries.
func readUniqueID(ctx context.Context, st blob.Storage) (string, error) {
	var buf outputBuffer
	if err := st.GetBlob(ctx, format.KopiaRepositoryBlobID, 0, -1, &buf); err != nil {
		if errors.Is(err, blob.ErrBlobNotFound) {
			return "", ErrNotInitialized
		}
		return "", fmt.Errorf("engine: read format blob: %w", err)
	}
	f, err := format.ParseKopiaRepositoryJSON(buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("engine: parse format blob: %w", err)
	}
	if len(f.UniqueID) == 0 {
		return "", errors.New("engine: format blob has no unique id")
	}
	sum := sha256.Sum256(f.UniqueID)
	return hex.EncodeToString(sum[:8]), nil
}

// writeConnection writes the connection file repo.Open reads. It stands in
// for repo.Connect, which opens the repository once more to check the
// password (loading every index twice per run) and, when that fails,
// deletes the cache directory of a repository that is fine.
func writeConnection(ctx context.Context, configFile string, st blob.Storage, cfg Config, uniqueID string) error {
	cacheBytes := cfg.CacheBytes
	if cacheBytes == 0 {
		cacheBytes = defaultCacheBytes
	}
	half := cacheBytes / 2
	ci := st.ConnectionInfo()
	lc := repo.LocalConfig{
		Storage: &ci,
		Caching: &content.CachingOptions{
			CacheDirectory:              filepath.Join(cfg.CacheDir, uniqueID),
			ContentCacheSizeBytes:       half,
			ContentCacheSizeLimitBytes:  half,
			MetadataCacheSizeBytes:      half,
			MetadataCacheSizeLimitBytes: half,
		},
		ClientOptions: repo.ClientOptions{
			Hostname:    cfg.ServerID,
			Username:    userName,
			ReadOnly:    cfg.ReadOnly,
			Description: "ODAC",
		}.ApplyDefaults(ctx, "ODAC"),
	}
	b, err := json.Marshal(lc)
	if err != nil {
		return fmt.Errorf("engine: connection file: %w", err)
	}
	if err := os.WriteFile(configFile, b, 0o600); err != nil {
		return fmt.Errorf("engine: connection file: %w", err)
	}
	return nil
}

type outputBuffer struct{ bytes.Buffer }

func (b *outputBuffer) Length() int { return b.Len() }

// claimMaintenance makes this server the repository's maintenance owner.
// Kopia refuses maintenance by anyone else, which is what keeps a clone
// with the same repository from collecting garbage under the original.
func (r *Repo) claimMaintenance(ctx context.Context) error {
	return repo.DirectWriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "odac:claim-maintenance"},
		func(ctx context.Context, w repo.DirectRepositoryWriter) error {
			p := maintenance.DefaultParams()
			p.Owner = w.ClientOptions().UsernameAtHost()
			return maintenance.SetParams(ctx, w, &p)
		})
}
