package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/docker"
)

const gib = int64(1) << 30

// live marks the harness's manager as started without its schedule, so
// the snapshot triggers run and nothing else does.
func (h *harness) live() {
	h.m.mu.Lock()
	h.m.ctx, h.m.cancel = context.WithCancel(context.Background())
	h.m.recoveredCh = make(chan struct{})
	close(h.m.recoveredCh)
	h.m.mu.Unlock()
	h.t.Cleanup(func() {
		h.m.mu.Lock()
		cancel := h.m.cancel
		h.m.cancel = nil
		h.m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		h.m.workers.Wait()
	})
}

// stateless adds an app without data that links others.
func (h *harness) stateless(name string, linked ...string) {
	h.apps.mu.Lock()
	h.apps.list = append(h.apps.list, settings.App{Name: name, Linked: linked})
	h.apps.mu.Unlock()
}

func (h *harness) drop(name string) {
	h.apps.mu.Lock()
	h.apps.list = slices.DeleteFunc(h.apps.list, func(a settings.App) bool { return a.Name == name })
	delete(h.apps.apps, name)
	h.apps.mu.Unlock()
}

func (h *harness) snapRuns() []string { return h.run.runs(h.snapRepo()) }

// The budget is the swap rule's share of free space plus the repository's
// own size, capped so the disk keeps its floor, and nothing below the
// floor. The floor of a 100 GiB disk is 10 GiB.
func TestSnapshotBudgetDecisions(t *testing.T) {
	cases := []struct {
		name        string
		s           snapSpace
		pct         int
		extra       int64
		limit       int64
		excess      int64
		fits        bool
		description string
	}{
		{name: "empty repository on a roomy disk", s: snapSpace{Free: 80 * gib, Total: 100 * gib, ok: true}, pct: 25, limit: 20 * gib, fits: true},
		{name: "own size counts as reclaimable", s: snapSpace{Free: 60 * gib, Total: 100 * gib, Used: 20 * gib, ok: true}, pct: 25, limit: 20 * gib, fits: true},
		{name: "apps fill the disk, the budget shrinks", s: snapSpace{Free: 20 * gib, Total: 100 * gib, Used: 20 * gib, ok: true}, pct: 25, limit: 10 * gib, excess: 10 * gib, fits: true},
		{name: "garbage on its way back is not deleted for again", s: snapSpace{Free: 20 * gib, Total: 100 * gib, Used: 20 * gib, Garbage: 12 * gib, ok: true}, pct: 25, limit: 10 * gib, fits: true},
		{name: "the next snapshot's estimate must fit too", s: snapSpace{Free: 60 * gib, Total: 100 * gib, Used: 18 * gib, ok: true}, pct: 25, extra: 4 * gib, limit: 19*gib + 512*1024*1024, excess: 2*gib + 512*1024*1024, fits: true},
		{name: "the floor caps the share", s: snapSpace{Free: 12 * gib, Total: 100 * gib, Used: 6 * gib, ok: true}, pct: 50, limit: 8 * gib, fits: true},
		{name: "below the floor snapshots get nothing", s: snapSpace{Free: 9 * gib, Total: 100 * gib, Used: 5 * gib, ok: true}, pct: 25, limit: 0, excess: 5 * gib, fits: false},
		{name: "a small disk keeps 5 GiB", s: snapSpace{Free: 6 * gib, Total: 20 * gib, ok: true}, pct: 25, limit: 1 * gib, fits: true},
		{name: "a small disk near its floor", s: snapSpace{Free: 5*gib + gib/4, Total: 20 * gib, ok: true}, pct: 25, limit: gib / 4, fits: false},
		{name: "an unmeasured disk applies only retention", s: snapSpace{Used: 5 * gib}, pct: 25, limit: -1, fits: true},
	}
	for _, c := range cases {
		if got := snapshotLimit(c.s, c.pct); got != c.limit {
			t.Errorf("%s: limit %d, want %d", c.name, got, c.limit)
		}
		if got := snapshotExcess(c.s, c.pct, c.extra); got != c.excess {
			t.Errorf("%s: excess %d, want %d", c.name, got, c.excess)
		}
		if got := snapshotFits(c.s, c.extra); got != c.fits {
			t.Errorf("%s: fits %v, want %v", c.name, got, c.fits)
		}
	}
}

