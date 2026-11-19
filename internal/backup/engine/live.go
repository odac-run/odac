package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"sort"
	"sync"

	"github.com/kopia/kopia/fs"
)

// liveRun watches one upload pass over a directory that is in use. A file
// is "changed" when its size or mtime moved between the directory listing,
// the start of its read and the end of it: the bytes stored for it may
// then be a mix of two versions. keep holds the intact copy (from an
// earlier snapshot) to store instead, by path relative to the root.
type liveRun struct {
	mu      sync.Mutex
	changed map[string]bool
	keep    map[string]fs.Entry
	kept    map[string]bool
}

func newLiveRun(keep map[string]fs.Entry) *liveRun {
	return &liveRun{changed: map[string]bool{}, keep: keep, kept: map[string]bool{}}
}

func (l *liveRun) mark(rel string) {
	l.mu.Lock()
	l.changed[rel] = true
	l.mu.Unlock()
}

func (l *liveRun) changedPaths() []string { return l.paths(l.changed) }

// keptPaths are the files stored as an earlier intact copy in this pass;
// a file in keep that has since disappeared is not among them.
func (l *liveRun) keptPaths() []string { return l.paths(l.kept) }

func (l *liveRun) paths(set map[string]bool) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (l *liveRun) wrap(rel string, e fs.Entry) fs.Entry {
	switch v := e.(type) {
	case fs.Directory:
		return &liveDir{Directory: v, rel: rel, run: l}
	case fs.File:
		if kept, ok := l.keep[rel]; ok {
			l.mu.Lock()
			l.kept[rel] = true
			l.mu.Unlock()
			return kept
		}
		return &liveFile{File: v, rel: rel, run: l}
	}
	return e
}

type liveDir struct {
	fs.Directory
	rel string
	run *liveRun
}

func (d *liveDir) Child(ctx context.Context, name string) (fs.Entry, error) {
	e, err := d.Directory.Child(ctx, name)
	if err != nil {
		return nil, err
	}
	return d.run.wrap(path.Join(d.rel, name), e), nil
}

func (d *liveDir) Iterate(ctx context.Context) (fs.DirectoryIterator, error) {
	it, err := d.Directory.Iterate(ctx)
	if err != nil {
		return nil, err
	}
	return &liveIterator{DirectoryIterator: it, dir: d}, nil
}

type liveIterator struct {
	fs.DirectoryIterator
	dir *liveDir
}

func (it *liveIterator) Next(ctx context.Context) (fs.Entry, error) {
	e, err := it.DirectoryIterator.Next(ctx)
	if e == nil {
		return nil, err
	}
	return it.dir.run.wrap(path.Join(it.dir.rel, e.Name()), e), err
}

type liveFile struct {
	fs.File
	rel string
	run *liveRun
}

// Open checks the file against its listing before the read and again when
// the uploader closes it. Kopia opens a large file once per part, so every
// part gets the same check.
func (f *liveFile) Open(ctx context.Context) (fs.Reader, error) {
	p := f.LocalFilesystemPath()
	before, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		// Gone since the listing: store nothing real and let the next
		// pass list the directory without it.
		f.run.mark(f.rel)
		return &emptyReader{entry: f}, nil
	}
	if err != nil {
		return nil, err
	}
	if !matchesEntry(f.File, before) {
		f.run.mark(f.rel)
	}
	r, err := f.File.Open(ctx)
	if errors.Is(err, os.ErrNotExist) {
		f.run.mark(f.rel)
		return &emptyReader{entry: f}, nil
	}
	if err != nil {
		return nil, err
	}
	return &liveReader{Reader: r, file: f, before: before}, nil
}

type liveReader struct {
	fs.Reader
	file   *liveFile
	before os.FileInfo
	once   sync.Once
}

// afterRead lets tests change a file between its read and the check that
// follows, which a concurrent writer could only do by chance.
var afterRead func(rel string)

func (r *liveReader) Close() error {
	err := r.Reader.Close()
	r.once.Do(func() {
		if afterRead != nil {
			afterRead(r.file.rel)
		}
		after, serr := os.Lstat(r.file.LocalFilesystemPath())
		if serr != nil || !sameVersion(r.before, after) {
			r.file.run.mark(r.file.rel)
		}
	})
	return err
}

func matchesEntry(e fs.Entry, fi os.FileInfo) bool {
	return e.Size() == fi.Size() && e.ModTime().Equal(fi.ModTime())
}

func sameVersion(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

type emptyReader struct {
	bytes.Reader
	entry fs.Entry
}

func (r *emptyReader) Close() error             { return nil }
func (r *emptyReader) Entry() (fs.Entry, error) { return r.entry, nil }
