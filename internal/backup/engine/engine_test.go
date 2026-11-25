package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

const (
	testServer = "0123456789abcdef0123456789abcdef"
	otherHost  = "fedcba9876543210fedcba9876543210"
	testPass   = "correct horse battery staple"
	mib        = 1 << 20
)

var testMeta = Meta{Run: "r1", Trigger: "schedule", Consistency: Consistent}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	repoDir  string
	cacheDir string
}

func newFixture(t *testing.T) *fixture {
	base := t.TempDir()
	return &fixture{
		t:        t,
		ctx:      context.Background(),
		repoDir:  filepath.Join(base, "repo"),
		cacheDir: filepath.Join(base, "cache"),
	}
}

func (f *fixture) config(server string) Config {
	return Config{
		Storage:  Storage{Filesystem: &FilesystemStorage{Path: f.repoDir}},
		Password: testPass,
		ServerID: server,
		WorkDir:  f.t.TempDir(),
		CacheDir: f.cacheDir,
	}
}

func (f *fixture) create() *Repo {
	f.t.Helper()
	r, err := Create(f.ctx, f.config(testServer))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { r.Close(f.ctx) })
	return r
}

func (f *fixture) open(cfg Config) *Repo {
	f.t.Helper()
	r, err := Open(f.ctx, cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { r.Close(f.ctx) })
	return r
}

func size(t *testing.T, r *Repo) int64 {
	t.Helper()
	n, err := r.Size(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// seededBytes is reproducible data. Content-defined chunk boundaries move
// with the data, so a size bound on fresh random bytes holds only most of
// the time: one 1 MiB edit grew the repository by 1.3 to 6.8 MB.
func seededBytes(seed uint64, n int) []byte {
	b := make([]byte, n)
	mrand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)}).Read(b)
	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func backupDir(t *testing.T, r *Repo, src Source, dir string) *BackupResult {
	t.Helper()
	res, err := r.BackupDir(context.Background(), src, dir, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// dump mimics a plain-format SQL dump: stable rows, with extra rows
// inserted in the middle so that everything after them shifts.
func dump(seed string, inserted int) []byte {
	var b bytes.Buffer
	for i := range 200000 {
		h := sha256.Sum256(fmt.Appendf(nil, "%s-%d", seed, i))
		fmt.Fprintf(&b, "INSERT INTO t VALUES (%d, '%x', 'user%d@example.com');\n", i, h[:12], i%997)
		if i == 100000 {
			for j := range inserted {
				fmt.Fprintf(&b, "INSERT INTO t VALUES (%d, 'new row');\n", 9000000+j)
			}
		}
	}
	return b.Bytes()
}

func restoreFile(t *testing.T, r *Repo, id, name string) []byte {
	t.Helper()
	target := filepath.Join(t.TempDir(), "out")
	if _, err := r.RestoreDir(context.Background(), id, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, name))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCreateAndOpenErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := Open(f.ctx, f.config(testServer)); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Open before Create = %v, want ErrNotInitialized", err)
	}
	f.create()
	if _, err := Create(f.ctx, f.config(testServer)); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	cfg := f.config(testServer)
	cfg.Password = "a different long password"
	if _, err := Open(f.ctx, cfg); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("Open with wrong password = %v, want ErrWrongPassword", err)
	}
	entries, err := os.ReadDir(f.cacheDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("cache dir should hold one per-repository subdirectory, got %v (%v)", entries, err)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for name, mutate := range map[string]func(*Config){
		"short password":     func(c *Config) { c.Password = "short" },
		"bad server id":      func(c *Config) { c.ServerID = "host-1" },
		"relative cache":     func(c *Config) { c.CacheDir = "cache" },
		"no cache":           func(c *Config) { c.CacheDir = "" },
		"no storage":         func(c *Config) { c.Storage = Storage{} },
		"two storages":       func(c *Config) { c.Storage.WebDAV = &WebDAVStorage{URL: "https://dav.example.com/"} },
		"relative repo path": func(c *Config) { c.Storage.Filesystem.Path = "repo" },
		"webdav not http":    func(c *Config) { c.Storage = Storage{WebDAV: &WebDAVStorage{URL: "ftp://x/"}} },
		"webdav userinfo":    func(c *Config) { c.Storage = Storage{WebDAV: &WebDAVStorage{URL: "https://u:secret@dav.example.com/"}} },
	} {
		cfg := f.config(testServer)
		mutate(&cfg)
		if _, err := Create(f.ctx, cfg); err == nil {
			t.Errorf("%s: Create accepted the config", name)
		}
	}
	r := f.create()
	if _, err := r.BackupStream(f.ctx, Source{Job: "j", App: "../etc", Name: "db"}, "dump.sql", bytes.NewReader(nil), testMeta); err == nil {
		t.Error("BackupStream accepted a source name with a path in it")
	}
	if _, err := r.BackupStream(f.ctx, Source{Job: "j", App: "a", Name: "db"}, "dump.sql", bytes.NewReader(nil), Meta{Run: "r1", Trigger: "x", Consistency: "mostly"}); err == nil {
		t.Error("BackupStream accepted an unknown consistency level")
	}
	if _, err := r.BackupStream(f.ctx, Source{Job: "j", App: "a", Name: "db"}, "dump.sql", bytes.NewReader(nil), Meta{Trigger: "x", Consistency: Live}); err == nil {
		t.Error("BackupStream accepted a snapshot without a run")
	}
}

