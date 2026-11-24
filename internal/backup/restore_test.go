package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/docker"
)

// fakeControl plays the app manager for restores.
type fakeControl struct {
	h       *harness
	mu      sync.Mutex
	active  map[string]bool
	busy    map[string]bool
	events  []string
	defs    map[string]map[string]any
	created []string
	deleted []string
	// onStart sees each held app start, as its database would.
	onStart func(name string)
}

func newControl(h *harness) *fakeControl {
	c := &fakeControl{h: h, active: map[string]bool{}, busy: map[string]bool{}, defs: map[string]map[string]any{}}
	h.m.cfg.Control = c
	h.m.cfg.Loader = fakeLoader{}
	return c
}

func (c *fakeControl) event(format string, a ...any) {
	c.mu.Lock()
	c.events = append(c.events, fmt.Sprintf(format, a...))
	c.mu.Unlock()
}

func (c *fakeControl) has(e string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.events, e)
}

func (c *fakeControl) HoldApp(name string) (AppHold, error) {
	if _, ok := c.h.apps.BackupApp(name); !ok {
		return nil, fmt.Errorf("app %s not found", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[name] {
		return nil, errors.New("busy")
	}
	c.busy[name] = true
	was := c.active[name]
	c.active[name] = false
	c.events = append(c.events, "hold "+name)
	return &fakeHold{c: c, name: name, was: was}, nil
}

func (c *fakeControl) SetAppActive(name string, active bool) bool {
	c.mu.Lock()
	c.active[name] = active
	c.mu.Unlock()
	c.event("active %s %v", name, active)
	return true
}

func (c *fakeControl) AppDefinition(name string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.defs[name]
	return d, ok
}

func (c *fakeControl) UniqueAppName(base string) string {
	name := base
	for i := 1; ; i++ {
		if _, ok := c.h.apps.BackupApp(name); !ok {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

func (c *fakeControl) AppsDir() string { return filepath.Join(c.h.base, "apps") }

func (c *fakeControl) CreateRestoredApp(def map[string]any, name string, renames map[string]string, group string) ([]string, error) {
	old, _ := def["name"].(string)
	src, ok := c.h.apps.BackupApp(old)
	if !ok {
		vols, _ := def["volumes"].([]any)
		for _, v := range vols {
			m := v.(map[string]any)
			src.Volumes = append(src.Volumes, consistency.Volume{Host: m["host"].(string), Container: m["container"].(string)})
		}
	}
	app := consistency.App{Name: name, Container: name, Image: src.Image}
	for _, v := range src.Volumes {
		app.Volumes = append(app.Volumes, consistency.Volume{Host: filepath.Join(c.AppsDir(), name, filepath.Base(v.Host)), Container: v.Container})
	}
	c.h.apps.mu.Lock()
	c.h.apps.apps[name] = app
	var linked []string
	for _, l := range definitionLinks(def) {
		linked = append(linked, renames[l])
	}
	c.h.apps.list = append(c.h.apps.list, settings.App{Name: name, Stateful: true, Linked: linked})
	c.h.apps.mu.Unlock()
	c.mu.Lock()
	c.created = append(c.created, name)
	c.mu.Unlock()
	return []string{"the published host port 8080"}, nil
}

func (c *fakeControl) DeleteRestoredApp(name string) error {
	c.h.apps.mu.Lock()
	delete(c.h.apps.apps, name)
	c.h.apps.list = slices.DeleteFunc(c.h.apps.list, func(a settings.App) bool { return a.Name == name })
	c.h.apps.mu.Unlock()
	c.mu.Lock()
	c.deleted = append(c.deleted, name)
	c.mu.Unlock()
	return os.RemoveAll(filepath.Join(c.AppsDir(), name))
}

type fakeHold struct {
	c    *fakeControl
	name string
	was  bool
}

func (h *fakeHold) WasActive() bool { return h.was }
func (h *fakeHold) Start() error {
	if h.c.onStart != nil {
		h.c.onStart(h.name)
	}
	h.c.event("start %s", h.name)
	return nil
}
func (h *fakeHold) Stop() { h.c.event("stop %s", h.name) }
func (h *fakeHold) Abandon() {
	h.c.mu.Lock()
	delete(h.c.busy, h.name)
	h.c.mu.Unlock()
	h.c.event("abandon %s", h.name)
}

func (h *fakeHold) Release(active bool) error {
	h.c.mu.Lock()
	delete(h.c.busy, h.name)
	h.c.active[h.name] = active
	h.c.mu.Unlock()
	h.c.event("release %s %v", h.name, active)
	return nil
}

type fakeLoader struct{}

func (fakeLoader) Load(_ context.Context, app consistency.App, name, level string) (*consistency.Load, error) {
	l, err := consistency.Shape(app, name)
	if err != nil {
		return nil, err
	}
	if l.File == "" {
		l.Spec = &docker.DumpSpec{Image: app.Image, App: app.Container, Cmd: []string{"psql"}}
	}
	return l, nil
}

// restoreSetup makes a web app linking a Postgres db app, backs both up,
// then changes their data, and starts the manager.
type restoreSetup struct {
	*harness
	ctl      *fakeControl
	runID    string
	web, pg  string
	restores chan RestoreResult
}

func newRestoreSetup(t *testing.T) *restoreSetup {
	h := newHarness(t)
	ctl := newControl(h)
	s := &restoreSetup{harness: h, ctl: ctl, restores: make(chan RestoreResult, 4)}
	h.m.cfg.OnRestore = func(r RestoreResult) { s.restores <- r }
	h.app("web", "db")
	s.web = filepath.Join(h.base, "apps", "web", "data")
	s.pg = filepath.Join(h.base, "apps", "db", "pg")
	os.MkdirAll(s.pg, 0o755)
	h.apps.mu.Lock()
	h.apps.list = append(h.apps.list, settings.App{Name: "db", Stateful: true})
	h.apps.apps["db"] = consistency.App{Name: "db", Container: "db", Image: "postgres:16", Volumes: []consistency.Volume{{Host: s.pg, Container: "/var/lib/postgresql/data"}}}
	h.apps.mu.Unlock()
	ctl.active["web"], ctl.active["db"] = true, true
	ctl.defs["web"] = map[string]any{"name": "web", "type": "container", "env": map[string]any{"manual": map[string]any{}, "linked": []any{"db"}}}
	ctl.defs["db"] = map[string]any{"name": "db", "type": "container", "image": "postgres:16.4"}

	os.WriteFile(filepath.Join(s.web, "a.txt"), []byte("v1"), 0o644)
	os.MkdirAll(filepath.Join(s.web, "sub"), 0o755)
	os.WriteFile(filepath.Join(s.web, "sub", "b.txt"), []byte("b1"), 0o644)
	os.WriteFile(filepath.Join(s.pg, "PG_VERSION"), []byte("16"), 0o644)
	j := h.job("j1", "t1", 60, "web", "db")
	rec := h.runNow(j)
	if rec.Status != StatusOK {
		t.Fatalf("backup: %+v", rec)
	}
	s.runID = rec.Run
	h.set.mu.Lock()
	h.set.jobs = nil
	h.set.mu.Unlock()

	os.WriteFile(filepath.Join(s.web, "a.txt"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(s.web, "new.txt"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(s.pg, "PG_VERSION"), []byte("16-live"), 0o644)
	h.clock.Add(time.Hour)
	h.m.Start()
	t.Cleanup(h.m.Stop)
	waitRecovered(t, h.m)
	return s
}

// waitRecovered waits until a started manager takes restores.
func waitRecovered(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	ch := m.recoveredCh
	m.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the manager did not finish starting")
	}
}

func (s *restoreSetup) wait() RestoreResult {
	s.t.Helper()
	select {
	case r := <-s.restores:
		return r
	case <-time.After(10 * time.Second):
		s.t.Fatal("the restore did not finish")
	}
	return RestoreResult{}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func noLeftovers(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		names, _ := entries(d)
		for _, n := range names {
			if strings.HasPrefix(n, ".odac-restore") {
				t.Errorf("%s left %s behind", d, n)
			}
		}
	}
}

func TestRestoreInPlaceReturnsTheGroup(t *testing.T) {
	s := newRestoreSetup(t)
	// The image initializes only an empty data directory (postgres with
	// PGDATA at the mount point), so the kept data must not be in it.
	var atStart []string
	var planned []AppRestore
	s.ctl.onStart = func(name string) {
		if name == "db" {
			atStart, _ = entries(s.pg)
			if atStart == nil {
				atStart = []string{}
			}
			if j, err := readJournal(s.base); err == nil && j != nil {
				planned = j.Planned
			}
		}
	}
	got, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Apps: []string{"web"}, Mode: RestoreInPlace})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lost != time.Hour.Milliseconds() || len(got.Apps) != 2 {
		t.Fatalf("plan: %+v", got)
	}
	res := s.wait()
	if res.Status != StatusOK || res.Safety == "" {
		t.Fatalf("restore: %+v", res)
	}
	if read(t, filepath.Join(s.web, "a.txt")) != "v1" || read(t, filepath.Join(s.web, "sub", "b.txt")) != "b1" {
		t.Fatal("web's files did not come back")
	}
	if _, err := os.Stat(filepath.Join(s.web, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("a file written after the backup survived the restore")
	}
	// The database volume is emptied for its image to initialize, and
	// the dump is loaded into it.
	if atStart == nil || len(atStart) != 0 {
		t.Fatalf("the database started on %v", atStart)
	}
	// A server taking over mid-restore names the apps from the journal.
	if len(planned) != 2 {
		t.Fatalf("the journal plans %+v", planned)
	}
	if names, _ := entries(s.pg); len(names) != 0 {
		t.Fatalf("database volume holds %v", names)
	}
	s.harness.run.mu.Lock()
	loads := slices.Clone(s.harness.run.loads)
	s.harness.run.mu.Unlock()
	if len(loads) != 1 || string(loads[0].data) != "dump of db" {
		t.Fatalf("loads %+v", loads)
	}
	noLeftovers(t, s.web, s.pg, filepath.Dir(s.pg))
	// The state before the restore is a snapshot, filed under the app
	// asked for, not a run on the target.
	var safety []proto.Snapshot
	for _, sn := range s.harness.run.repo(s.harness.snapRepo()).snaps {
		if sn.Run == res.Safety {
			safety = append(safety, sn)
		}
	}
	if len(safety) < 3 || safety[0].Source.Job != "web" || safety[0].Trigger != EventPreRestore {
		t.Fatalf("safety snapshot %+v", safety)
	}
	for _, sn := range s.harness.run.repo("t1").snaps {
		if sn.Run == res.Safety {
			t.Fatal("the safety copy went to the target")
		}
	}
	for _, want := range []string{"hold web", "hold db", "start db", "release db true", "release web true"} {
		if !s.ctl.has(want) {
			t.Errorf("no %q in %v", want, s.ctl.events)
		}
	}
	if _, err := os.Stat(journalFile(s.base)); !os.IsNotExist(err) {
		t.Fatal("the journal stayed")
	}
	if list := s.m.Restores(); len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("restores %+v", list)
	}
}

func TestRestoreRollsBackAFailedLoad(t *testing.T) {
	s := newRestoreSetup(t)
	s.harness.run.mu.Lock()
	s.harness.run.loadErr = errors.New("psql: disk full")
	s.harness.run.mu.Unlock()
	if _, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace}); err != nil {
		t.Fatal(err)
	}
	res := s.wait()
	if res.Status != StatusFailed || !res.RolledBack || !strings.Contains(res.Error, "disk full") {
		t.Fatalf("restore: %+v", res)
	}
	if read(t, filepath.Join(s.web, "a.txt")) != "v2" || read(t, filepath.Join(s.web, "new.txt")) != "new" || read(t, filepath.Join(s.pg, "PG_VERSION")) != "16-live" {
		t.Fatal("the live data did not come back")
	}
	noLeftovers(t, s.web, s.pg)
	if !s.ctl.has("release web true") || !s.ctl.has("release db true") {
		t.Fatalf("apps not given back: %v", s.ctl.events)
	}
}

func TestRestoreRefusedWithoutSafetyBackup(t *testing.T) {
	s := newRestoreSetup(t)
	s.harness.run.mu.Lock()
	s.harness.run.failOp = proto.OpBackup
	s.harness.run.mu.Unlock()
	if _, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace}); err != nil {
		t.Fatal(err)
	}
	res := s.wait()
	if res.Status != StatusFailed || !strings.Contains(res.Error, "backed up first") {
		t.Fatalf("restore: %+v", res)
	}
	if read(t, filepath.Join(s.web, "a.txt")) != "v2" || s.ctl.has("hold web") {
		t.Fatal("the restore touched an app without its safety backup")
	}
}

