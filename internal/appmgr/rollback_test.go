package appmgr

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/docker"
)

// ---- fake Docker: the build retention seams ----

func (f *fakeDocker) ImageTags(pattern string) ([]docker.ImageTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagLists = append(f.tagLists, pattern)
	if f.tagListErr != nil {
		return nil, f.tagListErr
	}
	out := []docker.ImageTag{}
	for ref, id := range f.images {
		repo, tag, ok := strings.Cut(ref, ":")
		if strings.HasPrefix(ref, "sha256:") {
			continue
		}
		if !ok {
			tag = "latest"
		}
		match := repo == pattern
		if prefix, glob := strings.CutSuffix(pattern, "*"); glob {
			match = strings.HasPrefix(repo, prefix)
		}
		if match {
			out = append(out, docker.ImageTag{Repository: repo, Tag: tag, ImageID: id})
		}
	}
	return out, nil
}

func (f *fakeDocker) UntagImage(ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.untagErr[ref]; err != nil {
		return err
	}
	delete(f.images, ref)
	f.untagged = append(f.untagged, ref)
	return nil
}

func (f *fakeDocker) DataRootSpace() (uint64, uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diskFree, f.diskTotal, f.diskKnown
}

// ---- pure logic ----

func TestValidBuildID(t *testing.T) {
	for id, want := range map[string]bool{
		"build_1785879774117_52c29d07": true,
		"b":                            true,
		"_x.y-z":                       true,
		"latest":                       false,
		"":                             false,
		".hidden":                      false,
		"-dash":                        false,
		"a/b":                          false,
		"a:b":                          false,
		strings.Repeat("a", 128):       true,
		strings.Repeat("a", 129):       false,
	} {
		if got := validBuildID(id); got != want {
			t.Errorf("validBuildID(%q) = %t, want %t", id, got, want)
		}
	}
}

