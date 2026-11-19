package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kopia/kopia/fs"
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
	st, err := restore.Entry(ctx, r.rep, out, root, restore.Options{
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