func TestRestoreAsNewApps(t *testing.T) {
	s := newRestoreSetup(t)
	got, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Apps: []string{"web"}, Mode: RestoreNewApp})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lost != 0 || got.Apps[1].As != "web-restored" || got.Apps[0].As != "db-restored" {
		t.Fatalf("plan: %+v", got.Apps)
	}
	res := s.wait()
	if res.Status != StatusOK {
		t.Fatalf("restore: %+v", res)
	}
	nw := filepath.Join(s.base, "apps", "web-restored", "data")
	if read(t, filepath.Join(nw, "a.txt")) != "v1" || read(t, filepath.Join(nw, "sub", "b.txt")) != "b1" {
		t.Fatal("the new app has not got the data")
	}
	if read(t, filepath.Join(s.web, "a.txt")) != "v2" {
		t.Fatal("the live app was touched")
	}
	if !s.ctl.has("release web-restored true") || !s.ctl.has("release db-restored true") || s.ctl.has("hold web") {
		t.Fatalf("events %v", s.ctl.events)
	}
	if len(res.Apps) != 2 || len(res.Apps[1].Dropped) != 1 {
		t.Fatalf("apps %+v", res.Apps)
	}
	apps := s.apps.BackupApps()
	for _, a := range apps {
		if a.Name == "web-restored" && !slices.Equal(a.Linked, []string{"db-restored"}) {
			t.Fatalf("links not renamed: %+v", a)
		}
	}
}