func TestSortBuildsIsNewestFirstByIDTime(t *testing.T) {
	builds := []localBuild{
		{ID: "build_100_a"}, {ID: "custom"}, {ID: "build_1000_b"}, {ID: "build_300_c"}, {ID: "build_300_d"},
	}
	sortBuilds(builds)
	var got []string
	for _, b := range builds {
		got = append(got, b.ID)
	}
	want := []string{"build_1000_b", "build_300_d", "build_300_c", "build_100_a", "custom"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestGroupBuildTagsKeepsOnlyBuildTags(t *testing.T) {
	got := groupBuildTags([]docker.ImageTag{
		{Repository: "odac-app-web", Tag: "latest", ImageID: "i3"},
		{Repository: "odac-app-web", Tag: "build_100_a", ImageID: "i1"},
		{Repository: "odac-app-web", Tag: "build_300_c", ImageID: "i3"},
		{Repository: "odac-app-web", Tag: "build_300_c", ImageID: "i3"},
		{Repository: "odac-app-api", Tag: "build_200_b", ImageID: "i2"},
		{Repository: "postgres", Tag: "build_1_x", ImageID: "i9"},
		{Repository: "odac-app-web", Tag: "build_400_e", ImageID: ""},
	})
	want := map[string][]localBuild{
		"odac-app-web": {{ID: "build_300_c", ImageID: "i3"}, {ID: "build_100_a", ImageID: "i1"}},
		"odac-app-api": {{ID: "build_200_b", ImageID: "i2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
}

func TestBuildsToPrune(t *testing.T) {
	builds := []localBuild{
		{ID: "e", ImageID: "i5"}, {ID: "d", ImageID: "i4"}, {ID: "c", ImageID: "i3"}, {ID: "b", ImageID: "i2"}, {ID: "a", ImageID: "i1"},
	}
	cases := []struct {
		name    string
		keep    int
		keepID  string
		running string
		want    []string
	}{
		{"keeps the newest three", 3, "e", "i5", []string{"b", "a"}},
		{"low disk keeps only the live build", 1, "e", "i5", []string{"d", "c", "b", "a"}},
		{"never the running image", 1, "e", "i2", []string{"d", "c", "a"}},
		{"the live build counts even when it is not the newest", 2, "b", "i2", []string{"d", "c", "a"}},
		{"unknown running image", 3, "e", "", []string{"b", "a"}},
		{"nothing to prune", 5, "e", "i5", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildsToPrune(builds, c.keep, c.keepID, c.running); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("pruned %v, want %v", got, c.want)
			}
		})
	}
}

func TestCurrentBuild(t *testing.T) {
	builds := []localBuild{{ID: "c", ImageID: "i1"}, {ID: "b", ImageID: "i2"}, {ID: "a", ImageID: "i1"}}
	cases := []struct {
		name, recorded, running, want string
	}{
		{"the recorded build when its tag names the image", "a", "i1", "a"},
		{"else the newest tag naming the image", "", "i1", "c"},
		{"a stale record falls back to the image", "b", "i1", "c"},
		{"an image no build names", "c", "i9", ""},
		{"no running image", "c", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := currentBuild(builds, c.recorded, c.running); got != c.want {
				t.Fatalf("currentBuild = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRollbackReplyShapes(t *testing.T) {
	keys := func(o []string) string { return strings.Join(o, ",") }
	cases := []struct {
		name string
		res  RollbackResult
		want string
	}{
		{"failure", RollbackResult{Reason: ImageUnhealthy, RolledBack: true, Message: "m"}, "success,reason,rolledBack,message"},
		{"unchanged", RollbackResult{Success: true}, "success,changed"},
		{"changed, everything known", RollbackResult{Success: true, Changed: true, BuildID: "b", Previous: "c", CommitSha: "x", ImageID: "i"}, "success,changed,buildId,previous,commitSha,imageId"},
		{"changed, previous and commit unknown", RollbackResult{Success: true, Changed: true, BuildID: "b", ImageID: "i"}, "success,changed,buildId,imageId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, f := range c.res.ReplyFields() {
				got = append(got, f.K)
			}
			if keys(got) != c.want {
				t.Fatalf("keys = %v, want %s", got, c.want)
			}
		})
	}
}

// ---- retention on deploy, create and delete ----

var (
	idBuildA = "sha256:" + strings.Repeat("a", 64)
	idBuildB = "sha256:" + strings.Repeat("b", 64)
	idBuildC = "sha256:" + strings.Repeat("c", 64)
	idBuildD = "sha256:" + strings.Repeat("d", 64)
	shaB     = strings.Repeat("b2", 20)
	shaC     = strings.Repeat("c3", 20)
)

func (fx *fixture) imageAt(ref string) string {
	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	return fx.dock.images[ref]
}

func (fx *fixture) untaggedRefs() []string {
	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	return append([]string(nil), fx.dock.untagged...)
}

func TestRedeployKeepsTheLastThreeBuilds(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{"image": "odac-app-app", "active": true})})
	fx.dock.images = map[string]string{
		"odac-app-app": idBuildC, "odac-app-app:build_300_c": idBuildC,
		"odac-app-app:build_200_b": idBuildB, "odac-app-app:build_100_a": idBuildA,
	}
	fx.dock.buildImageID = idBuildD
	fx.dock.head = builtSHA

	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("app")
	buildID, _ := app[buildIDKey].(string)
	if !strings.HasPrefix(buildID, "build_") || fx.imageAt(buildRef("app", buildID)) != idBuildD {
		t.Fatalf("the live build must be tagged and recorded: buildId=%q", buildID)
	}
	if got := fx.untaggedRefs(); !reflect.DeepEqual(got, []string{"odac-app-app:build_100_a"}) {
		t.Fatalf("untagged = %v, want only the oldest", got)
	}
	info := applog.New(fx.m.logsRoot, "app").Build(buildID)
	if info == nil || info.Metadata["commitSha"] != builtSHA {
		t.Fatalf("the build summary must carry the commit: %+v", info)
	}
}

func TestRedeployOnALowDiskKeepsOnlyTheLiveBuild(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{"image": "odac-app-app", "active": true})})
	fx.dock.images = map[string]string{
		"odac-app-app": idBuildC, "odac-app-app:build_300_c": idBuildC, "odac-app-app:build_200_b": idBuildB,
	}
	fx.dock.buildImageID = idBuildD
	fx.dock.diskFree, fx.dock.diskTotal, fx.dock.diskKnown = 9, 100, true

	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)
	got := fx.untaggedRefs()
	if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "build_300_c") || !strings.Contains(strings.Join(got, ","), "build_200_b") {
		t.Fatalf("untagged = %v, want every older build", got)
	}
}

