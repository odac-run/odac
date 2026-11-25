// Package consistency decides how each piece of an app's data is read for
// a backup, so that what is stored can be restored: a database through its
// own dump tool, everything else as files read live (where odac-backup
// takes SQLite databases through SQLite itself, see sqlitecopy). The
// method is chosen per piece of data, not per app: an app's database, its
// SQLite files and its plain files each get their own.
//
// A Planner turns an App into a Plan. It finds the engine from the
// recipe's declaration or the image name and verifies the tool by running
// it in the image the dump will use; the plan then says which stream to
// dump, which directories to read, and which volumes the dump stands in
// for. Nothing here stops an app or takes a lock it cannot bound: every
// dump runs against the live database without blocking its writers, lock
// waits are capped, and a case that would lock (MyISAM tables, a Redis
// fork with no memory for its copy-on-write pages) is reported as a
// warning and read in the way that does not.
//
// Every command runs in a dump container (docker.DumpSpec): the database
// app's image, in its network namespace, entrypoint replaced. Credentials
// reach it on stdin and nowhere else, because `docker inspect` shows a
// container's command and environment, and `ps` on the host shows its
// command line.
package consistency

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"odac/internal/docker"
)

// Consistency levels a snapshot carries; the engine stores them as given.
const (
	Consistent      = "consistent"
	CrashConsistent = "crash-consistent"
	Live            = "live"
)

// Engine names a storage engine ODAC knows how to dump.
type Engine string

// Known engines. SQLite and None are declarations only: an app whose data
// is SQLite files or plain files needs no dump, since odac-backup finds
// SQLite databases by their header.
const (
	Postgres   Engine = "postgres"
	MySQL      Engine = "mysql"
	MariaDB    Engine = "mariadb"
	Redis      Engine = "redis"
	Mongo      Engine = "mongo"
	ClickHouse Engine = "clickhouse"
	SQLite     Engine = "sqlite"
	None       Engine = "none"
)

// Volume is one bind mount of the app: Host as this server sees it,
// Container inside the app.
type Volume struct {
	Host      string
	Container string
}

// App is what the planner needs to know about one app. The caller builds
// it from the app record and the running container.
type App struct {
	Name string
	// Container is the running container whose network namespace dumps
	// join.
	Container string
	// Image is the image as configured, which names the engine.
	Image string
	// ImageID is the image the container runs, which dumps run too, so the
	// tool matches the server's version; empty means Image.
	ImageID string
	Cmd     []string
	// Env is the container's environment as the engine reports it, image
	// defaults included: that is where the credentials are.
	Env map[string]string
	// Volumes are the data volumes to back up.
	Volumes []Volume
	// Spec is the recipe's backup declaration, if any.
	Spec *Spec
	// MemoryLimit is the container's memory limit in bytes, zero for none.
	MemoryLimit int64
}

func (a App) image() string {
	if a.ImageID != "" {
		return a.ImageID
	}
	return a.Image
}

// Dump is the stream a backup reads from the database.
type Dump struct {
	// Name is the source name in the repository, File the name the stream
	// is stored under.
	Name        string
	File        string
	Consistency string
	// Spec runs the dump; the runner adds limits and labels.
	Spec docker.DumpSpec
}

// Dir is a directory a backup reads.
type Dir struct {
	// Name is the source name in the repository, stable for one container
	// path so that each run deduplicates against the last.
	Name string
	// Path is the directory as this server sees it.
	Path string
	// Container is where the app sees it, which is where a restore puts it.
	Container   string
	Consistency string
	// Prepared is a directory the plan's Prepare makes (ClickHouse's
	// frozen parts), so it can exist only after Prepare has run.
	Prepared bool
}