func TestRestoreAsNewAppsRollsBack(t *testing.T) {
	s := newRestoreSetup(t)
	s.harness.run.mu.Lock()
	s.harness.run.loadErr = errors.New("boom")
	s.harness.run.mu.Unlock()
	if _, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreNewApp}); err != nil {
		t.Fatal(err)
	}
	res := s.wait()
	if res.Status != StatusFailed || !res.RolledBack {
		t.Fatalf("restore: %+v", res)
	}
	s.ctl.mu.Lock()
	deleted := slices.Clone(s.ctl.deleted)
	s.ctl.mu.Unlock()
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"db-restored", "web-restored"}) {
		t.Fatalf("deleted %v", deleted)
	}
	if _, err := os.Stat(filepath.Join(s.base, "apps", "web-restored")); !os.IsNotExist(err) {
		t.Fatal("the new app's directory stayed")
	}
}

func TestRestoreRecoversAnInterruptedSwap(t *testing.T) {
	h := newHarness(t)
	ctl := newControl(h)
	h.app("web")
	vol := filepath.Join(h.base, "apps", "web", "data")
	tag := ".odac-restore-x"
	// The crash came half way through moving the restored entries in:
	// one is in the volume, one still in staging, the originals kept.
	os.MkdirAll(filepath.Join(vol, tag+"-keep"), 0o700)
	os.WriteFile(filepath.Join(vol, tag+"-keep", "a.txt"), []byte("live"), 0o644)
	os.MkdirAll(filepath.Join(vol, tag), 0o700)
	os.WriteFile(filepath.Join(vol, tag, "b.txt"), []byte("restored"), 0o644)
	os.WriteFile(filepath.Join(vol, "a.txt"), []byte("restored"), 0o644)
	j := &restoreJournal{ID: "rx", Mode: RestoreInPlace, Target: "t1", Run: "r", Phase: phaseSwap,
		Apps:    []journalApp{{Name: "web", WasActive: true, Held: true}},
		Volumes: []*volumeJob{{App: "web", Path: vol, Mode: modeReplace, Phase: volIn, Staging: tag, Keep: tag + "-keep"}},
		Planned: []AppRestore{{App: "web", As: "web", Method: "files"}}, Point: 7}
	if err := writeJSON(journalFile(h.base), j); err != nil {
		t.Fatal(err)
	}
	var got RestoreResult
	h.m.cfg.OnRestore = func(r RestoreResult) { got = r }
	h.m.recoverRestore()
	if read(t, filepath.Join(vol, "a.txt")) != "live" {
		t.Fatal("the original did not come back")
	}
	if names, _ := entries(vol); !slices.Equal(names, []string{"a.txt"}) {
		t.Fatalf("volume holds %v", names)
	}
	if !ctl.has("release web true") || got.Status != StatusFailed || !got.RolledBack {
		t.Fatalf("recovery: %v, %+v", ctl.events, got)
	}
	if len(got.Apps) != 1 || got.Apps[0].App != "web" || got.Point != 7 || strings.Contains(got.Error, "rolled back") {
		t.Fatalf("the record of the recovered restore: %+v", got)
	}
	if _, err := os.Stat(journalFile(h.base)); !os.IsNotExist(err) {
		t.Fatal("the journal stayed")
	}
}

