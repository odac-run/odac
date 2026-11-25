package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/docker"
	"odac/internal/lang"
)

// AppControl is the app manager's side of a restore; the composition root
// adapts *appmgr.Manager to it.
type AppControl interface {
	// HoldApp stops an app and keeps the check pulse and every other
	// operation off it until the hold is released. The app stays
	// inactive, persisted, while held.
	HoldApp(name string) (AppHold, error)
	// SetAppActive gives an app its active flag back after a restore that
	// did not finish.
	SetAppActive(name string, active bool) bool
	// AppDefinition is the app's record, stored beside its backups so it
	// can be restored as a new app.
	AppDefinition(name string) (map[string]any, bool)
	// UniqueAppName is base when no app has it, else base-N.
	UniqueAppName(base string) string
	// AppsDir is where app directories live; no symlink may lead below it
	// to a restore's destination.
	AppsDir() string
	// CreateRestoredApp adds an app made from a definition under a new
	// name, stopped and inactive, without anything it would share with
	// the original (published ports, host networking, devices, the API
	// grant), its links renamed through renames and its template group
	// set to group. It reports what it dropped.
	CreateRestoredApp(def map[string]any, name string, renames map[string]string, group string) ([]string, error)
	// DeleteRestoredApp removes an app a failed restore created, with its
	// directory.
	DeleteRestoredApp(name string) error
}

// AppHold is one held app.
type AppHold interface {
	// WasActive is whether the app was meant to run before the hold.
	WasActive() bool
	// Start starts its container while it is still held and inactive.
	Start() error
	// Stop stops it again.
	Stop()
	// Release ends the hold; with active the app is started and marked
	// active, without it is left stopped.
	Release(active bool) error
	// Abandon drops the hold and touches nothing, for a server that stops
	// mid-restore: the next one rolls the restore back.
	Abandon()
}

// Loader turns a stored dump back into a load; *consistency.Planner
// implements it.
type Loader interface {
	Load(ctx context.Context, app consistency.App, name, level string) (*consistency.Load, error)
}

// Restore modes. In place returns the apps' data to the restore point and
// first backs up the current state, so the restore itself can be undone;
// a new app leaves the live apps alone.
const (
	RestoreInPlace = "in-place"
	RestoreNewApp  = "new-app"
)

// Restore statuses, besides the run statuses.
const (
	StatusPlanned = "planned"
	StatusWaiting = "waiting"
	StatusRunning = "running"
)

// KindRestore is a restore's record and what holds its target.
const KindRestore = "restore"

// safetyJob is the job id the backup before an in-place restore was
// stored under on the restore's target, before the snapshot repository
// took that over; legacySafety thins what is left of them.
const safetyJob = "pre-restore"

// definitionSource is the snapshot that holds an app's definition beside
// its data, and definitionFile the file in it.
const (
	definitionSource = "definition"
	definitionFile   = "app.json"
)

// RestoreRequest is what to restore and how.
type RestoreRequest struct {
	Target string `json:"target"`
	// Server is the server whose directory on the target is read; empty
	// is this server. Another server's directory is how a server moves.
	Server string `json:"server,omitempty"`
	// Key opens that directory when its key is not this target's.
	Key string `json:"-"`
	Run string `json:"run"`
	// Apps are the apps of the run to restore, as the run names them;
	// empty is every app in it. The apps they link (env.linked) come with
	// them, so an app and its database return from the same moment.
	Apps []string `json:"apps,omitempty"`
	Mode string   `json:"mode"`
	// Names gives a new app its name, by the name in the run. An app with
	// no entry keeps its name when it is free (a fresh server), else it
	// gets "<name>-restored".
	Names map[string]string `json:"names,omitempty"`
	// SkipSafety restores in place without backing up the current state
	// first. It is the operator's to ask for: without it a target that
	// cannot take that backup refuses the restore.
	SkipSafety bool `json:"skipSafety,omitempty"`
}

// RestoreResult is a restore as the API and the Hub see it. Times are Unix
// milliseconds.
type RestoreResult struct {
	ID     string `json:"id"`
	Mode   string `json:"mode"`
	Target string `json:"target"`
	Server string `json:"server,omitempty"`
	Run    string `json:"run"`
	Job    string `json:"job,omitempty"`
	Status string `json:"status"`
	Phase  string `json:"phase,omitempty"`
	// Point is the moment the restored data comes from: the start of the
	// earliest snapshot restored. Lost, in place, is how much of the
	// apps' history the restore gives up, measured when it was planned.
	Point int64        `json:"point"`
	Lost  int64        `json:"lost,omitempty"`
	Apps  []AppRestore `json:"apps"`
	// Restarted are apps that link a restored one: stopped while it was
	// restored and started again, their own data untouched.
	Restarted []string `json:"restarted,omitempty"`
	// Safety is the run that holds the state before an in-place restore.
	Safety     string `json:"safety,omitempty"`
	Error      string `json:"error,omitempty"`
	RolledBack bool   `json:"rolledBack,omitempty"`
	Start      int64  `json:"start"`
	End        int64  `json:"end,omitempty"`
}

