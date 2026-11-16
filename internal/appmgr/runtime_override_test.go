package appmgr

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"odac/internal/api"
	"odac/internal/docker"
)

func TestParseRuntimeOverride(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		version string
		set     bool
		bad     bool
	}{
		{"absent keeps", nil, false, "", false, false},
		{"null clears", nil, true, "", true, false},
		{"null version clears", map[string]any{"version": nil}, true, "", true, false},
		{"empty version clears", map[string]any{"version": ""}, true, "", true, false},
		{"no version member clears", map[string]any{}, true, "", true, false},
		{"node major", map[string]any{"version": "22"}, true, "22", true, false},
		{"python minor", map[string]any{"version": "3.12"}, true, "3.12", true, false},
		{"number refused", map[string]any{"version": float64(22)}, true, "", true, true},
		{"bare string refused", "22", true, "", true, true},
		{"list refused", []any{"22"}, true, "", true, true},
		{"alias refused", map[string]any{"version": "lts"}, true, "", true, true},
		{"patch refused", map[string]any{"version": "22.1.1"}, true, "", true, true},
		{"injection refused", map[string]any{"version": "22;id"}, true, "", true, true},
		{"padded refused", map[string]any{"version": " 22"}, true, "", true, true},
	}
	for _, c := range cases {
		version, set, err := parseRuntimeOverride(c.v, c.present)
		if c.bad != errors.Is(err, docker.ErrUnsupportedRuntimeVersion) || version != c.version || set != c.set {
			t.Errorf("%s: (%q, %v, %v), want (%q, %v, bad=%v)", c.name, version, set, err, c.version, c.set, c.bad)
		}
	}
}

func (fx *fixture) buildOverrides() []string {
	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	return slices.Clone(fx.dock.buildOverrides)
}

// TestRedeployRuntimeOverrideSemantics walks the three payload shapes
// through one app: a version is stored and built, an absent member reuses
// the stored one, a null one clears it.
func TestRedeployRuntimeOverrideSemantics(t *testing.T) {
	fx := newFixture(t, []any{gitApp(nil)})
	redeploy := func(p RedeployPayload) {
		t.Helper()
		p.Container = "app"
		if r := fx.m.Redeploy(p); !r.Status {
			t.Fatalf("redeploy failed: %v", r.Message)
		}
		fx.waitIdle(t)
	}

	redeploy(RedeployPayload{HasRuntime: true, Runtime: map[string]any{"version": "22"}})
	if got := fx.findApp("app")[runtimeOverrideKey]; got != "22" {
		t.Fatalf("stored override = %v", got)
	}
	redeploy(RedeployPayload{})
	if got := fx.findApp("app")[runtimeOverrideKey]; got != "22" {
		t.Fatalf("absent member dropped the override: %v", got)
	}
	redeploy(RedeployPayload{HasRuntime: true, Runtime: nil})
	if _, ok := fx.findApp("app")[runtimeOverrideKey]; ok {
		t.Fatalf("null member kept the override: %v", fx.findApp("app"))
	}
	redeploy(RedeployPayload{HasRuntime: true, Runtime: map[string]any{"version": "3.12"}})
	redeploy(RedeployPayload{HasRuntime: true, Runtime: map[string]any{"version": ""}})

	if got, want := fx.buildOverrides(), []string{"22", "22", "", "3.12", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("builds got %v, want %v", got, want)
	}
	if slices.Contains(ephemeralFields, runtimeOverrideKey) {
		t.Error("the override must survive a restart")
	}
}