func TestFailedRedeployIsNeverTagged(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{"image": "odac-app-app", "active": true, buildIDKey: "build_300_c"})})
	fx.dock.images = map[string]string{"odac-app-app": idBuildC, "odac-app-app:build_300_c": idBuildC}
	fx.dock.buildErr = errors.New("compile error")

	if r := fx.m.Redeploy(RedeployPayload{Container: "app"}); r.Status {
		t.Fatal("redeploy must fail")
	}
	fx.waitIdle(t)
	fx.dock.mu.Lock()
	tags := len(fx.dock.tags)
	fx.dock.mu.Unlock()
	if tags != 0 || len(fx.untaggedRefs()) != 0 || fx.findApp("app")[buildIDKey] != "build_300_c" {
		t.Fatalf("a failed build touched retention: tags=%d untagged=%v record=%v", tags, fx.untaggedRefs(), fx.findApp("app"))
	}
}

func TestCreateFromGitDropsTagsOfADeletedNamesake(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.dock.images = map[string]string{"odac-app-site:build_100_a": idBuildA}
	fx.dock.buildImageID = idBuildD

	r := fx.m.Create(api.Operator, map[string]any{"type": "git", "url": "https://github.com/u/site.git", "name": "site"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)
	app := fx.findApp("site")
	buildID, _ := app[buildIDKey].(string)
	if buildID == "" || fx.imageAt(buildRef("site", buildID)) != idBuildD {
		t.Fatalf("first build not kept: %v", app)
	}
	if got := fx.untaggedRefs(); !reflect.DeepEqual(got, []string{"odac-app-site:build_100_a"}) {
		t.Fatalf("untagged = %v, a namesake's build must go", got)
	}
}

func TestDeleteRemovesEveryBuildTag(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{"image": "odac-app-app"})})
	fx.dock.images = map[string]string{
		"odac-app-app": idBuildC, "odac-app-app:build_300_c": idBuildC, "odac-app-app:build_200_b": idBuildB,
		"odac-app-other:build_1_x": idBuildA,
	}
	if r := fx.m.Delete(api.Operator, "app", false); !r.Status {
		t.Fatalf("delete failed: %v", r.Message)
	}
	fx.waitIdle(t)
	fx.dock.mu.Lock()
	removed := strings.Join(fx.dock.removedImages, ",")
	fx.dock.mu.Unlock()
	for _, ref := range []string{"odac-app-app:build_300_c", "odac-app-app:build_200_b", "odac-app-app"} {
		if !strings.Contains(removed, ref) {
			t.Errorf("%s was not removed: %s", ref, removed)
		}
	}
	if strings.Contains(removed, "odac-app-other") {
		t.Fatalf("another app's build was removed: %s", removed)
	}
}

// ---- app.list ----

func TestListReportsLocalBuildsOfGitAppsOnly(t *testing.T) {
	fx := newFixture(t, []any{
		gitApp(map[string]any{"image": "odac-app-app", "active": true, buildIDKey: "build_200_b"}),
		map[string]any{"id": float64(2), "name": "db", "type": "container", "image": "postgres:17", "active": true},
	})
	// build_300_c and build_200_b produced the same image; the record says
	// which one was made live.
	fx.dock.images = map[string]string{
		"odac-app-app": idBuildB, "odac-app-app:build_300_c": idBuildB,
		"odac-app-app:build_200_b": idBuildB, "odac-app-app:build_100_a": idBuildA,
	}
	fx.dock.running["app"] = true
	fx.dock.containerImage = map[string]string{"app": idBuildB, "db": idBuildD}

	row := fx.listRow(t, "app")
	if want := []string{"build_300_c", "build_200_b", "build_100_a"}; !reflect.DeepEqual(row["localBuilds"], want) {
		t.Fatalf("localBuilds = %v, want %v", row["localBuilds"], want)
	}
	if row["currentBuild"] != "build_200_b" {
		t.Fatalf("currentBuild = %v", row["currentBuild"])
	}
	if _, leaked := row[buildIDKey]; leaked {
		t.Fatal("the record's buildId must not reach app.list")
	}
	db := fx.listRow(t, "db")
	if _, ok := db["localBuilds"]; ok {
		t.Fatalf("a catalog app must not report builds: %v", db)
	}
	fx.dock.mu.Lock()
	lists := len(fx.dock.tagLists)
	fx.dock.mu.Unlock()
	if lists != 2 {
		t.Fatalf("image lists = %d for two app.list calls, want one per call", lists)
	}
}