func TestFileDedupAndRestore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	src := Source{Job: "nightly", App: "web", Name: "files"}
	dir := t.TempDir()
	big := seededBytes(1, 32*mib)
	writeFile(t, filepath.Join(dir, "big.bin"), big)
	writeFile(t, filepath.Join(dir, "sub", "small.txt"), []byte("hello"))

	first := backupDir(t, r, src, dir)
	if first.Files != 2 || first.Bytes != int64(len(big))+5 {
		t.Fatalf("first snapshot counts %d files, %d bytes", first.Files, first.Bytes)
	}

	// One edit's cost depends on where chunk boundaries fall, so the bound
	// is on the mean of several: a 4 MiB average chunk rewrites about 8 MB
	// around a 1 MiB edit, a 1 MiB one about 3 MB.
	const edits = 6
	before := size(t, r)
	var second *BackupResult
	for i := range edits {
		copy(big[(3+5*i)*mib:], seededBytes(uint64(2+i), mib))
		writeFile(t, filepath.Join(dir, "big.bin"), big)
		if res := backupDir(t, r, src, dir); second == nil {
			second = res
		}
	}
	if mean := (size(t, r) - before) / edits; mean > 4*mib {
		t.Fatalf("a 1 MiB change grew the repository by %d bytes on average", mean)
	}

	before = size(t, r)
	third := backupDir(t, r, src, dir)
	if grew := size(t, r) - before; grew > 256<<10 {
		t.Fatalf("unchanged backup grew the repository by %d bytes", grew)
	}

	// Deleting a snapshot in the middle leaves the others whole.
	if err := r.Delete(f.ctx, []string{second.ID}); err != nil {
		t.Fatal(err)
	}
	if got := restoreFile(t, r, third.ID, "big.bin"); !bytes.Equal(got, big) {
		t.Fatal("restore of the newest snapshot differs from the source")
	}
	if got := restoreFile(t, r, first.ID, "sub/small.txt"); string(got) != "hello" {
		t.Fatalf("restore of the oldest snapshot gave %q", got)
	}
}

