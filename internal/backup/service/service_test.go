package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/api"
	"odac/internal/backup"
	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
	"odac/internal/config"
	"odac/internal/jscanon"
	"odac/internal/lang"
	"odac/internal/logx"
)

func init() { lang.SetLocale("en-US") }

const serverID = "0123456789abcdef0123456789abcdef"

type fakeInit struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeInit) Init(context.Context, settings.Access) (*proto.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &proto.Result{Created: true}, nil
}

type fakeApps []settings.App

func (f fakeApps) BackupApps() []settings.App { return f }

type local struct{}

func (local) IsLocal(context.Context, string) bool { return false }

// fakeManager answers with nothing and records what it was asked.
type fakeManager struct {
	mu      sync.Mutex
	req     backup.RestoreRequest
	server  backup.ServerRestoreRequest
	woken   int
	servers []proto.ServerDir
	snapReq backup.SnapshotRestoreRequest
	taken   string
	health  backup.Health
}

func (f *fakeManager) Health() backup.Health { return f.health }

func (f *fakeManager) SnapshotStatus() backup.SnapshotStatus {
	return backup.SnapshotStatus{Periodic: []backup.PeriodicSnapshot{}, Deploys: []backup.DeployMark{}, Warnings: []backup.SnapshotWarning{}}
}
func (f *fakeManager) SnapshotHistory(string, int) []backup.SnapshotRecord { return nil }
func (f *fakeManager) SnapshotPoints(string) []backup.SnapshotPoint        { return nil }
func (f *fakeManager) TakeSnapshot(app string) (backup.SnapshotRunning, error) {
	f.mu.Lock()
	f.taken = app
	f.mu.Unlock()
	return backup.SnapshotRunning{Run: "s1", App: app, Event: backup.EventManual}, nil
}
func (f *fakeManager) DeleteSnapshot(context.Context, string) error { return nil }
func (f *fakeManager) PreviewSnapshotRestore(_ context.Context, req backup.SnapshotRestoreRequest) (*backup.RestoreResult, error) {
	f.mu.Lock()
	f.snapReq = req
	f.mu.Unlock()
	return &backup.RestoreResult{Mode: backup.RestoreInPlace, Target: backup.SnapshotTarget, Run: req.Run, Status: backup.StatusPlanned, Apps: []backup.AppRestore{}}, nil
}
func (f *fakeManager) StartSnapshotRestore(ctx context.Context, req backup.SnapshotRestoreRequest) (*backup.RestoreResult, error) {
	r, err := f.PreviewSnapshotRestore(ctx, req)
	r.ID = "r2"
	return r, err
}

func (f *fakeManager) Status() ([]backup.JobStatus, []backup.TargetStatus) { return nil, nil }
func (f *fakeManager) History(string, int) []backup.RunRecord              { return nil }
func (f *fakeManager) RunNow(job string) (backup.JobStatus, error) {
	return backup.JobStatus{Job: job, Manual: 1}, nil
}
func (f *fakeManager) Wake() {
	f.mu.Lock()
	f.woken++
	f.mu.Unlock()
}
func (f *fakeManager) Servers(context.Context, string) ([]proto.ServerDir, error) {
	return f.servers, nil
}
func (f *fakeManager) RestorePoints(context.Context, string, string, string, string) ([]backup.RestorePoint, error) {
	return nil, nil
}
func (f *fakeManager) PreviewRestore(_ context.Context, req backup.RestoreRequest) (*backup.RestoreResult, error) {
	f.mu.Lock()
	f.req = req
	f.mu.Unlock()
	return &backup.RestoreResult{Mode: req.Mode, Target: req.Target, Run: req.Run, Status: backup.StatusPlanned, Apps: []backup.AppRestore{}}, nil
}
func (f *fakeManager) StartRestore(ctx context.Context, req backup.RestoreRequest) (*backup.RestoreResult, error) {
	r, err := f.PreviewRestore(ctx, req)
	r.ID = "r1"
	return r, err
}
func (f *fakeManager) CancelRestore(id string) bool     { return id == "r1" }
func (f *fakeManager) Restores() []backup.RestoreResult { return nil }
func (f *fakeManager) StageServerRestore(_ context.Context, req backup.ServerRestoreRequest) (*backup.ServerRestore, error) {
	f.mu.Lock()
	f.server = req
	f.mu.Unlock()
	return &backup.ServerRestore{ID: "s1", Target: req.Target, Run: req.Run, Dirs: []string{"config"}}, nil
}