func TestRestoreRecoversAnInterruptedEmptying(t *testing.T) {
	h := newHarness(t)
	ctl := newControl(h)
	h.app("db")
	vol := filepath.Join(h.base, "apps", "db", "pg")
	keep := ".odac-restore-x-keep-pg"
	// The crash came half way through moving the originals beside the
	// volume, and the image had started writing into it.
	os.MkdirAll(filepath.Join(h.base, "apps", "db", keep), 0o700)
	os.WriteFile(filepath.Join(h.base, "apps", "db", keep, "PG_VERSION"), []byte("16-live"), 0o644)
	os.MkdirAll(vol, 0o755)
	os.WriteFile(filepath.Join(vol, "base"), []byte("live"), 0o644)
	j := &restoreJournal{ID: "rx", Mode: RestoreInPlace, Target: "t1", Run: "r", Phase: phaseSwap,
		Apps:    []journalApp{{Name: "db", WasActive: true, Held: true}},
		Volumes: []*volumeJob{{App: "db", Path: vol, Root: filepath.Join(h.base, "apps"), Mode: modeEmpty, Phase: volOut, Keep: keep, Beside: true}}}
	if err := writeJSON(journalFile(h.base), j); err != nil {
		t.Fatal(err)
	}
	h.m.recoverRestore()
	if read(t, filepath.Join(vol, "PG_VERSION")) != "16-live" || read(t, filepath.Join(vol, "base")) != "live" {
		t.Fatal("the originals did not come back")
	}
	noLeftovers(t, vol, filepath.Dir(vol))
	if !ctl.has("release db true") {
		t.Fatalf("recovery: %v", ctl.events)
	}
}