func TestStreamDedupAndRestore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	src := Source{Job: "nightly", App: "db", Name: "postgres"}
	stream := func(data []byte) *BackupResult {
		t.Helper()
		res, err := r.BackupStream(f.ctx, src, "dump.sql", bytes.NewReader(data), testMeta)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	base := dump("seed", 0)
	stream(base)
	shifted := dump("seed", 500)
	before := size(t, r)
	s := stream(shifted)
	if grew := size(t, r) - before; grew > int64(len(base))/4 {
		t.Fatalf("500 inserted rows grew the repository by %d of %d bytes", grew, len(base))
	}
	before = size(t, r)
	again := stream(shifted)
	if grew := size(t, r) - before; grew > 256<<10 {
		t.Fatalf("identical dump grew the repository by %d bytes", grew)
	}
	if s.Kind != KindStream || s.Bytes != int64(len(shifted)) {
		t.Fatalf("stream snapshot: kind %q, %d bytes", s.Kind, s.Bytes)
	}

	var out bytes.Buffer
	n, err := r.RestoreStream(f.ctx, again.ID, &out)
	if err != nil || n != int64(len(shifted)) || !bytes.Equal(out.Bytes(), shifted) {
		t.Fatalf("RestoreStream = %d, %v; equal %v", n, err, bytes.Equal(out.Bytes(), shifted))
	}
	if _, err := r.RestoreDir(f.ctx, again.ID, filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrKind) {
		t.Fatalf("RestoreDir of a stream = %v, want ErrKind", err)
	}
}

func TestStreamFailureStoresNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	src := Source{Job: "nightly", App: "db", Name: "postgres"}
	rd := io.MultiReader(bytes.NewReader(dump("x", 0)), errReader{})
	if _, err := r.BackupStream(f.ctx, src, "dump.sql", rd, testMeta); err == nil {
		t.Fatal("a failing stream produced a snapshot")
	}
	if got, _ := r.List(f.ctx, Filter{}); len(got) != 0 {
		t.Fatalf("List after failed stream = %d snapshots", len(got))
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("dump died") }

func TestListByTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), []byte("a"))
	backupDir(t, r, Source{Job: "nightly", App: "web", Name: "files"}, dir)
	backupDir(t, r, Source{Job: "hourly", App: "web", Name: "files"}, dir)
	if _, err := r.BackupStream(f.ctx, Source{Job: "hourly", App: "db", Name: "postgres"}, "dump.sql", bytes.NewReader([]byte("x")), Meta{Run: "r2", Trigger: "manual", Consistency: Live}); err != nil {
		t.Fatal(err)
	}

	all, _ := r.List(f.ctx, Filter{})
	hourly, _ := r.List(f.ctx, Filter{Job: "hourly"})
	web, _ := r.List(f.ctx, Filter{App: "web"})
	if len(all) != 3 || len(hourly) != 2 || len(web) != 2 {
		t.Fatalf("List counts: all %d, hourly %d, web %d", len(all), len(hourly), len(web))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Start.Before(all[i-1].Start) {
			t.Fatal("List is not oldest first")
		}
	}
	db, _ := r.List(f.ctx, Filter{App: "db"})
	if len(db) != 1 || db[0].Trigger != "manual" || db[0].Consistency != Live || db[0].Host != testServer {
		t.Fatalf("db snapshot = %+v", db)
	}
	if other, _ := r.List(f.ctx, Filter{Host: otherHost}); len(other) != 0 {
		t.Fatalf("List for another host = %d", len(other))
	}
	if run, _ := r.List(f.ctx, Filter{Run: "r2"}); len(run) != 1 || run[0].Run != "r2" || run[0].Source.App != "db" {
		t.Fatalf("List by run = %+v", run)
	}
}

