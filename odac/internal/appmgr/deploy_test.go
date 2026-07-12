package appmgr

// Deploy.js spec tests. Node covers ~14% of Deploy.js, so these pin the
// behavior from the line-by-line read (PLAN 3.4 trap note): green readiness
// gating, the switch order, rename retries, artifact sweeps.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/docker"
)

// gitAppWithDomain builds a fixture whose single git app has a routed domain
// (the ZDD gate).
func gitAppWithDomain(t *testing.T) *fixture {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "git", "image": "odac-app-web",
		"url":   "https://github.com/a/b.git",
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000)}},
	}})
	fx.cfg.Mutate(func() {
		fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "web"}})
	})
	fx.setHTTPPorts(3000)
	return fx
}

func TestBlueGreenRestartSwitchesContainers(t *testing.T) {
	fx := gitAppWithDomain(t)

	r := fx.m.Restart(float64(1))
	if !r.Status {
		t.Fatalf("restart failed: %v", r.Message)
	}
	if !strings.Contains(jsString(r.Message), "zero-downtime") {
		t.Fatalf("message = %q", jsString(r.Message))
	}
	fx.waitIdle(t)

	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()

	// Green container ran under a temporary name.
	if len(fx.dock.runCalls) != 1 {
		t.Fatalf("runApp calls = %d", len(fx.dock.runCalls))
	}
	greenName := fx.dock.runCalls[0].name
	if !strings.HasPrefix(greenName, "web-green-") || !greenSuffix.MatchString(greenName) {
		t.Fatalf("green name = %q", greenName)
	}

	// The blue container was stopped+removed, green renamed into its place.
	if len(fx.dock.renames) != 1 || fx.dock.renames[0] != [2]string{greenName, "web"} {
		t.Fatalf("renames = %v", fx.dock.renames)
	}
	stoppedWeb := false
	for _, s := range fx.dock.stopped {
		if s == "web" {
			stoppedWeb = true
		}
	}
	if !stoppedWeb {
		t.Fatalf("blue container not stopped: %v", fx.dock.stopped)
	}

	// activeContainerId cleared after the successful rename.
	if acid, present := fx.app(0)["activeContainerId"]; !present || acid != nil {
		t.Fatalf("activeContainerId = %v (present=%v)", acid, present)
	}
}

func TestBlueGreenAbortsWhenGreenNeverListens(t *testing.T) {
	fx := gitAppWithDomain(t)
	// Only the blue app's name gets listening ports; the green container
	// never binds -> readiness must fail and the green gets cleaned up.
	fx.dock.setListening("web", []int{3000})

	r := fx.m.Restart(float64(1))
	if r.Status {
		t.Fatal("expected failure")
	}
	if !strings.Contains(jsString(r.Message), "Failed to restart app web") {
		t.Fatalf("message = %q", jsString(r.Message))
	}
	fx.waitIdle(t)

	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	greenName := fx.dock.runCalls[0].name
	removedGreen := false
	for _, rm := range fx.dock.removed {
		if rm == greenName {
			removedGreen = true
		}
	}
	if !removedGreen {
		t.Fatalf("green not removed: %v", fx.dock.removed)
	}
	// The blue container must NOT have been stopped — uptime maintained.
	for _, s := range fx.dock.stopped {
		if s == "web" {
			t.Fatalf("blue container stopped on aborted deploy: %v", fx.dock.stopped)
		}
	}
	if len(fx.dock.renames) != 0 {
		t.Fatalf("unexpected rename: %v", fx.dock.renames)
	}
}

func TestBlueGreenAbortsWhenGreenFailsHTTPProbe(t *testing.T) {
	fx := gitAppWithDomain(t)
	fx.setHTTPPorts() // TCP listens (default), but nothing answers HTTP

	r := fx.m.Restart(float64(1))
	if r.Status {
		t.Fatal("expected failure")
	}
	fx.waitIdle(t)

	// Status marked errored (Restart's catch).
	// Note: status is ephemeral, but the working set keeps it until the
	// next Check reload — read it through the manager, not the store.
	fx.cfg.View(func() {
		app := fx.m.getLocked(float64(1))
		if app["status"] != "errored" {
			t.Fatalf("status = %v", app["status"])
		}
	})
}

func TestBlueGreenRenameFailureKeepsActiveContainerID(t *testing.T) {
	fx := gitAppWithDomain(t)
	fx.dock.mu.Lock()
	fx.dock.renameErr = errTest
	fx.dock.mu.Unlock()

	r := fx.m.Restart(float64(1))
	// The deploy itself still succeeds — ZDD persists with the green name.
	if !r.Status {
		t.Fatalf("restart failed: %v", r.Message)
	}
	fx.waitIdle(t)

	acid, _ := fx.app(0)["activeContainerId"].(string)
	if !strings.HasPrefix(acid, "web-green-") {
		t.Fatalf("activeContainerId = %q", acid)
	}
}