func TestKeepBesideOnlyWhereARenameStaysSafe(t *testing.T) {
	root := t.TempDir()
	vol := func(p string) *volumeJob {
		return &volumeJob{Path: filepath.Join(root, p), Root: root, Keep: ".odac-restore-x-keep"}
	}
	for _, p := range []string{"db/pg", "db/data", "db/data/sub", "top"} {
		os.MkdirAll(filepath.Join(root, p), 0o755)
	}
	pg := vol("db/pg")
	pg.keepBeside([]*volumeJob{pg})
	if !pg.Beside || pg.keep() != filepath.Join(root, "db", ".odac-restore-x-keep-pg") {
		t.Fatalf("an app's volume keeps its data at %s", pg.keep())
	}
	// A volume right below the root would put the keep directory among
	// the apps; one without a trusted root has no parent to trust.
	top, loose := vol("top"), vol("db/data")
	loose.Root = ""
	for _, v := range []*volumeJob{top, loose} {
		v.keepBeside([]*volumeJob{v})
		if v.Beside || v.keep() != filepath.Join(v.Path, ".odac-restore-x-keep") {
			t.Fatalf("%s keeps its data at %s", v.Path, v.keep())
		}
	}
	// Beside a volume nested in another one, the outer volume's swap
	// would move the keep directory with its own entries.
	outer, inner := vol("db/data"), vol("db/data/sub")
	inner.keepBeside([]*volumeJob{outer, inner})
	if inner.Beside {
		t.Fatal("a nested volume kept its data in the outer one")
	}
}