// AppRestore is one app's part of a restore.
type AppRestore struct {
	// App is the name in the run; As the app restored into.
	App string `json:"app"`
	As  string `json:"as"`
	// Method is how its database comes back: the engine's dump, or files
	// only.
	Method      string   `json:"method"`
	Consistency string   `json:"consistency,omitempty"`
	Point       int64    `json:"point"`
	Lost        int64    `json:"lost,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	// Dropped is what a new app does not keep of its definition.
	Dropped []string `json:"dropped,omitempty"`
	// Image is, in place, the app's image at the point and now, when both
	// are known; Blocked (BlockedImageMismatch) is an app the restore
	// refuses because they differ in repository or major version.
	Image   *RestoreImage `json:"image,omitempty"`
	Blocked string        `json:"blocked,omitempty"`
}

// restorePlan is a restore worked out from the run's snapshots.
type restorePlan struct {
	req    RestoreRequest
	access settings.Access
	apps   []*appPlan
	// dependents link a restored app and are stopped around the restore.
	dependents     []string
	dependentHolds []depHold
	result         *RestoreResult
}

// appPlan is one app to restore.
type appPlan struct {
	source string
	name   string
	dump   *proto.Snapshot
	dirs   []proto.Snapshot
	// defSnap is the definition stored with the run; def is it read back,
	// for a new app.
	defSnap *proto.Snapshot
	def     map[string]any
	links   []string
	out     *AppRestore
	// filled in when the restore runs
	app     consistency.App
	load    *consistency.Load
	hold    AppHold
	volumes []*volumeJob
	started bool
}

func newRestoreID(now time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return "r" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// Servers lists the server directories on a target, the choices of a
// restore from another server.
func (m *Manager) Servers(ctx context.Context, target string) ([]proto.ServerDir, error) {
	res, err := m.call(ctx, target, false, func(req *proto.Request) { req.Op = proto.OpServers }, proto.Handler{})
	if err != nil {
		return nil, err
	}
	return res.Servers, nil
}

// RestorePoint is one run a restore can return to.
type RestorePoint struct {
	Run     string `json:"run"`
	Job     string `json:"job"`
	Trigger string `json:"trigger,omitempty"`
	// Time is the start of the run's earliest snapshot.
	Time int64             `json:"time"`
	Apps []RestorePointApp `json:"apps"`
	// Server says the run holds ODAC's own state too.
	Server bool `json:"server,omitempty"`
}

// RestorePointApp is one app's data in a run.
type RestorePointApp struct {
	App         string `json:"app"`
	Consistency string `json:"consistency"`
	Dump        bool   `json:"dump,omitempty"`
}

// RestorePoints lists the runs of a server's directory on a target, newest
// first, of one app when app is set. server and key are as in a
// RestoreRequest.
func (m *Manager) RestorePoints(ctx context.Context, target, server, key, app string) ([]RestorePoint, error) {
	a, err := m.accessFor(target, server, key)
	if err != nil {
		return nil, err
	}
	snaps, err := m.listWith(ctx, a, target, proto.List{Host: hostOf(a, server), App: app})
	if err != nil {
		return nil, err
	}
	byRun := map[string]*RestorePoint{}
	levels := map[string]map[string][]string{}
	for _, s := range snaps {
		if s.Run == "" {
			continue
		}
		p := byRun[s.Run]
		if p == nil {
			p = &RestorePoint{Run: s.Run, Job: s.Source.Job, Trigger: s.Trigger, Time: ms(s.Start)}
			byRun[s.Run] = p
			levels[s.Run] = map[string][]string{}
		}
		p.Time = min(p.Time, ms(s.Start))
		if s.Source.App == serverApp {
			p.Server = true
			continue
		}
		if s.Source.Name == definitionSource {
			continue
		}
		levels[s.Run][s.Source.App] = append(levels[s.Run][s.Source.App], s.Consistency)
		if s.Kind == "stream" {
			found := false
			for i := range p.Apps {
				if p.Apps[i].App == s.Source.App {
					p.Apps[i].Dump, found = true, true
				}
			}
			if !found {
				p.Apps = append(p.Apps, RestorePointApp{App: s.Source.App, Dump: true})
			}
		} else if !slices.ContainsFunc(p.Apps, func(x RestorePointApp) bool { return x.App == s.Source.App }) {
			p.Apps = append(p.Apps, RestorePointApp{App: s.Source.App})
		}
	}
	out := make([]RestorePoint, 0, len(byRun))
	for run, p := range byRun {
		for i := range p.Apps {
			p.Apps[i].Consistency = weakest(levels[run][p.Apps[i].App])
		}
		sort.Slice(p.Apps, func(i, j int) bool { return p.Apps[i].App < p.Apps[j].App })
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out, nil
}

// hostOf is the Kopia host a listing asks for: the server whose
// directory it is.
func hostOf(a settings.Access, server string) string {
	if server != "" {
		return server
	}
	return a.Repo.ServerID
}

func (m *Manager) accessFor(target, server, key string) (settings.Access, error) {
	a, err := m.access(target, settings.AccessOptions{Server: server})
	if err != nil {
		return a, err
	}
	if key != "" {
		a.Repo.Password = key
	}
	return a, nil
}

// runWith runs one request with a given access, which may open another
// server's directory read-only.
func (m *Manager) runWith(ctx context.Context, a settings.Access, target string, shape func(*proto.Request), h proto.Handler, labels map[string]string) (*proto.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := m.currentRunner()
	if err != nil {
		return nil, err
	}
	req, err := m.request("", a, true)
	if err != nil {
		return nil, err
	}
	shape(req)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelKind], labels[labelTarget] = req.Op, target
	return r.Run(ctx, runner.Invocation{Request: req, Target: a.Target, Handler: h, Labels: labels})
}

func (m *Manager) listWith(ctx context.Context, a settings.Access, target string, f proto.List) ([]proto.Snapshot, error) {
	var (
		mu    sync.Mutex
		snaps []proto.Snapshot
	)
	_, err := m.runWith(ctx, a, target, func(req *proto.Request) { req.Op, req.List = proto.OpList, &f }, proto.Handler{
		Snapshot: func(s proto.Snapshot) {
			mu.Lock()
			snaps = append(snaps, s)
			mu.Unlock()
		},
	}, nil)
	mu.Lock()
	defer mu.Unlock()
	return snaps, err
}

// streamSnapshot reads a stream snapshot back. Like storedDump, the pipe
// ends with io.EOF only once the restore helper has succeeded and every
// byte it reported has arrived.
func (m *Manager) streamSnapshot(ctx context.Context, a settings.Access, target, id string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := m.runWith(rctx, a, target, func(req *proto.Request) {
			req.Op, req.Restore = proto.OpRestore, &proto.Restore{ID: id}
		}, proto.Handler{Output: pw}, nil)
		pw.CloseWithError(err)
	}()
	return &restoreStream{PipeReader: pr, cancel: cancel, done: done}, nil
}

// restoreDir restores a directory snapshot into dir, which must be empty.
func (m *Manager) restoreDir(ctx context.Context, a settings.Access, target, id, dir string) error {
	_, err := m.runWith(ctx, a, target, func(req *proto.Request) {
		req.Op, req.Restore = proto.OpRestore, &proto.Restore{ID: id, Dir: dir}
	}, proto.Handler{}, nil)
	return err
}

// PreviewRestore works out a restore without changing anything: which
// apps it covers, how each comes back, and for a restore in place how
// much each app's history it gives up.
func (m *Manager) PreviewRestore(ctx context.Context, req RestoreRequest) (*RestoreResult, error) {
	p, err := m.planRestore(ctx, req, "")
	if err != nil {
		return nil, err
	}
	return p.result, nil
}

// restoreErr and refusal build a refusal in the operator's language:
// format is the catalog key, with %s for every argument (see lang.T).
func restoreErr(format string, a ...any) error {
	return refusal(settings.CodeInvalid, format, a...)
}

func refusal(code, format string, a ...any) error {
	return &settings.Error{Code: code, Message: lang.T(format, a...)}
}

// planRestore lists the run and decides what the restore does. work is a
// directory for definitions a new app is made from; empty reads them into
// a temporary one.
func (m *Manager) planRestore(ctx context.Context, req RestoreRequest, work string) (*restorePlan, error) {
	ctl := m.cfg.Control
	if ctl == nil || m.cfg.Loader == nil {
		return nil, errors.New("backup: restores are not available on this server")
	}
	if req.Mode != RestoreInPlace && req.Mode != RestoreNewApp {
		return nil, restoreErr("A restore is made in place or as a new app.")
	}
	if req.Run == "" || req.Target == "" {
		return nil, restoreErr("A restore names a target and a run.")
	}
	a, err := m.accessFor(req.Target, req.Server, req.Key)
	if err != nil {
		return nil, err
	}
	snaps, err := m.listWith(ctx, a, req.Target, proto.List{Host: hostOf(a, req.Server), Run: req.Run})
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, refusal(settings.CodeNotFound, "Run %s is not on this target.", req.Run)
	}
	byApp := map[string]*appPlan{}
	var job string
	for i := range snaps {
		s := snaps[i]
		job = s.Source.Job
		if s.Source.App == serverApp {
			continue
		}
		ap := byApp[s.Source.App]
		if ap == nil {
			ap = &appPlan{source: s.Source.App}
			byApp[s.Source.App] = ap
		}
		switch {
		case s.Kind == "stream":
			if ap.dump != nil && ap.dump.Start.After(s.Start) {
				continue
			}
			ap.dump = &s
		case s.Source.Name == definitionSource:
			ap.defSnap = &s
		default:
			ap.dirs = append(ap.dirs, s)
		}
	}
	// A new app is made from its definition: each read takes a helper run.
	if req.Mode == RestoreNewApp {
		for name, ap := range byApp {
			if ap.defSnap == nil {
				continue
			}
			if def, err := m.readDefinition(ctx, a, req.Target, *ap.defSnap, work); err != nil {
				m.log.Warn("Cannot read the definition of %s stored with run %s: %s", name, req.Run, err.Error())
			} else {
				ap.def = def
			}
		}
	}

	local := map[string][]string{}
	for _, app := range m.cfg.Apps.BackupApps() {
		local[app.Name] = app.Linked
	}
	for name, ap := range byApp {
		if links, ok := local[name]; ok && req.Mode == RestoreInPlace {
			ap.links = links
		} else if ap.def != nil {
			ap.links = definitionLinks(ap.def)
		} else {
			ap.links = local[name]
		}
	}

	wanted := req.Apps
	if len(wanted) == 0 {
		for name := range byApp {
			wanted = append(wanted, name)
		}
	}
	chosen := map[string]bool{}
	var add func(name string, direct bool) error
	add = func(name string, direct bool) error {
		if chosen[name] {
			return nil
		}
		ap := byApp[name]
		if ap == nil {
			if direct {
				return refusal(settings.CodeNotFound, "Run %s holds no data of app %s.", req.Run, name)
			}
			return nil
		}
		chosen[name] = true
		for _, l := range ap.links {
			if err := add(l, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range wanted {
		if err := add(name, true); err != nil {
			return nil, err
		}
	}

	now := m.now()
	res := &RestoreResult{Mode: req.Mode, Target: req.Target, Server: req.Server, Run: req.Run, Job: job, Status: StatusPlanned, Start: ms(now)}
	plan := &restorePlan{req: req, access: a, result: res}
	names := map[string]string{}
	var sources []string
	for name := range chosen {
		sources = append(sources, name)
	}
	sort.Strings(sources)
	for _, name := range sources {
		ap := byApp[name]
		ap.name = name
		if req.Mode == RestoreNewApp {
			ap.name = req.Names[name]
			if ap.name == "" {
				ap.name = ctl.UniqueAppName(name)
				if ap.name != name {
					ap.name = ctl.UniqueAppName(name + "-restored")
				}
			}
			if slices.Contains(slices.Collect(maps.Values(names)), ap.name) {
				return nil, restoreErr("Two apps would be restored as %s.", ap.name)
			}
			if _, exists := local[ap.name]; exists {
				return nil, restoreErr("An app named %s exists already.", ap.name)
			}
			names[name] = ap.name
		} else if _, exists := local[name]; !exists {
			return nil, restoreErr("App %s does not exist on this server; restore it as a new app.", name)
		}
		plan.apps = append(plan.apps, ap)
	}
	if req.Mode == RestoreNewApp {
		for _, ap := range plan.apps {
			if ap.def == nil {
				if def, ok := ctl.AppDefinition(ap.source); ok {
					ap.def = def
				} else {
					return nil, restoreErr("Run %s holds no definition of app %s, and no app of that name is on this server to copy one from.", req.Run, ap.source)
				}
			}
			if t, _ := ap.def["type"].(string); t != "container" {
				return nil, restoreErr("App %s is a %s app, which is restored in place: create it again and restore its data into it.", ap.source, t)
			}
		}
	}

	var point time.Time
	for _, ap := range plan.apps {
		out := &AppRestore{App: ap.source, As: ap.name, Method: MethodFiles}
		if req.Mode == RestoreInPlace {
			out.Image, out.Blocked = m.imageCheck(ctx, a, req, ap, work)
		}
		var first time.Time
		var levels []string
		for _, s := range ap.allSnapshots() {
			if first.IsZero() || s.Start.Before(first) {
				first = s.Start
			}
			levels = append(levels, s.Consistency)
		}
		if ap.dump != nil {
			out.Method = MethodDump
		}
		out.Consistency = weakest(levels)
		out.Point = ms(first)
		if req.Mode == RestoreInPlace {
			out.Lost = max(0, ms(now)-ms(first))
		}
		if point.IsZero() || first.Before(point) {
			point = first
		}
		ap.out = out
		res.Apps = append(res.Apps, *out)
	}
	res.Point = ms(point)
	if req.Mode == RestoreInPlace {
		res.Lost = max(0, ms(now)-ms(point))
		for name, links := range local {
			if chosen[name] {
				continue
			}
			for _, l := range links {
				if chosen[l] {
					plan.dependents = append(plan.dependents, name)
					break
				}
			}
		}
		sort.Strings(plan.dependents)
		res.Restarted = plan.dependents
	}
	return plan, nil
}

func (ap *appPlan) allSnapshots() []proto.Snapshot {
	out := slices.Clone(ap.dirs)
	if ap.dump != nil {
		out = append(out, *ap.dump)
	}
	return out
}

// definitionLinks reads env.linked from a stored definition.
func definitionLinks(def map[string]any) []string {
	env, _ := def["env"].(map[string]any)
	list, _ := env["linked"].([]any)
	var out []string
	name, _ := def["name"].(string)
	for _, l := range list {
		if s, ok := l.(string); ok && s != "" && s != name {
			out = append(out, s)
		}
	}
	return out
}

// readDefinition restores an app's definition snapshot into a directory on
// ODAC's own disk and reads it.
func (m *Manager) readDefinition(ctx context.Context, a settings.Access, target string, s proto.Snapshot, work string) (map[string]any, error) {
	dir := work
	if dir == "" {
		tmp, err := m.workDir("preview")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	dest := filepath.Join(dir, "definition-"+s.Source.App)
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dest)
	if err := m.restoreDir(ctx, a, target, s.ID, dest); err != nil {
		return nil, err
	}
	return readDefinitionFile(filepath.Join(dest, definitionFile))
}

// imageCheck compares the image an in-place restore's point was taken on,
// from the definition stored with it, with the image the app runs now
// (compareImages). A point without a readable definition (one older than
// definitions, or a read that failed) reports nothing and blocks nothing;
// a failed read is logged. Preview and restore both run it, and the
// restore refuses on its own answer, never on what a preview said.
func (m *Manager) imageCheck(ctx context.Context, a settings.Access, req RestoreRequest, ap *appPlan, work string) (*RestoreImage, string) {
	if ap.defSnap == nil {
		return nil, ""
	}
	cur, ok := m.cfg.Control.AppDefinition(ap.name)
	if !ok || definitionImage(cur) == "" {
		return nil, ""
	}
	def, err := m.readDefinition(ctx, a, req.Target, *ap.defSnap, work)
	if err != nil {
		m.log.Warn("Cannot read the definition of %s stored with run %s, so its image is not compared: %s", ap.source, req.Run, err.Error())
		return nil, ""
	}
	img, blocked := compareImages(definitionImage(def), definitionImage(cur))
	if blocked {
		return img, BlockedImageMismatch
	}
	return img, ""
}

// workDir makes a private directory under <base>/backup/restore.
func (m *Manager) workDir(name string) (string, error) {
	root := filepath.Join(m.cfg.BaseDir, "backup", "restore")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, name+"-")
}

// StartRestore plans a restore and runs it in the background. The reply
// is the plan, with the time an in-place restore gives up; the outcome
// comes through Config.OnRestore and Restores. One restore runs at a time:
// it waits for the server's backup slot and its target, and no backup or
// maintenance starts beside it in that repository.
func (m *Manager) StartRestore(ctx context.Context, req RestoreRequest) (*RestoreResult, error) {
	m.mu.Lock()
	if m.cancel == nil {
		m.mu.Unlock()
		return nil, errors.New("backup: backups are not running")
	}
	if m.restore != nil {
		m.mu.Unlock()
		return nil, refusal(settings.CodeInUse, "Another restore is in progress.")
	}
	select {
	case <-m.recoveredCh:
	default:
		m.mu.Unlock()
		return nil, refusal(settings.CodeInUse, "Backups are still starting; try the restore again in a moment.")
	}
	m.restore = &activeRestore{result: &RestoreResult{Status: StatusPlanned}}
	runCtx := m.ctx
	m.mu.Unlock()
	clear := func() {
		m.mu.Lock()
		m.restore = nil
		m.mu.Unlock()
	}
	if err := m.recovered(); err != nil {
		clear()
		return nil, err
	}

	id := newRestoreID(m.now())
	work, err := m.workDir(id)
	if err != nil {
		clear()
		return nil, err
	}
	p, err := m.planRestore(ctx, req, work)
	if err != nil {
		os.RemoveAll(work)
		clear()
		return nil, err
	}
	p.result.ID = id
	if err := m.checkRestore(p); err != nil {
		os.RemoveAll(work)
		clear()
		return nil, err
	}
	rctx, cancel := context.WithCancel(runCtx)
	m.mu.Lock()
	if m.cancel == nil || m.released {
		m.restore = nil
		m.mu.Unlock()
		cancel()
		os.RemoveAll(work)
		return nil, errors.New("backup: backups are not running")
	}
	m.restore = &activeRestore{result: p.result, cancel: cancel}
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer cancel()
		defer os.RemoveAll(work)
		m.runRestore(rctx, p, work)
	}()
	return m.copyResult(p.result), nil
}

// activeRestore is the restore in flight.
type activeRestore struct {
	mu     sync.Mutex
	result *RestoreResult
	cancel context.CancelFunc
}

// CancelRestore stops the restore in flight, which rolls it back.
func (m *Manager) CancelRestore(id string) bool {
	m.mu.Lock()
	r := m.restore
	m.mu.Unlock()
	if r == nil || r.cancel == nil {
		return false
	}
	r.mu.Lock()
	match := r.result.ID == id
	r.mu.Unlock()
	if match {
		r.cancel()
	}
	return match
}

// Restores returns the restore in flight, if any, then the recorded ones,
// newest first.
func (m *Manager) Restores() []RestoreResult {
	var out []RestoreResult
	m.mu.Lock()
	r := m.restore
	m.mu.Unlock()
	if r != nil && r.result.ID != "" {
		out = append(out, *m.copyResult(r.result))
	}
	m.state.view(func(d *stateData) {
		for i := len(d.Restores) - 1; i >= 0; i-- {
			c := d.Restores[i]
			c.Apps = slices.Clone(c.Apps)
			out = append(out, c)
		}
	})
	return out
}

func (m *Manager) copyResult(r *RestoreResult) *RestoreResult {
	m.mu.Lock()
	a := m.restore
	m.mu.Unlock()
	if a != nil && a.result == r {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	c := *r
	c.Apps = slices.Clone(r.Apps)
	c.Restarted = slices.Clone(r.Restarted)
	return &c
}

// update changes the result in flight under its lock.
func (p *restorePlan) update(m *Manager, fn func(r *RestoreResult)) {
	m.mu.Lock()
	a := m.restore
	m.mu.Unlock()
	if a != nil && a.result == p.result {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	fn(p.result)
}

// checkRestore refuses what the restore cannot do safely before it waits
// for its slot: a deploy holding one of its apps, or a disk without room
// for the restored data beside the data it replaces, which stays until
// the restore has succeeded.
func (m *Manager) checkRestore(p *restorePlan) error {
	for _, ap := range p.apps {
		if out := ap.out; out != nil && out.Blocked == BlockedImageMismatch && out.Image != nil {
			return refusal(settings.CodeImageMismatch, "App %s ran %s when this point was taken and runs %s now. Change its version back before restoring this data.", ap.name, out.Image.Point, out.Image.Current)
		}
	}
	names := []string{}
	for _, ap := range p.apps {
		names = append(names, ap.name)
	}
	names = append(names, p.dependents...)
	if p.req.Mode == RestoreInPlace {
		m.mu.Lock()
		for _, n := range names {
			if m.deploying[n] > 0 {
				m.mu.Unlock()
				return refusal(settings.CodeInUse, "App %s is being deployed; restore it once the deploy has ended.", n)
			}
		}
		m.mu.Unlock()
	}
	need := map[string]int64{}
	dirOf := map[string]string{}
	for _, ap := range p.apps {
		app, ok := m.cfg.Apps.BackupApp(ap.source)
		var sum int64
		for _, s := range ap.allSnapshots() {
			sum += s.Bytes
		}
		dir := m.cfg.Control.AppsDir()
		if ok && len(app.Volumes) > 0 && p.req.Mode == RestoreInPlace {
			dir = app.Volumes[0].Host
		}
		for !isDir(dir) && dir != filepath.Dir(dir) {
			dir = filepath.Dir(dir)
		}
		key := dir
		dirOf[key] = dir
		need[key] += sum
	}
	for key, n := range need {
		free, ok := freeBytes(dirOf[key])
		if ok && free < n+restoreReserve {
			return refusal(settings.CodeInvalid, "The restore needs about %s bytes on the disk of %s, which has %s free.", n+restoreReserve, dirOf[key], free)
		}
	}
	return nil
}

// restoreReserve is what a restore leaves free on a disk beyond its data.
const restoreReserve = 1 << 30

// acquireRestore waits for the server's backup slot and the target, ahead
// of any scheduled backup.
func (m *Manager) acquireRestore(ctx context.Context, target string) error {
	m.mu.Lock()
	m.restoreWait[target]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.restoreWait[target]--; m.restoreWait[target] <= 0 {
			delete(m.restoreWait, target)
		}
		m.mu.Unlock()
	}()
	for {
		m.mu.Lock()
		if !m.backupSlot && m.busy[target] == "" {
			m.backupSlot = true
			m.busy[target] = KindRestore
			m.mu.Unlock()
			return nil
		}
		freed := m.freed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-freed:
		case <-time.After(m.t.poll):
		}
	}
}

func (m *Manager) releaseRestore(target string) {
	m.mu.Lock()
	if m.busy[target] == KindRestore {
		delete(m.busy, target)
	}
	m.backupSlot = false
	m.signalFreedLocked()
	m.mu.Unlock()
	m.poke()
}

// runRestore makes the restore and records how it went. A failure before
// the commit rolls everything back: the volumes get their data again and
// every app its state. A server stopping (an update) leaves the journal
// and the apps as they are, stopped, for the next server to roll back.
func (m *Manager) runRestore(ctx context.Context, p *restorePlan, work string) {
	res := p.result
	p.update(m, func(r *RestoreResult) { r.Status = StatusWaiting })
	err := m.acquireRestore(ctx, p.req.Target)
	if err == nil {
		defer m.releaseRestore(p.req.Target)
		p.update(m, func(r *RestoreResult) { r.Status, r.Start = StatusRunning, ms(m.now()) })
		m.log.Log("Restoring run %s from backup target %s (%s): %s", p.req.Run, p.req.Target, p.req.Mode, strings.Join(p.names(), ", "))
		err = m.restoreApps(ctx, p, work)
	}
	if m.isReleased() {
		m.log.Warn("Restore %s was interrupted by the server stopping; the next server rolls it back", res.ID)
		m.mu.Lock()
		if m.restore != nil && m.restore.result == res {
			m.restore = nil
		}
		m.mu.Unlock()
		return
	}
	p.update(m, func(r *RestoreResult) {
		r.End = ms(m.now())
		r.Status = StatusOK
		if err != nil {
			r.Status, r.Error = StatusFailed, err.Error()
		}
		for i, ap := range p.apps {
			if ap.out != nil && i < len(r.Apps) {
				r.Apps[i] = *ap.out
			}
		}
	})
	m.finishRestore(p.result)
}

func (p *restorePlan) names() []string {
	var out []string
	for _, ap := range p.apps {
		if ap.name != ap.source {
			out = append(out, ap.source+" as "+ap.name)
		} else {
			out = append(out, ap.name)
		}
	}
	return out
}

// restoreHistory bounds the restore records kept.
const restoreHistory = 50

func (m *Manager) finishRestore(res *RestoreResult) {
	c := *m.copyResult(res)
	err := m.state.update(func(d *stateData) {
		d.Restores = append(d.Restores, c)
		if len(d.Restores) > restoreHistory {
			d.Restores = slices.Clone(d.Restores[len(d.Restores)-restoreHistory:])
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
	m.mu.Lock()
	if m.restore != nil && m.restore.result == res {
		m.restore = nil
	}
	m.mu.Unlock()
	switch c.Status {
	case StatusOK:
		m.log.Log("Restore %s finished: %s apps back to %s", c.ID, len(c.Apps), time.UnixMilli(c.Point).UTC().Format(time.RFC3339))
	default:
		m.log.Warn("Restore %s %s: %s", c.ID, c.Status, c.Error)
	}
	if m.cfg.OnRestore != nil {
		m.cfg.OnRestore(c)
	}
}

// restoreApps is the restore itself, journal first.
func (m *Manager) restoreApps(ctx context.Context, p *restorePlan, work string) (err error) {
	ctl := m.cfg.Control
	j := &restoreJournal{ID: p.result.ID, Mode: p.req.Mode, Target: p.req.Target, Run: p.req.Run, Phase: phasePrepare, Started: ms(m.now())}
	p.update(m, func(r *RestoreResult) {
		j.Planned, j.Restarted = slices.Clone(r.Apps), slices.Clone(r.Restarted)
		j.Server, j.Job, j.Point = r.Server, r.Job, r.Point
	})
	save := func() error {
		if err := writeJSON(journalFile(m.cfg.BaseDir), j); err != nil {
			return fmt.Errorf("backup: write the restore journal: %w", err)
		}
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	phase := func(name string) error {
		j.Phase = name
		p.update(m, func(r *RestoreResult) { r.Phase = name })
		return save()
	}
	defer func() {
		if m.isReleased() {
			for _, ap := range p.apps {
				if ap.hold != nil {
					ap.hold.Abandon()
				}
			}
			m.abandonHolds(p)
			return
		}
		if err == nil {
			return
		}
		m.log.Warn("Restore %s failed, rolling it back: %s", p.result.ID, err.Error())
		if rerr := m.rollback(j, p.holds()); rerr != nil {
			err = fmt.Errorf("%w; rolling back failed too, the journal stays for the next start: %v", err, rerr)
			return
		}
		p.update(m, func(r *RestoreResult) { r.RolledBack = true })
		os.Remove(journalFile(m.cfg.BaseDir))
	}()

	if p.req.Mode == RestoreInPlace && !p.req.SkipSafety {
		run, err := m.safetyBackup(ctx, p)
		if err != nil {
			return fmt.Errorf("backup: the current state could not be backed up first, so nothing was restored (a restore without it must be asked for): %w", err)
		}
		p.update(m, func(r *RestoreResult) { r.Safety = run })
	}

	if p.req.Mode == RestoreNewApp {
		renames := map[string]string{}
		for _, ap := range p.apps {
			renames[ap.source] = ap.name
		}
		// A copy next to its original joins a template group of its own;
		// apps that keep their names (a fresh server) keep their group.
		group := ""
		for _, ap := range p.apps {
			if ap.name != ap.source {
				id := p.result.ID
				group = "restored-" + id[max(0, len(id)-6):]
				break
			}
		}
		for _, ap := range p.apps {
			g := ""
			if tpl, _ := ap.def["template"].(map[string]any); tpl != nil && group != "" {
				g, _ = tpl["group"].(string)
				g += "-" + group
			}
			dropped, err := ctl.CreateRestoredApp(ap.def, ap.name, renames, g)
			if err != nil {
				return fmt.Errorf("backup: create app %s: %w", ap.name, err)
			}
			j.Apps = append(j.Apps, journalApp{Name: ap.name, Created: true})
			if err := save(); err != nil {
				return err
			}
			ap.out.Dropped = dropped
		}
	}

	// Apps that link a restored one stop too: they would write into a
	// database being replaced.
	for _, name := range p.dependents {
		h, err := ctl.HoldApp(name)
		if err != nil {
			return fmt.Errorf("backup: stop %s: %w", name, err)
		}
		p.dependentHolds = append(p.dependentHolds, depHold{name, h})
		j.Apps = append(j.Apps, journalApp{Name: name, WasActive: h.WasActive(), Held: true})
		if err := save(); err != nil {
			return err
		}
	}
	for _, ap := range p.apps {
		h, err := ctl.HoldApp(ap.name)
		if err != nil {
			return fmt.Errorf("backup: stop %s: %w", ap.name, err)
		}
		ap.hold = h
		if i := slices.IndexFunc(j.Apps, func(a journalApp) bool { return a.Name == ap.name }); i >= 0 {
			j.Apps[i].Held, j.Apps[i].WasActive = true, h.WasActive()
		} else {
			j.Apps = append(j.Apps, journalApp{Name: ap.name, WasActive: h.WasActive(), Held: true})
		}
		if err := save(); err != nil {
			return err
		}
	}

	if err := phase(phaseStage); err != nil {
		return err
	}
	for _, ap := range p.apps {
		if err := m.planVolumes(ctx, p, ap, j); err != nil {
			return err
		}
		if err := save(); err != nil {
			return err
		}
	}
	for _, ap := range p.apps {
		for _, v := range ap.volumes {
			snap := ap.dirFor(v)
			if snap.ID == "" {
				continue
			}
			if err := v.prepare(); err != nil {
				return err
			}
			if v.Mode == modeReplace {
				v.Phase = volStaged
			}
			if err := save(); err != nil {
				return err
			}
			dest := v.Path
			if v.Mode == modeReplace {
				dest = v.staging()
			}
			if err := m.restoreDir(ctx, p.access, p.req.Target, snap.ID, dest); err != nil {
				return fmt.Errorf("backup: restore %s of %s: %w", snap.Source.Name, ap.source, err)
			}
		}
	}

	if err := phase(phaseSwap); err != nil {
		return err
	}
	for _, ap := range p.apps {
		for _, v := range ap.volumes {
			if v.Mode == modeNew {
				v.Phase = volIn
				continue
			}
			if v.Mode == modeEmpty {
				v.Staging = ""
				v.keepBeside(j.Volumes)
			}
			v.Phase = volOut
			if err := save(); err != nil {
				return err
			}
			if err := v.moveOut(); err != nil {
				return fmt.Errorf("backup: set the data of %s aside: %w", ap.name, err)
			}
			v.Phase = volIn
			if err := save(); err != nil {
				return err
			}
			if err := v.moveIn(); err != nil {
				return fmt.Errorf("backup: swap in the data of %s: %w", ap.name, err)
			}
		}
		if err := m.placeExtras(ctx, p, ap); err != nil {
			return err
		}
	}

	if err := phase(phaseLoad); err != nil {
		return err
	}
	for _, ap := range orderByLinks(p.apps) {
		if ap.load == nil || ap.load.File != "" {
			continue
		}
		if err := m.loadDump(ctx, p, ap); err != nil {
			return err
		}
	}

	if err := phase(phaseCommitted); err != nil {
		return err
	}
	for _, ap := range p.apps {
		for _, v := range ap.volumes {
			if err := v.commit(); err != nil {
				m.log.Error("Restore %s: could not remove the replaced data of %s in %s: %s", p.result.ID, ap.name, v.Path, err.Error())
			}
		}
	}
	m.releaseAll(p)
	os.Remove(journalFile(m.cfg.BaseDir))
	return nil
}

type depHold struct {
	name string
	hold AppHold
}

func (p *restorePlan) holds() map[string]AppHold {
	out := map[string]AppHold{}
	for _, ap := range p.apps {
		if ap.hold != nil {
			out[ap.name] = ap.hold
		}
	}
	for _, d := range p.dependentHolds {
		out[d.name] = d.hold
	}
	return out
}

func (m *Manager) abandonHolds(p *restorePlan) {
	for _, d := range p.dependentHolds {
		d.hold.Abandon()
	}
}

// releaseAll gives the apps back after a committed restore: the restored
// ones first, then the apps that link them. A restored app runs if it was
// meant to, a new one always.
func (m *Manager) releaseAll(p *restorePlan) {
	for _, ap := range orderByLinks(p.apps) {
		active := ap.hold.WasActive() || p.req.Mode == RestoreNewApp
		if err := ap.hold.Release(active); err != nil {
			m.log.Error("Restore %s: %s did not start after the restore: %s", p.result.ID, ap.name, err.Error())
			ap.out.Warnings = append(ap.out.Warnings, "the app did not start after the restore: "+err.Error())
		}
	}
	for _, d := range p.dependentHolds {
		if err := d.hold.Release(d.hold.WasActive()); err != nil {
			m.log.Error("Restore %s: %s did not start after the restore: %s", p.result.ID, d.name, err.Error())
		}
	}
}

// orderByLinks puts every app after the apps it links, so a database is
// loaded and running before the app that uses it starts.
func orderByLinks(apps []*appPlan) []*appPlan {
	byName := map[string]*appPlan{}
	for _, ap := range apps {
		byName[ap.source] = ap
	}
	var out []*appPlan
	seen := map[string]bool{}
	var visit func(ap *appPlan)
	visit = func(ap *appPlan) {
		if seen[ap.source] {
			return
		}
		seen[ap.source] = true
		for _, l := range ap.links {
			if dep := byName[l]; dep != nil {
				visit(dep)
			}
		}
		out = append(out, ap)
	}
	for _, ap := range apps {
		visit(ap)
	}
	return out
}

func (ap *appPlan) dirFor(v *volumeJob) proto.Snapshot {
	for _, s := range ap.dirs {
		if s.Source.Name == v.source {
			return s
		}
	}
	return proto.Snapshot{}
}

// planVolumes decides what happens to each of the app's volumes and how
// its dump is loaded. A volume with a files snapshot is replaced by it;
// with a dump the engine's data volumes are emptied so the database
// starts fresh from the image's own init; anything else is left as it is
// and said so.
func (m *Manager) planVolumes(ctx context.Context, p *restorePlan, ap *appPlan, j *restoreJournal) error {
	app, ok := m.cfg.Apps.BackupApp(ap.name)
	if !ok {
		return fmt.Errorf("backup: app %s is gone", ap.name)
	}
	ap.app = app
	root := m.cfg.Control.AppsDir()
	if root != "" {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
	}
	tag := ".odac-restore-" + strings.TrimPrefix(p.result.ID, "r")
	fresh := map[string]bool{}
	if ap.dump != nil {
		// The container is stopped, not removed, so it still says how it
		// was started; a new app has none and keeps its record's values.
		if _, err := m.describeRunning(ctx, ap); err != nil {
			return err
		}
		load, err := consistency.Shape(ap.app, ap.dump.Source.Name)
		if err != nil {
			return fmt.Errorf("backup: %s: %w", ap.name, err)
		}
		ap.load = load
		if load.Fresh {
			for _, v := range consistency.DataVolumes(ap.app, load.Engine) {
				fresh[v.Container] = true
			}
		}
	}
	used := map[string]bool{}
	for _, v := range ap.app.Volumes {
		name := consistency.DirName(v.Container)
		path := filepath.Clean(v.Host)
		if root != "" && !within(path, root) {
			if r, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
				path = filepath.Join(r, filepath.Base(path))
			}
		}
		vj := &volumeJob{App: ap.name, Path: path, Root: root, source: name}
		switch {
		case fresh[v.Container]:
			vj.Mode, vj.Keep = modeEmpty, tag+"-keep"
			if p.req.Mode == RestoreNewApp {
				vj.Mode, vj.Keep = modeNew, ""
				if err := vj.prepare(); err != nil {
					return err
				}
				vj.Phase = volIn
			}
		case ap.hasDir(name):
			used[name] = true
			vj.Mode, vj.Staging, vj.Keep = modeReplace, tag, tag+"-keep"
			if p.req.Mode == RestoreNewApp {
				vj.Mode, vj.Staging, vj.Keep = modeNew, "", ""
			}
		default:
			if p.req.Mode == RestoreNewApp {
				vj.Mode = modeNew
				if err := vj.prepare(); err != nil {
					return err
				}
				vj.Phase = volIn
			} else {
				ap.out.Warnings = append(ap.out.Warnings, fmt.Sprintf("%s is not in the backup and is left as it is", strings.TrimSuffix(v.Container, ":ro")))
				continue
			}
		}
		ap.volumes = append(ap.volumes, vj)
		j.Volumes = append(j.Volumes, vj)
	}
	for _, s := range ap.dirs {
		if !used[s.Source.Name] && !strings.HasPrefix(s.Source.Name, "clickhouse-") {
			ap.out.Warnings = append(ap.out.Warnings, fmt.Sprintf("%s has no volume to go to and is not restored", s.Source.Name))
		}
	}
	return nil
}

func (ap *appPlan) hasDir(name string) bool {
	return slices.ContainsFunc(ap.dirs, func(s proto.Snapshot) bool { return s.Source.Name == name })
}

// describeRunning fills in what the app's container says about itself:
// the image it runs and its environment, where the credentials are.
func (m *Manager) describeRunning(ctx context.Context, ap *appPlan) (docker.AppState, error) {
	st, err := m.cfg.States.BackupAppState(ctx, ap.app.Container)
	if err != nil {
		return st, fmt.Errorf("backup: inspect %s: %w", ap.name, err)
	}
	if st.ImageID == "" {
		return st, nil
	}
	ap.app.ImageID, ap.app.Env, ap.app.MemoryLimit = st.ImageID, st.Env, st.MemoryLimit
	if len(st.Cmd) > 0 {
		ap.app.Cmd = st.Cmd
	}
	return st, nil
}

// placeExtras puts what goes into an emptied data volume before its
// database starts: Redis's save file, and ClickHouse's SQL users and the
// frozen parts it attaches after the load.
func (m *Manager) placeExtras(ctx context.Context, p *restorePlan, ap *appPlan) error {
	if ap.load == nil {
		return nil
	}
	if ap.load.File != "" {
		return m.placeFile(ctx, p, ap)
	}
	if !ap.load.AttachesParts() {
		return nil
	}
	root := consistency.ClickHouseRoot(ap.app)
	for _, s := range ap.dirs {
		var dest string
		switch s.Source.Name {
		case "clickhouse-access":
			if root == "" {
				continue
			}
			host, ok := consistency.HostPath(ap.app, root+"/access")
			if !ok {
				continue
			}
			dest = host
		case "clickhouse-parts":
			if root == "" {
				ap.out.Warnings = append(ap.out.Warnings, "ClickHouse's data directory is not where ODAC knows it; its MergeTree parts are not restored")
				continue
			}
			host, ok := consistency.HostPath(ap.app, root+"/"+partsStaging(p))
			if !ok {
				continue
			}
			dest = host
		default:
			continue
		}
		v := ap.volumeHolding(dest)
		if v == nil {
			return fmt.Errorf("backup: %s of %s has no volume to go to", s.Source.Name, ap.name)
		}
		if err := ensureDir(v.Root, dest, 0o700); err != nil {
			return err
		}
		if err := m.restoreDir(ctx, p.access, p.req.Target, s.ID, dest); err != nil {
			return fmt.Errorf("backup: restore %s of %s: %w", s.Source.Name, ap.source, err)
		}
	}
	return nil
}

func partsStaging(p *restorePlan) string {
	return ".odac-restore-" + strings.TrimPrefix(p.result.ID, "r") + "-parts"
}

func (ap *appPlan) volumeHolding(p string) *volumeJob {
	var best *volumeJob
	for _, v := range ap.volumes {
		if within(p, v.Path) && (best == nil || len(v.Path) > len(best.Path)) {
			best = v
		}
	}
	return best
}

// placeFile writes a dump the engine loads from a file (Redis's RDB) into
// the emptied volume, owned like the volume, since the database reads it
// as its own user.
func (m *Manager) placeFile(ctx context.Context, p *restorePlan, ap *appPlan) error {
	host, ok := consistency.HostPath(ap.app, ap.load.File)
	if !ok {
		return fmt.Errorf("backup: %s of %s is not on a volume", ap.load.File, ap.name)
	}
	v := ap.volumeHolding(host)
	if v == nil {
		return fmt.Errorf("backup: %s of %s is not on a volume this restore emptied", ap.load.File, ap.name)
	}
	if err := ensureDir(v.Root, filepath.Dir(host), 0o755); err != nil {
		return err
	}
	rc, err := m.streamSnapshot(ctx, p.access, p.req.Target, ap.dump.ID)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := writeRDB(host, rc); err != nil {
		return fmt.Errorf("backup: write %s of %s: %w", ap.load.File, ap.name, err)
	}
	files := []string{host}
	if ap.load.AOF != "" {
		aof, ok := consistency.HostPath(ap.app, ap.load.AOF)
		if !ok || ap.volumeHolding(aof) != v {
			return fmt.Errorf("backup: %s of %s is not on the volume of its save", ap.load.AOF, ap.name)
		}
		src, err := os.Open(host)
		if err != nil {
			return err
		}
		err = writeRDB(aof, src)
		src.Close()
		if err != nil {
			return fmt.Errorf("backup: write %s of %s: %w", ap.load.AOF, ap.name, err)
		}
		files = append(files, aof)
	}
	for _, w := range ap.load.Warnings {
		ap.out.Warnings = append(ap.out.Warnings, w.Message)
	}
	if st, err := os.Stat(v.Path); err == nil && os.Geteuid() == 0 {
		if uid, gid, ok := ownerOf(st); ok {
			for _, f := range files {
				if err := os.Lchown(f, uid, gid); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeRDB writes a Redis dump to a new file, without the end mark of a
// diskless transfer that the stored stream may carry.
func writeRDB(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if err == nil && n >= 64 {
		tail := make([]byte, 64)
		if _, err = f.ReadAt(tail, n-64); err == nil {
			if cut := consistency.RDBTrailerSize(tail); cut > 0 {
				err = f.Truncate(n - int64(cut))
			}
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// loadDump starts the database on its emptied volumes, waits until its
// image has initialized it and loads the dump with the engine's own
// client.
func (m *Manager) loadDump(ctx context.Context, p *restorePlan, ap *appPlan) error {
	if err := ap.hold.Start(); err != nil {
		return fmt.Errorf("backup: start %s: %w", ap.name, err)
	}
	ap.started = true
	// The container is new: a load reads its image and environment again.
	st, err := m.describeRunning(ctx, ap)
	if err != nil {
		return err
	}
	load, err := m.cfg.Loader.Load(ctx, ap.app, ap.dump.Source.Name, ap.dump.Consistency)
	if err != nil {
		return fmt.Errorf("backup: %s: %w", ap.name, err)
	}
	rctx, cancel := context.WithTimeout(ctx, readyLimit)
	err = load.WaitReady(rctx, m.crashCheck(ap.app.Container, st.Restarts))
	cancel()
	if err != nil {
		return fmt.Errorf("backup: %s: %w", ap.name, err)
	}
	r, err := m.currentRunner()
	if err != nil {
		return err
	}
	rc, err := m.streamSnapshot(ctx, p.access, p.req.Target, ap.dump.ID)
	if err != nil {
		return err
	}
	defer rc.Close()
	stderr, err := r.Load(ctx, *load.Spec, rc, map[string]string{labelKind: KindRestore, labelTarget: p.req.Target})
	if err == nil && load.Judge != nil {
		err = load.Judge(stderr)
	}
	if err != nil {
		return fmt.Errorf("backup: load the %s dump of %s: %w", ap.dump.Source.Name, ap.name, err)
	}
	if err := load.Finish(ctx); err != nil {
		return fmt.Errorf("backup: %s: %w", ap.name, err)
	}
	if load.AttachesParts() {
		if err := m.attachParts(ctx, p, ap, load); err != nil {
			return err
		}
	}
	m.log.Log("Restore %s: loaded the %s dump of %s", p.result.ID, ap.dump.Source.Name, ap.name)
	return nil
}

// readyLimit bounds the wait for a database's first start, which runs its
// image's init. crashRestarts is how many restarts by the engine since the
// start make a database crash-looping rather than slow.
const (
	readyLimit    = 10 * time.Minute
	crashRestarts = 3
)

// crashCheck reports a container the engine keeps restarting, so a
// database that cannot start on the restored data fails the restore at
// once instead of after readyLimit. A failed inspect is no verdict.
func (m *Manager) crashCheck(container string, base int) func(context.Context) error {
	return func(ctx context.Context) error {
		st, err := m.cfg.States.BackupAppState(ctx, container)
		if err != nil || st.Restarts-base < crashRestarts {
			return nil
		}
		return fmt.Errorf("the container keeps restarting (%d restarts, last exit code %d)", st.Restarts-base, st.ExitCode)
	}
}

// attachParts moves each frozen part into its table's detached directory
// and attaches it. The tables were made again with their UUIDs, so a part
// stored under store/<prefix>/<uuid>/ belongs to the table with that UUID.
func (m *Manager) attachParts(ctx context.Context, p *restorePlan, ap *appPlan, load *consistency.Load) error {
	root := consistency.ClickHouseRoot(ap.app)
	staging, ok := consistency.HostPath(ap.app, root+"/"+partsStaging(p))
	if !ok || !isDir(staging) {
		return nil
	}
	tables, disk, err := load.Tables(ctx)
	if err != nil {
		return err
	}
	data, ok := consistency.HostPath(ap.app, disk)
	if !ok {
		return fmt.Errorf("backup: ClickHouse's disk %s is not on a volume", disk)
	}
	v := ap.volumeHolding(staging)
	parts := map[string][]string{}
	store := filepath.Join(staging, "store")
	prefixes, _ := entries(store)
	for _, pre := range prefixes {
		uuids, _ := entries(filepath.Join(store, pre))
		for _, uuid := range uuids {
			ref := tables[uuid]
			names, _ := entries(filepath.Join(store, pre, uuid))
			if ref == "" {
				if len(names) > 0 {
					ap.out.Warnings = append(ap.out.Warnings, fmt.Sprintf("ClickHouse parts of table %s have no table to go to", uuid))
				}
				continue
			}
			detached := filepath.Join(data, "store", pre, uuid, "detached")
			if err := ensureDir(v.Root, detached, 0o750); err != nil {
				return err
			}
			if st, err := os.Stat(filepath.Dir(detached)); err == nil && os.Geteuid() == 0 {
				if uid, gid, ok := ownerOf(st); ok {
					os.Lchown(detached, uid, gid)
				}
			}
			for _, part := range names {
				if err := os.Rename(filepath.Join(store, pre, uuid, part), filepath.Join(detached, part)); err != nil {
					return fmt.Errorf("backup: move ClickHouse part %s: %w", part, err)
				}
				parts[ref] = append(parts[ref], part)
			}
		}
	}
	if err := load.Attach(ctx, parts); err != nil {
		return err
	}
	return os.RemoveAll(staging)
}

// rollback undoes a restore from its journal: volumes back in reverse
// order, created apps deleted, and every held app released with the
// state it had. holds are the holds this process still has; recovery
// after a restart takes new ones.
func (m *Manager) rollback(j *restoreJournal, holds map[string]AppHold) error {
	ctl := m.cfg.Control
	for _, a := range j.Apps {
		if holds[a.Name] == nil && (a.Held || a.Created) {
			h, err := ctl.HoldApp(a.Name)
			if err != nil {
				m.log.Warn("Rolling back restore %s: cannot stop %s: %s", j.ID, a.Name, err.Error())
				continue
			}
			holds[a.Name] = h
		} else if h := holds[a.Name]; h != nil {
			h.Stop()
		}
	}
	var first error
	for i := len(j.Volumes) - 1; i >= 0; i-- {
		v := j.Volumes[i]
		if err := v.rollback(); err != nil && first == nil {
			first = fmt.Errorf("put back %s: %w", v.Path, err)
		}
	}
	if first != nil {
		return first
	}
	for _, a := range j.Apps {
		h := holds[a.Name]
		switch {
		case a.Created:
			if h != nil {
				h.Release(false)
			}
			if err := ctl.DeleteRestoredApp(a.Name); err != nil {
				m.log.Error("Rolling back restore %s: %s", j.ID, err.Error())
			}
		case h != nil:
			if err := h.Release(a.WasActive); err != nil {
				m.log.Error("Rolling back restore %s: %s did not start: %s", j.ID, a.Name, err.Error())
			}
		default:
			ctl.SetAppActive(a.Name, a.WasActive)
		}
	}
	for _, t := range j.Temp {
		os.RemoveAll(t)
	}
	return nil
}

// recoverRestore finishes what a restore left when this server's
// predecessor stopped or crashed in the middle of it: a committed one is
// cleaned up and its apps given back, anything else rolled back. It runs
// after the previous server's helpers are adopted or removed, so none of
// them still writes into a staging directory.
func (m *Manager) recoverRestore() {
	j, err := readJournal(m.cfg.BaseDir)
	if err != nil {
		m.log.Error("%s; restores stay off until it is resolved by hand", err.Error())
		m.mu.Lock()
		m.recoverErr = err
		m.mu.Unlock()
		return
	}
	if j == nil || m.cfg.Control == nil {
		return
	}
	res := RestoreResult{ID: j.ID, Mode: j.Mode, Target: j.Target, Server: j.Server, Run: j.Run, Job: j.Job,
		Point: j.Point, Apps: j.Planned, Restarted: j.Restarted, Start: j.Started, Phase: j.Phase}
	if res.Apps == nil {
		res.Apps = []AppRestore{}
	}
	if j.Phase == phaseCommitted {
		for _, v := range j.Volumes {
			if err := v.commit(); err != nil {
				m.log.Error("Restore %s: could not remove the replaced data in %s: %s", j.ID, v.Path, err.Error())
			}
		}
		for _, a := range j.Apps {
			active := a.WasActive || a.Created
			if h, err := m.cfg.Control.HoldApp(a.Name); err == nil {
				h.Release(active)
			} else {
				m.cfg.Control.SetAppActive(a.Name, active)
			}
		}
		res.Status = StatusOK
	} else {
		m.log.Warn("Restore %s did not finish before the server stopped; rolling it back", j.ID)
		if err := m.rollback(j, map[string]AppHold{}); err != nil {
			m.log.Error("Rolling back restore %s failed: %s; restores stay off until it is resolved by hand", j.ID, err.Error())
			m.mu.Lock()
			m.recoverErr = err
			m.mu.Unlock()
			return
		}
		res.Status, res.RolledBack = StatusFailed, true
		res.Error = "the server stopped during the restore"
	}
	os.Remove(journalFile(m.cfg.BaseDir))
	res.End = ms(m.now())
	m.finishRestore(&res)
}

// recovered refuses a restore while an interrupted one could not be
// resolved.
func (m *Manager) recovered() error {
	m.mu.Lock()
	rerr := m.recoverErr
	m.mu.Unlock()
	if rerr != nil {
		return fmt.Errorf("backup: an interrupted restore could not be rolled back (%v); see %s", rerr, journalFile(m.cfg.BaseDir))
	}
	// The journal of an interrupted restore is rolled back once the
	// previous server's helpers are adopted; a new restore must not
	// replace it before that.
	if _, err := os.Stat(journalFile(m.cfg.BaseDir)); err == nil {
		return refusal(settings.CodeInUse, "An interrupted restore is being rolled back; try again in a moment.")
	}
	return nil
}

// addDefinitions writes each app's definition into a private directory and
// adds it to the app's piece as one more directory snapshot, so an app can
// be restored as a new one from any of its backups. The returned function
// removes the directories.
func (m *Manager) addDefinitions(run, job string, pieces []*piece) func() {
	if m.cfg.Control == nil {
		return func() {}
	}
	root := filepath.Join(m.cfg.BaseDir, "backup", "definitions", run)
	for _, pc := range pieces {
		for _, app := range pc.apps {
			def, ok := m.cfg.Control.AppDefinition(app)
			if !ok {
				continue
			}
			dir := filepath.Join(root, app)
			if err := writeDefinition(dir, def); err != nil {
				m.log.Warn("The definition of %s is not backed up: %s", app, err.Error())
				continue
			}
			pc.items = append(pc.items, proto.Item{Source: proto.Source{Job: job, App: app, Name: definitionSource}, Consistency: consistency.Consistent, Dir: dir})
		}
	}
	return func() { os.RemoveAll(root) }
}
