package appmgr

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/docker"
)

// switchSnapshots plays the backup manager for version switches: it
// records what the switch had done when the snapshot started and when it
// was waited for, and answers with run (taken) or none (BeforeDeploy took
// nothing, for that reason).
type switchSnapshots struct {
	fx       *fixture
	mu       sync.Mutex
	none     string
	run      string
	note     string
	events   []string
	limits   []time.Duration
	canceled int
}

func (s *switchSnapshots) progress() string {
	s.fx.dock.mu.Lock()
	defer s.fx.dock.mu.Unlock()
	return fmt.Sprintf("pulls=%d stopped=%d runs=%d tags=%d", len(s.fx.dock.pulls), len(s.fx.dock.stopped), len(s.fx.dock.runCalls), len(s.fx.dock.tags))
}

func (s *switchSnapshots) BeforeDeploy(app, event string) (DeploySnapshot, string) {
	p := s.progress()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "start "+app+" "+event+" "+p)
	if s.none != "" {
		return nil, s.none
	}
	return s, ""
}

func (s *switchSnapshots) BeforeDelete(string) string { return "" }

func (s *switchSnapshots) Wait(limit time.Duration) (string, string) {
	p := s.progress()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "wait "+p)
	s.limits = append(s.limits, limit)
	if s.run != "" {
		return s.run, "Snapshot " + s.run + " taken before the version change."
	}
	return "", s.note
}

func (s *switchSnapshots) Cancel() {
	s.mu.Lock()
	s.canceled++
	s.mu.Unlock()
}

func (s *switchSnapshots) seen() ([]string, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...), append([]time.Duration(nil), s.limits...)
}

const snapRun = "s20261124T030000Z-a1b2c3"

// The snapshot of an image change starts after the pull and is waited for
// before the old container stops, as the version event.
func TestSetImageSnapshotsAfterThePullAndBeforeTheStop(t *testing.T) {
	fx := newImageFixture(t)
	snaps := &switchSnapshots{fx: fx, run: snapRun}
	fx.m.SetSnapshotter(snaps)
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Snapshot != snapRun {
		t.Fatalf("res = %+v", res)
	}
	events, _ := snaps.seen()
	want := []string{"start db version pulls=1 stopped=0 runs=0 tags=0", "wait pulls=1 stopped=0 runs=0 tags=0"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events %v, want %v", events, want)
	}
	keys := []string{}
	for _, f := range res.ReplyFields() {
		keys = append(keys, f.K)
	}
	if strings.Join(keys, ",") != "success,changed,previous,image,imageId,snapshot" {
		t.Fatalf("reply keys = %v", keys)
	}
	if res.ReplyFields()[len(keys)-1].V != snapRun {
		t.Fatal("reply does not name the snapshot")
	}
}

// A switch that fails after its snapshot still names it: it is the way
// back to the data as it was.
func TestSetImageFailureAfterTheSnapshotNamesIt(t *testing.T) {
	fx := newImageFixture(t)
	fx.m.SetSnapshotter(&switchSnapshots{fx: fx, run: snapRun})
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		if st.ImageID == idNew {
			st.Restarts = 1
		}
		return st
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageUnhealthy || !res.RolledBack || res.Snapshot != snapRun {
		t.Fatalf("res = %+v", res)
	}
	fields := res.ReplyFields()
	if fields[len(fields)-1].K != "snapshot" || fields[len(fields)-1].V != snapRun {
		t.Fatalf("reply %v", fields)
	}
}

// Without requireSnapshot a missing snapshot is a log line and the switch
// goes on; the reply then has no snapshot member at all.
func TestSetImageGoesOnWithoutASnapshotUnlessRequired(t *testing.T) {
	fx := newImageFixture(t)
	fx.m.SetSnapshotter(&switchSnapshots{fx: fx, note: "No snapshot before the version change: it did not finish in time"})
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Snapshot != "" {
		t.Fatalf("res = %+v", res)
	}
	for _, f := range res.ReplyFields() {
		if f.K == "snapshot" {
			t.Fatal("a reply without a snapshot names one")
		}
	}
}

// requireSnapshot refuses before anything is touched, whatever kept the
// snapshot from being taken.
func TestSetImageRequiringASnapshotLeavesTheAppUntouched(t *testing.T) {
	cases := map[string]func(fx *fixture){
		"no snapshotter": func(*fixture) {},
		"snapshots off": func(fx *fixture) {
			fx.m.SetSnapshotter(&switchSnapshots{fx: fx, none: "No snapshot before the version change: snapshots are turned off"})
		},
		"snapshot failed": func(fx *fixture) {
			fx.m.SetSnapshotter(&switchSnapshots{fx: fx, note: "No snapshot before the version change: it did not finish in time"})
		},
		"no time left": func(fx *fixture) {
			fx.m.SetSnapshotter(&switchSnapshots{fx: fx, run: snapRun})
			// The gate alone needs more than the whole budget.
			fx.m.imageStableFor = time.Hour
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newImageFixture(t)
			fx.m.set(float64(1), map[string]any{"status": "running"})
			setup(fx)
			res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6", RequireSnapshot: true})
			fx.waitIdle(t)
			if res.Success || res.Reason != ImageSnapshot || res.RolledBack || res.Snapshot != "" ||
				!strings.Contains(res.Message, "requires a snapshot") {
				t.Fatalf("res = %+v", res)
			}
			assertUntouched(t, fx)
			fx.dock.mu.Lock()
			tags := len(fx.dock.tags)
			fx.dock.mu.Unlock()
			if tags != 0 {
				t.Fatal("a refused change held the previous image")
			}
			var status any
			fx.cfg.View(func() { status = fx.m.getLocked(float64(1))["status"] })
			if status != "running" {
				t.Fatalf("status %v after a refusal", status)
			}
			if !fx.m.tryLockProcessing(1) {
				t.Fatal("a refused change kept the processing lock")
			}
			fx.m.unlockProcessing(1)
		})
	}
}

