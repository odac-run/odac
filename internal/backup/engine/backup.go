package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
)

// livePasses is how many times a directory is read before files that keep
// changing are given up on. Each pass after the first re-reads only what
// changed, because the previous pass is its cache.
const livePasses = 3

// BackupResult is a stored snapshot plus the paths behind its Torn and
// Stale counts, which only the run that made it knows. SQLitePaths are the
// databases stored as a consistent copy instead of as read.
type BackupResult struct {
	Snapshot
	TornPaths   []string `json:"tornPaths,omitempty"`
	StalePaths  []string `json:"stalePaths,omitempty"`
	SQLitePaths []string `json:"sqlitePaths,omitempty"`
}

// uploadPolicy is the policy for every ODAC upload, built from Kopia's
// defaults with the values that matter set explicitly.
func uploadPolicy() *policy.Tree {
	p := *policy.DefaultPolicy
	p.CompressionPolicy = policy.CompressionPolicy{CompressorName: compressor}
	p.SplitterPolicy = policy.SplitterPolicy{Algorithm: splitter}
	// No .kopiaignore: an app's data directory is the app's to write, and
	// a file there must not be able to drop other files from its backup.
	p.FilesPolicy = policy.FilesPolicy{}
	return policy.BuildTree(nil, &p)
}

func (r *Repo) newUploader(w repo.RepositoryWriter, tags map[string]string, p *Progress) *upload.Uploader {
	u := upload.NewUploader(w)
	u.DisableIgnoreRules = true
	if p != nil {
		u.Progress = &uploadProgress{p: p}
	}
	// Checkpoints let an interrupted first upload resume. They are saved as
	// incomplete snapshots, which listing skips.
	u.CheckpointLabels = tags
	return u
}

