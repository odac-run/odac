package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/studio-b12/gowebdav"

	"odac/internal/serverid"
)

// Layout of a server's directory on a target; proto mirrors both names.
const (
	RepositoryDir = "repository"
	OwnerFile     = "server.json"
)

const (
	ownerFormat  = 1
	ownerMaxSize = 64 << 10
	davTimeout   = time.Minute
)

// ErrOwnerConflict is a server directory whose ownership file is ahead of
// the claim or names another server: some other machine writes under this
// server id, most likely a clone of this one.
var ErrOwnerConflict = errors.New("engine: another machine writes under this server id")

// OwnerClaim is one run's claim on its server directory. Seq is never sent
// twice by the server, so a file holding Seq or more was written by
// someone else. Adopt takes the directory over and writes the number after
// the one found.
type OwnerClaim struct {
	ServerID string
	Name     string
	Seq      int64
	Adopt    bool
	Now      time.Time
}

// OwnerRecord is the ownership file. The directory is named by the id, so
// the name is there for a person browsing the target.
type OwnerRecord struct {
	Format   int       `json:"format"`
	ServerID string    `json:"serverId"`
	Name     string    `json:"name,omitempty"`
	Seq      int64     `json:"seq"`
	Updated  time.Time `json:"updated"`
}

// ClaimOwner checks and advances the ownership file of the server
// directory holding the repository at st, before anything is written into
// the repository. It returns the number it wrote. Check and write are not
// atomic: two machines claiming within the same moment can both pass
// once, and the next claim of the one behind fails.
func ClaimOwner(ctx context.Context, st Storage, c OwnerClaim) (int64, error) {
	if err := st.validate(); err != nil {
		return 0, err
	}
	if !serverid.Valid(c.ServerID) {
		return 0, errors.New("engine: invalid server id")
	}
	if c.Seq < 1 {
		return 0, errors.New("engine: owner sequence must be positive")
	}
	f, err := ownerFileOf(ctx, st, c.ServerID)
	if err != nil {
		return 0, err
	}
	raw, err := f.read(ctx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("engine: read ownership file: %w", err)
	}
	seq := c.Seq
	if err == nil {
		var cur OwnerRecord
		if jerr := json.Unmarshal(raw, &cur); jerr != nil || cur.ServerID == "" {
			if !c.Adopt {
				return 0, fmt.Errorf("%w: the ownership file is unreadable", ErrOwnerConflict)
			}
		} else if cur.ServerID != c.ServerID {
			return 0, fmt.Errorf("%w: the ownership file names server %s", ErrOwnerConflict, cur.ServerID)
		} else if cur.Seq >= c.Seq {
			if !c.Adopt {
				return 0, fmt.Errorf("%w: the ownership file holds sequence %d, this server last wrote below %d", ErrOwnerConflict, cur.Seq, c.Seq)
			}
			seq = cur.Seq + 1
		}
	}
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	out, err := json.MarshalIndent(OwnerRecord{Format: ownerFormat, ServerID: c.ServerID, Name: c.Name, Seq: seq, Updated: now.UTC()}, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := f.write(ctx, append(out, '\n')); err != nil {
		return 0, fmt.Errorf("engine: write ownership file: %w", err)
	}
	return seq, nil
}

// ReadOwner returns the ownership file of the server directory holding
// the repository at st, or os.ErrNotExist.
func ReadOwner(ctx context.Context, st Storage, serverID string) (*OwnerRecord, error) {
	if err := st.validate(); err != nil {
		return nil, err
	}
	f, err := ownerFileOf(ctx, st, serverID)
	if err != nil {
		return nil, err
	}
	raw, err := f.read(ctx)
	if err != nil {
		return nil, err
	}
	var rec OwnerRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("engine: parse ownership file: %w", err)
	}
	return &rec, nil
}

type ownerFile interface {
	read(ctx context.Context) ([]byte, error)
	write(ctx context.Context, data []byte) error
}