func TestSnapshotGroupsAndPeriodicRoots(t *testing.T) {
	all := []settings.App{
		{Name: "web", Linked: []string{"db", "cache"}},
		{Name: "db", Stateful: true},
		{Name: "cache", Stateful: true, Linked: []string{"db"}},
		{Name: "blog", Stateful: true, Linked: []string{"blogdb"}},
		{Name: "blogdb", Stateful: true},
		{Name: "a", Stateful: true, Linked: []string{"b"}},
		{Name: "b", Stateful: true, Linked: []string{"a"}},
		{Name: "static"},
	}
	if got := snapshotGroup("web", all); !slices.Equal(got, []string{"cache", "db"}) {
		t.Errorf("a stateless app's group is its linked data: %v", got)
	}
	if got := snapshotGroup("static", all); len(got) != 0 {
		t.Errorf("an app without data and links has no group: %v", got)
	}
	if got := snapshotGroup("blog", all); !slices.Equal(got, []string{"blog", "blogdb"}) {
		t.Errorf("group %v", got)
	}
	// A database another app with data takes anyway gets no series of
	// its own; in a cycle the first name takes the other.
	if got := periodicRoots(all); !slices.Equal(got, []string{"a", "blog", "cache"}) {
		t.Errorf("roots %v", got)
	}
}

func TestDeploySnapshotTakesTheLinkedDatabaseAndIsNotCancelled(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.stateless("web", "db")
	h.live()
	// The deploy holds web, which cancels any backup dump of db; the
	// snapshot is part of the deploy and goes on.
	h.m.DeployStarted("web")
	started := make(chan struct{})
	release := make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-release
		return []byte("dump of db"), nil
	}
	p, _ := h.m.BeforeDeploy("web", EventDeploy)
	if p == nil {
		t.Fatal("no snapshot before deploying an app whose database holds data")
	}
	<-started
	h.m.DeployStarted("web")
	close(release)
	run, note := p.Wait(0)
	if run == "" || !strings.Contains(note, "taken before the deploy") {
		t.Fatalf("wait: %q %q", run, note)
	}
	rec := h.snapshot()
	if rec.Run != run {
		t.Fatalf("wait named run %s, the record %s", run, rec.Run)
	}
	if rec.Status != StatusOK || rec.Event != EventDeploy || rec.App != "web" || len(rec.Apps) != 1 || rec.Apps[0].App != "db" {
		t.Fatalf("record %+v", rec)
	}
	snaps := h.run.repo(h.snapRepo()).snaps
	if len(snaps) == 0 || snaps[0].Source.Job != "web" || snaps[0].Trigger != EventDeploy || snaps[0].Kind != "stream" {
		t.Fatalf("snapshots %+v", snaps)
	}
	if c := h.run.ops(proto.OpBackup)[0]; c.labels[labelKind] != KindSnapshot || c.req.Repo.Owner != nil {
		t.Errorf("helper labels %v, owner %+v", c.labels, c.req.Repo.Owner)
	}
	st := h.m.SnapshotStatus()
	if len(st.Deploys) != 1 || st.Deploys[0].Status != StatusOK || st.Deploys[0].Run != rec.Run {
		t.Errorf("deploy mark %+v", st.Deploys)
	}
	if pts := h.m.SnapshotPoints("db"); len(pts) != 1 || pts[0].Run != rec.Run || !pts[0].Apps[0].Dump {
		t.Errorf("points %+v", pts)
	}
	h.m.DeployEnded("web")
	h.m.DeployEnded("web")
}

func TestStatelessDeployTakesNoSnapshot(t *testing.T) {
	h := newHarness(t)
	h.stateless("static")
	h.live()
	p, note := h.m.BeforeDeploy("static", EventDeploy)
	if p != nil {
		t.Fatal("a snapshot before deploying an app without data")
	}
	if !strings.Contains(note, "hold no data") {
		t.Fatalf("why none: %q", note)
	}
	if run, note := (*PendingSnapshot)(nil).Wait(0); run != "" || note != "" {
		t.Fatal("no snapshot is no news")
	}
}

