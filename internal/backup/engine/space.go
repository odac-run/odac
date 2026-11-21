package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// SpaceReport is what a repository's storage looks like, for the manager's
// space decisions. Free and Total are -1 where the storage has no notion
// of free space. The rest is filled only by Space, which walks every
// snapshot.
type SpaceReport struct {
	Free     int64 `json:"free"`
	Total    int64 `json:"total"`
	Detailed bool  `json:"detailed,omitempty"`
	// Garbage is the packed size of the contents no snapshot references,
	// deleted ones included: what maintenance is still returning. A
	// decision that ignores it deletes again for space already on its way
	// back, because under SafetyFull it takes several full runs.
	Garbage int64            `json:"garbage,omitempty"`
	Jobs    map[string]int64 `json:"jobs,omitempty"`
	Freed   []Freed          `json:"freed,omitempty"`
}

// Freed is what deleting a prefix of an order frees: Repo for the whole
// repository, Job for the job of the run at that position alone.
type Freed struct {
	Repo int64 `json:"repo"`
	Job  int64 `json:"job"`
}

// FreeSpace reports a filesystem repository's disk; -1 for both where
// there is none to report.
func (r *Repo) FreeSpace() SpaceReport {
	out := SpaceReport{Free: -1, Total: -1}
	if fs := r.cfg.Storage.Filesystem; fs != nil {
		if avail, total, ok := diskSpace(fs.Path); ok {
			out.Free, out.Total = avail, total
		}
	}
	return out
}

// use is what is known about one content: whether a snapshot outside the
// order references it, the last order position that does, and the same
// per job.
type use struct {
	kept bool
	last int32
	jobs []jobUse
}

type jobUse struct {
	job  int32
	kept bool
	last int32
}

// scope is a set of snapshots walked together: one job's snapshots outside
// the order (pos -1), or one run of the order.
type scope struct {
	job int32
	pos int32
}

// Space walks every snapshot and reports, besides FreeSpace, the garbage,
// each job's referenced size and what deleting each prefix of order (runs
// as lists of snapshot IDs) would free. Contents are attributed by the
// snapshots that reference them, so a content shared by two runs is freed
// only once both are gone, and one shared with a snapshot outside the
// order never is.
func (r *Repo) Space(ctx context.Context, order [][]string) (SpaceReport, error) {
	out := r.FreeSpace()
	if err := r.err(); err != nil {
		return out, err
	}
	ids, err := snapshot.ListSnapshotManifests(ctx, r.rep, nil, nil)
	if err != nil {
		return out, fmt.Errorf("engine: space: %w", err)
	}
	ms, err := snapshot.LoadSnapshots(ctx, r.rep, ids)
	if err != nil {
		return out, fmt.Errorf("engine: space: %w", err)
	}

	position := map[manifest.ID]int32{}
	for i, run := range order {
		for _, id := range run {
			position[manifest.ID(id)] = int32(i)
		}
	}
	var jobNames []string
	jobIndex := map[string]int32{}
	jobOf := func(name string) int32 {
		if i, ok := jobIndex[name]; ok {
			return i
		}
		jobIndex[name] = int32(len(jobNames))
		jobNames = append(jobNames, name)
		return jobIndex[name]
	}
	posJob := make([]int32, len(order))
	for i := range posJob {
		posJob[i] = -1
	}
	scopes := map[scope][]*snapshot.Manifest{}
	var scopeOrder []scope
	for _, m := range ms {
		job := jobOf(m.Tags[tagJob])
		pos, ok := position[m.ID]
		if !ok {
			pos = -1
		} else {
			posJob[pos] = job
		}
		sc := scope{job: job, pos: pos}
		if _, seen := scopes[sc]; !seen {
			scopeOrder = append(scopeOrder, sc)
		}
		scopes[sc] = append(scopes[sc], m)
	}

	var mu sync.Mutex
	uses := map[string]*use{}
	for _, sc := range scopeOrder {
		if err := r.walkScope(ctx, sc, scopes[sc], &mu, uses); err != nil {
			return out, err
		}
	}

	repoFreed := make([]int64, len(order))
	jobFreed := make([]int64, len(order))
	jobBytes := make([]int64, len(jobNames))
	var buf [128]byte
	err = r.rep.ContentReader().IterateContents(ctx, content.IterateOptions{IncludeDeleted: true}, func(ci content.Info) error {
		if ci.ContentID.Prefix() == manifest.ContentPrefix {
			return nil
		}
		n := int64(ci.PackedLength)
		u := uses[string(ci.ContentID.Append(buf[:0]))]
		if u == nil {
			out.Garbage += n
			return nil
		}
		if !u.kept && u.last >= 0 {
			repoFreed[u.last] += n
		}
		for _, ju := range u.jobs {
			jobBytes[ju.job] += n
			if !ju.kept && ju.last >= 0 {
				jobFreed[ju.last] += n
			}
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("engine: space: %w", err)
	}

	out.Detailed = true
	out.Jobs = map[string]int64{}
	for i, name := range jobNames {
		if name != "" && jobBytes[i] > 0 {
			out.Jobs[name] = jobBytes[i]
		}
	}
	if len(order) > 0 {
		out.Freed = make([]Freed, len(order))
		var repoSum int64
		jobSum := map[int32]int64{}
		for i := range order {
			repoSum += repoFreed[i]
			jobSum[posJob[i]] += jobFreed[i]
			out.Freed[i] = Freed{Repo: repoSum, Job: jobSum[posJob[i]]}
		}
	}
	return out, ctx.Err()
}

// walkScope records every content the scope's snapshots reference. One
// walker per scope: a walker visits each object once, which is right
// within a scope and would hide a shared object from the next one.
func (r *Repo) walkScope(ctx context.Context, sc scope, ms []*snapshot.Manifest, mu *sync.Mutex, uses map[string]*use) error {
	w, err := snapshotfs.NewTreeWalker(ctx, snapshotfs.TreeWalkerOptions{
		EntryCallback: func(ctx context.Context, _ fs.Entry, oid object.ID, _ string) error {
			cids, err := r.rep.VerifyObject(ctx, oid)
			if err != nil {
				return fmt.Errorf("verify %v: %w", oid, err)
			}
			var buf [128]byte
			mu.Lock()
			defer mu.Unlock()
			for _, cid := range cids {
				key := string(cid.Append(buf[:0]))
				u := uses[key]
				if u == nil {
					u = &use{last: -1}
					uses[key] = u
				}
				u.mark(sc)
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("engine: space: %w", err)
	}
	defer w.Close(ctx)
	for _, m := range ms {
		root, err := snapshotfs.SnapshotRoot(r.rep, m)
		if err != nil {
			return fmt.Errorf("engine: space: snapshot %s: %w", m.ID, err)
		}
		if err := w.Process(ctx, root, ""); err != nil {
			return fmt.Errorf("engine: space: snapshot %s: %w", m.ID, err)
		}
	}
	return nil
}

func (u *use) mark(sc scope) {
	if sc.pos < 0 {
		u.kept = true
	} else {
		u.last = max(u.last, sc.pos)
	}
	for i := range u.jobs {
		if u.jobs[i].job == sc.job {
			if sc.pos < 0 {
				u.jobs[i].kept = true
			} else {
				u.jobs[i].last = max(u.jobs[i].last, sc.pos)
			}
			return
		}
	}
	ju := jobUse{job: sc.job, last: -1}
	if sc.pos < 0 {
		ju.kept = true
	} else {
		ju.last = sc.pos
	}
	u.jobs = append(u.jobs, ju)
}