func TestListOmitsBuildsWhenTheListFails(t *testing.T) {
	fx := newFixture(t, []any{gitApp(map[string]any{"image": "odac-app-app", "active": true})})
	fx.dock.tagListErr = errors.New("daemon stalled")
	row := fx.listRow(t, "app")
	if _, ok := row["localBuilds"]; ok {
		t.Fatal("an unknown listing must leave localBuilds out")
	}

	fx.dock.tagListErr = nil
	fx.dock.running["app"] = true
	fx.dock.containerImage = map[string]string{"app": idBuildA}
	row = fx.listRow(t, "app")
	if got, ok := row["localBuilds"].([]string); !ok || len(got) != 0 {
		t.Fatalf("no tags must report an empty list: %v", row["localBuilds"])
	}
	if _, ok := row["currentBuild"]; ok {
		t.Fatal("currentBuild must be absent without a matching tag")
	}
}

// ---- app.rollback ----

// newRollbackFixture builds a running git app "web" on build_300_c, with
// build_200_b and build_100_a kept and build_200_b's summary on disk.
func newRollbackFixture(t *testing.T) *fixture {
	t.Helper()
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "git", "image": "odac-app-web", "active": true,
		"url": "https://github.com/a/b.git", buildIDKey: "build_300_c", "commitSha": shaC,
		"runtime":          map[string]any{"strategy": "node-npm", "image": "node:22-alpine"},
		runtimeOverrideKey: "22",
		"ports":            []any{map[string]any{"host": "proxy", "container": float64(3000)}},
	}})
	fx.dock.images = map[string]string{
		"odac-app-web": idBuildC, "odac-app-web:build_300_c": idBuildC,
		"odac-app-web:build_200_b": idBuildB, "odac-app-web:build_100_a": idBuildA,
	}
	fx.dock.running["web"] = true
	fx.dock.containerImage = map[string]string{"web": idBuildC}
	fx.m.imageStableFor = 0
	fx.m.imageHealthTimeout = 0

	logger := applog.New(fx.m.logsRoot, "web")
	if err := logger.Init(); err != nil {
		t.Fatal(err)
	}
	b, err := logger.NewBuildStream("build_200_b", nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetMetadata("commitSha", shaB)
	b.SetMetadata("runtime", map[string]any{"strategy": "node-npm", "image": "node:20-alpine", "version": "20.11.1"})
	b.Finalize(true)
	return fx
}

func (fx *fixture) webRecord(t *testing.T) map[string]any {
	t.Helper()
	app := fx.findApp("web")
	if app == nil {
		t.Fatal("web record missing")
	}
	return app
}

func assertStillOnC(t *testing.T, fx *fixture) {
	t.Helper()
	rec := fx.webRecord(t)
	if rec[buildIDKey] != "build_300_c" || rec["commitSha"] != shaC || rec[imageChangeKey] != nil {
		t.Fatalf("record moved: %v", rec)
	}
	if fx.imageAt("odac-app-web") != idBuildC {
		t.Fatalf("odac-app-web names %s, want the previous build", fx.imageAt("odac-app-web"))
	}
}

func TestRollbackRefusals(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(app map[string]any)
		build  string
		reason string
	}{
		{"unknown app", nil, "build_200_b", ImageInvalid},
		{"catalog app", func(a map[string]any) { a["type"] = "container" }, "build_200_b", ImageInvalid},
		{"dev app", func(a map[string]any) { a["dev"] = true }, "build_200_b", ImageInvalid},
		{"stopped app", func(a map[string]any) { a["active"] = false }, "build_200_b", ImageInvalid},
		{"foreign image", func(a map[string]any) { a["image"] = "nginx:1" }, "build_200_b", ImageInvalid},
		{"bad build id", nil, "../x", ImageInvalid},
		{"latest", nil, "latest", ImageInvalid},
		{"not kept", nil, "build_050_z", BuildMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newRollbackFixture(t)
			container := "web"
			if c.name == "unknown app" {
				container = "nope"
			}
			if c.edit != nil {
				fx.cfg.Mutate(func() { c.edit(fx.m.getLocked("web")) })
			}
			res := fx.m.Rollback(RollbackPayload{Container: container, BuildID: c.build})
			if res.Success || res.Reason != c.reason || res.RolledBack || res.Message == "" {
				t.Fatalf("res = %+v", res)
			}
			fx.dock.mu.Lock()
			stopped, runs := len(fx.dock.stopped), len(fx.dock.runCalls)
			fx.dock.mu.Unlock()
			if stopped != 0 || runs != 0 {
				t.Fatalf("the running container was touched: stopped=%d runs=%d", stopped, runs)
			}
		})
	}
}