// A version switch's snapshot is its own event: listed as version, kept
// in a series of its own and never counted as a deploy's.
func TestVersionSnapshotIsItsOwnEvent(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	h.m.DeployStarted("db")
	defer h.m.DeployEnded("db")
	p, why := h.m.BeforeDeploy("db", EventVersion)
	if p == nil {
		t.Fatalf("no snapshot before a version change of an app with data: %q", why)
	}
	run, note := p.Wait(time.Minute)
	if run == "" || !strings.Contains(note, "before the version change") {
		t.Fatalf("wait: %q %q", run, note)
	}
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Event != EventVersion || rec.Run != run {
		t.Fatalf("record %+v", rec)
	}
	for _, sn := range h.run.repo(h.snapRepo()).snaps {
		if sn.Trigger != EventVersion {
			t.Fatalf("snapshot tagged %q", sn.Trigger)
		}
	}
	if pts := h.m.SnapshotPoints("db"); len(pts) != 1 || pts[0].Event != EventVersion || pts[0].Run != run {
		t.Fatalf("points %+v", pts)
	}
	if st := h.m.SnapshotStatus(); len(st.Deploys) != 0 {
		t.Fatalf("a version change marked as a deploy: %+v", st.Deploys)
	}
}

// A caller that must not go on without a snapshot learns why none is
// taken: snapshots off, or an event no operation takes.
func TestBeforeDeploySaysWhyItTakesNone(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	if p, why := h.m.BeforeDeploy("db", "periodic"); p != nil || why == "" {
		t.Fatalf("an event no operation takes: %v %q", p, why)
	}
	h.set.mu.Lock()
	h.set.snapshots = &settings.Snapshots{Enabled: false, MaxDiskPct: 25}
	h.set.mu.Unlock()
	p, why := h.m.BeforeDeploy("db", EventVersion)
	if p != nil || why != "No snapshot before the version change: snapshots are turned off" {
		t.Fatalf("snapshots off: %v %q", p, why)
	}
}

// A limit bounds the whole wait, the helpers' time to stop included, so a
// version switch keeps the rest of its budget for its health gate.
func TestWaitIsBoundedByTheCallersLimit(t *testing.T) {
	h := newHarness(t)
	h.m.t.snap.deployWait = time.Hour
	h.m.t.snap.abortGrace = time.Hour
	h.app("db")
	h.live()
	started, release := make(chan struct{}), make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-release
		return nil, ctx.Err()
	}
	defer close(release)
	p, _ := h.m.BeforeDeploy("db", EventVersion)
	<-started
	begin := time.Now()
	run, note := p.Wait(200 * time.Millisecond)
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("waited %s past a 200ms limit", took)
	}
	if run != "" || !strings.Contains(note, "did not finish in time") {
		t.Fatalf("wait: %q %q", run, note)
	}
}

func TestDeployGivesUpASlowSnapshot(t *testing.T) {
	h := newHarness(t)
	h.m.t.snap.deployWait = 50 * time.Millisecond
	h.app("db")
	h.live()
	started, stopped := make(chan struct{}), make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	p, _ := h.m.BeforeDeploy("db", EventDeploy)
	<-started
	run, note := p.Wait(0)
	if run != "" || !strings.Contains(note, "without a snapshot") {
		t.Fatalf("wait: %q %q", run, note)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the dump still runs beside the deploy")
	}
	rec := h.snapshot()
	if rec.Status != StatusSkipped || rec.Reason != ReasonTimeout {
		t.Fatalf("record %+v", rec)
	}
	if len(h.snapRuns()) != 0 {
		t.Fatal("a snapshot that was given up stayed")
	}
	if st := h.m.SnapshotStatus(); len(st.Deploys) != 1 || st.Deploys[0].Reason != ReasonTimeout {
		t.Fatalf("the deploy is not marked as one without a snapshot: %+v", st.Deploys)
	}
}

