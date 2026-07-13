package updater

// Ported from test/server/Updater.test.js (the 11a9b00 spec) plus NEW Go-only
// spec tests for the v1.11.0 dynamic-container-identity delta (2189336 +
// a4c6285), which shipped with NO jest coverage — these tests are the only
// spec for it (see PLAN.md's 3.7 trap note).

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- startup self-heal (init) — jest describe('startup self-heal') ---

func TestSelfHealDoesNothingOnHealthyHost(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if got := fx.w.eventList(); len(got) != 0 {
		t.Fatalf("expected no events, got %v", got)
	}
}

func TestSelfHealPreservesOperatorAlwaysPolicy(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "always", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if got := fx.w.eventList(); len(got) != 0 {
		t.Fatalf("expected no events, got %v", got)
	}
}

func TestSelfHealRepairsNoPolicy(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "no", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSelfHealReclaimsIdentityFromBackup(t *testing.T) {
	// The exact state a host is left in when a crashed update's rollback
	// never got to the rename.
	fx := newFixture(t)
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"rename:odac-backup->odac", "policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSelfHealReclaimsIdentityFromUpdate(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac-update"] = &fakeContainer{policy: "no", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"rename:odac-update->odac", "policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSelfHealLeavesStaleExitedBackupAlone(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: false}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if got := fx.w.eventList(); len(got) != 0 {
		t.Fatalf("expected no events, got %v", got)
	}
}

func TestSelfHealDoesNothingOutsideContainer(t *testing.T) {
	fx := newFixture(t)
	fx.setDockerenv(false)
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if got := fx.w.eventList(); len(got) != 0 {
		t.Fatalf("expected no events, got %v", got)
	}
}

func TestSelfHealDoesNothingWhenIdentityUnresolvable(t *testing.T) {
	fx := newFixture(t) // no odac, no odac-backup, no odac-update anywhere
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if got := fx.w.eventList(); len(got) != 0 {
		t.Fatalf("expected no events, got %v", got)
	}
}

func TestSelfHealResolvesIdentityViaCgroup(t *testing.T) {
	fx := newFixture(t)
	fakeID := strings.Repeat("a", 64)
	fx.setProcFile("/proc/self/cgroup", "0::/system.slice/docker-"+fakeID+".scope\n")
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: true}
	fx.w.idIndex[fakeID] = "odac-backup"

	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"rename:odac-backup->odac", "policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// NEW (v1.11.0 spec): the hostname probe runs FIRST — a bridge-network
// container's default hostname is its 12-hex short id.
func TestSelfHealResolvesIdentityViaHostnameShortID(t *testing.T) {
	fx := newFixture(t)
	shortID := "abcdef012345"
	fx.setHostname(shortID)
	fx.w.world["odac-renamed"] = &fakeContainer{policy: "no", running: true}
	fx.w.idIndex[shortID] = "odac-renamed"

	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"rename:odac-renamed->odac", "policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// NEW (v1.11.0 spec): a hostname that matches the short-id pattern but
// inspects to nothing falls through silently to the remaining probes.
func TestResolveSelfNameHostnameMissFallsThrough(t *testing.T) {
	fx := newFixture(t)
	fx.setHostname("badc0ffee012") // 12-hex but unknown to the daemon
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: true}

	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	want := []string{"rename:odac-backup->odac", "policy:odac=unless-stopped"}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestInitPrintsSwitchLogsAfterCrashedUpdate(t *testing.T) {
	fx := newFixture(t)
	out := captureStdout(t)
	t.Setenv("ODAC_LOG_NAME", ".odac-update")
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out(), "ODAC_CMD:SWITCH_LOGS") {
		t.Fatal("expected the bare ODAC_CMD:SWITCH_LOGS marker on stdout")
	}
}

// --- takeOver (v1.11.0 spec: ODAC_PREVIOUS_CONTAINER_NAME branches) ---

