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
func (h *fakeHold) Start() error    { h.c.event("start %s", h.name); return nil }
func (h *fakeHold) Stop()           { h.c.event("stop %s", h.name) }
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
	ctl.defs["db"] = map[string]any{"name": "db", "type": "container"}

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
	if names, _ := entries(s.pg); len(names) != 0 {
		t.Fatalf("database volume holds %v", names)
	}
	s.harness.run.mu.Lock()
	loads := slices.Clone(s.harness.run.loads)
	s.harness.run.mu.Unlock()
	if len(loads) != 1 || string(loads[0].data) != "dump of db" {
		t.Fatalf("loads %+v", loads)
	}
	noLeftovers(t, s.web, s.pg)
	// The state before the restore is a run of its own.
	var safety []proto.Snapshot
	for _, sn := range s.harness.run.repo("t1").snaps {
		if sn.Run == res.Safety {
			safety = append(safety, sn)
		}
	}
	if len(safety) < 3 || safety[0].Source.Job != safetyJob {
		t.Fatalf("safety backup %+v", safety)
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
		Volumes: []*volumeJob{{App: "web", Path: vol, Mode: modeReplace, Phase: volIn, Staging: tag, Keep: tag + "-keep"}}}
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
	if _, err := os.Stat(journalFile(h.base)); !os.IsNotExist(err) {
		t.Fatal("the journal stayed")
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
	write("app.json", `{"apps":[{"name":"web","active":true}]}`)
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