func TestSnapshotsOff(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	h.set.snapshots = &settings.Snapshots{Enabled: false, MaxDiskPct: 25}
	if p, _ := h.m.BeforeDeploy("db", EventDeploy); p != nil || h.m.BeforeDelete("db") != "" || h.m.BeforeUpdate() != "" {
		t.Fatal("a snapshot was taken while snapshots are off")
	}
	if _, err := h.m.TakeSnapshot("db"); err == nil {
		t.Fatal("a manual snapshot while snapshots are off")
	}
	if len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatal("a helper ran")
	}
}

func TestDeletedAppIsKeptForAWeekAndComesBackAsNewApp(t *testing.T) {
	h := newHarness(t)
	h.app("blog")
	h.live()
	os.WriteFile(filepath.Join(h.base, "apps", "blog", "data", "post.md"), []byte("hello"), 0o644)
	note := h.m.BeforeDelete("blog")
	if !strings.Contains(note, "taken before deleting") {
		t.Fatalf("note %q", note)
	}
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Event != EventDelete {
		t.Fatalf("record %+v", rec)
	}
	h.drop("blog")
	h.clock.Add(6 * 24 * time.Hour)
	h.m.tidySnapshots(context.Background(), nil, 0)
	pts := h.m.SnapshotPoints("blog")
	if len(pts) != 1 || pts[0].Gone == 0 || pts[0].Expires != pts[0].Gone+int64(7*24*time.Hour/time.Millisecond) {
		t.Fatalf("points %+v", pts)
	}
	if req := h.m.snapshotRestore(SnapshotRestoreRequest{Run: rec.Run}); req.Mode != RestoreNewApp || req.Target != SnapshotTarget {
		t.Fatalf("a deleted app comes back as a new app, got %+v", req)
	}
	h.clock.Add(2 * 24 * time.Hour)
	h.m.tidySnapshots(context.Background(), nil, 0)
	if len(h.snapRuns()) != 0 || len(h.m.SnapshotPoints("")) != 0 {
		t.Fatal("a deleted app's snapshot outlived its week")
	}
}

func TestSnapshotRestoreDefaultsToInPlace(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	if _, err := h.m.TakeSnapshot("db"); err != nil {
		t.Fatal(err)
	}
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Event != EventManual {
		t.Fatalf("record %+v", rec)
	}
	if req := h.m.snapshotRestore(SnapshotRestoreRequest{Run: rec.Run}); req.Mode != RestoreInPlace {
		t.Fatalf("mode %s", req.Mode)
	}
	if req := h.m.snapshotRestore(SnapshotRestoreRequest{Run: rec.Run, Mode: RestoreNewApp}); req.Mode != RestoreNewApp {
		t.Fatalf("an asked mode is kept, got %s", req.Mode)
	}
}

func TestSnapshotRefusedNearTheFloor(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	// 100 GiB with 10.5 GiB free: the floor is 10 GiB, the margin 1 GiB.
	h.disk.set(10*gib+gib/2, 100*gib)
	if _, err := h.m.TakeSnapshot("db"); err != nil {
		t.Fatal(err)
	}
	rec := h.snapshot()
	if rec.Status != StatusSkipped || rec.Reason != ReasonNoSpace || len(h.run.ops(proto.OpBackup)) != 0 {
		t.Fatalf("record %+v", rec)
	}
	st := h.m.SnapshotStatus()
	if len(st.Warnings) == 0 || st.Warnings[0].Code != ReasonNoSpace || st.Floor != 10*gib || st.Limit == nil {
		t.Fatalf("status %+v", st)
	}
}

