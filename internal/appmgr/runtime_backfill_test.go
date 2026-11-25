package appmgr

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"odac/internal/applog"
	"odac/internal/docker"
)

func (f *fakeDocker) ObservedRuntime(sourceDir, imageID string) (docker.Runtime, error) {
	f.mu.Lock()
	f.observedCalls = append(f.observedCalls, [2]string{sourceDir, imageID})
	hook := f.observedHook
	rt, err := f.observed, f.observedErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return rt, err
}

func (f *fakeDocker) CheckoutHead(targetDir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls = append(f.headCalls, targetDir)
	return f.checkoutHead, f.headErr
}

func (f *fakeDocker) observedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.observedCalls)
}

// backfillApp is a running git app as an agent from before runtime
// reporting left it: no runtime, and no commit unless extra adds one.
func backfillApp(extra map[string]any) map[string]any {
	app := map[string]any{
		"id": float64(1), "name": "web", "type": "git", "image": "odac-app-web",
		"active": true, "status": "running",
	}
	for k, v := range extra {
		app[k] = v
	}
	return app
}

// newBackfillFixture seeds one app with a checkout, a running container on
// imageID, and (when lastBuild is not "") a build history ending in it.
func newBackfillFixture(t *testing.T, app map[string]any, lastBuild string) *fixture {
	t.Helper()
	fx := newFixture(t, []any{app})
	name, _ := app["name"].(string)
	if err := os.MkdirAll(filepath.Join(fx.m.appsPath(), name), 0o755); err != nil {
		t.Fatal(err)
	}
	switch lastBuild {
	case "":
	case "pending":
		writeBuild(t, fx, name, "build_1700000000000", "success")
		writeBuild(t, fx, name, "build_1700000000001", "pending")
	default:
		writeBuild(t, fx, name, "build_1700000000000", lastBuild)
	}
	fx.dock.mu.Lock()
	fx.dock.running[name] = true
	fx.dock.status[name] = docker.Status{Running: true, ImageID: imageID}
	fx.dock.observed = docker.Runtime{Strategy: "node-npm", Version: "20.18.1"}
	fx.dock.mu.Unlock()
	return fx
}

// writeBuild records one build attempt; "pending" leaves it unfinalized,
// as a crash mid-build does.
func writeBuild(t *testing.T, fx *fixture, name, id, status string) {
	t.Helper()
	logger := applog.New(fx.m.logsRoot, name)
	if err := logger.Init(); err != nil {
		t.Fatal(err)
	}
	ctrl, err := logger.NewBuildStream(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		ctrl.Finalize(status == "success")
	}
}

func (fx *fixture) pulse(t *testing.T) {
	t.Helper()
	fx.m.Check()
	fx.waitIdle(t)
}

func TestRuntimeBackfillWritesWhatTheHostShows(t *testing.T) {
	fx := newBackfillFixture(t, backfillApp(nil), "success")
	fx.pulse(t)

	want := map[string]any{"strategy": "node-npm", "version": "20.18.1"}
	if got := fx.app(0)["runtime"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime = %v, want %v", got, want)
	}
	call := fx.dock.observedCalls[0]
	if call != [2]string{filepath.Join(fx.m.appsPath(), "web"), imageID} {
		t.Errorf("observed from %v", call)
	}
	if !fx.hub.sawTrigger("app.list") {
		t.Error("app.list not triggered after the backfill")
	}
	if row := fx.listRow(t, "web"); !reflect.DeepEqual(row["runtime"], want) {
		t.Errorf("app.list runtime = %v", row["runtime"])
	}
	if len(fx.dock.headCalls) != 0 {
		t.Errorf("HEAD probed with no recorded commit: %v", fx.dock.headCalls)
	}
}

func TestRuntimeBackfillNeverTouchesAnExistingRuntime(t *testing.T) {
	stored := map[string]any{"strategy": "python", "image": "python:3.12-slim", "version": "3.12.4", "source": "default"}
	fx := newBackfillFixture(t, backfillApp(map[string]any{"runtime": stored}), "success")
	fx.pulse(t)

	if n := fx.dock.observedCount(); n != 0 {
		t.Errorf("probed an app that has a runtime (%d calls)", n)
	}
	if got := fx.app(0)["runtime"]; !reflect.DeepEqual(got, stored) {
		t.Errorf("runtime = %v", got)
	}
}

func TestRuntimeBackfillChecksTheRecordedCommit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  string
		err   error
		wrote bool
	}{
		{"head matches", builtSHA, nil, true},
		{"checkout moved past the image", oldSHA, nil, false},
		{"head unknown", "", nil, false},
		{"sandbox failed", "", errors.New("no git image"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No build history: the commit alone decides.
			fx := newBackfillFixture(t, backfillApp(map[string]any{"commitSha": builtSHA}), "")
			fx.dock.mu.Lock()
			fx.dock.checkoutHead, fx.dock.headErr = tc.head, tc.err
			fx.dock.mu.Unlock()
			fx.pulse(t)

			_, wrote := fx.app(0)["runtime"]
			if wrote != tc.wrote {
				t.Errorf("wrote = %v, want %v", wrote, tc.wrote)
			}
			if len(fx.dock.headCalls) != 1 {
				t.Errorf("head calls = %v", fx.dock.headCalls)
			}
		})
	}
}

func TestRuntimeBackfillSkipsWhenTheCheckoutMayHaveMoved(t *testing.T) {
	for _, last := range []string{"failed", "pending", ""} {
		t.Run("last build "+last, func(t *testing.T) {
			fx := newBackfillFixture(t, backfillApp(nil), last)
			fx.pulse(t)
			if n := fx.dock.observedCount(); n != 0 {
				t.Errorf("probed with last build %q", last)
			}
			if _, ok := fx.app(0)["runtime"]; ok {
				t.Errorf("runtime written: %v", fx.app(0)["runtime"])
			}
		})
	}
}

