package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"odac/internal/backup/consistency"
)

// historyMax bounds the run records kept, across jobs.
const historyMax = 200

// Run statuses.
const (
	StatusOK = "ok"
	// StatusPartial: some pieces were stored and some were not.
	StatusPartial = "partial"
	StatusFailed  = "failed"
	// StatusDeferred: nothing ran, for a reason that is not the backup's
	// (Docker did not answer, a deploy held the app); it is tried again
	// soon and does not count as a failure.
	StatusDeferred = "deferred"
)

// Record kinds.
const (
	KindBackup   = "backup"
	KindMaintain = "maintain"
)

// RunRecord is one finished run or maintenance, as the history keeps it
// for the API and the Hub. Times are Unix milliseconds.
type RunRecord struct {
	Kind    string `json:"kind"`
	Run     string `json:"run,omitempty"`
	Job     string `json:"job,omitempty"`
	Target  string `json:"target"`
	Trigger string `json:"trigger,omitempty"`
	Status  string `json:"status"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	// Snapshots and Bytes are what the run stored.
	Snapshots int         `json:"snapshots,omitempty"`
	Bytes     int64       `json:"bytes,omitempty"`
	Apps      []AppResult `json:"apps,omitempty"`
	// Server is the result for ODAC's own state, when the job takes it.
	Server *AppResult `json:"server,omitempty"`
	// Deleted counts the backups thinning and space decisions removed
	// after the run.
	Deleted int    `json:"deleted,omitempty"`
	Error   string `json:"error,omitempty"`
	// Adopted: the server before an update started the run and this one
	// learned its result from the helper's log.
	Adopted bool `json:"adopted,omitempty"`
}

// AppResult is one app's part of a run.
type AppResult struct {
	App    string `json:"app"`
	Status string `json:"status"`
	// Method is how the data was read: the engine's dump, files only, or
	// a dump reused from another job's run moments before.
	Method      string                `json:"method,omitempty"`
	Consistency string                `json:"consistency,omitempty"`
	Warnings    []consistency.Warning `json:"warnings,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// Methods an app was read with.
const (
	MethodDump   = "dump"
	MethodFiles  = "files"
	MethodReused = "reused"
)

type jobState struct {
	// Next is when the job runs next; it is written when a run starts, so
	// a run handed over in an update is not started again by the next
	// server.
	Next     int64  `json:"next"`
	Running  string `json:"running,omitempty"`
	Failures int    `json:"failures,omitempty"`
	// PostponedSince is when the server's load first held the job back.
	PostponedSince int64 `json:"postponedSince,omitempty"`
	// Manual is when the operator asked for a run that has not started
	// yet; it runs beside the schedule, which keeps its own clock.
	Manual int64 `json:"manual,omitempty"`
}

type targetState struct {
	NextMaintain int64 `json:"nextMaintain,omitempty"`
	LastMaintain int64 `json:"lastMaintain,omitempty"`
	// Garbage and the sizes are the last measurement, for the Hub and for
	// how soon the next maintenance runs.
	Garbage int64 `json:"garbage,omitempty"`
	Used    int64 `json:"used,omitempty"`
	Free    int64 `json:"free,omitempty"`
	Total   int64 `json:"total,omitempty"`
	// Partial are runs that stored only some of their pieces. They stay
	// out of thinning and go once a newer complete run of their job
	// exists, so a partial restore point never outlives a complete one.
	Partial []string `json:"partial,omitempty"`
}

// dumpRecord is the last dump stored for one app, which a run of another
// job may reuse instead of dumping the database again.
type dumpRecord struct {
	Target      string `json:"target"`
	Snapshot    string `json:"snapshot"`
	Name        string `json:"name"`
	File        string `json:"file"`
	Consistency string `json:"consistency"`
	At          int64  `json:"at"`
}

// followUp is the rest of a run whose dumps a deploy cancelled: the same
// run, for those apps only, once the deploy has ended.
type followUp struct {
	Run       string   `json:"run"`
	Apps      []string `json:"apps"`
	Until     int64    `json:"until"`
	Scheduled int64    `json:"scheduled"`
	// Clean: nothing else of the run failed, so the run is complete once
	// these apps are stored.
	Clean bool `json:"clean,omitempty"`
}

type stateData struct {
	Jobs      map[string]*jobState    `json:"jobs,omitempty"`
	Targets   map[string]*targetState `json:"targets,omitempty"`
	Dumps     map[string]*dumpRecord  `json:"dumps,omitempty"`
	FollowUps map[string]*followUp    `json:"followUps,omitempty"`
	History   []RunRecord             `json:"history,omitempty"`
	Restores  []RestoreResult         `json:"restores,omitempty"`
}

// store keeps the manager's state in <base>/backup/state.json, beside the
// owner sequence files and outside the config store: it changes with every
// run, and the config module is the operator's.
type store struct {
	path string
	mu   sync.Mutex
	d    stateData
}

func stateFile(base string) string { return filepath.Join(base, "backup", "state.json") }

// openStore loads the state from path.
func openStore(path string) (*store, error) {
	s := &store{path: path}
	s.d.init()
	return s, s.load()
}

// load reads the state again. The manager loads it at every Start rather
// than once: the server is built while the one it replaces in an update
// still runs and writes this file, until its Stop. A damaged file is set
// aside and replaced by an empty state: it only holds schedules and
// history, and refusing to start would leave every job without backups.
func (s *store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var d stateData
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = nil
	case err != nil:
		return err
	default:
		if jerr := json.Unmarshal(raw, &d); jerr != nil {
			os.Rename(s.path, s.path+".damaged")
			d = stateData{}
			err = fmt.Errorf("backup state %s was damaged and set aside: %w", s.path, jerr)
		}
	}
	d.init()
	s.d = d
	return err
}

func (d *stateData) init() {
	if d.Jobs == nil {
		d.Jobs = map[string]*jobState{}
	}
	if d.Targets == nil {
		d.Targets = map[string]*targetState{}
	}
	if d.Dumps == nil {
		d.Dumps = map[string]*dumpRecord{}
	}
	if d.FollowUps == nil {
		d.FollowUps = map[string]*followUp{}
	}
}

// update changes the state under its lock and writes it.
func (s *store) update(fn func(d *stateData)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
	return s.saveLocked()
}

// view reads the state under its lock.
func (s *store) view(fn func(d *stateData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}

func (s *store) saveLocked() error {
	if len(s.d.History) > historyMax {
		s.d.History = slices.Clone(s.d.History[len(s.d.History)-historyMax:])
	}
	raw, err := json.Marshal(&s.d)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
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
	return os.Rename(tmp.Name(), s.path)
}

func (d *stateData) job(id string) *jobState {
	j := d.Jobs[id]
	if j == nil {
		j = &jobState{}
		d.Jobs[id] = j
	}
	return j
}

func (d *stateData) target(id string) *targetState {
	t := d.Targets[id]
	if t == nil {
		t = &targetState{}
		d.Targets[id] = t
	}
	return t
}