// TestRedeployRefusesMalformedOverride pins "immediately": nothing is
// fetched, built or stopped, and the stored override is not touched.
func TestRedeployRefusesMalformedOverride(t *testing.T) {
	for _, rt := range []any{map[string]any{"version": "22;id"}, map[string]any{"version": float64(22)}, "22"} {
		fx := newFixture(t, []any{gitApp(map[string]any{runtimeOverrideKey: "20"})})
		fx.dock.mu.Lock()
		fx.dock.running["app"] = true
		fx.dock.mu.Unlock()

		r := fx.m.Redeploy(RedeployPayload{Container: "app", HasRuntime: true, Runtime: rt})
		if r.Status || r.Message != "unsupported runtime version" {
			t.Errorf("%v: r = %+v", rt, r)
		}
		fx.dock.mu.Lock()
		calls := len(fx.dock.cloneCalls) + len(fx.dock.fetchCalls) + len(fx.dock.buildCalls) + len(fx.dock.stopped)
		fx.dock.mu.Unlock()
		if calls != 0 {
			t.Errorf("%v: the redeploy did work before refusing", rt)
		}
		if got := fx.findApp("app")[runtimeOverrideKey]; got != "20" {
			t.Errorf("%v: stored override = %v", rt, got)
		}
	}
}

func TestRedeployRefusesHandEditedOverride(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{runtimeOverrideKey: float64(22)})})
	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); r.Status || r.Message != "unsupported runtime version" {
		t.Fatalf("r = %+v", r)
	}
	// A payload that decides the override replaces the broken value.
	if r := fx.m.Redeploy(RedeployPayload{Container: "app", HasRuntime: true}); !r.Status {
		t.Fatalf("clearing redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	if _, ok := fx.findApp("app")[runtimeOverrideKey]; ok {
		t.Error("broken override survived a clear")
	}
}

// TestRedeployBuildRefusalKeepsServing covers the builder's own refusal (a
// version the detected language cannot take) and any build failure: the
// running container is never stopped, and the override stays stored because
// it is the operator's choice, not the build's outcome.
func TestRedeployBuildRefusalKeepsServing(t *testing.T) {
	for _, buildErr := range []error{docker.ErrUnsupportedRuntimeVersion, errors.New("manifest for node:99-alpine not found")} {
		fx := newFixture(t, []any{gitApp(map[string]any{"active": true})})
		fx.dock.mu.Lock()
		fx.dock.running["app"] = true
		fx.dock.buildErr = buildErr
		fx.dock.mu.Unlock()

		r := fx.m.Redeploy(RedeployPayload{Container: "app", HasRuntime: true, Runtime: map[string]any{"version": "99"}})
		if r.Status {
			t.Fatalf("%v: redeploy succeeded", buildErr)
		}
		if errors.Is(buildErr, docker.ErrUnsupportedRuntimeVersion) && r.Message != "unsupported runtime version" {
			t.Errorf("message = %v", r.Message)
		}
		fx.dock.mu.Lock()
		stopped := slices.Clone(fx.dock.stopped)
		fx.dock.mu.Unlock()
		if len(stopped) != 0 {
			t.Errorf("%v: stopped %v on a failed build", buildErr, stopped)
		}
		if got := fx.findApp("app")[runtimeOverrideKey]; got != "99" {
			t.Errorf("%v: stored override = %v", buildErr, got)
		}
	}
}

func TestListOmitsRuntimeOverride(t *testing.T) {
	reported := map[string]any{"strategy": "node-npm", "image": "node:22-alpine", "version": "22.11.0", "source": "override"}
	fx := newFixture(t, []any{gitApp(map[string]any{runtimeOverrideKey: "22", "runtime": reported})})
	row := fx.listRow(t, "app")
	if _, ok := row[runtimeOverrideKey]; ok {
		t.Errorf("app.list carries %s", runtimeOverrideKey)
	}
	if !reflect.DeepEqual(row["runtime"], reported) {
		t.Errorf("runtime = %v", row["runtime"])
	}
}