// A refusal after a re-pull of the record's own tag points the tag back
// at what runs: the next restart must not start the new image.
func TestSetImageSnapshotRefusalRestoresARePulledTag(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullResult["postgres:17.5"] = idNew
	fx.m.SetSnapshotter(&switchSnapshots{fx: fx, none: "No snapshot before the version change: snapshots are turned off"})
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.5", Pull: true, RequireSnapshot: true})
	if res.Reason != ImageSnapshot {
		t.Fatalf("res = %+v", res)
	}
	if fx.imageAt("postgres:17.5") != idOld {
		t.Fatalf("postgres:17.5 names %s after the refusal", fx.imageAt("postgres:17.5"))
	}
	assertUntouched(t, fx)
}

// The wait is what is left of the budget after the start, the health gate
// and a rollback have their share.
func TestSwitchSnapshotWaitIsCappedByTheBudget(t *testing.T) {
	fx := newImageFixture(t)
	snaps := &switchSnapshots{fx: fx, run: snapRun}
	fx.m.SetSnapshotter(snaps)
	fx.m.imageSetBudget = 12 * time.Minute
	fx.m.imageHealthTimeout = 2 * time.Minute
	fx.dock.imageHealth = map[string]bool{idNew: true}
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		if st.ImageID == idNew {
			st.Health = "healthy"
		}
		return st
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	_, limits := snaps.seen()
	// 12m - 1m (rollback) - 30s (start) - 2m (healthcheck) = 8m30s.
	want := 8*time.Minute + 30*time.Second
	if len(limits) != 1 || limits[0] > want || limits[0] < want-5*time.Second {
		t.Fatalf("limits %v, want about %s", limits, want)
	}
}

// A rollback snapshots while the current build serves, before its target
// is tagged, and a required snapshot it cannot get leaves it on that build.
func TestRollbackSnapshotsBeforeTheTargetIsTagged(t *testing.T) {
	fx := newRollbackFixture(t)
	snaps := &switchSnapshots{fx: fx, run: snapRun}
	fx.m.SetSnapshotter(snaps)
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b", RequireSnapshot: true})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Snapshot != snapRun {
		t.Fatalf("res = %+v", res)
	}
	events, _ := snaps.seen()
	if len(events) != 2 || events[0] != "start web version pulls=0 stopped=0 runs=0 tags=0" || events[1] != "wait pulls=0 stopped=0 runs=0 tags=0" {
		t.Fatalf("events %v", events)
	}
	if f := res.ReplyFields(); f[len(f)-1].K != "snapshot" {
		t.Fatalf("reply %v", f)
	}
}

func TestRollbackBlueGreenSnapshotsBeforeTheGreenStarts(t *testing.T) {
	fx := newRollbackFixture(t)
	fx.cfg.Mutate(func() {
		fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "web"}})
	})
	fx.setHTTPPorts(3000)
	snaps := &switchSnapshots{fx: fx, run: snapRun}
	fx.m.SetSnapshotter(snaps)
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
	fx.waitIdle(t)
	if !res.Success || res.Snapshot != snapRun {
		t.Fatalf("res = %+v", res)
	}
	events, _ := snaps.seen()
	if len(events) != 2 || !strings.HasSuffix(events[1], "runs=0 tags=0") {
		t.Fatalf("the green started before the snapshot: %v", events)
	}
}

func TestRollbackRequiringASnapshotLeavesTheBuild(t *testing.T) {
	fx := newRollbackFixture(t)
	fx.m.SetSnapshotter(&switchSnapshots{fx: fx, note: "No snapshot before the version change: there is not enough free disk space"})
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b", RequireSnapshot: true})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageSnapshot || res.RolledBack || res.Snapshot != "" || !strings.Contains(res.Message, "free disk space") {
		t.Fatalf("res = %+v", res)
	}
	fx.dock.mu.Lock()
	touched := len(fx.dock.stopped) + len(fx.dock.runCalls) + len(fx.dock.tags)
	fx.dock.mu.Unlock()
	if touched != 0 {
		t.Fatal("a refused rollback touched Docker")
	}
	assertStillOnC(t, fx)
}