// Kopia applies its default retention (keep 10 latest, 48 hourly, ...) on
// its own, at every checkpoint. Pinned snapshots must survive it.
func TestPinnedSnapshotsSurviveKopiaRetention(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	src := Source{Job: "nightly", App: "db", Name: "redis"}
	for i := range 13 {
		if _, err := r.BackupStream(f.ctx, src, "dump.rdb", bytes.NewReader([]byte{byte(i)}), testMeta); err != nil {
			t.Fatal(err)
		}
	}
	var expired []string
	err := repo.WriteSession(f.ctx, r.rep, repo.WriteSessionOptions{Purpose: "test"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		ids, err := policy.ApplyRetentionPolicy(ctx, w, r.sourceInfo(src), true)
		for _, id := range ids {
			expired = append(expired, string(id))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.List(f.ctx, Filter{}); len(got) != 13 || len(expired) != 0 {
		t.Fatalf("after Kopia retention: %d snapshots left, %d expired", len(got), len(expired))
	}
}

func TestGarbageCollection(t *testing.T) {
	f := newFixture(t)
	// Kopia records content timestamps and deletion watermarks in whole Unix
	// seconds, bumping a same-second delete to T+1. Shifting the live clock
	// slightly behind wall time (well within Kopia's 5m clock-skew bound) and
	// stepping it forward per SafetyNone pass preserves real sub-second order
	// between manifest writes while ensuring each pass crosses a second
	// boundary and stays behind filesystem mtimes under coarse Linux ticks.
	var offset atomic.Int64
	offset.Store(int64(-10 * time.Second))
	cfg := f.config(testServer)
	cfg.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	r, err := Create(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(f.ctx) })

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "big.bin"), randomBytes(t, 8*mib))
	res := backupDir(t, r, Source{Job: "nightly", App: "web", Name: "files"}, dir)
	if err := r.Delete(f.ctx, []string{res.ID}); err != nil {
		t.Fatal(err)
	}
	full := size(t, r)

	// SafetyFull holds deleted contents back across runs, so one run right
	// after the delete gives nothing back. That is the protection for a
	// backup being written while maintenance runs.
	if err := r.Maintain(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := size(t, r); got < full*9/10 {
		t.Fatalf("one SafetyFull run dropped the repository from %d to %d bytes", full, got)
	}

	maintenanceSafety = maintenance.SafetyNone
	defer func() { maintenanceSafety = maintenance.SafetyFull }()
	for range 3 {
		offset.Add(int64(2 * time.Second))
		if err := r.Maintain(f.ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := size(t, r); got > mib {
		t.Fatalf("after collection the repository still holds %d of %d bytes", got, full)
	}
	if err := r.Maintain(f.ctx, false); err != nil {
		t.Fatalf("quick maintenance: %v", err)
	}
}

func TestAnotherServerCannotDeleteOrMaintain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	res, err := r.BackupStream(f.ctx, Source{Job: "j", App: "db", Name: "pg"}, "dump.sql", bytes.NewReader([]byte("data")), testMeta)
	if err != nil {
		t.Fatal(err)
	}
	other := f.open(f.config(otherHost))
	if err := other.Delete(f.ctx, []string{res.ID}); !errors.Is(err, ErrForeign) {
		t.Fatalf("foreign Delete = %v, want ErrForeign", err)
	}
	if err := other.Maintain(f.ctx, true); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign Maintain = %v, want ErrNotOwner", err)
	}
	if got, _ := r.List(f.ctx, Filter{}); len(got) != 1 {
		t.Fatalf("owner sees %d snapshots after the foreign attempts", len(got))
	}
	if err := r.Delete(f.ctx, []string{res.ID, res.ID, "k0000000000000000000000000000000"}); err != nil {
		t.Fatalf("Delete with a repeated and an unknown id = %v", err)
	}
}

func TestReadOnlyRestoresAnotherServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "data.txt"), []byte("migrate me"))
	res := backupDir(t, r, Source{Job: "j", App: "web", Name: "files"}, dir)
	r.Close(f.ctx)

	cfg := f.config(otherHost)
	cfg.ReadOnly = true
	ro := f.open(cfg)
	if _, err := ro.BackupDir(f.ctx, Source{Job: "j", App: "web", Name: "files"}, dir, testMeta); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only BackupDir = %v", err)
	}
	if err := ro.Maintain(f.ctx, false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Maintain = %v", err)
	}
	theirs, err := ro.List(f.ctx, Filter{Host: testServer})
	if err != nil || len(theirs) != 1 {
		t.Fatalf("List of the other server = %d, %v", len(theirs), err)
	}
	if got := restoreFile(t, ro, res.ID, "data.txt"); string(got) != "migrate me" {
		t.Fatalf("restored %q", got)
	}
}

