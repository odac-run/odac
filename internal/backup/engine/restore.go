package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// RestoreStats reports what a directory restore wrote.
type RestoreStats struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// RestoreDir writes a directory snapshot into target, which must be
// absent or an empty directory. It never merges into live data: an
// in-place restore restores beside the data and swaps, which is the
// caller's step. Snapshots of any server can be restored, so a repository
// opened read-only is enough.
func (r *Repo) RestoreDir(ctx context.Context, id, target string) (RestoreStats, error) {
	if err := r.err(); err != nil {
		return RestoreStats{}, err
	}
	if !filepath.IsAbs(target) {
		return RestoreStats{}, errors.New("engine: restore target must be an absolute path")
	}
	if err := emptyOrAbsent(target); err != nil {
		return RestoreStats{}, err
	}
	m, s, err := r.load(ctx, id)
	if err != nil {
		return RestoreStats{}, err
	}
	if s.Kind != KindDir {
		return RestoreStats{}, ErrKind
	}
	root, err := snapshotfs.SnapshotRoot(r.rep, m)
	if err != nil {
		return RestoreStats{}, fmt.Errorf("engine: restore: %w", err)
	}

	out := &restore.FilesystemOutput{
		TargetPath:             target,
		OverwriteDirectories:   false,
		OverwriteFiles:         false,
		OverwriteSymlinks:      false,
		IgnorePermissionErrors: false,
		WriteFilesAtomically:   true,
		FlushFiles:             true,
		// Ownership is part of app data (a database refuses files it does
		// not own), and only root can give it back. A development run as
		// an ordinary user restores the files under its own uid.
		SkipOwners: os.Geteuid() != 0,
	}
	if err := out.Init(ctx); err != nil {
		return RestoreStats{}, fmt.Errorf("engine: restore: %w", err)
	}
	st, err := restore.Entry(ctx, r.rep, &guardedOutput{FilesystemOutput: out}, root, restore.Options{
		Parallel: runtime.NumCPU(),
		// Kopia's zero value here writes placeholders instead of files
		// below the root. Every entry is restored in full.
		RestoreDirEntryAtDepth: math.MaxInt32,
		MinSizeForPlaceholder:  0,
		IgnoreErrors:           false,
	})
	if err != nil {
		return RestoreStats{}, fmt.Errorf("engine: restore: %w", err)
	}
	return RestoreStats{Files: int64(st.RestoredFileCount), Bytes: st.RestoredTotalFileSize}, nil
}

// ErrUnsafeEntry is a snapshot entry that would be written outside the
// restore target: a name that climbs out, or a path through a symlink the
// snapshot itself restored. A repository is only as trustworthy as
// whoever could write to its target, and another server's directory is
// read in a migration.
var ErrUnsafeEntry = errors.New("engine: snapshot entry leads outside the restore target")

// guardedOutput is Kopia's filesystem output with every path checked
// first. Kopia joins entry names into paths as they are, so a crafted
// snapshot could name ".." or restore a symlink and then a file below a
// directory of the same name. Here every name must stay local, every
// directory a path passes through must be a real directory, never a
// symlink, and nothing is written over an entry that already exists.
type guardedOutput struct {
	*restore.FilesystemOutput
	dirs sync.Map // relative path -> verified real directory
}

func (g *guardedOutput) check(rel string, leaf bool) error {
	if rel == "" {
		return nil
	}
	if path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return fmt.Errorf("%w: %q", ErrUnsafeEntry, rel)
	}
	parent := path.Dir(rel)
	for _, dir := range ancestors(parent) {
		if _, ok := g.dirs.Load(dir); ok {
			continue
		}
		st, err := os.Lstat(filepath.Join(g.TargetPath, filepath.FromSlash(dir)))
		if err != nil || !st.IsDir() {
			return fmt.Errorf("%w: %q is not a directory", ErrUnsafeEntry, dir)
		}
		g.dirs.Store(dir, true)
	}
	if leaf {
		if _, err := os.Lstat(filepath.Join(g.TargetPath, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %q is restored twice", ErrUnsafeEntry, rel)
		}
	}
	return nil
}