func TestRuntimeBackfillNeedsContainerImageAndCheckout(t *testing.T) {
	t.Run("no image", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		fx.dock.mu.Lock()
		fx.dock.status["web"] = docker.Status{Running: true}
		fx.dock.mu.Unlock()
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; ok || fx.dock.observedCount() != 0 {
			t.Errorf("runtime = %v", fx.app(0)["runtime"])
		}
	})
	t.Run("no checkout", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		if err := os.RemoveAll(filepath.Join(fx.m.appsPath(), "web")); err != nil {
			t.Fatal(err)
		}
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; ok || fx.dock.observedCount() != 0 {
			t.Errorf("runtime = %v", fx.app(0)["runtime"])
		}
	})
	t.Run("not running", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(map[string]any{"status": "stopped"}), "success")
		fx.dock.mu.Lock()
		fx.dock.running["web"] = false
		fx.dock.mu.Unlock()
		fx.pulse(t)
		if fx.dock.observedCount() != 0 {
			t.Error("probed a stopped app")
		}
		// Not counted as an attempt: it is tried once it runs.
		fx.dock.mu.Lock()
		fx.dock.running["web"] = true
		fx.dock.mu.Unlock()
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; !ok {
			t.Error("not backfilled once running")
		}
	})
	t.Run("detection fails", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		fx.dock.mu.Lock()
		fx.dock.observed, fx.dock.observedErr = docker.Runtime{}, errors.New("no strategy")
		fx.dock.mu.Unlock()
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; ok {
			t.Errorf("runtime = %v", fx.app(0)["runtime"])
		}
	})
}

func TestRuntimeBackfillSkipsDevApps(t *testing.T) {
	fx := newBackfillFixture(t, backfillApp(map[string]any{"dev": true}), "success")
	fx.pulse(t)
	if fx.dock.observedCount() != 0 {
		t.Error("probed a dev app")
	}
}

func TestRuntimeBackfillRunsOncePerProcess(t *testing.T) {
	fx := newBackfillFixture(t, backfillApp(nil), "success")
	fx.dock.mu.Lock()
	fx.dock.observedErr = errors.New("no strategy")
	fx.dock.mu.Unlock()
	for range 5 {
		fx.pulse(t)
	}
	if n := fx.dock.observedCount(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	// Even a later success is not retried: the next build fills it.
	fx.dock.mu.Lock()
	fx.dock.observedErr = nil
	fx.dock.mu.Unlock()
	fx.pulse(t)
	if n := fx.dock.observedCount(); n != 1 {
		t.Errorf("attempts = %d after a later pulse", n)
	}
}

// TestRuntimeBackfillYieldsToOperations pins the race rule: an operation
// claiming the app while the backfill probes, or a build writing the field,
// wins, and the backfill never takes the lock a redeploy needs.
func TestRuntimeBackfillYieldsToOperations(t *testing.T) {
	t.Run("operation ran meanwhile", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		fx.dock.observedHook = func() {
			if !fx.m.tryLockProcessing(1) {
				t.Error("backfill holds the processing lock")
			}
			fx.m.unlockProcessing(1)
		}
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; ok {
			t.Errorf("runtime = %v", fx.app(0)["runtime"])
		}
	})
	t.Run("operation in flight at write", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		fx.dock.observedHook = func() { fx.m.tryLockProcessing(1) }
		fx.pulse(t)
		fx.m.unlockProcessing(1)
		if _, ok := fx.app(0)["runtime"]; ok {
			t.Errorf("runtime = %v", fx.app(0)["runtime"])
		}
	})
	t.Run("build wrote the field", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		built := map[string]any{"strategy": "bun", "image": "oven/bun:alpine", "source": "default"}
		fx.dock.observedHook = func() { fx.m.set(float64(1), map[string]any{"runtime": built}) }
		fx.pulse(t)
		if got := fx.app(0)["runtime"]; !reflect.DeepEqual(got, built) {
			t.Errorf("runtime = %v, want the build's %v", got, built)
		}
	})
	t.Run("busy app is not queued", func(t *testing.T) {
		fx := newBackfillFixture(t, backfillApp(nil), "success")
		fx.m.tryLockProcessing(1)
		fx.pulse(t)
		fx.m.unlockProcessing(1)
		if fx.dock.observedCount() != 0 {
			t.Error("probed an app an operation holds")
		}
		fx.pulse(t)
		if _, ok := fx.app(0)["runtime"]; !ok {
			t.Error("not backfilled once the operation ended")
		}
	})
}

func TestLastAttemptStatusSeesUnfinishedBuilds(t *testing.T) {
	fx := newFixture(t, nil)
	logger := applog.New(fx.m.logsRoot, "x")
	if got := logger.LastAttemptStatus(); got != "" {
		t.Errorf("no history = %q", got)
	}
	writeBuild(t, fx, "x", "build_1700000000000", "success")
	if got := logger.LastAttemptStatus(); got != "success" {
		t.Errorf("finished = %q", got)
	}
	writeBuild(t, fx, "x", "build_1700000000001", "pending")
	if got := logger.LastAttemptStatus(); got != "pending" {
		t.Errorf("unfinished = %q", got)
	}
	writeBuild(t, fx, "x", "build_1700000000002", "failed")
	if got := logger.LastAttemptStatus(); got != "failed" {
		t.Errorf("failed = %q", got)
	}
}