func TestRollbackIsBusyWhileAnotherOperationRuns(t *testing.T) {
	fx := newRollbackFixture(t)
	if !fx.m.tryLockProcessing(1) {
		t.Fatal("lock")
	}
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
	fx.m.unlockProcessing(1)
	if res.Success || res.Reason != ImageBusy || res.RolledBack {
		t.Fatalf("res = %+v", res)
	}
	assertStillOnC(t, fx)
}

func TestRollbackToTheRunningBuildIsANoOp(t *testing.T) {
	fx := newRollbackFixture(t)
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_300_c"})
	if !res.Success || res.Changed || len(res.ReplyFields()) != 2 {
		t.Fatalf("res = %+v", res)
	}
	fx.dock.mu.Lock()
	touched := len(fx.dock.stopped) + len(fx.dock.runCalls) + len(fx.dock.tags)
	fx.dock.mu.Unlock()
	if touched != 0 {
		t.Fatal("a no-op rollback touched Docker")
	}
	assertStillOnC(t, fx)
}

func TestRollbackSwitchesWithoutARebuild(t *testing.T) {
	fx := newRollbackFixture(t)
	logsBefore := len(applog.New(fx.m.logsRoot, "web").GetDailySummary().Builds)

	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.BuildID != "build_200_b" || res.Previous != "build_300_c" ||
		res.CommitSha != shaB || res.ImageID != idBuildB {
		t.Fatalf("res = %+v", res)
	}
	if fx.dock.containerImage["web"] != idBuildB || fx.imageAt("odac-app-web") != idBuildB {
		t.Fatal("the app does not run build_200_b under its own reference")
	}
	rec := fx.webRecord(t)
	wantRuntime := map[string]any{"strategy": "node-npm", "image": "node:20-alpine", "version": "20.11.1"}
	if rec[buildIDKey] != "build_200_b" || rec["commitSha"] != shaB || !reflect.DeepEqual(rec["runtime"], wantRuntime) {
		t.Fatalf("record = %v", rec)
	}
	if rec[runtimeOverrideKey] != "22" || rec[imageChangeKey] != nil {
		t.Fatalf("override must stay, marker must go: %v", rec)
	}
	fx.dock.mu.Lock()
	builds, untagged := len(fx.dock.buildCalls), len(fx.dock.untagged)
	fx.dock.mu.Unlock()
	if builds != 0 || untagged != 0 || fx.imageAt("odac-app-web:build_300_c") != idBuildC {
		t.Fatalf("a rollback must not rebuild or prune: builds=%d untagged=%d", builds, untagged)
	}
	if after := len(applog.New(fx.m.logsRoot, "web").GetDailySummary().Builds); after != logsBefore {
		t.Fatalf("a rollback wrote a build log: %d -> %d", logsBefore, after)
	}
	if fx.imageAt(holdTag(1)) != "" {
		// The fake keeps a removed hold in removedImages only.
		fx.dock.mu.Lock()
		removed := strings.Join(fx.dock.removedImages, ",")
		fx.dock.mu.Unlock()
		if !strings.Contains(removed, holdTag(1)) {
			t.Fatal("the hold tag must be dropped after the switch")
		}
	}
	row := fx.listRow(t, "web")
	if row["currentBuild"] != "build_200_b" || row["commitSha"] != shaB {
		t.Fatalf("app.list after rollback: currentBuild=%v commitSha=%v", row["currentBuild"], row["commitSha"])
	}
}

func TestRollbackWithoutASummaryForgetsTheCommit(t *testing.T) {
	fx := newRollbackFixture(t)
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_100_a"})
	fx.waitIdle(t)
	if !res.Success || res.CommitSha != "" {
		t.Fatalf("res = %+v", res)
	}
	rec := fx.webRecord(t)
	if _, ok := rec["commitSha"]; ok {
		t.Fatal("an unknown commit must not keep the previous build's")
	}
	if _, ok := rec["runtime"]; ok {
		t.Fatal("an unknown runtime must not keep the previous build's")
	}
	if needsRuntimeBackfill(rec) {
		t.Fatal("a record that names its build is not a backfill candidate")
	}
}

