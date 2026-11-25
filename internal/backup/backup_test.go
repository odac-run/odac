package backup

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
	"odac/internal/docker"
	"odac/internal/sysinfo"
)

func TestRunStoresEveryPiece(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.app("web", "db")
	j := h.job("j1", "t1", 60, "db", "web")
	h.set.jobs[0].Server = true
	j.Server = true

	rec := h.runNow(j)
	if rec.Status != StatusOK || rec.Snapshots != 4 || rec.Server == nil || rec.Server.Status != StatusOK {
		t.Fatalf("record %+v", rec)
	}
	backups := h.run.ops(proto.OpBackup)
	if len(backups) != 2 {
		t.Fatalf("%d helper runs, want one for the dump and one for the files", len(backups))
	}
	first, second := backups[0], backups[1]
	if !first.dump || first.req.Backup.Items[0].Stream != "dump.sql" || first.labels[labelApps] != "db" || first.labels[labelComplete] != "" {
		t.Errorf("dump piece %+v", first)
	}
	if second.dump || second.labels[labelComplete] != "true" || len(second.req.Backup.Items) != 2 {
		t.Errorf("files piece %+v", second)
	}
	if first.req.Backup.Run != second.req.Backup.Run || first.req.Repo.Owner == nil || second.req.Repo.Owner == nil || first.req.Repo.Owner.Seq == second.req.Repo.Owner.Seq {
		t.Error("the pieces of a run share the run and claim a sequence number each")
	}
	if !strings.HasPrefix(first.req.Repo.CacheDir, filepath.Join(h.base, "backup")) {
		t.Errorf("cache %s is not on ODAC's data disk", first.req.Repo.CacheDir)
	}
	var dbRes AppResult
	for _, a := range rec.Apps {
		if a.App == "db" {
			dbRes = a
		}
	}
	if dbRes.Method != MethodDump || dbRes.Consistency != consistency.Live {
		t.Errorf("db result %+v", dbRes)
	}
	var js *jobState
	h.m.state.view(func(d *stateData) { js = d.Jobs["j1"]; _ = d.Dumps["db"] })
	if js == nil || js.Failures != 0 {
		t.Errorf("job state %+v", js)
	}
	if got := h.m.History("j1", 0); len(got) != 1 || got[0].Run != rec.Run {
		t.Errorf("history %+v", got)
	}
}

func TestRunReadsWhatPrepareMakes(t *testing.T) {
	h := newHarness(t)
	h.app("frozen")
	rec := h.runNow(h.job("j1", "t1", 60, "frozen"))
	if rec.Status != StatusOK || len(rec.Apps) != 1 || len(rec.Apps[0].Warnings) != 0 {
		t.Fatalf("record %+v", rec)
	}
	backups := h.run.ops(proto.OpBackup)
	if len(backups) != 1 {
		t.Fatalf("%d helper runs", len(backups))
	}
	var names []string
	for _, it := range backups[0].req.Backup.Items {
		names = append(names, it.Source.Name)
	}
	// The parts exist only once Prepare has frozen them; a disk whose
	// tables had none to freeze has nothing to read.
	if !slices.Contains(names, "parts") || slices.Contains(names, "parts-cold") {
		t.Fatalf("items %v", names)
	}
	rp := h.run.repo("t1")
	found := false
	for _, sn := range rp.snaps {
		if sn.Source.Name == "parts" {
			found = string(rp.files[sn.ID]["part.bin"]) == "rows"
		}
	}
	if !found {
		t.Fatal("the frozen parts were not stored")
	}
	if _, err := os.Stat(filepath.Join(h.base, "apps", "frozen", "shadow")); err == nil {
		if names, _ := entries(filepath.Join(h.base, "apps", "frozen", "shadow")); len(names) != 0 {
			t.Fatalf("Finish left %v", names)
		}
	}
}