// ownerFileOf finds the ownership file next to the repository. The storage
// must name <server id>/repository, so a misconfigured server can never
// leave the file anywhere else.
func ownerFileOf(ctx context.Context, st Storage, id string) (ownerFile, error) {
	suffix := id + "/" + RepositoryDir
	layout := fmt.Errorf("engine: an owned repository lives in %s of its target", suffix)
	switch {
	case st.Filesystem != nil:
		p := filepath.Clean(st.Filesystem.Path)
		if filepath.Base(p) != RepositoryDir || filepath.Base(filepath.Dir(p)) != id {
			return nil, layout
		}
		return fsOwner{dir: filepath.Dir(p)}, nil
	case st.S3 != nil:
		if !strings.HasSuffix("/"+st.S3.Prefix, "/"+suffix+"/") {
			return nil, layout
		}
		o := *st.S3
		o.Prefix = strings.TrimSuffix(o.Prefix, RepositoryDir+"/")
		bs, err := s3.New(ctx, &s3.Options{
			Endpoint:        o.Endpoint,
			Region:          o.Region,
			BucketName:      o.Bucket,
			Prefix:          o.Prefix,
			AccessKeyID:     o.AccessKeyID,
			SecretAccessKey: o.SecretAccessKey,
			SessionToken:    o.SessionToken,
			DoNotUseTLS:     o.DisableTLS,
		}, false)
		if err != nil {
			return nil, fmt.Errorf("engine: storage: %w", err)
		}
		return blobOwner{st: bs}, nil
	case st.WebDAV != nil:
		u, err := url.Parse(st.WebDAV.URL)
		if err != nil {
			return nil, err
		}
		p := strings.TrimSuffix(u.Path, "/")
		if path.Base(p) != RepositoryDir || path.Base(path.Dir(p)) != id {
			return nil, layout
		}
		u.Path = path.Dir(path.Dir(p)) + "/"
		u.RawPath = ""
		cli := gowebdav.NewClient(u.String(), st.WebDAV.Username, st.WebDAV.Password)
		cli.SetTimeout(davTimeout)
		cli.SetHeader("Accept-Encoding", "identity")
		return davOwner{cli: cli, dir: "/" + id + "/"}, nil
	}
	return nil, errors.New("engine: exactly one storage kind must be set")
}

type fsOwner struct{ dir string }

func (f fsOwner) read(context.Context) ([]byte, error) {
	file, err := os.Open(filepath.Join(f.dir, OwnerFile))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, ownerMaxSize))
}

// write replaces the file through a rename, so a crash leaves the old
// number or the new one and never a torn file, which would read as a
// conflict.
func (f fsOwner) write(_ context.Context, data []byte) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, "."+OwnerFile+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(f.dir, OwnerFile)); err != nil {
		return err
	}
	if d, err := os.Open(f.dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// blobOwner keeps the file as an object; S3 names objects without
// sharding, so the key reads <prefix><server id>/server.json.
type blobOwner struct{ st blob.Storage }

func (b blobOwner) read(ctx context.Context) ([]byte, error) {
	defer b.st.Close(ctx)
	var buf outputBuffer
	err := b.st.GetBlob(ctx, OwnerFile, 0, -1, &buf)
	if errors.Is(err, blob.ErrBlobNotFound) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if buf.Len() > ownerMaxSize {
		return nil, errors.New("ownership file too large")
	}
	return buf.Bytes(), nil
}

func (b blobOwner) write(ctx context.Context, data []byte) error {
	defer b.st.Close(ctx)
	return b.st.PutBlob(ctx, OwnerFile, byteBlob(data), blob.PutOptions{})
}

type byteBlob []byte

func (b byteBlob) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

func (b byteBlob) Length() int { return len(b) }

func (b byteBlob) Reader() io.ReadSeekCloser { return nopSeekCloser{bytes.NewReader(b)} }

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// davOwner talks to the collection directly: Kopia's WebDAV storage shards
// names and adds a suffix, which would hide the file from a person. Its
// client is rooted at the target, the collection the operator named.
type davOwner struct {
	cli *gowebdav.Client
	dir string
}

func (d davOwner) read(context.Context) ([]byte, error) {
	b, err := d.cli.Read(d.dir + OwnerFile)
	if gowebdav.IsErrNotFound(err) {
		return nil, os.ErrNotExist
	}
	if err == nil && len(b) > ownerMaxSize {
		return nil, errors.New("ownership file too large")
	}
	return b, err
}

// write also creates the repository's collection: Kopia's WebDAV storage
// creates the collections below its root, never the root itself.
func (d davOwner) write(_ context.Context, data []byte) error {
	if err := d.cli.MkdirAll(d.dir+RepositoryDir, 0o700); err != nil {
		return err
	}
	return d.cli.Write(d.dir+OwnerFile, data, 0o600)
}
