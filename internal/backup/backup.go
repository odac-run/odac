// Package backup is the server's backup manager. It runs the jobs the
// settings declare (internal/backup/settings) on their schedule, one heavy
// backup at a time, and keeps each target's repository in shape: thinning
// after every run, maintenance and space decisions on their own schedule,
// never a backup and maintenance in one repository at the same time.
//
// Every run goes through the runner (internal/backup/runner), which starts
// odac-backup in a helper container; the server never links the engine.
// Each app is read the way its consistency plan says
// (internal/backup/consistency): a database through its dump tool, the
// rest as files. A dump gives way to a deploy, since a migration queued
// behind a dump's lock queues every query behind it: a deploy cancels the
// dump and the rest of the run follows once the deploy has ended. A dump
// stored by another job moments before is read back instead of dumping
// the database again.
//
// Nothing here rides the 1-second check tick: the schedule runs in its own
// goroutine and every run in a tracked goroutine that Stop ends. A server
// being replaced by an update releases its runs (their helpers keep
// running), and the next server adopts them once the handover is complete
// and before its own first run.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/docker"
	"odac/internal/logx"
	"odac/internal/sysinfo"
)

// Settings is the slice of the backup settings the manager reads and
// reports to; *settings.Settings implements it.
type Settings interface {
	Jobs() ([]settings.JobView, error)
	Retired() ([]settings.RetiredView, error)
	Access(id string, opts settings.AccessOptions) (settings.Access, error)
	Observe(id string, err error)
	Purged(id string) error
	Snapshots() (settings.Snapshots, error)
}

// Apps lists the apps and describes one for a backup; the app manager
// implements it.
type Apps interface {
	BackupApps() []settings.App
	BackupApp(name string) (consistency.App, bool)
}

// AppStates reads an app's running container; *docker.Client implements
// it.
type AppStates interface {
	BackupAppState(ctx context.Context, name string) (docker.AppState, error)
}

// Planner decides how an app is read; *consistency.Planner implements it.
type Planner interface {
	Plan(ctx context.Context, app consistency.App, run string) (*consistency.Plan, error)
}

// Runner runs odac-backup; FromRunner adapts *runner.Runner. Release is
// final, which is why the manager makes a new one on every Start.
type Runner interface {
	Run(ctx context.Context, inv runner.Invocation) (*proto.Result, error)
	// Load runs a restore's load: a dump container reading input.
	Load(ctx context.Context, spec docker.DumpSpec, input io.Reader, labels map[string]string) (string, error)
	Release()
	Adopt() ([]Orphan, error)
}

// Orphan is a helper the previous server handed over.
type Orphan interface {
	Labels() map[string]string
	Follow(ctx context.Context, h proto.Handler) (*proto.Result, error)
}

// FromRunner adapts the runner to Runner.
func FromRunner(r *runner.Runner) Runner { return runnerAdapter{r} }

type runnerAdapter struct{ *runner.Runner }

func (a runnerAdapter) Adopt() ([]Orphan, error) {
	list, err := a.Runner.Adopt()
	out := make([]Orphan, len(list))
	for i, o := range list {
		out[i] = o
	}
	return out, err
}

// Config wires a Manager.
type Config struct {
	Apps      Apps
	States    AppStates
	Planner   Planner
	NewRunner func() (Runner, error)
	// Load reads the host's load, which postpones a run on a busy server;
	// nil never postpones.
	Load func() sysinfo.Load
	// BaseDir is ODAC's base directory: the manager keeps its state and
	// the repositories' caches under <base>/backup.
	BaseDir string
	// ServerDirs are ODAC's own state, read for a job that takes it; nil
	// means config, cert, db and mail under BaseDir.
	ServerDirs []string
	// CacheBytes bounds each repository's cache; zero is the engine's
	// default.
	CacheBytes int64
	// OnRecord sees every finished run and maintenance, for the Hub.
	OnRecord func(RunRecord)
	// Control and Loader make restores possible: the app manager's side
	// (stopping, starting and creating apps) and the planner's (loading
	// dumps). Without them a run stores no app definitions and every
	// restore is refused.
	Control AppControl
	Loader  Loader
	// OnRestore sees every finished restore, for the Hub.
	OnRestore func(RestoreResult)
	// ServerID names this server in the snapshot repository; without it
	// there are no snapshots.
	ServerID string
	// Disk measures the disk the snapshot repository lives on; nil is the
	// host's.
	Disk Disk
	// OnSnapshot sees every snapshot taken or given up, and every
	// maintenance of the snapshot repository, for the Hub.
	OnSnapshot func(SnapshotRecord)
	Log        *logx.Logger
	Now        func() time.Time
}

