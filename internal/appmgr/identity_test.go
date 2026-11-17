package appmgr

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/docker"
)

var (
	imageID     = "sha256:" + strings.Repeat("c", 64)
	imageDigest = "sha256:" + strings.Repeat("d", 64)
	builtSHA    = strings.Repeat("9f", 20)
	oldSHA      = strings.Repeat("1e", 20)
)

// listRow returns the detailed app.list row of one app.
func (fx *fixture) listRow(t *testing.T, name string) map[string]any {
	t.Helper()
	data, _ := fx.m.List(true).Data.([]any)
	for _, entry := range data {
		if row, _ := entry.(map[string]any); row != nil && row["name"] == name {
			return row
		}
	}
	t.Fatalf("no list row for %s", name)
	return nil
}

func TestListReportsImageIdentity(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "db", "type": "container", "image": "postgres:16"},
		map[string]any{"id": float64(2), "name": "web", "type": "git", "image": "odac-app-web"},
		map[string]any{"id": float64(3), "name": "gone", "type": "container", "image": "redis:7"},
	})
	fx.dock.mu.Lock()
	fx.dock.status["db"] = docker.Status{Running: true, ImageID: imageID}
	fx.dock.status["web"] = docker.Status{Running: true, ImageID: imageID}
	fx.dock.digests = map[string]string{imageID + "\x00postgres:16": imageDigest}
	fx.dock.mu.Unlock()

	db := fx.listRow(t, "db")
	if db["imageId"] != imageID || db["imageDigest"] != imageDigest {
		t.Errorf("db identity = %v / %v", db["imageId"], db["imageDigest"])
	}

	// A locally built image has an ID and no registry digest.
	web := fx.listRow(t, "web")
	if web["imageId"] != imageID {
		t.Errorf("web imageId = %v", web["imageId"])
	}
	if _, ok := web["imageDigest"]; ok {
		t.Errorf("web carries an imageDigest: %v", web["imageDigest"])
	}

	// No container, no identity: absent, never empty.
	gone := fx.listRow(t, "gone")
	for _, key := range []string{"imageId", "imageDigest", "runtime", "commitSha"} {
		if _, ok := gone[key]; ok {
			t.Errorf("gone carries %s = %v", key, gone[key])
		}
	}
	fx.dock.mu.Lock()
	before := fx.dock.digestCalls
	fx.dock.mu.Unlock()
	fx.m.List(true)
	fx.dock.mu.Lock()
	calls := fx.dock.digestCalls - before
	fx.dock.mu.Unlock()
	if calls != 2 {
		t.Errorf("digest lookups per list = %d, want one per app with a container", calls)
	}
}

func TestListRevalidatesStoredIdentity(t *testing.T) {
	runtime := map[string]any{"strategy": "node-npm", "image": "node:20-alpine", "version": "20.11.1"}
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "ok", "type": "git", "runtime": runtime, "commitSha": strings.ToUpper(builtSHA)},
		map[string]any{"id": float64(2), "name": "bad", "type": "git",
			"runtime": map[string]any{"strategy": "NODE_NPM", "image": "node:20"}, "commitSha": "abc123"},
		map[string]any{"id": float64(3), "name": "notgit", "type": "container", "runtime": runtime, "commitSha": ""},
		map[string]any{"id": float64(4), "name": "partial", "type": "git",
			"runtime": map[string]any{"strategy": "dockerfile", "image": "", "version": "$(id)"}},
	})

	ok := fx.listRow(t, "ok")
	if !reflect.DeepEqual(ok["runtime"], runtime) || ok["commitSha"] != builtSHA {
		t.Errorf("ok = runtime %v, commit %v", ok["runtime"], ok["commitSha"])
	}
	for _, name := range []string{"bad", "notgit"} {
		row := fx.listRow(t, name)
		if _, has := row["runtime"]; has {
			t.Errorf("%s runtime = %v", name, row["runtime"])
		}
		if _, has := row["commitSha"]; has {
			t.Errorf("%s commitSha = %v", name, row["commitSha"])
		}
	}
	if got := fx.listRow(t, "partial")["runtime"]; !reflect.DeepEqual(got, map[string]any{"strategy": "dockerfile"}) {
		t.Errorf("partial runtime = %v", got)
	}
}