func TestStandardRestartForAppWithoutDomains(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "plain", "type": "container", "image": "app", "active": true,
	}})

	r := fx.m.Restart(float64(1))
	if !r.Status {
		t.Fatalf("restart failed: %v", r.Message)
	}
	if strings.Contains(jsString(r.Message), "zero-downtime") {
		t.Fatalf("standard restart took the ZDD path: %q", jsString(r.Message))
	}
	fx.waitIdle(t)

	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	// Stop first, then a fresh run under the app's own name.
	if len(fx.dock.stopped) == 0 || fx.dock.stopped[0] != "plain" {
		t.Fatalf("stopped = %v", fx.dock.stopped)
	}
	if len(fx.dock.runCalls) != 1 || fx.dock.runCalls[0].name != "plain" {
		t.Fatalf("runCalls = %v", fx.dock.runCalls)
	}
}

func TestRestartRejectsConcurrentProcessing(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "busy", "type": "container", "image": "app",
	}})
	fx.m.mu.Lock()
	fx.m.processing[1] = true
	fx.m.mu.Unlock()

	r := fx.m.Restart(float64(1))
	if r.Status || !strings.Contains(jsString(r.Message), "already being processed") {
		t.Fatalf("r = %+v", r)
	}
}

func TestDeleteSweepsGreenCompanions(t *testing.T) {
	green := "web-green-1751234567890_abcd1234"
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "git",
		"activeContainerId": green,
	}})
	// A second ghost green is still visible to Docker.
	ghost := "web-green-1751234567891_deadbeef"
	fx.dock.mu.Lock()
	fx.dock.containers = []docker.ContainerInfo{{Names: []string{"/" + ghost}}}
	fx.dock.mu.Unlock()

	r := fx.m.Delete(float64(1), true)
	if !r.Status {
		t.Fatalf("delete failed: %v", r.Message)
	}

	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	removed := map[string]bool{}
	for _, rm := range fx.dock.removed {
		removed[rm] = true
	}
	if !removed["web"] || !removed[green] || !removed[ghost] {
		t.Fatalf("removed = %v", fx.dock.removed)
	}
}

func TestCleanupStaleGreenLogsSweepsOrphans(t *testing.T) {
	fx := newFixture(t, []any{})
	orphan := filepath.Join(fx.m.logsRoot, "old-green-1751234567890_abcd1234")
	keeper := filepath.Join(fx.m.logsRoot, "normal-app")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keeper, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := fx.m.CleanupStaleGreenLogs(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan green log dir survived")
	}
	if _, err := os.Stat(keeper); err != nil {
		t.Fatal("normal app log dir was swept")
	}
}

func TestRedeployWithDomainsUsesBlueGreen(t *testing.T) {
	fx := gitAppWithDomain(t)

	r := fx.m.Redeploy(RedeployPayload{Container: "web"})
	if !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)

	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	// Fresh clone (no .git in the temp dir) + build + green run + rename.
	if len(fx.dock.cloneCalls) != 1 {
		t.Fatalf("cloneCalls = %v", fx.dock.cloneCalls)
	}
	if len(fx.dock.buildCalls) != 1 || fx.dock.buildCalls[0] != "odac-app-web" {
		t.Fatalf("buildCalls = %v", fx.dock.buildCalls)
	}
	if len(fx.dock.renames) != 1 || fx.dock.renames[0][1] != "web" {
		t.Fatalf("renames = %v", fx.dock.renames)
	}
	// Cache purged for the app after the switch.
	fx.proxy.mu.Lock()
	defer fx.proxy.mu.Unlock()
	if len(fx.proxy.purges) == 0 {
		t.Fatal("proxy cache not purged")
	}
}

func TestStopEndsAppAndLogStream(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
	}})
	fx.checkAndSettle(t) // start it (attaches a log stream)

	r := fx.m.Stop(float64(1))
	if !r.Status {
		t.Fatalf("stop failed: %v", r.Message)
	}

	app := fx.app(0)
	if app["active"] != false {
		t.Fatalf("active = %v", app["active"])
	}
	fx.m.mu.Lock()
	_, hasStream := fx.m.logStreams["web"]
	fx.m.mu.Unlock()
	if hasStream {
		t.Fatal("log stream not cleaned up")
	}

	// Second stop: the working set still holds the ephemeral status.
	r2 := fx.m.Stop(float64(1))
	if !r2.Status || !strings.Contains(jsString(r2.Message), "already stopped") {
		t.Fatalf("r2 = %+v", r2)
	}
}