func TestDockerDownDefersWithoutFailing(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	j := h.job("j1", "t1", 60, "db")
	h.m.state.update(func(d *stateData) { d.job("j1").Next = ms(h.clock.Now().Add(time.Hour)) })
	h.states.err = errDocker
	rec := h.runNow(j)
	if rec.Status != StatusDeferred || len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatalf("record %+v", rec)
	}
	h.m.state.view(func(d *stateData) {
		js := d.Jobs["j1"]
		if js.Failures != 0 || !fromMs(js.Next).Equal(h.clock.Now().Add(h.m.t.dockerRetry)) {
			t.Errorf("job state %+v", js)
		}
	})
	if len(h.run.ops(proto.OpDelete)) != 0 {
		t.Error("a deferred run deleted backups")
	}
}

func TestStoppedAppIsReadAsFiles(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.m.cfg.States = stoppedStates{}
	rec := h.runNow(h.job("j1", "t1", 60, "db"))
	b := h.run.ops(proto.OpBackup)
	if rec.Status != StatusOK || len(b) != 1 || b[0].dump || b[0].req.Backup.Items[0].Consistency != consistency.CrashConsistent {
		t.Fatalf("record %+v calls %+v", rec, b)
	}
}

type stoppedStates struct{}

func (stoppedStates) BackupAppState(context.Context, string) (docker.AppState, error) {
	return docker.AppState{}, nil
}

func TestDeployCancelsTheDumpAndTheRunFollows(t *testing.T) {
	h := newHarness(t)
	h.m.t.deploySettle = 10 * time.Millisecond
	h.app("db")
	h.app("web", "db")
	j := h.job("j1", "t1", 60, "db", "web")
	h.m.state.update(func(d *stateData) { d.job("j1").Next = ms(h.clock.Now().Add(time.Hour)) })
	started := make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan RunRecord, 1)
	go func() { done <- h.runNow(j) }()
	<-started
	// web links db, so its migration would wait behind the dump.
	h.m.DeployStarted("web")
	rec := <-done
	if rec.Status != StatusPartial {
		t.Fatalf("record %+v", rec)
	}
	var f *followUp
	h.m.state.view(func(d *stateData) {
		if x := d.FollowUps["j1"]; x != nil {
			c := *x
			f = &c
		}
	})
	if f == nil || f.Run != rec.Run || !slices.Equal(f.Apps, []string{"db"}) || !f.Clean {
		t.Fatalf("follow-up %+v", f)
	}

	// While the deploy runs, the schedule leaves the follow-up alone.
	h.run.dump = nil
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	if n := len(h.run.ops(proto.OpBackup)); n != 2 {
		t.Fatalf("%d backups while the deploy ran", n)
	}
	h.m.DeployEnded("web")
	time.Sleep(20 * time.Millisecond)
	h.m.mu.Lock()
	h.m.deployEnded["web"] = h.clock.Now().Add(-time.Minute)
	h.m.mu.Unlock()
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	follow := h.record()
	b := h.run.ops(proto.OpBackup)
	last := b[len(b)-1]
	if follow.Status != StatusOK || follow.Run != rec.Run || follow.Trigger != triggerDeploy || last.req.Backup.Run != rec.Run || last.labels[labelApps] != "db" {
		t.Fatalf("follow-up record %+v, last call %+v", follow, last)
	}
	h.m.state.view(func(d *stateData) {
		if d.FollowUps["j1"] != nil || slices.Contains(d.Targets["t1"].Partial, rec.Run) {
			t.Errorf("the completed run is still pending: %+v %+v", d.FollowUps, d.Targets["t1"])
		}
	})
}

func TestDumpWaitsForADeployInProgress(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.m.DeployStarted("db")
	rec := h.runNow(h.job("j1", "t1", 60, "db"))
	if rec.Status != StatusDeferred || len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatalf("record %+v", rec)
	}
}