func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), []byte("a"))
	res := backupDir(t, r, Source{Job: "j", App: "web", Name: "files"}, dir)
	if _, err := r.RestoreDir(f.ctx, res.ID, dir); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("RestoreDir into live data = %v", err)
	}
	if _, err := r.RestoreDir(f.ctx, "k0000000000000000000000000000000", filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RestoreDir of unknown id = %v", err)
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.bin"), randomBytes(t, 2*mib))
	backupDir(t, r, Source{Job: "j", App: "web", Name: "files"}, dir)
	res, err := r.Verify(f.ctx, 100)
	if err != nil || len(res.Errors) != 0 || res.Snapshots != 1 || res.FilesRead != 1 {
		t.Fatalf("Verify of a sound repository = %+v, %v", res, err)
	}

	// Damage every pack blob: verification must report it, not fail.
	err = filepath.Walk(f.repoDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		// Data packs are the blobs whose id starts with "p"; the storage
		// shards ids into directories, so the prefix is on the path.
		if rel, _ := filepath.Rel(f.repoDir, p); !strings.HasPrefix(rel, "p") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		data[len(data)/2] ^= 0xff
		return os.WriteFile(p, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(testServer)
	cfg.CacheDir = t.TempDir()
	fresh := f.open(cfg)
	res, err = fresh.Verify(f.ctx, 100)
	if err != nil || len(res.Errors) == 0 {
		t.Fatalf("Verify of a damaged repository = %+v, %v", res, err)
	}
}

func TestLiveFileChangedOnceIsReadAgain(t *testing.T) {
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.dat")
	writeFile(t, path, []byte("version 1"))
	reads := 0
	afterRead = func(rel string) {
		if rel == "db.dat" {
			reads++
			if reads == 1 {
				writeFile(t, path, []byte("version 2, longer"))
			}
		}
	}
	defer func() { afterRead = nil }()
	res := backupDir(t, r, Source{Job: "j", App: "app", Name: "data"}, dir)
	afterRead = nil
	if res.Torn != 0 || res.Stale != 0 || reads != 2 {
		t.Fatalf("torn %d, stale %d, reads %d", res.Torn, res.Stale, reads)
	}
	if got := restoreFile(t, r, res.ID, "db.dat"); string(got) != "version 2, longer" {
		t.Fatalf("restored %q", got)
	}
}

func TestLiveFileAlwaysChangingKeepsIntactCopy(t *testing.T) {
	f := newFixture(t)
	r := f.create()
	src := Source{Job: "j", App: "app", Name: "data"}
	dir := t.TempDir()
	path := filepath.Join(dir, "db.dat")
	writeFile(t, path, []byte("intact"))
	writeFile(t, filepath.Join(dir, "other.txt"), []byte("calm"))
	backupDir(t, r, src, dir)

	writeFile(t, path, []byte("busy 0"))
	n := 0
	afterRead = func(rel string) {
		if rel == "db.dat" {
			n++
			writeFile(t, path, fmt.Appendf(nil, "busy %d with more", n))
		}
	}
	defer func() { afterRead = nil }()
	res := backupDir(t, r, src, dir)
	afterRead = nil
	if res.Stale != 1 || res.Torn != 0 || len(res.StalePaths) != 1 || res.StalePaths[0] != "db.dat" {
		t.Fatalf("result %+v", res)
	}
	if got := restoreFile(t, r, res.ID, "db.dat"); string(got) != "intact" {
		t.Fatalf("stale copy restored as %q", got)
	}
	listed, _ := r.List(f.ctx, Filter{})
	if last := listed[len(listed)-1]; last.Stale != 1 || last.Torn != 0 {
		t.Fatalf("listed snapshot %+v", last)
	}
}

func TestLiveFileAlwaysChangingWithoutHistoryIsTorn(t *testing.T) {
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	writeFile(t, path, []byte("line\n"))
	n := 0
	afterRead = func(rel string) {
		n++
		writeFile(t, path, bytes.Repeat([]byte("line\n"), n+1))
	}
	defer func() { afterRead = nil }()
	res := backupDir(t, r, Source{Job: "j", App: "app", Name: "logs"}, dir)
	afterRead = nil
	if res.Torn != 1 || res.Stale != 0 || res.TornPaths[0] != "log.txt" || n != livePasses {
		t.Fatalf("result %+v after %d reads", res, n)
	}
}

func TestLiveFileVanishing(t *testing.T) {
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmp.lock")
	writeFile(t, path, []byte("lock"))
	writeFile(t, filepath.Join(dir, "keep.txt"), []byte("keep"))
	afterRead = func(rel string) {
		if rel == "tmp.lock" {
			os.Remove(path)
		}
	}
	defer func() { afterRead = nil }()
	res := backupDir(t, r, Source{Job: "j", App: "app", Name: "data"}, dir)
	afterRead = nil
	if res.Torn != 0 || res.Files != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestFromManifestSkipsCheckpointsAndForeignSnapshots(t *testing.T) {
	t.Parallel()
	tags := tagsFor(Source{Job: "j", App: "a", Name: "n"}, KindDir, testMeta, 0, 0)
	ok := &snapshot.Manifest{Source: snapshot.SourceInfo{UserName: userName}, Tags: tags}
	if _, keep := fromManifest(ok); !keep {
		t.Fatal("a finished ODAC snapshot was skipped")
	}
	checkpoint := *ok
	checkpoint.IncompleteReason = "checkpoint"
	cli := &snapshot.Manifest{Source: snapshot.SourceInfo{UserName: userName}}
	other := *ok
	other.Source.UserName = "root"
	for name, m := range map[string]*snapshot.Manifest{"checkpoint": &checkpoint, "cli": cli, "other user": &other} {
		if _, keep := fromManifest(m); keep {
			t.Errorf("%s manifest was listed", name)
		}
	}
}

// endless is a dump that never ends; only cancellation can stop a backup
// of it.
type endless struct{ cancel func() }

func (e *endless) Read(p []byte) (int, error) {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	clear(p)
	return len(p), nil
}

func TestCanceledBackupStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	src := Source{Job: "nightly", App: "db", Name: "postgres"}
	_, err := r.BackupStream(ctx, src, "dump.sql", &endless{cancel: cancel}, testMeta)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BackupStream after cancel = %v, want context.Canceled", err)
	}
	if got, _ := r.List(f.ctx, Filter{}); len(got) != 0 {
		t.Fatalf("a canceled backup stored %d snapshots", len(got))
	}
}

func TestProgressCountsWork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), randomBytes(t, mib))
	writeFile(t, filepath.Join(dir, "sub", "b"), randomBytes(t, mib/2))

	var p Progress
	meta := testMeta
	meta.Progress = &p
	if _, err := r.BackupDir(f.ctx, Source{Job: "j", App: "web", Name: "files"}, dir, meta); err != nil {
		t.Fatal(err)
	}
	if p.Files() != 2 || p.Bytes() != mib+mib/2 {
		t.Fatalf("progress after a read = %d files, %d bytes", p.Files(), p.Bytes())
	}
	var again Progress
	meta.Progress = &again
	if _, err := r.BackupDir(f.ctx, Source{Job: "j", App: "web", Name: "files"}, dir, meta); err != nil {
		t.Fatal(err)
	}
	if again.Files() != 2 || again.Bytes() != mib+mib/2 {
		t.Fatalf("unchanged files must count too: %d files, %d bytes", again.Files(), again.Bytes())
	}
}