// Warning is a condition the Hub shows next to a backup. Code is stable;
// Message is for logs.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Warning codes.
const (
	// WarnToolMissing: the engine was named but its dump tool is not in the
	// image, so its data is read as files.
	WarnToolMissing = "tool_missing"
	// WarnCredentials: no credentials the dump could use were found.
	WarnCredentials = "credentials"
	// WarnProbe: the database could not be asked about itself.
	WarnProbe = "probe_failed"
	// WarnNonTransactional: tables without snapshot reads (MyISAM, Aria,
	// MEMORY) are dumped without the lock that would make them consistent.
	WarnNonTransactional = "non_transactional"
	// WarnMemory: a Redis fork would not have room for its copy-on-write
	// pages, so its last save on disk is read instead.
	WarnMemory = "memory"
	// WarnStandalone: a MongoDB without a replica set has no oplog, so its
	// collections are dumped one after another.
	WarnStandalone = "standalone"
	// WarnNotOnVolume: the engine's data is not on a volume ODAC can read.
	WarnNotOnVolume = "not_on_volume"
	// WarnUnknownEngine: the declared engine has no method and no hook.
	WarnUnknownEngine = "unknown_engine"
)

// Plan is how one app is backed up. A plan may leave state behind on the
// database (ClickHouse's frozen parts): call Prepare before the run and
// Finish after it, whatever the outcome.
type Plan struct {
	App    string
	Engine Engine
	// Dump is nil when no stream is read.
	Dump *Dump
	Dirs []Dir
	// Covered are the volumes the dump stands in for and that are not read
	// as files: a database's live data directory read as files is torn.
	Covered  []Volume
	Warnings []Warning

	prepare []func(context.Context) error
	finish  []func(context.Context) error
}

// Prepare runs what must happen right before the run.
func (p *Plan) Prepare(ctx context.Context) error {
	for _, f := range p.prepare {
		if err := f(ctx); err != nil {
			return err
		}
	}
	return nil
}

// AddPrepare adds a step to Prepare, and undo, when not nil, to Finish:
// a step that makes something for the run, such as frozen parts, is
// undone after it.
func (p *Plan) AddPrepare(step, undo func(context.Context) error) {
	p.prepare = append(p.prepare, step)
	if undo != nil {
		p.finish = append(p.finish, undo)
	}
}