func TestListReportsBuildRuntime(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "web", "type": "git"},
		map[string]any{"id": float64(2), "name": "old", "type": "git"},
	})
	build := func(name string, meta map[string]any) {
		logger := applog.New(fx.m.logsRoot, name)
		if err := logger.Init(); err != nil {
			t.Fatal(err)
		}
		ctrl, err := logger.NewBuildStream("build_1_x", map[string]any{"image": "odac-app-" + name, "strategy": "git-app"})
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range meta {
			ctrl.SetMetadata(k, v)
		}
		ctrl.Finalize(false)
	}
	build("web", map[string]any{"runtime": map[string]any{"strategy": "go", "image": "golang:1.22-alpine", "version": "1.22.5"}})
	build("old", nil)

	web, _ := fx.listRow(t, "web")["build"].(map[string]any)
	want := map[string]any{"strategy": "go", "image": "golang:1.22-alpine", "version": "1.22.5"}
	if !reflect.DeepEqual(web["runtime"], want) || web["status"] != "failed" {
		t.Errorf("web build = %v", web)
	}
	old, _ := fx.listRow(t, "old")["build"].(map[string]any)
	if old == nil {
		t.Fatal("old build summary missing")
	}
	if _, ok := old["runtime"]; ok {
		t.Errorf("a build summary without runtime grew one: %v", old)
	}
}

func gitApp(extra map[string]any) map[string]any {
	app := map[string]any{"id": float64(1), "name": "app", "type": "git", "url": "https://github.com/a/b.git"}
	for k, v := range extra {
		app[k] = v
	}
	return app
}

func TestRedeployRecordsRuntimeAndCommit(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{
		"runtime":   map[string]any{"strategy": "node-npm", "image": "node:18-alpine"},
		"commitSha": oldSHA,
	})})
	fx.dock.mu.Lock()
	fx.dock.head = builtSHA
	fx.dock.buildRuntime = docker.Runtime{Strategy: "node-npm", Image: "node:20-alpine", Version: "20.11.1"}
	fx.dock.mu.Unlock()

	// No commitSha in the payload: the sandbox's HEAD names the commit.
	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("app")
	want := map[string]any{"strategy": "node-npm", "image": "node:20-alpine", "version": "20.11.1"}
	if !reflect.DeepEqual(app["runtime"], want) || app["commitSha"] != builtSHA {
		t.Errorf("persisted runtime %v, commit %v", app["runtime"], app["commitSha"])
	}
	if slices.Contains(ephemeralFields, "runtime") || slices.Contains(ephemeralFields, "commitSha") {
		t.Error("runtime/commitSha must survive a restart")
	}
	last := applog.New(fx.m.logsRoot, "app").GetLastBuild()
	if last == nil || !reflect.DeepEqual(last.Metadata["runtime"], want) {
		t.Errorf("build summary runtime = %v", last)
	}
}

// TestRedeployFailureKeepsRunningIdentity pins the rule that only a deploy
// that went live may describe the app: the old container is still serving,
// so its runtime and commit stay, while the failed build's own summary says
// what it tried.
func TestRedeployFailureKeepsRunningIdentity(t *testing.T) {
	running := map[string]any{"strategy": "node-npm", "image": "node:18-alpine"}
	fx := newFixture(t, []any{gitApp(map[string]any{"runtime": running, "commitSha": oldSHA})})
	fx.dock.mu.Lock()
	fx.dock.head = builtSHA
	fx.dock.buildErr = errTest
	fx.dock.buildRuntime = docker.Runtime{Strategy: "node-npm", Image: "node:20-alpine"}
	fx.dock.mu.Unlock()

	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); r.Status {
		t.Fatal("redeploy succeeded")
	}
	fx.waitIdle(t)

	app := fx.findApp("app")
	if !reflect.DeepEqual(app["runtime"], running) || app["commitSha"] != oldSHA {
		t.Errorf("failed build overwrote identity: runtime %v, commit %v", app["runtime"], app["commitSha"])
	}
	last := applog.New(fx.m.logsRoot, "app").GetLastBuild()
	attempted := map[string]any{"strategy": "node-npm", "image": "node:20-alpine"}
	if last == nil || last.Status != "failed" || !reflect.DeepEqual(last.Metadata["runtime"], attempted) {
		t.Errorf("failed build summary = %+v", last)
	}
}