// timing holds the manager's intervals; tests shrink them.
type timing struct {
	poll          time.Duration
	adoptRetry    time.Duration
	dockerRetry   time.Duration
	loadRetry     time.Duration
	maxPostpone   time.Duration
	deploySettle  time.Duration
	reuseWindow   time.Duration
	maintainDue   time.Duration
	maintainSoon  time.Duration
	firstMaintain time.Duration
	runMin        time.Duration
	runMax        time.Duration
	finishLimit   time.Duration
	verifyEvery   time.Duration
	verifyLimit   time.Duration
	peerActive    time.Duration
	snap          snapTiming
}

func defaultTiming() timing {
	return timing{
		poll:          time.Minute,
		adoptRetry:    30 * time.Second,
		dockerRetry:   5 * time.Minute,
		loadRetry:     5 * time.Minute,
		maxPostpone:   2 * time.Hour,
		deploySettle:  2 * time.Minute,
		reuseWindow:   15 * time.Minute,
		maintainDue:   24 * time.Hour,
		maintainSoon:  6 * time.Hour,
		firstMaintain: time.Hour,
		runMin:        time.Hour,
		runMax:        12 * time.Hour,
		finishLimit:   time.Minute,
		verifyEvery:   7 * 24 * time.Hour,
		verifyLimit:   6 * time.Hour,
		peerActive:    7 * 24 * time.Hour,
		snap:          defaultSnapTiming(),
	}
}

// Manager runs the backup jobs. Start and Stop may be called again after
// Stop: an update that rolls back starts the old server's services again.
type Manager struct {
	cfg   Config
	t     timing
	log   *logx.Logger
	state *store

	mu       sync.Mutex
	set      Settings
	runner   Runner
	released bool
	cancel   context.CancelFunc
	ctx      context.Context
	wake     chan struct{}
	// workers are runs and maintenance; others are the loop and the
	// followers of adopted helpers. Stop waits for both.
	workers sync.WaitGroup
	others  sync.WaitGroup

	busy        map[string]string // target -> what holds it
	backupSlot  bool
	maintSlot   bool
	deploying   map[string]int
	deployEnded map[string]time.Time
	// dumps are the dumps in flight that a deploy cancels: a backup's and
	// a periodic snapshot's can run side by side.
	dumps map[*activeDump]bool
	// snapSlot is the snapshot repository's own slot, beside the backup
	// slot, so a deploy's snapshot never waits behind a remote backup.
	snapSlot bool
	// snapReady: the snapshot repository was opened in this start.
	snapReady bool
	// snapRunning is the snapshot holding the slot, for the status.
	snapRunning *SnapshotRunning
	// snapListed: the snapshot index was read from the repository in this
	// start.
	snapListed bool
	// snapListRetry is when a failed first listing is tried again.
	snapListRetry time.Time
	// snapQueued: the scheduler started snapshot work that has not ended.
	snapQueued bool
	snapInitMu sync.Mutex
	// freed is closed and replaced whenever a slot is given back, which
	// wakes a restore waiting for one.
	freed chan struct{}
	// restore is the restore in flight; restoreWait counts the restores
	// waiting per target, which no backup or maintenance overtakes.
	restore     *activeRestore
	restoreWait map[string]int
	// recoverErr is a restore journal this server could not roll back.
	recoverErr error
	// recoveredCh is closed once this start has adopted what the previous
	// server left and resolved its restore journal; no restore starts
	// before, or recovery would take the new restore's journal for the
	// old one.
	recoveredCh chan struct{}
}

// New checks cfg, fills its defaults and loads the state.
func New(cfg Config) (*Manager, error) {
	if cfg.Apps == nil || cfg.States == nil || cfg.Planner == nil || cfg.NewRunner == nil {
		return nil, errors.New("backup: apps, app states, a planner and a runner are required")
	}
	if !filepath.IsAbs(cfg.BaseDir) {
		return nil, errors.New("backup: the base directory must be absolute")
	}
	if cfg.Log == nil {
		cfg.Log = logx.New("Backup")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Disk == nil {
		cfg.Disk = hostDisk{}
	}
	if cfg.ServerDirs == nil {
		for _, d := range []string{"config", "cert", "db", "mail"} {
			cfg.ServerDirs = append(cfg.ServerDirs, filepath.Join(cfg.BaseDir, d))
		}
	}
	st, err := openStore(stateFile(cfg.BaseDir))
	if st == nil {
		return nil, err
	}
	if err != nil {
		cfg.Log.Error("%s", err.Error())
	}
	return &Manager{
		cfg:         cfg,
		t:           defaultTiming(),
		log:         cfg.Log,
		state:       st,
		wake:        make(chan struct{}, 1),
		busy:        map[string]string{},
		deploying:   map[string]int{},
		deployEnded: map[string]time.Time{},
		dumps:       map[*activeDump]bool{},
		freed:       make(chan struct{}),
		restoreWait: map[string]int{},
	}, nil
}

// SetSettings hands the manager the settings, which are built after it
// because their target init runs through it. The composition root calls it
// once, before Start.
func (m *Manager) SetSettings(s Settings) {
	m.mu.Lock()
	m.set = s
	m.mu.Unlock()
}

func (m *Manager) settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set
}