func TestRollbackUnhealthyReturnsToThePreviousBuild(t *testing.T) {
	fx := newRollbackFixture(t)
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		if st.ImageID == idBuildB {
			st.Restarts = 2
		}
		return st
	}
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageUnhealthy || !res.RolledBack || !strings.Contains(res.Message, "Rolled back to build build_300_c") {
		t.Fatalf("res = %+v", res)
	}
	if fx.dock.containerImage["web"] != idBuildC {
		t.Fatalf("app is on %s", fx.dock.containerImage["web"])
	}
	assertStillOnC(t, fx)
}

func TestRollbackFailureWhenThePreviousBuildDoesNotStart(t *testing.T) {
	fx := newRollbackFixture(t)
	calls := 0
	fx.dock.runErr = func(string) error {
		calls++
		if calls == 1 {
			return errors.New("exec format error")
		}
		return errors.New("disk full")
	}
	res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageRollbackFailed || res.RolledBack || !strings.Contains(res.Message, "not running") {
		t.Fatalf("res = %+v", res)
	}
	assertStillOnC(t, fx)
}

func TestRollbackBlueGreen(t *testing.T) {
	t.Run("switches once green is ready", func(t *testing.T) {
		fx := newRollbackFixture(t)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "web"}})
		})
		fx.setHTTPPorts(3000)
		res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
		fx.waitIdle(t)
		if !res.Success || !res.Changed || res.BuildID != "build_200_b" {
			t.Fatalf("res = %+v", res)
		}
		fx.dock.mu.Lock()
		green := fx.dock.runCalls[0].name
		renames := append([][2]string(nil), fx.dock.renames...)
		fx.dock.mu.Unlock()
		if !strings.HasPrefix(green, "web-green-") || len(renames) != 1 || renames[0] != [2]string{green, "web"} {
			t.Fatalf("green=%s renames=%v", green, renames)
		}
		if fx.webRecord(t)[buildIDKey] != "build_200_b" {
			t.Fatal("record did not follow the switch")
		}
	})

	t.Run("a green that never gets ready leaves the old one serving", func(t *testing.T) {
		fx := newRollbackFixture(t)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "web"}})
		})
		// The green container binds its port but never answers HTTP.
		res := fx.m.Rollback(RollbackPayload{Container: "web", BuildID: "build_200_b"})
		fx.waitIdle(t)
		if res.Success || res.Reason != ImageUnhealthy || !res.RolledBack {
			t.Fatalf("res = %+v", res)
		}
		fx.dock.mu.Lock()
		stoppedWeb := false
		for _, s := range fx.dock.stopped {
			stoppedWeb = stoppedWeb || s == "web"
		}
		fx.dock.mu.Unlock()
		if stoppedWeb || fx.dock.containerImage["web"] != idBuildC {
			t.Fatal("the serving container was touched")
		}
		assertStillOnC(t, fx)
	})
}

func TestCheckRecoversAnInterruptedRollback(t *testing.T) {
	green := "web-green-1785879774117_52c29d07"
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "git", "image": "odac-app-web", "active": true,
		buildIDKey: "build_300_c", "activeContainerId": green,
		imageChangeKey: map[string]any{
			"image": "odac-app-web:build_200_b", "build": "build_200_b", "previous": "odac-app-web",
			"previousId": idBuildC, "hold": holdTag(1), "green": green,
		},
	}})
	// The agent died after the retag, with the green container up.
	fx.dock.images = map[string]string{
		"odac-app-web": idBuildB, "odac-app-web:build_300_c": idBuildC, "odac-app-web:build_200_b": idBuildB, holdTag(1): idBuildC,
	}
	fx.dock.running["web"] = true
	fx.dock.running[green] = true
	fx.dock.containerImage = map[string]string{"web": idBuildC, green: idBuildB}

	fx.checkAndSettle(t)
	rec := fx.findApp("web")
	if rec[imageChangeKey] != nil || rec["activeContainerId"] != nil {
		t.Fatalf("record after recovery = %v", rec)
	}
	if fx.imageAt("odac-app-web") != idBuildC {
		t.Fatal("the reference must point back at the recorded build")
	}
	fx.dock.mu.Lock()
	removed := strings.Join(fx.dock.removed, ",")
	fx.dock.mu.Unlock()
	if !strings.Contains(removed, green) || strings.Contains(removed, "web,") || removed == "web" {
		t.Fatalf("removed = %s, want only the green container", removed)
	}
}