// BackupDir snapshots a directory that may be in use. SQLite databases in
// it, found by their header, are stored as consistent copies made before
// the first pass (SQLitePaths). Any other file whose size or mtime moves
// while it is read is read again on the next pass; one still moving after
// livePasses is stored as its last intact copy from an earlier snapshot
// (Stale), or, when there is none, as read (Torn).
func (r *Repo) BackupDir(ctx context.Context, src Source, dir string, meta Meta) (*BackupResult, error) {
	if err := src.validate(); err != nil {
		return nil, err
	}
	if err := meta.validate(); err != nil {
		return nil, err
	}
	if err := r.writable(); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("engine: source: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("engine: source %s is not a directory", dir)
	}
	root, err := localfs.Directory(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: source: %w", err)
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	dbs, err := r.copySQLite(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("engine: sqlite: %w", err)
	}
	defer dbs.remove()

	si := r.sourceInfo(src)
	var res *BackupResult
	err = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "odac:backup"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			saved, err := snapshot.ListSnapshots(ctx, w, si)
			if err != nil {
				return fmt.Errorf("engine: previous snapshots: %w", err)
			}
			prev, err := snapshot.FindPreviousManifests(ctx, w, si, nil)
			if err != nil {
				return fmt.Errorf("engine: previous snapshots: %w", err)
			}
			draftTags := tagsFor(src, KindDir, meta, 0, 0)

			var (
				man     *snapshot.Manifest
				changed []string
			)
			for pass := 0; pass < livePasses; pass++ {
				run := newLiveRun(nil, dbs)
				m, err := r.upload(ctx, w, &liveDir{Directory: root, run: run}, si, draftTags, prev, meta.Progress)
				if err != nil {
					return err
				}
				man, changed = m, run.changedPaths()
				if len(changed) == 0 {
					break
				}
				prev = append([]*snapshot.Manifest{m}, prev...)
			}

			torn := changed
			var stale []string
			if keep := intactCopies(ctx, w, saved, changed); len(keep) > 0 {
				// One more pass, with the intact copies standing in. What
				// is read in it and still moves has nothing to fall back to.
				run := newLiveRun(keep, dbs)
				m, err := r.upload(ctx, w, &liveDir{Directory: root, run: run}, si, draftTags, prev, meta.Progress)
				if err != nil {
					return err
				}
				man, torn, stale = m, run.changedPaths(), run.keptPaths()
			}

			man.Tags = tagsFor(src, KindDir, meta, len(torn), len(stale))
			man.Pins = []string{pin}
			if _, err := snapshot.SaveSnapshot(ctx, w, man); err != nil {
				return fmt.Errorf("engine: save snapshot: %w", err)
			}
			if err := dropCheckpoints(ctx, w, si); err != nil {
				return err
			}
			s, _ := fromManifest(man)
			res = &BackupResult{Snapshot: s, TornPaths: torn, StalePaths: stale, SQLitePaths: dbs.paths()}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// BackupStream snapshots a stream (a database dump) as one file called
// fileName. A stream that fails mid-way stores nothing.
func (r *Repo) BackupStream(ctx context.Context, src Source, fileName string, rd io.Reader, meta Meta) (*BackupResult, error) {
	if err := src.validate(); err != nil {
		return nil, err
	}
	if err := meta.validate(); err != nil {
		return nil, err
	}
	if err := validName("file name", fileName); err != nil {
		return nil, err
	}
	if err := r.writable(); err != nil {
		return nil, err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	si := r.sourceInfo(src)
	var res *BackupResult
	err := repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "odac:backup-stream"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			tags := tagsFor(src, KindStream, meta, 0, 0)
			file := virtualfs.StreamingFileWithModTimeFromReader(fileName, r.cfg.clock(), io.NopCloser(rd))
			root := virtualfs.NewStaticDirectory(src.Name, []fs.Entry{file})
			man, err := r.upload(ctx, w, root, si, tags, nil, meta.Progress)
			if err != nil {
				return err
			}
			man.Tags = tags
			man.Pins = []string{pin}
			if _, err := snapshot.SaveSnapshot(ctx, w, man); err != nil {
				return fmt.Errorf("engine: save snapshot: %w", err)
			}
			if err := dropCheckpoints(ctx, w, si); err != nil {
				return err
			}
			s, _ := fromManifest(man)
			res = &BackupResult{Snapshot: s}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (r *Repo) upload(ctx context.Context, w repo.RepositoryWriter, root fs.Entry, si snapshot.SourceInfo, tags map[string]string, prev []*snapshot.Manifest, p *Progress) (*snapshot.Manifest, error) {
	u := r.newUploader(w, tags, p)
	// The uploader stops only on Cancel. A canceled context alone surfaces
	// later, as a failed entry once some storage write sees it.
	stop := context.AfterFunc(ctx, u.Cancel)
	defer stop()
	m, err := u.Upload(ctx, root, uploadPolicy(), si, prev...)
	if err != nil {
		return nil, fmt.Errorf("engine: upload: %w", err)
	}
	if m.IncompleteReason != "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine: upload incomplete: %s", m.IncompleteReason)
	}
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil && m.RootEntry.DirSummary.FatalErrorCount > 0 {
		var msgs []string
		for _, e := range m.RootEntry.DirSummary.FailedEntries {
			msgs = append(msgs, e.EntryPath+": "+e.Error)
		}
		return nil, fmt.Errorf("engine: %d entries failed: %s", m.RootEntry.DirSummary.FatalErrorCount, strings.Join(msgs, "; "))
	}
	return m, nil
}

// intactCopies finds, for each path, the newest copy in an earlier
// finished snapshot that stored no torn file. A snapshot with any torn
// file is passed over whole, because which files were torn is not kept.
func intactCopies(ctx context.Context, rep repo.Repository, saved []*snapshot.Manifest, paths []string) map[string]fs.Entry {
	if len(paths) == 0 {
		return nil
	}
	var candidates []*snapshot.Manifest
	for _, m := range saved {
		if s, ok := fromManifest(m); ok && s.Torn == 0 {
			candidates = append(candidates, m)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].StartTime.After(candidates[j].StartTime)
	})

	roots := make([]fs.Entry, len(candidates))
	keep := map[string]fs.Entry{}
	for _, p := range paths {
		for i, m := range candidates {
			if roots[i] == nil {
				e, err := snapshotfs.SnapshotRoot(rep, m)
				if err != nil {
					continue
				}
				roots[i] = e
			}
			if e := lookup(ctx, roots[i], p); e != nil {
				keep[p] = e
				break
			}
		}
	}
	return keep
}

func lookup(ctx context.Context, root fs.Entry, rel string) fs.Entry {
	cur := root
	for _, part := range strings.Split(rel, "/") {
		d, ok := cur.(fs.Directory)
		if !ok {
			return nil
		}
		e, err := d.Child(ctx, part)
		if err != nil {
			return nil
		}
		cur = e
	}
	if _, ok := cur.(fs.File); !ok {
		return nil
	}
	return cur
}

// dropCheckpoints removes the incomplete snapshots an upload of this
// source left behind once a finished one is saved.
func dropCheckpoints(ctx context.Context, w repo.RepositoryWriter, si snapshot.SourceInfo) error {
	ms, err := snapshot.ListSnapshots(ctx, w, si)
	if err != nil {
		return fmt.Errorf("engine: list checkpoints: %w", err)
	}
	for _, m := range ms {
		if m.IncompleteReason == "" {
			continue
		}
		if err := w.DeleteManifest(ctx, m.ID); err != nil {
			return fmt.Errorf("engine: drop checkpoint: %w", err)
		}
	}
	return nil
}
