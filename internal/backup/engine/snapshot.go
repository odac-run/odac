package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
)

// Source is what one snapshot covers. Its Kopia path is
// "/<job>/<app>/<name>", so each job's history of each piece of data is
// its own Kopia source.
type Source struct {
	Job  string `json:"job"`
	App  string `json:"app"`
	Name string `json:"name"`
}

// Kinds of snapshot content.
const (
	KindDir    = "dir"
	KindStream = "stream"
)

// Consistency levels a snapshot can carry, reported as is to the Hub.
const (
	Consistent      = "consistent"
	CrashConsistent = "crash-consistent"
	Live            = "live"
)

// Meta is what the caller knows about a snapshot that the engine cannot
// work out itself.
type Meta struct {
	// Run names the job run the snapshot belongs to. A run is the unit
	// thinning keeps or deletes, so an app and its linked database always
	// come back from the same moment.
	Run         string
	Trigger     string
	Consistency string
	// Progress, when set, is advanced while the snapshot is read.
	Progress *Progress
}

// Snapshot describes one stored snapshot.
type Snapshot struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	Source      Source    `json:"source"`
	Kind        string    `json:"kind"`
	Run         string    `json:"run"`
	Trigger     string    `json:"trigger"`
	Consistency string    `json:"consistency"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Files       int64     `json:"files"`
	Bytes       int64     `json:"bytes"`
	// Torn counts files that kept changing on every read and had no
	// earlier intact copy, so the stored copy may be inconsistent.
	Torn int `json:"torn,omitempty"`
	// Stale counts files that kept changing and were stored as their last
	// intact copy from an earlier snapshot instead.
	Stale int `json:"stale,omitempty"`
}

// Filter selects snapshots. Empty fields match everything, except Host,
// which defaults to this server.
type Filter struct {
	Host string
	Job  string
	App  string
	Name string
	Run  string
}

const (
	tagJob         = "tag:odac-job"
	tagApp         = "tag:odac-app"
	tagName        = "tag:odac-name"
	tagKind        = "tag:odac-kind"
	tagRun         = "tag:odac-run"
	tagTrigger     = "tag:odac-trigger"
	tagConsistency = "tag:odac-consistency"
	tagTorn        = "tag:odac-torn"
	tagStale       = "tag:odac-stale"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validName(what, s string) error {
	if !namePattern.MatchString(s) {
		return fmt.Errorf("engine: invalid %s %q", what, s)
	}
	return nil
}

func (s Source) validate() error {
	if err := validName("job", s.Job); err != nil {
		return err
	}
	if err := validName("app", s.App); err != nil {
		return err
	}
	return validName("source name", s.Name)
}

func (m Meta) validate() error {
	if err := validName("run", m.Run); err != nil {
		return err
	}
	if err := validName("trigger", m.Trigger); err != nil {
		return err
	}
	switch m.Consistency {
	case Consistent, CrashConsistent, Live:
		return nil
	}
	return fmt.Errorf("engine: invalid consistency %q", m.Consistency)
}

func (s Source) path() string { return "/" + s.Job + "/" + s.App + "/" + s.Name }

func (r *Repo) sourceInfo(s Source) snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: r.cfg.ServerID, UserName: userName, Path: s.path()}
}

func tagsFor(s Source, kind string, m Meta, torn, stale int) map[string]string {
	return map[string]string{
		tagJob:         s.Job,
		tagApp:         s.App,
		tagName:        s.Name,
		tagKind:        kind,
		tagRun:         m.Run,
		tagTrigger:     m.Trigger,
		tagConsistency: m.Consistency,
		tagTorn:        strconv.Itoa(torn),
		tagStale:       strconv.Itoa(stale),
	}
}

// fromManifest converts a manifest, reporting false for anything ODAC did
// not write as a finished snapshot: checkpoints and snapshots someone
// made with the kopia CLI are invisible to listing and thinning.
func fromManifest(m *snapshot.Manifest) (Snapshot, bool) {
	if m.IncompleteReason != "" || m.Tags[tagJob] == "" || m.Source.UserName != userName {
		return Snapshot{}, false
	}
	s := Snapshot{
		ID:   string(m.ID),
		Host: m.Source.Host,
		Source: Source{
			Job:  m.Tags[tagJob],
			App:  m.Tags[tagApp],
			Name: m.Tags[tagName],
		},
		Kind:        m.Tags[tagKind],
		Run:         m.Tags[tagRun],
		Trigger:     m.Tags[tagTrigger],
		Consistency: m.Tags[tagConsistency],
		Start:       m.StartTime.ToTime(),
		End:         m.EndTime.ToTime(),
	}
	s.Torn, _ = strconv.Atoi(m.Tags[tagTorn])
	s.Stale, _ = strconv.Atoi(m.Tags[tagStale])
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		s.Files = m.RootEntry.DirSummary.TotalFileCount
		s.Bytes = m.RootEntry.DirSummary.TotalFileSize
	}
	return s, true
}

// List returns the matching snapshots, oldest first.
func (r *Repo) List(ctx context.Context, f Filter) ([]Snapshot, error) {
	if err := r.err(); err != nil {
		return nil, err
	}
	ms, err := r.find(ctx, r.rep, f)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(ms))
	for _, m := range ms {
		if s, ok := fromManifest(m); ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r *Repo) find(ctx context.Context, rep repo.Repository, f Filter) ([]*snapshot.Manifest, error) {
	host := f.Host
	if host == "" {
		host = r.cfg.ServerID
	}
	labels := map[string]string{
		manifest.TypeLabelKey:  snapshot.ManifestType,
		snapshot.HostnameLabel: host,
		snapshot.UsernameLabel: userName,
	}
	for k, v := range map[string]string{tagJob: f.Job, tagApp: f.App, tagName: f.Name, tagRun: f.Run} {
		if v != "" {
			labels[k] = v
		}
	}
	entries, err := rep.FindManifests(ctx, labels)
	if err != nil {
		return nil, fmt.Errorf("engine: list: %w", err)
	}
	ids := make([]manifest.ID, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	ms, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, fmt.Errorf("engine: load snapshots: %w", err)
	}
	return ms, nil
}

// Delete removes snapshots by id. Every id is checked before anything is
// deleted: one that belongs to another server fails the whole call with
// ErrForeign, and one that is already gone is skipped, so a retried delete
// succeeds. The space comes back only after maintenance.
func (r *Repo) Delete(ctx context.Context, ids []string) error {
	if err := r.writable(); err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "odac:delete"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			var doomed []manifest.ID
			for _, id := range ids {
				m, err := snapshot.LoadSnapshot(ctx, w, manifest.ID(id))
				if errors.Is(err, snapshot.ErrSnapshotNotFound) || errors.Is(err, manifest.ErrNotFound) {
					continue
				}
				if err != nil {
					return fmt.Errorf("engine: load %s: %w", id, err)
				}
				if m.Source.Host != r.cfg.ServerID || m.Source.UserName != userName {
					return fmt.Errorf("%w: %s", ErrForeign, id)
				}
				doomed = append(doomed, m.ID)
			}
			for _, id := range doomed {
				if err := w.DeleteManifest(ctx, id); err != nil {
					return fmt.Errorf("engine: delete %s: %w", id, err)
				}
			}
			return nil
		})
}

// load returns one finished ODAC snapshot.
func (r *Repo) load(ctx context.Context, id string) (*snapshot.Manifest, Snapshot, error) {
	m, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if errors.Is(err, snapshot.ErrSnapshotNotFound) || errors.Is(err, manifest.ErrNotFound) {
		return nil, Snapshot{}, ErrNotFound
	}
	if err != nil {
		return nil, Snapshot{}, fmt.Errorf("engine: load %s: %w", id, err)
	}
	s, ok := fromManifest(m)
	if !ok {
		return nil, Snapshot{}, ErrNotFound
	}
	return m, s, nil
}