func TestADumpIsReusedByTheNextJob(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	a := h.job("a", "t1", 24*60, "db")
	b := h.job("b", "t2", 24*60, "db")
	if rec := h.runNow(a); rec.Status != StatusOK {
		t.Fatalf("first job %+v", rec)
	}
	h.clock.Add(5 * time.Minute)
	rec := h.runNow(b)
	if rec.Status != StatusOK || rec.Apps[0].Method != MethodReused {
		t.Fatalf("second job %+v", rec)
	}
	calls := h.run.ops(proto.OpBackup)
	last := calls[len(calls)-1]
	if last.dump || !last.source || last.target != "t2" || last.req.Backup.Items[0].Consistency != consistency.Consistent {
		t.Fatalf("second job's dump piece %+v", last)
	}
	if r := h.run.ops(proto.OpRestore); len(r) != 1 || r[0].target != "t1" || r[0].req.Repo.Owner != nil {
		t.Fatalf("restore %+v", r)
	}

	// Past the window the database is dumped again.
	h.clock.Add(time.Hour)
	h.runNow(b)
	calls = h.run.ops(proto.OpBackup)
	if last := calls[len(calls)-1]; !last.dump {
		t.Fatal("an old dump was reused")
	}
}

// A dump taken before a deploy of its group holds what the migration
// replaced, and one taken before an in-place restore (whose holds report
// as deploys) what the restore replaced: either read back as a newer
// backup stores data that no longer exists.
func TestADumpIsNotReusedAcrossAChange(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.app("web", "db")
	a := h.job("a", "t1", 24*60, "db")
	b := h.job("b", "t2", 24*60, "db")
	if rec := h.runNow(a); rec.Status != StatusOK {
		t.Fatalf("first job %+v", rec)
	}
	h.clock.Add(time.Minute)
	h.m.DeployStarted("web")
	h.clock.Add(time.Minute)
	h.m.DeployEnded("web")
	h.clock.Add(5 * time.Minute)
	rec := h.runNow(b)
	if rec.Status != StatusOK || rec.Apps[0].Method == MethodReused {
		t.Fatalf("a dump from before a deploy of its group was reused: %+v", rec)
	}
	// What the second job dumped after the deploy is reusable again.
	h.clock.Add(time.Minute)
	if rec := h.runNow(a); rec.Apps[0].Method != MethodReused {
		t.Fatalf("a fresh dump was not reused: %+v", rec)
	}
	// A server knows nothing of what happened before it started.
	h.m.mu.Lock()
	h.m.started = h.clock.Now().Add(time.Second)
	h.m.mu.Unlock()
	h.clock.Add(time.Minute)
	if rec := h.runNow(b); rec.Apps[0].Method == MethodReused {
		t.Fatalf("a dump from before the server started was reused: %+v", rec)
	}
}

func TestThinningAfterARun(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	j := h.job("j1", "t1", 60, "web")
	now := h.clock.Now()
	// A run every hour for a month: far more than the rule keeps.
	for i := 1; i <= 24*30; i++ {
		h.run.seed("t1", "j1", "old"+pad(i), now.Add(-time.Duration(i)*time.Hour), 10)
	}
	// A deleted job's kept backups and one whose deletion was asked for.
	h.run.seed("t1", "kept", "k1", now.Add(-48*time.Hour), 10)
	h.run.seed("t1", "kept", "k2", now.Add(-47*time.Hour), 10)
	h.run.seed("t1", "gone", "g1", now.Add(-48*time.Hour), 10)
	h.set.retired = []settings.RetiredView{
		{ID: "kept", Retired: settings.Retired{Target: "t1"}},
		{ID: "gone", Retired: settings.Retired{Target: "t1", Purge: true}},
	}
	rec := h.runNow(j)
	runs := h.run.runs("t1")
	if rec.Deleted == 0 || len(runs) > 60 || !slices.Contains(runs, rec.Run) {
		t.Fatalf("deleted %d, %d runs left", rec.Deleted, len(runs))
	}
	if !slices.Contains(runs, "k1") || !slices.Contains(runs, "k2") || slices.Contains(runs, "g1") {
		t.Fatalf("retired runs: %v", runs)
	}
	if !slices.Equal(h.set.purged, []string{"gone"}) {
		t.Fatalf("purged %v", h.set.purged)
	}
	// The oldest backup survives: depth is given up last.
	if !slices.Contains(runs, "old"+pad(24*30)) {
		t.Fatal("the oldest backup was thinned")
	}
}