type fixture struct {
	svc  *Service
	set  *settings.Settings
	mgr  *fakeManager
	init *fakeInit
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	store, err := config.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{mgr: &fakeManager{}, init: &fakeInit{}}
	fx.set, err = settings.New(settings.Config{
		Store: store, ServerID: serverID, Init: fx.init,
		Apps:   fakeApps{{Name: "web", Stateful: true, Linked: []string{"db"}}, {Name: "db", Stateful: true}},
		Locate: local{}, Log: logx.New("Test"),
		Now: func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.svc, err = New(Config{Manager: fx.mgr, Settings: fx.set, ServerID: serverID, BaseDir: base})
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

// payload turns JSON text into what the API and the Hub decode a request to.
func payload(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func data(t *testing.T, r api.Result) string {
	t.Helper()
	if !r.Status {
		t.Fatalf("refused: %v %v", r.Message, r.Data)
	}
	raw, ok := r.Data.(json.RawMessage)
	if !ok {
		t.Fatalf("data is %T, not encoded once", r.Data)
	}
	// The Hub signs this, so it must canonicalize.
	if _, err := jscanon.Marshal(raw); err != nil {
		t.Fatalf("data does not canonicalize: %v", err)
	}
	return string(raw)
}

func code(r api.Result) string {
	if m, _ := r.Data.(map[string]any); m != nil {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

const s3Spec = `{"name": "Offsite", "s3": {"endpoint": "s3.example.com", "bucket": "odac-backups", "accessKeyId": "AKIA", "secretAccessKey": "SECRET-S3"}}`

func (fx *fixture) addTarget(t *testing.T) (id, key string) {
	t.Helper()
	var p settings.Pending
	if err := json.Unmarshal([]byte(data(t, fx.svc.PrepareTarget(payload(t, s3Spec)))), &p); err != nil {
		t.Fatal(err)
	}
	var v settings.TargetView
	if err := json.Unmarshal([]byte(data(t, fx.svc.ConfirmTarget(map[string]any{"token": p.Token}))), &v); err != nil {
		t.Fatal(err)
	}
	return v.ID, p.Recovery.Key
}

func TestNoListCarriesASecret(t *testing.T) {
	fx := newFixture(t)
	id, key := fx.addTarget(t)
	if key == "" {
		t.Fatal("prepare showed no key")
	}
	if _, err := fx.set.CreateJob(settings.JobSpec{Name: "nightly", Target: id, AllApps: true}); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]api.Result{
		"targets": fx.svc.Targets(), "jobs": fx.svc.Jobs(), "coverage": fx.svc.Coverage(),
		"retired": fx.svc.Retired(), "status": fx.svc.Status(),
	} {
		if text := data(t, r); strings.Contains(text, key) || strings.Contains(text, "SECRET-S3") {
			t.Errorf("%s carries a secret: %s", name, text)
		}
	}
	// The key is shown again only to the operator.
	if r := fx.svc.Key(api.Caller{}, map[string]any{"id": id}); r.Status || code(r) != CodeDenied {
		t.Fatalf("a caller that is not the operator saw the key: %+v", r)
	}
	if text := data(t, fx.svc.Key(api.Operator, map[string]any{"id": id})); !strings.Contains(text, key) || strings.Contains(text, "SECRET-S3") {
		t.Fatalf("key reply: %s", text)
	}
}

func TestUpdateTargetTakesAPatch(t *testing.T) {
	fx := newFixture(t)
	id, _ := fx.addTarget(t)
	before := fx.init.calls
	if text := data(t, fx.svc.UpdateTarget(map[string]any{"id": id, "name": "Remote"})); !strings.Contains(text, `"name":"Remote"`) || !strings.Contains(text, `"bucket":"odac-backups"`) {
		t.Fatalf("rename: %s", text)
	}
	if fx.init.calls != before {
		t.Fatal("a rename opened the repository")
	}
	data(t, fx.svc.UpdateTarget(payload(t, `{"id": "`+id+`", "s3": {"secretAccessKey": "NEW-SECRET"}}`)))
	if fx.init.calls != before+1 {
		t.Fatal("new credentials were saved without opening the repository with them")
	}
	if r := fx.svc.UpdateTarget(map[string]any{"id": id, "disk": map[string]any{"path": "/mnt/b"}}); r.Status || code(r) != settings.CodeInvalid {
		t.Fatalf("moving a target: %+v", r)
	}
	if r := fx.svc.UpdateTarget(map[string]any{"id": "0000000000000000"}); code(r) != settings.CodeNotFound {
		t.Fatalf("unknown target: %+v", r)
	}
}

func TestJobsTakePresetsAndPatches(t *testing.T) {
	fx := newFixture(t)
	id, _ := fx.addTarget(t)
	var j settings.JobView
	if err := json.Unmarshal([]byte(data(t, fx.svc.CreateJob(map[string]any{"name": "db", "target": id, "apps": []any{"web", "db"}, "density": "high"}))), &j); err != nil {
		t.Fatal(err)
	}
	if j.Density != thin.High || fx.mgr.woken == 0 {
		t.Fatalf("created %+v, woken %d", j, fx.mgr.woken)
	}
	if r := fx.svc.CreateJob(map[string]any{"name": "x", "target": id, "allApps": true, "density": "lots"}); code(r) != settings.CodeInvalid {
		t.Fatalf("unknown preset: %+v", r)
	}
	text := data(t, fx.svc.UpdateJob(map[string]any{"id": j.ID, "every": 60}))
	if !strings.Contains(text, `"apps":["web","db"]`) || !strings.Contains(text, `"every":60`) || !strings.Contains(text, `"density":7`) {
		t.Fatalf("a patch lost what it left out: %s", text)
	}
	text = data(t, fx.svc.UpdateJob(map[string]any{"id": j.ID, "allApps": true}))
	if strings.Contains(text, `"apps"`) || !strings.Contains(text, `"allApps":true`) {
		t.Fatalf("allApps did not replace the list: %s", text)
	}
	if r := fx.svc.DeleteJob(map[string]any{"id": j.ID}); !r.Status {
		t.Fatalf("delete: %+v", r)
	}
	if text := data(t, fx.svc.Retired()); !strings.Contains(text, j.ID) || strings.Contains(text, `"purge"`) {
		t.Fatalf("a deleted job's backups were not kept: %s", text)
	}
}

func TestRestoresDefaultToANewApp(t *testing.T) {
	fx := newFixture(t)
	text := data(t, fx.svc.PreviewRestore(payload(t, `{"target": "t1", "run": "r", "server": "`+strings.Repeat("a", 32)+`", "key": "other-key"}`)))
	if fx.mgr.req.Mode != backup.RestoreNewApp || fx.mgr.req.Key != "other-key" {
		t.Fatalf("request %+v", fx.mgr.req)
	}
	if strings.Contains(text, "other-key") {
		t.Fatalf("the key came back: %s", text)
	}
	data(t, fx.svc.StartRestore(map[string]any{"target": "t1", "run": "r", "mode": backup.RestoreInPlace}))
	if fx.mgr.req.Mode != backup.RestoreInPlace {
		t.Fatalf("an asked-for mode was replaced: %+v", fx.mgr.req)
	}
	if r := fx.svc.CancelRestore(map[string]any{"id": "nope"}); code(r) != settings.CodeNotFound {
		t.Fatalf("cancel unknown: %+v", r)
	}
	if r := fx.svc.PreviewRestore("not an object"); code(r) != settings.CodeInvalid {
		t.Fatalf("a request that is no object: %+v", r)
	}
}

func TestListsAreNeverNull(t *testing.T) {
	fx := newFixture(t)
	for name, r := range map[string]api.Result{
		"targets": fx.svc.Targets(), "jobs": fx.svc.Jobs(), "retired": fx.svc.Retired(),
		"history": fx.svc.History(nil), "restores": fx.svc.Restores(),
		"points":  fx.svc.Points(map[string]any{"target": "t"}),
		"servers": fx.svc.TargetServers(map[string]any{"id": "t"}),
	} {
		if text := data(t, r); text != "[]" {
			t.Errorf("%s = %s", name, text)
		}
	}
	if text := data(t, fx.svc.Status()); text != `{"jobs":[],"targets":[]}` {
		t.Errorf("status = %s", text)
	}
	// Coverage says only what there is to say.
	if text := data(t, fx.svc.Coverage()); text != `{"unprotected":["db","web"],"noJobs":true}` {
		t.Errorf("coverage = %s", text)
	}
}

func TestCoverageCarriesWhatTheTargetsReport(t *testing.T) {
	fx := newFixture(t)
	id, _ := fx.addTarget(t)
	if _, err := fx.set.CreateJob(settings.JobSpec{Name: "nightly", Target: id, AllApps: true}); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("c", 32)
	fx.mgr.health = backup.Health{
		Corrupt: []string{id},
		Orphans: []backup.OrphanServer{{Target: id, ServerShare: backup.ServerShare{Server: other, Name: "gone", Used: 7}}},
		Shared:  []string{id},
	}
	want := `{"corrupt":["` + id + `"],"orphans":[{"target":"` + id + `","server":"` + other + `","name":"gone","used":7}],"unbudgeted":["` + id + `"]}`
	if text := data(t, fx.svc.Coverage()); !strings.HasSuffix(text, want[1:]) {
		t.Fatalf("coverage = %s, want it to end with %s", text, want[1:])
	}
	// A budget on every job on the shared target answers the suggestion.
	jobs, _ := fx.set.Jobs()
	if _, err := fx.set.UpdateJob(jobs[0].ID, settings.JobSpec{Name: "nightly", Target: id, AllApps: true, Budget: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if text := data(t, fx.svc.Coverage()); strings.Contains(text, "unbudgeted") {
		t.Fatalf("coverage = %s", text)
	}
}

func TestServerDirectoriesMarkThisServer(t *testing.T) {
	fx := newFixture(t)
	other := strings.Repeat("b", 32)
	fx.mgr.servers = []proto.ServerDir{{ID: other, Name: "old", Updated: time.UnixMilli(5)}, {ID: serverID, Name: "new"}}
	text := data(t, fx.svc.TargetServers(map[string]any{"id": "t"}))
	if want := `[{"id":"` + serverID + `","name":"new","self":true},{"id":"` + other + `","name":"old","updated":5}]`; text != want {
		t.Fatalf("servers = %s\nwant %s", text, want)
	}
}

func TestServerStateIsTheOperatorsAlone(t *testing.T) {
	fx := newFixture(t)
	req := map[string]any{"target": "t1", "server": "s", "run": "r9", "key": "k"}
	for _, r := range []api.Result{fx.svc.StageServerRestore(api.Caller{App: "x"}, req), fx.svc.PendingServerRestore(api.Caller{}), fx.svc.CancelServerRestore(api.Caller{})} {
		if code(r) != CodeDenied {
			t.Fatalf("not the operator: %+v", r)
		}
	}
	data(t, fx.svc.StageServerRestore(api.Operator, req))
	if fx.mgr.server != (backup.ServerRestoreRequest{Target: "t1", Server: "s", Key: "k", Run: "r9"}) {
		t.Fatalf("stage request %+v", fx.mgr.server)
	}
	if r := fx.svc.PendingServerRestore(api.Operator); !r.Status || r.Data != nil {
		t.Fatalf("nothing staged on disk: %+v", r)
	}
}

func TestSnapshotActions(t *testing.T) {
	fx := newFixture(t)
	r := fx.svc.SnapshotSettings()
	if !r.Status || string(r.Data.(json.RawMessage)) != `{"enabled":true,"maxDiskPct":25}` {
		t.Fatalf("settings %+v", r)
	}
	if r := fx.svc.UpdateSnapshotSettings(payload(t, `{"maxDiskPct":60}`)); r.Status {
		t.Fatal("a share above the maximum was accepted")
	}
	r = fx.svc.UpdateSnapshotSettings(payload(t, `{"enabled":false}`))
	if !r.Status || string(r.Data.(json.RawMessage)) != `{"enabled":false,"maxDiskPct":25}` || fx.mgr.woken != 1 {
		t.Fatalf("update %+v", r)
	}
	for name, r := range map[string]api.Result{
		"status":  fx.svc.SnapshotStatus(),
		"history": fx.svc.SnapshotHistory(nil),
		"list":    fx.svc.SnapshotPoints(payload(t, `{"app":"web"}`)),
	} {
		raw := string(r.Data.(json.RawMessage))
		if !r.Status || strings.Contains(raw, "null") {
			t.Errorf("%s: %s", name, raw)
		}
	}
	if r := fx.svc.TakeSnapshot(payload(t, `{"app":"web"}`)); !r.Status || fx.mgr.taken != "web" {
		t.Fatalf("take %+v", r)
	}
	// The mode is left to the manager, which restores a snapshot in place
	// unless an app of it no longer exists.
	r = fx.svc.StartSnapshotRestore(payload(t, `{"run":"x","apps":["web"],"skipSafety":true}`))
	if !r.Status || fx.mgr.snapReq.Mode != "" || !fx.mgr.snapReq.SkipSafety || fx.mgr.snapReq.Run != "x" {
		t.Fatalf("restore %+v %+v", r, fx.mgr.snapReq)
	}
}