// Over the budget, the periodic snapshots go first, then the older event
// snapshots, and only then the newest one of each app.
func TestSnapshotBudgetThinsPeriodicFirst(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	now := h.clock.Now()
	repo := h.snapRepo()
	for i := range 3 {
		h.run.seedSnapshot(repo, "db", EventPeriodic, fmt.Sprintf("p%d", i), now.Add(-time.Duration(3-i)*time.Hour), 10*gib)
	}
	h.run.seedSnapshot(repo, "db", EventDeploy, "d0", now.Add(-30*time.Minute), 10*gib)
	// 200 GiB free without the repository's 40 GiB: 25% of 200 is 50 GiB
	// of room, and the next snapshot is expected to add 20 GiB.
	h.disk.set(200*gib, 400*gib)
	h.m.state.update(func(d *stateData) { d.Snap.Growth["db"] = 20 * gib })
	if _, err := h.m.TakeSnapshot("db"); err != nil {
		t.Fatal(err)
	}
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Deleted == 0 {
		t.Fatalf("record %+v", rec)
	}
	// The middle periodic one is the least harm: the oldest is the
	// series' depth and goes last of the periodic ones.
	runs := h.snapRuns()
	if !slices.Contains(runs, "d0") || !slices.Contains(runs, "p0") || slices.Contains(runs, "p1") {
		t.Fatalf("runs left %v", runs)
	}
}

// A snapshot that brings the disk close to its floor while it writes is
// given up and leaves nothing.
func TestSnapshotGivenUpWhenTheDiskFills(t *testing.T) {
	h := newHarness(t)
	h.m.t.snap.spaceCheck = 5 * time.Millisecond
	h.app("db")
	h.live()
	h.disk.set(40*gib, 100*gib)
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		// An app fills the disk meanwhile.
		h.disk.set(10*gib, 100*gib)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := h.m.TakeSnapshot("db"); err != nil {
		t.Fatal(err)
	}
	rec := h.snapshot()
	if rec.Status != StatusFailed || rec.Reason != ReasonNoSpace {
		t.Fatalf("record %+v", rec)
	}
	if len(h.snapRuns()) != 0 {
		t.Fatal("the given up snapshot left a restore point")
	}
}

func TestEventSnapshotsKeepTheLastFive(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	now := h.clock.Now()
	for i := range 7 {
		h.run.seedSnapshot(h.snapRepo(), "db", EventDeploy, fmt.Sprintf("d%d", i), now.Add(-time.Duration(7-i)*time.Hour), 1)
	}
	ensureSnapshotKey(h.base)
	h.m.tidySnapshots(context.Background(), nil, 0)
	if runs := h.snapRuns(); !slices.Equal(runs, []string{"d2", "d3", "d4", "d5", "d6"}) {
		t.Fatalf("runs %v", runs)
	}
	if pts := h.m.SnapshotPoints(""); len(pts) != 5 || pts[0].Run != "d6" {
		t.Fatalf("index %+v", pts)
	}
}

// Periodic snapshots come ten times the last one's duration apart, at
// least an hour.
func TestPeriodicSnapshotSchedule(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	ensureSnapshotKey(h.base)
	h.m.mu.Lock()
	h.m.snapListed = true
	h.m.mu.Unlock()
	next := h.clock.Now().Add(time.Hour)
	h.m.scheduleSnapshots(h.m.ctx, h.clock.Now(), &next)
	h.m.workers.Wait()
	var p periodicState
	h.m.state.view(func(d *stateData) { p = *d.Snap.Periodic["db"] })
	if want := ms(h.clock.Now().Add(h.m.t.snap.periodicFirst)); p.Next != want {
		t.Fatalf("first periodic snapshot at %d, want %d", p.Next, want)
	}
	h.clock.Add(h.m.t.snap.periodicFirst)
	h.m.scheduleSnapshots(h.m.ctx, h.clock.Now(), &next)
	h.m.workers.Wait()
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Event != EventPeriodic {
		t.Fatalf("record %+v", rec)
	}
	h.m.state.view(func(d *stateData) { p = *d.Snap.Periodic["db"] })
	if p.Every != int64(time.Hour/time.Millisecond) || p.Next != rec.End+p.Every {
		t.Fatalf("periodic %+v", p)
	}

	h.m.afterPeriodic("db", SnapshotRecord{Kind: KindSnapshot, Status: StatusOK, Start: 0, End: int64(3 * time.Hour / time.Millisecond)})
	h.m.state.view(func(d *stateData) { p = *d.Snap.Periodic["db"] })
	if p.Every != int64(24*time.Hour/time.Millisecond) {
		t.Fatalf("a 3 hour dump gives a day's interval, got %d", p.Every)
	}
}