func TestRedeployPersistsRuntimeSource(t *testing.T) {
	fx := newFixture(t, []any{gitApp(nil)})
	fx.dock.mu.Lock()
	fx.dock.buildRuntime = docker.Runtime{Strategy: "python", Image: "python:3.12-slim", Version: "3.12.7", Source: ".python-version"}
	fx.dock.mu.Unlock()
	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	want := map[string]any{"strategy": "python", "image": "python:3.12-slim", "version": "3.12.7", "source": ".python-version"}
	if got := fx.findApp("app")["runtime"]; !reflect.DeepEqual(got, want) {
		t.Errorf("persisted runtime = %v", got)
	}
	build, _ := fx.listRow(t, "app")["build"].(map[string]any)
	if !reflect.DeepEqual(build["runtime"], want) {
		t.Errorf("build.runtime = %v", build["runtime"])
	}
}

func TestCreateFromGitRuntimeOverride(t *testing.T) {
	t.Run("stored and built", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(api.Operator, map[string]any{
			"type": "git", "url": "https://github.com/u/r.git", "name": "web",
			"runtime": map[string]any{"version": "20"},
		})
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)
		if got := fx.findApp("web")[runtimeOverrideKey]; got != "20" {
			t.Errorf("stored override = %v", got)
		}
		if got := fx.buildOverrides(); !reflect.DeepEqual(got, []string{"20"}) {
			t.Errorf("builds got %v", got)
		}
	})
	t.Run("null stores nothing", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(api.Operator, map[string]any{
			"type": "git", "url": "https://github.com/u/r.git", "name": "web", "runtime": nil,
		})
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)
		if _, ok := fx.findApp("web")[runtimeOverrideKey]; ok {
			t.Error("override stored for a null runtime")
		}
	})
	t.Run("malformed refused before the clone", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(api.Operator, map[string]any{
			"type": "git", "url": "https://github.com/u/r.git", "name": "web",
			"runtime": map[string]any{"version": "latest"},
		})
		if r.Status || r.Message != "unsupported runtime version" {
			t.Fatalf("r = %+v", r)
		}
		fx.dock.mu.Lock()
		clones := len(fx.dock.cloneCalls)
		fx.dock.mu.Unlock()
		if clones != 0 || fx.findApp("web") != nil {
			t.Error("a refused create cloned or recorded the app")
		}
	})
}

// TestRedeployPinsCommitOnFreshClone covers the path for an app whose .git
// is gone: the clone checks out the branch head, so the pinned commit must
// be fetched onto it rather than silently replaced by the head.
func TestRedeployPinsCommitOnFreshClone(t *testing.T) {
	fx := newFixture(t, []any{gitApp(nil)})
	if r := fx.m.Redeploy(RedeployPayload{Container: "app", CommitSha: builtSHA}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	fx.dock.mu.Lock()
	clones, commits := len(fx.dock.cloneCalls), slices.Clone(fx.dock.fetchCommits)
	fx.dock.mu.Unlock()
	if clones != 1 || !reflect.DeepEqual(commits, []string{builtSHA}) {
		t.Errorf("clones %d, fetched commits %v", clones, commits)
	}

	// With a clone in place the commit goes straight to the fetch.
	fx = newFixture(t, []any{gitApp(nil)})
	if err := os.MkdirAll(filepath.Join(fx.m.appsPath(), "app", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := fx.m.Redeploy(RedeployPayload{Container: "app", CommitSha: builtSHA}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	fx.dock.mu.Lock()
	clones, commits = len(fx.dock.cloneCalls), slices.Clone(fx.dock.fetchCommits)
	fx.dock.mu.Unlock()
	if clones != 0 || !reflect.DeepEqual(commits, []string{builtSHA}) {
		t.Errorf("clones %d, fetched commits %v", clones, commits)
	}

	// A head deploy on a fresh clone needs no second fetch.
	fx = newFixture(t, []any{gitApp(nil)})
	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	fx.dock.mu.Lock()
	fetches := len(fx.dock.fetchCalls)
	fx.dock.mu.Unlock()
	if fetches != 0 {
		t.Errorf("head deploy fetched %d times after cloning", fetches)
	}
}