func TestRestoreWaitsForTheBackupSlot(t *testing.T) {
	s := newRestoreSetup(t)
	s.m.mu.Lock()
	s.m.backupSlot = true
	s.m.mu.Unlock()
	if _, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace, SkipSafety: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if l := s.m.Restores(); len(l) == 0 || l[0].Status != StatusWaiting {
		t.Fatalf("restores %+v", l)
	}
	if _, err := s.m.StartRestore(context.Background(), RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace}); err == nil {
		t.Fatal("a second restore started")
	}
	s.m.free("other", KindBackup)
	if res := s.wait(); res.Status != StatusOK || res.Safety != "" {
		t.Fatalf("restore: %+v", res)
	}
}

func TestRestoreRefusals(t *testing.T) {
	s := newRestoreSetup(t)
	ctx := context.Background()
	for name, req := range map[string]RestoreRequest{
		"no mode":     {Target: "t1", Run: s.runID},
		"unknown run": {Target: "t1", Run: "nope", Mode: RestoreInPlace},
		"unknown app": {Target: "t1", Run: s.runID, Apps: []string{"mail"}, Mode: RestoreInPlace},
		"taken name":  {Target: "t1", Run: s.runID, Apps: []string{"db"}, Mode: RestoreNewApp, Names: map[string]string{"db": "web"}},
	} {
		if _, err := s.m.PreviewRestore(ctx, req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s.m.DeployStarted("db")
	if _, err := s.m.StartRestore(ctx, RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace}); err == nil || !strings.Contains(err.Error(), "deployed") {
		t.Fatalf("restore during a deploy: %v", err)
	}
	s.m.DeployEnded("db")
	points, err := s.m.RestorePoints(ctx, "t1", "", "", "")
	if err != nil || len(points) != 1 || points[0].Run != s.runID || len(points[0].Apps) != 2 || !points[0].Apps[0].Dump {
		t.Fatalf("points %+v, %v", points, err)
	}
	servers, err := s.m.Servers(ctx, "t1")
	if err != nil || len(servers) != 1 {
		t.Fatalf("servers %+v, %v", servers, err)
	}
}

func TestServerRestoreStagesAndApplies(t *testing.T) {
	h := newHarness(t)
	newControl(h)
	config := filepath.Join(h.base, "config")
	write := func(name, body string) { os.WriteFile(filepath.Join(config, name), []byte(body), 0o600) }
	write("app.json", `{"apps":[{"name":"web","active":true,"type":"git","buildId":"build_1_a","runtimeOverride":"22","imageChange":{"image":"odac-app-web:build_0_z","previous":"odac-app-web"}}]}`)
	write("domain.json", `{"domains":{"a.com":{}}}`)
	write("server.json", `{"server":{"pid":1}}`)
	j := h.job("j1", "t1", 60)
	j.Server = true
	h.set.mu.Lock()
	h.set.jobs = []settings.JobView{j}
	h.set.mu.Unlock()
	rec := h.runNow(j)
	if rec.Server == nil || rec.Server.Status != StatusOK {
		t.Fatalf("backup %+v", rec)
	}
	h.set.mu.Lock()
	h.set.jobs = nil
	h.set.mu.Unlock()
	write("domain.json", `{"domains":{}}`)
	write("server.json", `{"server":{"pid":2}}`)
	write("backup.json", `{"backup":{"targets":{}}}`)

	sr, err := h.m.StageServerRestore(context.Background(), ServerRestoreRequest{Target: "t1", Run: rec.Run})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sr.Dirs, []string{"config"}) {
		t.Fatalf("staged %+v", sr)
	}
	if p, _ := PendingServerRestore(h.base); p == nil || p.ID != sr.ID {
		t.Fatal("nothing pending")
	}
	if read(t, filepath.Join(config, "domain.json")) != `{"domains":{}}` {
		t.Fatal("staging touched the live config")
	}
	applied, err := ApplyServerRestore(h.base)
	if err != nil || applied == nil {
		t.Fatalf("apply: %+v, %v", applied, err)
	}
	if read(t, filepath.Join(config, "domain.json")) != `{"domains":{"a.com":{}}}` {
		t.Fatal("the restored config is not in place")
	}
	if read(t, filepath.Join(config, "server.json")) != `{"server":{"pid":2}}` || read(t, filepath.Join(config, "backup.json")) != `{"backup":{"targets":{}}}` {
		t.Fatal("this server's own modules were replaced")
	}
	var mod struct {
		Apps []map[string]any `json:"apps"`
	}
	json.Unmarshal([]byte(read(t, filepath.Join(config, "app.json"))), &mod)
	if len(mod.Apps) != 1 || mod.Apps[0]["active"] != false {
		t.Fatalf("apps %+v", mod.Apps)
	}
	// What named the old server's images goes; the operator's runtime pin
	// is a choice, not an image, and stays.
	if a := mod.Apps[0]; a["imageChange"] != nil || a["buildId"] != nil || a["runtimeOverride"] != "22" || a["type"] != "git" {
		t.Fatalf("restored app %+v", a)
	}
	if again, err := ApplyServerRestore(h.base); err != nil || again != nil {
		t.Fatalf("second apply: %+v, %v", again, err)
	}

	h.app("web")
	if _, err := h.m.StageServerRestore(context.Background(), ServerRestoreRequest{Target: "t1", Run: rec.Run}); err == nil {
		t.Fatal("a server with apps took a server restore")
	}
}

func TestCheckPathRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.Symlink(other, filepath.Join(root, "link"))
	if err := checkPath(root, filepath.Join(root, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := checkPath(root, filepath.Join(root, "a", "missing")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "link"), filepath.Join(root, "link", "x")} {
		if err := checkPath(root, p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := ensureDir(root, filepath.Join(root, "link", "y"), 0o755); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ensureDir through a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "y")); !os.IsNotExist(err) {
		t.Fatal("a directory was made through the symlink")
	}
}

func TestCrashCheckCountsRestartsSinceTheStart(t *testing.T) {
	h := newHarness(t)
	h.states.restarts = 5
	check := h.m.crashCheck("db", 5)
	if err := check(context.Background()); err != nil {
		t.Fatalf("restarts before the start counted: %v", err)
	}
	h.states.mu.Lock()
	h.states.restarts = 5 + crashRestarts
	h.states.mu.Unlock()
	if err := check(context.Background()); err == nil || !strings.Contains(err.Error(), "keeps restarting") {
		t.Fatalf("a crash loop passed: %v", err)
	}
	h.states.mu.Lock()
	h.states.err = errors.New("daemon stalled")
	h.states.mu.Unlock()
	if err := check(context.Background()); err != nil {
		t.Fatalf("a failed inspect read as a crash: %v", err)
	}
}