func pad(i int) string {
	s := "0000" + itoa(i)
	return s[len(s)-4:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestPartialRunsGoOnceSuperseded(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	j := h.job("j1", "t1", 60, "web")
	now := h.clock.Now()
	h.run.seed("t1", "j1", "p1", now.Add(-2*time.Hour), 10)
	h.m.state.update(func(d *stateData) { d.target("t1").Partial = []string{"p1"} })
	h.runNow(j)
	if slices.Contains(h.run.runs("t1"), "p1") {
		t.Fatal("a partial run outlived a newer complete one")
	}
	h.m.state.view(func(d *stateData) {
		if len(d.Targets["t1"].Partial) != 0 {
			t.Errorf("partial marks %v", d.Targets["t1"].Partial)
		}
	})
}

func TestMaintenanceDeletesForTheFloor(t *testing.T) {
	h := newHarness(t)
	h.set.fs["t1"] = true
	h.job("j1", "t1", 60, "web")
	now := h.clock.Now()
	for i := 1; i <= 6; i++ {
		h.run.seed("t1", "j1", "r"+itoa(i), now.Add(-time.Duration(i)*24*time.Hour), 1<<30)
	}
	rp := h.run.repo("t1")
	// 100 GB disk: the floor is 10 GB, 7.5 GB are free, so 2.5 GB must go.
	rp.total, rp.free = 100<<30, 15<<29
	before := len(h.run.runs("t1"))
	h.m.maintain(context.Background(), "t1")
	rec := h.record()
	after := len(h.run.runs("t1"))
	if rec.Kind != KindMaintain || rec.Status != StatusOK || before-after != 3 || rec.Deleted != 3 {
		t.Fatalf("record %+v, %d runs before, %d after", rec, before, after)
	}
	h.m.state.view(func(d *stateData) {
		ts := d.Targets["t1"]
		if ts.Garbage != 0 || !fromMs(ts.NextMaintain).Equal(now.Add(h.m.t.maintainSoon)) {
			t.Errorf("target state %+v", ts)
		}
	})

	// Garbage on its way back counts: the next pass deletes nothing more.
	rp.garbage = 3 << 30
	h.m.maintain(context.Background(), "t1")
	if rec := h.record(); rec.Deleted != 0 {
		t.Fatalf("deleted again for space already coming back: %+v", rec)
	}
}

func TestMaintenanceKeepsAJobWithinItsBudget(t *testing.T) {
	h := newHarness(t)
	j := h.job("j1", "t1", 60, "web")
	h.set.jobs[0].Budget = 350
	_ = j
	now := h.clock.Now()
	for i := 1; i <= 5; i++ {
		h.run.seed("t1", "j1", "r"+itoa(i), now.Add(-time.Duration(i)*24*time.Hour), 100)
	}
	h.m.state.update(func(d *stateData) { d.job("j1") })
	h.m.maintain(context.Background(), "t1")
	rec := h.record()
	if len(h.run.runs("t1")) != 3 || rec.Deleted != 2 {
		t.Fatalf("record %+v, runs %v", rec, h.run.runs("t1"))
	}
	// The job reports what it references after the deletions, not the
	// measurement they were decided on.
	jobs, _ := h.m.Status()
	if len(jobs) != 1 || jobs[0].Used != 300 {
		t.Fatalf("job status %+v", jobs)
	}
}

func TestJobUsageStopsAtASparedRun(t *testing.T) {
	runs := map[string]*runInfo{"a1": {job: "a"}, "a2": {job: "a"}, "a3": {job: "a"}, "b1": {job: "b"}}
	order := []string{"a1", "b1", "a2", "a3"}
	sp := &proto.Space{
		Jobs:  map[string]int64{"a": 300, "b": 50},
		Freed: []proto.Freed{{Repo: 100, Job: 100}, {Repo: 150, Job: 50}, {Repo: 250, Job: 200}, {Repo: 350, Job: 300}},
	}
	policies := map[string]thin.Policy{"a": {}, "b": {}, "c": {}}
	// a2 was spared (a restore reads it), so only a1 counts as gone for a.
	got := jobUsage(sp, order, runs, map[string]bool{"a1": true, "a3": true, "b1": true}, policies)
	want := map[string]int64{"a": 200, "b": 0, "c": 0}
	if !maps.Equal(got, want) {
		t.Fatalf("usage %v, want %v", got, want)
	}
	if got := jobUsage(sp, order, runs, nil, policies); got["a"] != 300 || got["b"] != 50 {
		t.Fatalf("usage without deletions %v", got)
	}
}

func TestScheduleRunsOneBackupAtATime(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	h.job("j1", "t1", 60, "web")
	h.job("j2", "t2", 60, "web")
	block := make(chan struct{})
	h.m.cfg.States = blockingStates{block}
	h.m.schedule(context.Background())
	h.m.schedule(context.Background())
	h.m.mu.Lock()
	busy := len(h.m.busy)
	h.m.mu.Unlock()
	close(block)
	h.m.workers.Wait()
	if busy != 1 {
		t.Fatalf("%d targets busy at once", busy)
	}
}

type blockingStates struct{ c chan struct{} }

func (b blockingStates) BackupAppState(ctx context.Context, _ string) (docker.AppState, error) {
	<-b.c
	return docker.AppState{Running: true}, nil
}

func TestBusyHostPostponesWithinALimit(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	h.job("j1", "t1", 60, "web")
	h.m.cfg.Load = func() sysinfo.Load { return sysinfo.Load{Cores: 2, Load1: 9} }
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	if len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatal("a busy host ran the backup")
	}
	h.clock.Add(h.m.t.maxPostpone + time.Minute)
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	if len(h.run.ops(proto.OpBackup)) != 1 {
		t.Fatal("the backup was postponed past its limit")
	}
}

func TestStopHandsTheRunOverAndTheNextServerAdoptsIt(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.job("j1", "t1", 60, "db")
	started := make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-h.run.released
		return nil, runner.ErrReleased
	}
	h.m.Start()
	<-started
	h.m.Stop()
	select {
	case r := <-h.recs:
		t.Fatalf("a handed over run was recorded: %+v", r)
	default:
	}
	var running string
	h.m.state.view(func(d *stateData) { running = d.Jobs["j1"].Running })
	if running == "" {
		t.Fatal("the run in flight is not in the state")
	}

	// The next server: same base directory, a new runner with the helper.
	next := newFakeRunner(h.clock)
	next.orphans = []Orphan{&fakeOrphan{labels: map[string]string{
		labelKind: KindBackup, labelJob: "j1", labelTarget: "t1", labelApps: "db", labelComplete: "true", runner.LabelRun: running,
	}, snaps: []proto.Snapshot{{ID: "x1", Run: running, Source: proto.Source{Job: "j1", App: "db", Name: "postgres"}, Start: h.clock.Now()}}}}
	h.run = next
	m2 := h.manager()
	m2.Start()
	rec := h.record()
	m2.Stop()
	if !rec.Adopted || rec.Run != running || rec.Status != StatusOK || rec.Snapshots != 1 {
		t.Fatalf("adopted record %+v", rec)
	}
	m2.state.view(func(d *stateData) {
		if d.Jobs["j1"].Running != "" {
			t.Error("the adopted run still reads as running")
		}
	})
}

