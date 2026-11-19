package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	kfs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"

	"odac/internal/backup/consistency/sqlitecopy"
)

// sqliteLockWait bounds how long a copy waits for a writer's lock.
const sqliteLockWait = 30 * time.Second

// scratchMaxAge is when a scratch directory a killed run left is swept.
// It is far above any run's length, because a sweep cannot tell a
// directory that is still being read from an abandoned one.
const scratchMaxAge = 7 * 24 * time.Hour

// scratchReserve is the free space a copy must leave on the scratch disk,
// which is ODAC's own data disk: a backup must never be what fills it.
var scratchReserve = func(total int64) int64 { return max(1<<30, total/20) }

// sqliteSet is the consistent copies of the SQLite databases under one
// source directory, made once before its upload passes. A copied database
// is stored in place of the live file, and its WAL, WAL index and journal
// are left out: next to the copy, a WAL from another moment would be
// replayed onto it the first time the restored database is opened.
type sqliteSet struct {
	dir    string
	copies map[string]kfs.File
	hidden map[string]bool
}

func (s *sqliteSet) paths() []string {
	if s == nil || len(s.copies) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.copies))
	for p := range s.copies {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (s *sqliteSet) remove() {
	if s != nil && s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

func (r *Repo) scratchDir() string {
	if r.cfg.ScratchDir != "" {
		return r.cfg.ScratchDir
	}
	return filepath.Join(r.cfg.CacheDir, "scratch")
}

// copySQLite finds the databases under root by their header and copies
// each. A database that cannot be copied (locked past the wait, no room
// on the scratch disk, damaged) is left to the live copy, which detects
// what changed while it was read.
func (r *Repo) copySQLite(ctx context.Context, root string) (*sqliteSet, error) {
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// The upload reports what cannot be read.
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() || isSibling(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !sqlitecopy.Candidate(fi.Size()) {
			return nil
		}
		if _, ok, _ := sqlitecopy.Detect(p); ok {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}

	base := r.scratchDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	sweepScratch(base)
	dir, err := os.MkdirTemp(base, "sqlite-")
	if err != nil {
		return nil, err
	}
	set := &sqliteSet{dir: dir, copies: map[string]kfs.File{}, hidden: map[string]bool{}}
	for i, p := range found {
		if err := ctx.Err(); err != nil {
			set.remove()
			return nil, err
		}
		fi, err := os.Stat(p)
		if err != nil || !roomFor(dir, fi.Size()) {
			continue
		}
		dst := filepath.Join(dir, strconv.Itoa(i))
		if _, err := sqlitecopy.Copy(ctx, p, dst, sqliteLockWait); err != nil {
			if ctx.Err() != nil {
				set.remove()
				return nil, ctx.Err()
			}
			continue
		}
		e, err := localfs.NewEntry(dst)
		if err != nil {
			continue
		}
		f, ok := e.(kfs.File)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		set.copies[rel] = f
		for _, s := range sqlitecopy.Siblings(rel) {
			set.hidden[s] = true
		}
	}
	return set, nil
}

func isSibling(name string) bool {
	return strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")
}

func roomFor(dir string, size int64) bool {
	avail, total, ok := diskSpace(dir)
	if !ok {
		return true
	}
	return avail-size >= scratchReserve(total)
}

func sweepScratch(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil && time.Since(fi.ModTime()) > scratchMaxAge {
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// sqliteFile is a database's copy stored under the live file's name, mode
// and owner. Its modification time is the copy's own: a WAL database's
// file keeps its mtime while its WAL takes the writes, and the uploader
// would reuse the previous snapshot's content for an unchanged mtime and
// size.
type sqliteFile struct {
	kfs.File
	orig kfs.Entry
}

func (f *sqliteFile) Name() string           { return f.orig.Name() }
func (f *sqliteFile) Mode() os.FileMode      { return f.orig.Mode() }
func (f *sqliteFile) Owner() kfs.OwnerInfo   { return f.orig.Owner() }
func (f *sqliteFile) Device() kfs.DeviceInfo { return f.orig.Device() }