// ancestors lists dir and every directory above it, outermost first,
// without the root.
func ancestors(dir string) []string {
	var out []string
	for dir != "." && dir != "/" && dir != "" {
		out = append(out, dir)
		dir = path.Dir(dir)
	}
	slices.Reverse(out)
	return out
}

func (g *guardedOutput) BeginDirectory(ctx context.Context, rel string, e fs.Directory) error {
	if err := g.check(rel, true); err != nil {
		return err
	}
	return g.FilesystemOutput.BeginDirectory(ctx, rel, e)
}

// FinishDirectory sets a directory's attributes, then Kopia removes the
// placeholder a shallow restore would have left beside it. For the root
// that is a sibling of the target, which in a helper is the read-only root
// file system: removing a file that is not there fails there with EROFS
// rather than ENOENT, after the attributes were set.
func (g *guardedOutput) FinishDirectory(ctx context.Context, rel string, e fs.Directory) error {
	if err := g.check(rel, false); err != nil {
		return err
	}
	err := g.FilesystemOutput.FinishDirectory(ctx, rel, e)
	if err != nil && rel == "" {
		if _, serr := os.Lstat(g.TargetPath + localfs.ShallowEntrySuffix); errors.Is(serr, os.ErrNotExist) {
			return nil
		}
	}
	return err
}

func (g *guardedOutput) WriteFile(ctx context.Context, rel string, f fs.File, cb restore.FileWriteProgress) error {
	if err := g.check(rel, true); err != nil {
		return err
	}
	return g.FilesystemOutput.WriteFile(ctx, rel, f, cb)
}

func (g *guardedOutput) CreateSymlink(ctx context.Context, rel string, e fs.Symlink) error {
	if err := g.check(rel, true); err != nil {
		return err
	}
	return g.FilesystemOutput.CreateSymlink(ctx, rel, e)
}

// FileExists and SymlinkExists answer false, so an entry is always
// written and its check above always runs: the target starts empty, and
// an entry found there was restored a moment ago under the same name.
func (g *guardedOutput) FileExists(context.Context, string, fs.File) bool { return false }

func (g *guardedOutput) SymlinkExists(context.Context, string, fs.Symlink) bool { return false }

// RestoreStream writes a stream snapshot's bytes to w and returns how many
// were written.
func (r *Repo) RestoreStream(ctx context.Context, id string, w io.Writer) (int64, error) {
	if err := r.err(); err != nil {
		return 0, err
	}
	m, s, err := r.load(ctx, id)
	if err != nil {
		return 0, err
	}
	if s.Kind != KindStream {
		return 0, ErrKind
	}
	root, err := snapshotfs.SnapshotRoot(r.rep, m)
	if err != nil {
		return 0, fmt.Errorf("engine: restore: %w", err)
	}
	dir, ok := root.(fs.Directory)
	if !ok {
		return 0, errors.New("engine: stream snapshot has no directory root")
	}
	entries, err := fs.GetAllEntries(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("engine: restore: %w", err)
	}
	if len(entries) != 1 {
		return 0, fmt.Errorf("engine: stream snapshot holds %d entries", len(entries))
	}
	f, ok := entries[0].(fs.File)
	if !ok {
		return 0, errors.New("engine: stream snapshot entry is not a file")
	}
	rd, err := f.Open(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: restore: %w", err)
	}
	defer rd.Close()
	n, err := io.Copy(w, rd)
	if err != nil {
		return n, fmt.Errorf("engine: restore: %w", err)
	}
	return n, nil
}

func emptyOrAbsent(dir string) error {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("engine: restore target: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("engine: restore target: %w", err)
	}
	if !fi.IsDir() {
		return ErrTargetNotEmpty
	}
	if names, _ := f.Readdirnames(1); len(names) > 0 {
		return ErrTargetNotEmpty
	}
	return nil
}