type fakeOrphan struct {
	labels map[string]string
	snaps  []proto.Snapshot
}

func (o *fakeOrphan) Labels() map[string]string { return o.labels }
func (o *fakeOrphan) Follow(_ context.Context, h proto.Handler) (*proto.Result, error) {
	for _, s := range o.snaps {
		h.Snapshot(s)
	}
	return &proto.Result{Count: len(o.snaps)}, nil
}

func TestNoRunBeforeAdoption(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	h.job("j1", "t1", 60, "web")
	h.run.adoptErr = errDocker
	h.m.t.adoptRetry = time.Hour
	h.m.Start()
	time.Sleep(50 * time.Millisecond)
	h.m.Stop()
	if len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatal("a run started before the previous server's helpers were adopted")
	}
}

func TestNextAfter(t *testing.T) {
	due := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Duration
		want time.Duration
	}{
		{0, time.Hour},
		{30 * time.Minute, time.Hour},
		{time.Hour, 2 * time.Hour},
		{5*time.Hour + time.Minute, 6 * time.Hour},
	} {
		if got := nextAfter(due, time.Hour, due.Add(c.now)); got != due.Add(c.want) {
			t.Errorf("now +%v: %v, want +%v", c.now, got.Sub(due), c.want)
		}
	}
}