// A periodic snapshot is no part of a deploy: the deploy cancels its dump.
func TestDeployCancelsAPeriodicSnapshot(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	ensureSnapshotKey(h.base)
	started := make(chan struct{})
	h.run.dump = func(ctx context.Context, _ docker.DumpSpec) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan SnapshotRecord, 1)
	go func() {
		done <- h.m.takeSnapshot(context.Background(), snapRequest{series: "db", event: EventPeriodic, apps: []string{"db"}, reuse: true})
	}()
	<-started
	h.m.DeployStarted("db")
	rec := <-done
	if rec.Status != StatusDeferred || rec.Reason != ReasonDeploy {
		t.Fatalf("record %+v", rec)
	}
	h.m.DeployEnded("db")
}

func TestUpdateSnapshotTakesOnlyODACsState(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	if note := h.m.BeforeUpdate(); !strings.Contains(note, "before the update") {
		t.Fatalf("note %q", note)
	}
	rec := h.snapshot()
	if rec.Status != StatusOK || rec.Server == nil || len(rec.Apps) != 0 {
		t.Fatalf("record %+v", rec)
	}
	for _, s := range h.run.repo(h.snapRepo()).snaps {
		if s.Source.App != serverApp {
			t.Fatalf("an app's data in the update snapshot: %+v", s)
		}
	}
}

func TestSnapshotKeyIsMadeOnceAndAWrongKeyStartsOver(t *testing.T) {
	base := t.TempDir()
	if err := ensureSnapshotKey(base); err != nil {
		t.Fatal(err)
	}
	k1, err := readSnapshotKey(base)
	if err != nil {
		t.Fatal(err)
	}
	ensureSnapshotKey(base)
	if k2, _ := readSnapshotKey(base); k2 != k1 {
		t.Fatal("the key was rewritten")
	}
	if fi, _ := os.Stat(snapshotKey(base)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
	os.WriteFile(snapshotKey(base), []byte("damaged"), 0o600)
	if err := ensureSnapshotKey(base); err == nil {
		t.Fatal("a damaged key was replaced")
	}
}

// The safety backups an in-place restore left on a target before
// snapshots existed are kept like event snapshots.
func TestLegacySafetyRunsAreThinned(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	runs := map[string]*runInfo{"job": {id: "job", job: "j1", time: now.Add(-30 * 24 * time.Hour)}}
	for i := range 7 {
		id := fmt.Sprintf("s%d", i)
		runs[id] = &runInfo{id: id, job: safetyJob, time: now.Add(-time.Duration(7-i) * time.Hour)}
	}
	runs["old"] = &runInfo{id: "old", job: safetyJob, time: now.Add(-8 * 24 * time.Hour)}
	got := legacySafety(now, runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"old", "s0", "s1"}) {
		t.Fatalf("deleted %v", got)
	}
}

func TestSnapshotRepositoryIsMaintainedOnItsOwn(t *testing.T) {
	h := newHarness(t)
	h.app("db")
	h.live()
	if _, err := h.m.TakeSnapshot("db"); err != nil {
		t.Fatal(err)
	}
	h.snapshot()
	h.clock.Add(2 * time.Hour)
	next := h.clock.Now().Add(time.Hour)
	h.m.scheduleMaintenance(context.Background(), nil, nil, h.clock.Now(), &next)
	h.m.workers.Wait()
	rec := h.snapshot()
	if rec.Kind != KindMaintain || rec.Status != StatusOK {
		t.Fatalf("record %+v", rec)
	}
	if got := h.m.History("", 0); len(got) != 0 {
		t.Fatalf("the snapshot repository's maintenance is in the backup history: %+v", got)
	}
	if _, targets := h.m.Status(); len(targets) != 0 {
		t.Fatalf("the snapshot repository is listed as a target: %+v", targets)
	}
	if h.run.repo(h.snapRepo()).maint != 1 {
		t.Fatal("not maintained")
	}
}