func TestTakeOverRenamesResolvedPreviousName(t *testing.T) {
	fx := newFixture(t)
	t.Setenv("ODAC_PREVIOUS_CONTAINER_NAME", "my-odac")
	fx.w.world["my-odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	fx.w.world["odac-update"] = &fakeContainer{policy: "no", running: true}
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: false} // stale

	fx.u.takeOver()

	want := []string{
		"remove:odac-backup",
		"rename:my-odac->odac-backup",
		"policy:odac-backup=no",
		"rename:odac-update->odac",
		"policy:odac=unless-stopped",
	}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTakeOverWhenPreviousIsBackupRemovesLeftoverTarget(t *testing.T) {
	// Updating a rolled-back host: OLD already runs AS odac-backup. The
	// backup must NOT be removed (it is the live old instance); a leftover
	// container squatting the 'odac' name is force-removed instead.
	fx := newFixture(t)
	t.Setenv("ODAC_PREVIOUS_CONTAINER_NAME", "odac-backup")
	fx.w.world["odac-backup"] = &fakeContainer{policy: "unless-stopped", running: true}
	fx.w.world["odac-update"] = &fakeContainer{policy: "no", running: true}
	fx.w.world["odac"] = &fakeContainer{policy: "no", running: false} // leftover

	fx.u.takeOver()

	want := []string{
		"remove:odac",
		"policy:odac-backup=no",
		"rename:odac-update->odac",
		"policy:odac=unless-stopped",
	}
	if got := fx.w.eventList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if !fx.w.has("odac-backup") {
		t.Fatal("the live old instance (odac-backup) must never be removed")
	}
}

func TestTakeOverRenameFailureForceRemovesPreviousName(t *testing.T) {
	// The backup name is somehow still taken when the rename runs — Node
	// force-removes the previous container to clear the name.
	fx := newFixture(t)
	t.Setenv("ODAC_PREVIOUS_CONTAINER_NAME", "odac")
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	fx.w.world["odac-update"] = &fakeContainer{policy: "no", running: true}

	// Re-create the backup right after the remove step so the rename hits
	// "name already in use".
	fx.w.onEvent = func(event string) {
		if event == "remove:odac-backup" {
			fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: false}
		}
	}
	fx.w.world["odac-backup"] = &fakeContainer{policy: "no", running: false}

	fx.u.takeOver()

	if !fx.w.hasEvent("remove:odac") {
		t.Fatalf("expected the failed rename to force-remove the previous container; events = %v", fx.w.eventList())
	}
	if got := fx.w.container("odac"); got == nil || got.policy != "unless-stopped" {
		t.Fatalf("odac-update must still claim the name with the persistent policy; got %+v", got)
	}
}

// --- Start() result strings (lifecycle.md CLI-visible parity) ---

func TestStartBlocksWhenAlreadyUpdating(t *testing.T) {
	fx := newFixture(t)
	fx.u.setUpdating(true)
	r, err := fx.u.Start()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != false || r.Message != "Update already in progress" {
		t.Fatalf("got %+v", r)
	}
}

func TestStartUpToDate(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true, image: "sha256:same"}
	fx.w.imageIDs["odacrun/odac:latest"] = "sha256:same"

	r, err := fx.u.Start()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != true || r.Message != "System is up to date" {
		t.Fatalf("got %+v", r)
	}
	found := false
	for _, p := range fx.w.pulled {
		if p == "odacrun/odac:latest" {
			found = true
		}
	}
	if !found {
		t.Fatal("check must pull the image before comparing IDs")
	}
	// The latch must be released for the next attempt.
	if r2, _ := fx.u.Start(); r2.Message == "Update already in progress" {
		t.Fatal("updating latch leaked after an up-to-date check")
	}
}

// NEW (v1.11.0 spec): the local image ID comes from the RESOLVED self
// container's .Image, not the literal 'odac'.
func TestStartComparesResolvedSelfImageID(t *testing.T) {
	fx := newFixture(t)
	shortID := "0123456789ab"
	fx.setHostname(shortID)
	fx.w.world["my-odac"] = &fakeContainer{policy: "unless-stopped", running: true, image: "sha256:current"}
	fx.w.idIndex[shortID] = "my-odac"
	fx.w.imageIDs["odacrun/odac:latest"] = "sha256:current"

	r, err := fx.u.Start()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != true || r.Message != "System is up to date" {
		t.Fatalf("expected the resolved container's image ID to match; got %+v", r)
	}
}

func TestStartPullFailureClearsLatch(t *testing.T) {
	// Deviation from Node (documented): a throwing availability check left
	// #updating latched forever; the Go port releases it.
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true, image: "sha256:x"}
	fx.w.pullErr["odacrun/odac:latest"] = errors.New("pull access denied")

	if _, err := fx.u.Start(); err == nil {
		t.Fatal("expected the pull failure to surface as an error")
	}
	// The retry must reach the pull again instead of answering the
	// 'Update already in progress' guard.
	if _, err := fx.u.Start(); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("retry err = %v, want the pull failure again", err)
	}
}