func TestRedeployCommitFallsBackToFullRequestedSHA(t *testing.T) {
	cases := []struct {
		name, requested string
		want            any
	}{
		{"full sha, normalized", strings.ToUpper(builtSHA), builtSHA},
		{"abbreviation is no record", "abc1234", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, []any{gitApp(map[string]any{"commitSha": oldSHA})})
			// HEAD unresolved (fake head "").
			if r := fx.m.Redeploy(RedeployPayload{Container: "app", CommitSha: tc.requested}); !r.Status {
				t.Fatalf("redeploy failed: %v", r.Message)
			}
			fx.waitIdle(t)
			if got := fx.findApp("app")["commitSha"]; got != tc.want {
				t.Errorf("commitSha = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreateFromGitRecordsRuntimeAndCommit(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.dock.mu.Lock()
	fx.dock.head = builtSHA
	fx.dock.buildRuntime = docker.Runtime{Strategy: "python", Image: "python:3-slim", Version: "3.13.1"}
	fx.dock.mu.Unlock()

	r := fx.m.Create(api.Operator, map[string]any{"type": "git", "url": "https://github.com/u/r.git", "name": "py"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("py")
	want := map[string]any{"strategy": "python", "image": "python:3-slim", "version": "3.13.1"}
	if !reflect.DeepEqual(app["runtime"], want) || app["commitSha"] != builtSHA {
		t.Errorf("persisted runtime %v, commit %v", app["runtime"], app["commitSha"])
	}
}

func TestCreateFromGitWithoutIdentityAddsNoKeys(t *testing.T) {
	fx := newFixture(t, []any{})
	r := fx.m.Create(api.Operator, map[string]any{"type": "git", "url": "https://github.com/u/r.git", "name": "bare"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("bare")
	for _, key := range []string{"runtime", "commitSha"} {
		if _, ok := app[key]; ok {
			t.Errorf("record carries %s = %v", key, app[key])
		}
	}
}

func TestListReportsImageMetaForImageAppsOnly(t *testing.T) {
	otherID := "sha256:" + strings.Repeat("e", 64)
	unreadableID := "sha256:" + strings.Repeat("f", 64)
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "ch", "type": "container", "image": "clickhouse/clickhouse-server:latest",
			"env": map[string]any{"manual": map[string]any{"CLICKHOUSE_PASSWORD": "hunter2"}}},
		map[string]any{"id": float64(2), "name": "web", "type": "git", "image": "odac-app-web"},
		map[string]any{"id": float64(3), "name": "devapp", "type": "git", "dev": true, "image": "odac-app-devapp"},
		map[string]any{"id": float64(4), "name": "bare", "type": "container", "image": "grafana/grafana:latest"},
		map[string]any{"id": float64(5), "name": "lost", "type": "container", "image": "redis:7",
			"imageMeta": map[string]any{"labels": map[string]any{"stale.version": "1"}, "env": map[string]any{}}},
		map[string]any{"id": float64(6), "name": "stopped", "type": "container", "image": "mongo:latest",
			"imageMeta": map[string]any{"labels": map[string]any{}, "env": map[string]any{}}},
	})
	fx.dock.mu.Lock()
	fx.dock.status["ch"] = docker.Status{Running: true, ImageID: imageID}
	fx.dock.status["web"] = docker.Status{Running: true, ImageID: otherID}
	fx.dock.status["devapp"] = docker.Status{Running: true, ImageID: otherID}
	fx.dock.status["bare"] = docker.Status{Running: true, ImageID: otherID}
	fx.dock.status["lost"] = docker.Status{Running: true, ImageID: unreadableID}
	fx.dock.versionMeta = map[string]docker.ImageVersionMeta{
		imageID: {
			Labels: map[string]string{"com.clickhouse.build.version": "26.10.4.207", "org.opencontainers.image.version": "22.04"},
			Env:    map[string]string{},
		},
		otherID: {Labels: map[string]string{}, Env: map[string]string{}},
	}
	fx.dock.mu.Unlock()

	want := map[string]any{
		"labels": map[string]any{"com.clickhouse.build.version": "26.10.4.207", "org.opencontainers.image.version": "22.04"},
		"env":    map[string]any{},
	}
	if got := fx.listRow(t, "ch")["imageMeta"]; !reflect.DeepEqual(got, want) {
		t.Errorf("ch imageMeta = %v", got)
	}
	// Read, nothing declared: empty maps, which the Cloud reads as "clear".
	empty := map[string]any{"labels": map[string]any{}, "env": map[string]any{}}
	if got := fx.listRow(t, "bare")["imageMeta"]; !reflect.DeepEqual(got, empty) {
		t.Errorf("bare imageMeta = %v", got)
	}
	// Git and dev apps never carry it; an unreadable image or no container
	// leaves it out, a hand-edited record's copy included.
	for _, name := range []string{"web", "devapp", "lost", "stopped"} {
		if got, ok := fx.listRow(t, name)["imageMeta"]; ok {
			t.Errorf("%s carries imageMeta = %v", name, got)
		}
	}

	fx.dock.mu.Lock()
	retained := fx.dock.retained
	fx.dock.mu.Unlock()
	if !reflect.DeepEqual(retained, map[string]bool{imageID: true, otherID: true, unreadableID: true}) {
		t.Errorf("retained image IDs = %v", retained)
	}
}