// Start begins scheduling. The system calls it once this server is the
// live one, after an update's handover, so that adopting the previous
// server's helpers cannot race that server. It returns at once: adoption
// and everything after wait for Docker in the background.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	if m.set == nil {
		m.log.Error("Backups are not started: no settings")
		return
	}
	if err := m.state.load(); err != nil {
		m.log.Error("%s", err.Error())
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.recoveredCh = make(chan struct{})
	m.snapReady, m.snapListed = false, false
	if m.released {
		m.runner = nil
		m.released = false
	}
	m.others.Add(1)
	go func(ctx context.Context) {
		defer m.others.Done()
		m.loop(ctx)
	}(m.ctx)
}

// Stop hands every run in flight to the next server and stops scheduling.
// The release comes first and runs carry a context of their own, so the
// cancel that follows cannot stop a helper the release has just detached.
// It returns once nothing of the manager runs, so the next server never
// shares the state file with this one.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	r := m.runner
	if cancel == nil {
		m.mu.Unlock()
		return
	}
	m.cancel = nil
	m.released = true
	m.mu.Unlock()
	if r != nil {
		r.Release()
	}
	cancel()
	m.workers.Wait()
	m.others.Wait()
	m.log.Log("Backups stopped; runs in flight are left to the next server")
}

// currentRunner returns this start's runner, making it on first use.
func (m *Manager) currentRunner() (Runner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.released {
		return nil, errReleased
	}
	if m.runner == nil {
		r, err := m.cfg.NewRunner()
		if err != nil {
			return nil, err
		}
		m.runner = r
	}
	return m.runner, nil
}

var errReleased = errors.New("backup: runs are handed over to the next server")

func (m *Manager) isReleased() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.released
}

// poke wakes the schedule.
func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v) }

// cacheDir is where the repositories' caches live, on ODAC's data disk
// (init's same-disk check relies on that); the engine keeps one
// subdirectory per repository. A run that reads a stored dump back gets a
// root of its own, since it runs next to a backup of the same repository.
func (m *Manager) cacheDir(read bool) (string, error) {
	name := "cache"
	if read {
		name = "cache-read"
	}
	dir := filepath.Join(m.cfg.BaseDir, "backup", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backup: cache directory: %w", err)
	}
	return dir, nil
}

// request builds a request with a target's access.
func (m *Manager) request(op string, a settings.Access, read bool) (*proto.Request, error) {
	dir, err := m.cacheDir(read)
	if err != nil {
		return nil, err
	}
	repo := a.Repo
	repo.CacheDir = dir
	repo.CacheBytes = m.cfg.CacheBytes
	return &proto.Request{Op: op, Repo: repo}, nil
}

// Init runs init on a target for the settings (settings.Initializer).
func (m *Manager) Init(ctx context.Context, a settings.Access) (*proto.Result, error) {
	r, err := m.currentRunner()
	if err != nil {
		return nil, err
	}
	req, err := m.request(proto.OpInit, a, false)
	if err != nil {
		return nil, err
	}
	return r.Run(ctx, runner.Invocation{Request: req, Target: a.Target, Labels: map[string]string{labelKind: proto.OpInit}})
}

// Labels a run's containers carry, so the next server knows what an
// adopted helper was doing.
const (
	labelKind     = docker.LabelBackupPrefix + "kind"
	labelJob      = docker.LabelBackupPrefix + "job"
	labelTarget   = docker.LabelBackupPrefix + "target"
	labelApps     = docker.LabelBackupPrefix + "apps"
	labelFinish   = docker.LabelBackupPrefix + "finish"
	labelComplete = docker.LabelBackupPrefix + "complete"
	labelTrigger  = docker.LabelBackupPrefix + "trigger"
)

// access opens a target for a run: a target of the settings, or the
// snapshot repository, which is the manager's own.
func (m *Manager) access(target string, opts settings.AccessOptions) (settings.Access, error) {
	if target == SnapshotTarget {
		if opts.Server != "" {
			return settings.Access{}, refusal(settings.CodeInvalid, "Snapshots belong to this server.")
		}
		return m.snapshotAccess()
	}
	return m.settings().Access(target, opts)
}

// observe passes a failed run to the settings, which keep what it says
// about a target; the snapshot repository has nothing to say there.
func (m *Manager) observe(target string, err error) {
	if target != SnapshotTarget {
		m.settings().Observe(target, err)
	}
}