func TestStartMissingImageIDsMeansUpToDate(t *testing.T) {
	// Node: unresolvable IDs answer "no update" (result true, up to date).
	fx := newFixture(t) // no containers, no image IDs — both lookups fail
	r, err := fx.u.Start()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != true || r.Message != "System is up to date" {
		t.Fatalf("got %+v", r)
	}
}

// --- non-Linux container-swap strategy (v1.11.0 spec: resolved name) ---

func TestNonLinuxSwapUsesResolvedSelfName(t *testing.T) {
	fx := newFixture(t)
	fx.u.platform = "darwin"
	shortID := "aabbccddeeff"
	fx.setHostname(shortID)
	fx.w.world["my-odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		env:   []string{"PATH=/usr/bin"},
		binds: []string{"/host/.odac:/app/.odac"},
	}
	fx.w.idIndex[shortID] = "my-odac"

	if err := fx.u.execute(); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-fx.exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	default:
		t.Fatal("the swap strategy must exit(0) after spawning the runner")
	}

	// The stopped update container exists with the production policy.
	if c := fx.w.container("odac-update"); c == nil || c.policy != "unless-stopped" || c.running {
		t.Fatalf("odac-update = %+v, want stopped with unless-stopped", c)
	}

	// The runner is an anonymous docker:cli container whose swap command
	// stops/removes the RESOLVED name but renames/starts the literal odac.
	runner := fx.w.container("anon1")
	if runner == nil {
		t.Fatal("runner container was not created")
	}
	if !fx.w.hasEvent("start:anon1") {
		t.Fatal("runner container was not started")
	}
	// The command travels via CreateOptions.Cmd, which the fake does not
	// store — assert through the onCreate hook instead.
}

func TestNonLinuxSwapRunnerCommand(t *testing.T) {
	fx := newFixture(t)
	fx.u.platform = "darwin"
	shortID := "aabbccddeeff"
	fx.setHostname(shortID)
	fx.w.world["my-odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	fx.w.idIndex[shortID] = "my-odac"

	var runnerCmd string
	fx.w.onCreate = func(name string, opts CreateOptions) {
		if opts.Image == "docker:cli" {
			runnerCmd = strings.Join(opts.Cmd, " ")
		}
	}

	if err := fx.u.execute(); err != nil {
		t.Fatal(err)
	}

	want := "sleep 5 && docker stop my-odac && docker rm my-odac && docker rename odac-update odac && docker start odac"
	if !strings.Contains(runnerCmd, want) {
		t.Fatalf("runner cmd = %q, want it to contain %q", runnerCmd, want)
	}
}

// --- build-from-source (beta channel) ---

func TestBuildFromSourceFailsWithoutHostBind(t *testing.T) {
	fx := newFixture(t)
	t.Setenv("ODAC_CHANNEL", "beta")
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true} // no binds

	err := fx.u.download()
	if err == nil || !strings.Contains(err.Error(), "Build failed: Could not determine host path for storage volume") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildFromSourceCloneAndBuild(t *testing.T) {
	fx := newFixture(t)
	t.Setenv("ODAC_CHANNEL", "beta")
	fx.w.world["odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		binds: []string{"/host/.odac:" + fx.u.baseDir},
	}

	var sidecarCmd, buildCmd []string
	fx.w.onCreate = func(name string, opts CreateOptions) {
		switch opts.Image {
		case "alpine/git":
			sidecarCmd = opts.Cmd
			// Simulate the clone the sidecar would perform on the shared
			// volume.
			dir := fx.u.downloadPath()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
				t.Error(err)
			}
		case "docker:cli":
			buildCmd = opts.Cmd
		}
	}

	if err := fx.u.download(); err != nil {
		t.Fatal(err)
	}

	wantClone := []string{"clone", "-b", "dev", "--depth", "1", "https://github.com/odac-run/odac.git", "/git_target/tmp/odac_source"}
	if !reflect.DeepEqual(sidecarCmd, wantClone) {
		t.Fatalf("sidecar cmd = %v, want %v", sidecarCmd, wantClone)
	}
	joined := strings.Join(buildCmd, " ")
	if !strings.Contains(joined, "docker build -t odacrun/odac:latest /git_target/tmp/odac_source") {
		t.Fatalf("build cmd = %q", joined)
	}
	// Source dir cleaned up after the build.
	if _, err := os.Stat(fx.u.downloadPath()); err == nil {
		t.Fatal("download path must be removed after a successful build")
	}
}