func TestCompareImagesBlocksAnotherRepositoryOrMajor(t *testing.T) {
	cases := []struct {
		point, current string
		reported       bool
		blocked        bool
	}{
		{"postgres:17.6-alpine", "postgres:17.2", true, false},
		{"postgres:17.6-alpine", "postgres:18", true, true},
		{"postgres:16", "docker.io/library/postgres:16.9", true, false},
		{"postgres:16", "mysql:8", true, true},
		{"postgres:16", "ghcr.io/library/postgres:16", true, true},
		{"traefik:v3.1", "traefik:v2.11", true, true},
		{"traefik:v3.1", "traefik:3.2", true, false},
		{"postgres:latest", "postgres:18", true, false},
		{"postgres", "postgres:16", true, false},
		{"redis:alpine", "redis:alpine3.20", true, false},
		{"", "postgres:16", false, false},
		{"Not A Ref::", "postgres:16", false, false},
		{"postgres:1234567890", "postgres:1", true, false},
	}
	for _, c := range cases {
		img, blocked := compareImages(c.point, c.current)
		if (img != nil) != c.reported || blocked != c.blocked {
			t.Errorf("%s -> %s: image %+v blocked %v, want reported %v blocked %v", c.point, c.current, img, blocked, c.reported, c.blocked)
		}
		if img != nil && (img.Point != c.point || img.Current != c.current) {
			t.Errorf("%s -> %s reported as %+v", c.point, c.current, img)
		}
	}
}

// An in-place restore compares the image the point was taken on with the
// one the app runs: another major is refused by the restore itself, a minor
// is not, and a new app is never compared.
func TestRestoreInPlaceRefusesAnotherImageMajor(t *testing.T) {
	s := newRestoreSetup(t)
	ctx := context.Background()
	setImage := func(img string) {
		s.ctl.mu.Lock()
		s.ctl.defs["db"]["image"] = img
		s.ctl.mu.Unlock()
	}
	preview := func() map[string]AppRestore {
		t.Helper()
		r, err := s.m.PreviewRestore(ctx, RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]AppRestore{}
		for _, a := range r.Apps {
			out[a.App] = a
		}
		return out
	}

	apps := preview()
	if db := apps["db"]; db.Image == nil || *db.Image != (RestoreImage{Point: "postgres:16.4", Current: "postgres:16.4"}) || db.Blocked != "" {
		t.Fatalf("same image: %+v", db)
	}
	if web := apps["web"]; web.Image != nil || web.Blocked != "" {
		t.Fatalf("an app whose records name no image: %+v", web)
	}

	setImage("postgres:16.6")
	if db := preview()["db"]; db.Blocked != "" || db.Image.Current != "postgres:16.6" {
		t.Fatalf("a minor difference: %+v", db)
	}

	setImage("postgres:17.1")
	db := preview()["db"]
	if db.Blocked != BlockedImageMismatch || *db.Image != (RestoreImage{Point: "postgres:16.4", Current: "postgres:17.1"}) {
		t.Fatalf("a major difference: %+v", db)
	}
	raw, _ := json.Marshal(db)
	if !strings.Contains(string(raw), `"image":{"point":"postgres:16.4","current":"postgres:17.1"},"blocked":"image_mismatch"`) {
		t.Fatalf("wire shape %s", raw)
	}
	// The restore checks again and never trusts the preview.
	_, err := s.m.StartRestore(ctx, RestoreRequest{Target: "t1", Run: s.runID, Apps: []string{"web"}, Mode: RestoreInPlace})
	var se *settings.Error
	if !errors.As(err, &se) || se.Code != settings.CodeImageMismatch || !strings.Contains(se.Message, "postgres:17.1") {
		t.Fatalf("restore across a major: %v", err)
	}
	if s.ctl.has("hold web") || s.ctl.has("hold db") || read(t, filepath.Join(s.pg, "PG_VERSION")) != "16-live" {
		t.Fatal("a refused restore touched an app")
	}

	setImage("mysql:8")
	if preview()["db"].Blocked != BlockedImageMismatch {
		t.Fatal("another repository was not blocked")
	}

	// A new app is made from the point's own definition: nothing to compare.
	r, err := s.m.PreviewRestore(ctx, RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreNewApp})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Apps {
		if a.Image != nil || a.Blocked != "" {
			t.Fatalf("new app compared: %+v", a)
		}
	}

	setImage("postgres:16.9")
	if _, err := s.m.StartRestore(ctx, RestoreRequest{Target: "t1", Run: s.runID, Mode: RestoreInPlace}); err != nil {
		t.Fatalf("restore within the major: %v", err)
	}
	if res := s.wait(); res.Status != StatusOK {
		t.Fatalf("restore: %+v", res)
	}
}