// Finish undoes what Prepare left; it tries every step and reports the
// first failure.
func (p *Plan) Finish(ctx context.Context) error {
	var first error
	for _, f := range p.finish {
		if err := f(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (p *Plan) warn(code, format string, a ...any) {
	p.Warnings = append(p.Warnings, Warning{Code: code, Message: fmt.Sprintf(format, a...)})
}

// Config wires a Planner to its collaborators.
type Config struct {
	Prober Prober
	// AvailableMemory reports the host's available memory, which a Redis
	// fork's copy-on-write pages come out of.
	AvailableMemory func() (int64, error)
	// LockWait bounds how long a dump waits for a lock; zero means 30s.
	LockWait time.Duration
	// RemoveAll removes a directory this server sees; nil means os.RemoveAll.
	RemoveAll func(string) error
}

// DefaultLockWait is how long a dump waits for a lock by default. A
// migration holding a table longer than this makes the run fail and retry
// later, which is cheaper than a dump that queues behind it.
const DefaultLockWait = 30 * time.Second

// Planner makes plans. It is safe for concurrent use.
type Planner struct {
	cfg Config

	mu    sync.Mutex
	tools map[string]toolResult
}

type toolResult struct {
	version string
	missing bool
}

// NewPlanner checks cfg and fills its defaults.
func NewPlanner(cfg Config) (*Planner, error) {
	if cfg.Prober == nil {
		return nil, errors.New("consistency: a prober is required")
	}
	if cfg.AvailableMemory == nil {
		return nil, errors.New("consistency: an available memory source is required")
	}
	if cfg.LockWait <= 0 {
		cfg.LockWait = DefaultLockWait
	}
	if cfg.RemoveAll == nil {
		cfg.RemoveAll = os.RemoveAll
	}
	return &Planner{cfg: cfg, tools: map[string]toolResult{}}, nil
}

var runPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Plan decides how app is backed up in run. An error is a failure worth
// retrying (Docker unavailable); a method that cannot be used is a warning
// and its data falls back to files read live.
func (p *Planner) Plan(ctx context.Context, app App, run string) (*Plan, error) {
	if !runPattern.MatchString(run) {
		return nil, fmt.Errorf("consistency: invalid run %q", run)
	}
	if app.Name == "" || app.Container == "" || app.image() == "" {
		return nil, errors.New("consistency: an app needs a name, a container and an image")
	}
	plan := &Plan{App: app.Name}
	vols := dataVolumes(app.Volumes)

	engine, hook := p.engineFor(app)
	plan.Engine = engine
	var covered []string
	var err error
	switch {
	case hook != nil:
		covered, err = p.planHook(app, hook, plan)
	case engine == Postgres:
		covered, err = p.planPostgres(ctx, app, plan)
	case engine == MySQL || engine == MariaDB:
		covered, err = p.planMySQL(ctx, app, plan)
	case engine == Redis:
		covered, err = p.planRedis(ctx, app, plan)
	case engine == Mongo:
		covered, err = p.planMongo(ctx, app, plan)
	case engine == ClickHouse:
		covered, err = p.planClickHouse(ctx, app, vols, run, plan)
	case engine == "" || engine == None || engine == SQLite:
	default:
		plan.warn(WarnUnknownEngine, "engine %q has no dump method and the recipe declares no hook", engine)
	}
	if err != nil {
		return nil, err
	}

	for _, v := range vols {
		if underAny(v.Container, covered) {
			plan.Covered = append(plan.Covered, v)
			continue
		}
		plan.Dirs = append(plan.Dirs, Dir{Name: dirName(v.Container), Path: v.Host, Container: v.Container, Consistency: Live})
	}
	return plan, nil
}

// FilesPlan reads every data volume of app as files at level, with no
// dump. It is the plan for an app whose container is not running: no dump
// or probe can join its network namespace, and with nothing writing its
// files they read as the app left them, which is crash-consistent at
// worst.
func FilesPlan(app App, level string) *Plan {
	plan := &Plan{App: app.Name}
	for _, v := range dataVolumes(app.Volumes) {
		plan.Dirs = append(plan.Dirs, Dir{Name: dirName(v.Container), Path: v.Host, Container: v.Container, Consistency: level})
	}
	return plan
}

// dataVolumes drops volumes nested in another one, which the outer one
// already reads.
func dataVolumes(in []Volume) []Volume {
	var out []Volume
	for i, v := range in {
		if v.Host == "" || v.Container == "" {
			continue
		}
		nested := false
		for j, w := range in {
			if i != j && w.Host != "" && (within(v.Host, w.Host) && v.Host != w.Host || v.Host == w.Host && j < i) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, v)
		}
	}
	return out
}

func within(p, root string) bool {
	p, root = path.Clean(p), path.Clean(root)
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// hostPath maps a path inside the app to this server's view through the
// deepest volume that holds it.
func hostPath(vols []Volume, p string) (string, Volume, bool) {
	var best Volume
	found := false
	for _, v := range vols {
		if within(p, v.Container) && (!found || len(v.Container) > len(best.Container)) {
			best, found = v, true
		}
	}
	if !found {
		return "", best, false
	}
	rel := strings.TrimPrefix(path.Clean(p), path.Clean(best.Container))
	return path.Join(best.Host, rel), best, true
}

var nameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// dirName is a source name for a container path. It must stay the same
// for the same path, so the name is derived, never numbered.
func dirName(container string) string {
	slug := strings.Trim(nameChars.ReplaceAllString(strings.Trim(container, "/"), "-"), "-.")
	if slug == "" {
		slug = "root"
	}
	name := "files-" + slug
	if len(name) > 100 || slug != strings.ReplaceAll(strings.Trim(container, "/"), "/", "-") {
		name = fmt.Sprintf("%.90s-%s", name, shortHash(container))
	}
	return name
}