func TestLoaded(t *testing.T) {
	for _, c := range []struct {
		l    sysinfo.Load
		want bool
	}{
		{sysinfo.Load{}, false},
		{sysinfo.Load{Cores: 4, Load1: 4}, false},
		{sysinfo.Load{Cores: 4, Load1: 7}, true},
		{sysinfo.Load{MemTotal: 100, MemAvailable: 5}, true},
		{sysinfo.Load{MemTotal: 100}, false},
	} {
		if got := loaded(c.l); got != c.want {
			t.Errorf("%+v: %v", c.l, got)
		}
	}
}

func TestRunLimit(t *testing.T) {
	m := &Manager{t: defaultTiming()}
	if m.runLimit(15) != time.Hour || m.runLimit(24*60) != 12*time.Hour || m.runLimit(120) != 8*time.Hour {
		t.Fatal("run limits")
	}
}

func TestDamagedStateIsSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup", "state.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{nope"), 0o600)
	s, err := openStore(path)
	if s == nil || err == nil {
		t.Fatalf("store %v, err %v", s, err)
	}
	if _, err := os.Stat(path + ".damaged"); err != nil {
		t.Fatal("the damaged file was not kept")
	}
}

func TestInitUsesTheCache(t *testing.T) {
	h := newHarness(t)
	a, _ := h.set.Access("t1", settings.AccessOptions{Write: true})
	if _, err := h.m.Init(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	c := h.run.ops(proto.OpInit)
	if len(c) != 1 || c[0].req.Repo.CacheDir == "" || c[0].req.Repo.Owner == nil {
		t.Fatalf("init %+v", c)
	}
}

func TestRunNowRunsBesideTheSchedule(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	j := h.job("j1", "t1", 60, "web")
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	h.record()
	jobs, _ := h.m.Status()
	regular := jobs[0].Next

	if _, err := h.m.RunNow("nope"); code(err) != settings.CodeNotFound {
		t.Fatalf("unknown job: %v", err)
	}
	h.clock.Add(10 * time.Minute)
	st, err := h.m.RunNow(j.ID)
	if err != nil || st.Manual == 0 {
		t.Fatalf("run now: %+v %v", st, err)
	}
	// A busy host does not hold an operator's request back.
	h.m.cfg.Load = func() sysinfo.Load { return sysinfo.Load{Cores: 2, Load1: 9} }
	h.m.schedule(context.Background())
	h.m.workers.Wait()
	if rec := h.record(); rec.Trigger != triggerManual || rec.Status != StatusOK {
		t.Fatalf("manual run: %+v", rec)
	}
	jobs, _ = h.m.Status()
	if jobs[0].Next != regular || jobs[0].Manual != 0 {
		t.Fatalf("after the manual run the schedule is %+v, want next %d", jobs[0], regular)
	}
}

func TestRunNowRefusesARunningJob(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	j := h.job("j1", "t1", 60, "web")
	block := make(chan struct{})
	h.m.cfg.States = blockingStates{block}
	h.m.schedule(context.Background())
	defer func() {
		close(block)
		h.m.workers.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, _ := h.m.Status()
		if len(jobs) > 0 && jobs[0].Running != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.m.RunNow(j.ID); code(err) != settings.CodeInUse {
		t.Fatalf("run now while running: %v", err)
	}
}

func code(err error) string {
	var e *settings.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}

func TestFairShareOnASharedTarget(t *testing.T) {
	const gb = 1 << 30
	seed := func(h *harness, size int64) {
		now := h.clock.Now()
		for i := 1; i <= 6; i++ {
			h.run.seed("t1", "j1", "r"+itoa(i), now.Add(-time.Duration(i)*24*time.Hour), size)
		}
	}
	fresh := func(h *harness, id string, used int64) proto.PeerUsage {
		return proto.PeerUsage{ID: id, Bytes: used, Updated: h.clock.Now().Add(-time.Hour)}
	}
	peerA, peerB := strings.Repeat("a", 32), strings.Repeat("b", 32)

	// 1000 GB disk, floor 100 GB, 90 GB free: 10 GB short. This server
	// holds 6 GB next to a peer holding 800 GB: it is far below its share
	// and deletes nothing for the peer's growth.
	h := newHarness(t)
	h.set.fs["t1"] = true
	h.job("j1", "t1", 60, "web")
	seed(h, gb)
	rp := h.run.repo("t1")
	rp.total, rp.free = 1000*gb, 90*gb
	rp.peers = []proto.PeerUsage{fresh(h, peerA, 800*gb)}
	h.m.maintain(context.Background(), "t1")
	if rec := h.record(); rec.Status != StatusOK || rec.Deleted != 0 || len(h.run.runs("t1")) != 6 {
		t.Fatalf("a server below its share deleted for another: %+v", rec)
	}
	if rp.shared == 0 {
		t.Fatal("maintenance did not write the usage file")
	}
	_, targets := h.m.Status()
	if len(targets) != 1 || targets[0].Share == 0 || len(targets[0].Servers) != 1 || !targets[0].Servers[0].Active {
		t.Fatalf("status: %+v", targets)
	}

	// The same server above its share (600 GB beside 200 GB) deletes;
	// an orphan's usage file (a month old) takes no part and is reported.
	h = newHarness(t)
	h.set.fs["t1"] = true
	h.job("j1", "t1", 60, "web")
	seed(h, 100*gb)
	rp = h.run.repo("t1")
	rp.total, rp.free = 1000*gb, 90*gb
	old := fresh(h, peerB, 50*gb)
	old.Updated = h.clock.Now().Add(-30 * 24 * time.Hour)
	rp.peers = []proto.PeerUsage{fresh(h, peerA, 200*gb), old}
	h.m.maintain(context.Background(), "t1")
	rec := h.record()
	if rec.Deleted != 1 || !strings.Contains(rec.Error, "fair share") {
		t.Fatalf("a server above its share: %+v", rec)
	}
	health := h.m.Health()
	if len(health.Orphans) != 1 || health.Orphans[0].Server != peerB || health.Orphans[0].Target != "t1" || len(health.Shared) != 1 {
		t.Fatalf("health: %+v", health)
	}
}

func TestVerificationReportsDamageUntilAFullOneIsClean(t *testing.T) {
	h := newHarness(t)
	h.job("j1", "t1", 60, "web")
	h.run.seed("t1", "j1", "r1", h.clock.Now().Add(-time.Hour), 100)
	verify := func() (RunRecord, float64) {
		h.t.Helper()
		h.m.verify(context.Background(), "t1")
		rec := h.record()
		calls := h.run.ops(proto.OpVerify)
		return rec, calls[len(calls)-1].req.Verify.Percent
	}

	rec, pct := verify()
	if rec.Kind != KindVerify || rec.Status != StatusOK || rec.Verify == nil || rec.Verify.Snapshots != 1 || pct != verifyRemote {
		t.Fatalf("clean verification: %+v at %v%%", rec, pct)
	}
	h.m.state.view(func(d *stateData) {
		if ts := d.Targets["t1"]; !fromMs(ts.NextVerify).Equal(h.clock.Now().Add(h.m.t.verifyEvery)) || ts.Verifying != nil {
			t.Errorf("schedule: %+v", ts)
		}
	})

	h.run.repo("t1").damage = []string{"object k123 is backed by missing blob p456"}
	if rec, _ := verify(); rec.Status != StatusFailed || rec.Verify.Damaged != 1 || rec.Error == "" {
		t.Fatalf("damage: %+v", rec)
	}
	if c := h.m.Health().Corrupt; len(c) != 1 || c[0] != "t1" {
		t.Fatalf("corrupt: %v", c)
	}
	// Sampling could miss the damaged file, so only a full read clears it.
	h.run.repo("t1").damage = nil
	if rec, pct := verify(); rec.Status != StatusOK || pct != verifyFull {
		t.Fatalf("after damage: %+v at %v%%", rec, pct)
	}
	if c := h.m.Health().Corrupt; len(c) != 0 {
		t.Fatalf("still corrupt after a clean full verification: %v", c)
	}
	if _, pct := verify(); pct != verifyRemote {
		t.Fatalf("back to sampling at %v%%", pct)
	}
}

func TestAnAdoptedVerificationIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.m.state.update(func(d *stateData) { d.target("t1").Verifying = &VerifyReport{Percent: verifyFull} })
	h.m.follow(context.Background(), &verifyOrphan{})
	rec := h.record()
	if rec.Kind != KindVerify || !rec.Adopted || rec.Verify == nil || rec.Verify.Percent != verifyFull || rec.Verify.Damaged != 1 {
		t.Fatalf("adopted verification: %+v", rec)
	}
	if c := h.m.Health().Corrupt; len(c) != 1 {
		t.Fatalf("corrupt: %v", c)
	}
	h.m.mu.Lock()
	slot := h.m.maintSlot
	h.m.mu.Unlock()
	if slot {
		t.Fatal("the maintenance slot was not given back")
	}
}

type verifyOrphan struct{}

func (verifyOrphan) Labels() map[string]string {
	return map[string]string{labelKind: KindVerify, labelTarget: "t1"}
}
func (verifyOrphan) Follow(context.Context, proto.Handler) (*proto.Result, error) {
	return &proto.Result{Verify: &proto.VerifyResult{Snapshots: 2, Errors: []string{"damaged"}}}, nil
}

func TestSnapshotRepositoryVerification(t *testing.T) {
	h := newHarness(t)
	if err := h.m.ensureStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.run.repo(h.snapRepo()).damage = []string{"damaged"}
	h.m.verify(context.Background(), SnapshotTarget)
	rec := h.snapshot()
	if rec.Kind != KindVerify || rec.Status != StatusFailed || rec.Verify == nil {
		t.Fatalf("record: %+v", rec)
	}
	if st := h.m.SnapshotStatus(); len(st.Warnings) != 1 || st.Warnings[0].Code != ReasonCorrupt || st.LastVerify == 0 {
		t.Fatalf("status: %+v", st)
	}
	if c := h.m.Health().Corrupt; len(c) != 0 {
		t.Fatalf("the snapshot repository is not a target: %v", c)
	}
	h.run.repo(h.snapRepo()).damage = nil
	h.m.verify(context.Background(), SnapshotTarget)
	h.snapshot()
	if st := h.m.SnapshotStatus(); len(st.Warnings) != 0 {
		t.Fatalf("warning not cleared: %+v", st.Warnings)
	}
}

func TestVerificationIsScheduledInTheMaintenanceSlot(t *testing.T) {
	h := newHarness(t)
	h.job("j1", "t1", 60, "web")
	now := h.clock.Now()
	h.m.state.update(func(d *stateData) {
		ts := d.target("t1")
		ts.LastMaintain, ts.NextMaintain = ms(now.Add(-time.Hour)), ms(now.Add(time.Hour))
	})
	jobs, _ := h.set.Jobs()
	next := now.Add(time.Hour)
	h.m.mu.Lock()
	h.m.busy["t1"] = KindBackup
	h.m.mu.Unlock()
	h.m.scheduleMaintenance(context.Background(), jobs, nil, now, &next)
	if len(h.run.ops(proto.OpVerify)) != 0 {
		t.Fatal("verified a target a backup holds")
	}
	h.m.free("t1", KindBackup)
	h.m.scheduleMaintenance(context.Background(), jobs, nil, now, &next)
	if rec := h.record(); rec.Kind != KindVerify {
		t.Fatalf("record: %+v", rec)
	}
	h.m.workers.Wait()
}

func TestStopWhileSchedulingHasNoWorkerRace(t *testing.T) {
	h := newHarness(t)
	h.app("web")
	h.job("j1", "t1", 60, "web")
	now := h.clock.Now()
	for range 20 {
		h.m.state.update(func(d *stateData) {
			d.job("j1").Next = ms(now.Add(-time.Minute))
			d.target("t1").NextMaintain = ms(now.Add(-time.Minute))
		})
		h.m.Start()
		h.m.poke()
		h.m.Stop()
	}
}
