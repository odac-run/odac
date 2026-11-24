package appmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"odac/internal/docker"
)

// ---- fake Docker: the image change seams ----

func (f *fakeDocker) PullImage(ctx context.Context, ref string, force bool) error {
	f.mu.Lock()
	waits := f.pullWaits
	f.mu.Unlock()
	if waits {
		<-ctx.Done()
		return errors.New("read tcp: use of closed network connection")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, ref+" "+map[bool]string{true: "force", false: "missing"}[force])
	if err := f.pullErr[ref]; err != nil {
		return err
	}
	if f.images == nil {
		f.images = map[string]string{}
	}
	if _, local := f.images[ref]; local && !force {
		return nil
	}
	if id, ok := f.pullResult[ref]; ok {
		f.images[ref] = id
	} else if _, local := f.images[ref]; !local {
		f.images[ref] = "sha256:" + strings.Repeat("f", 64)
	}
	return nil
}

func (f *fakeDocker) ImageMeta(ref string) (docker.ImageMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.images[ref]
	if !ok {
		return docker.ImageMeta{}, errors.New("No such image: " + ref)
	}
	return docker.ImageMeta{ID: id, Healthcheck: f.imageHealth[id]}, nil
}

func (f *fakeDocker) TagImage(source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.images[source]
	if !ok && strings.HasPrefix(source, "sha256:") {
		id, ok = source, true
	}
	if !ok {
		return errors.New("No such image: " + source)
	}
	f.images[target] = id
	f.tags = append(f.tags, [2]string{source, target})
	return nil
}

func (f *fakeDocker) InspectState(name string) (docker.ContainerState, error) {
	f.mu.Lock()
	hook := f.stateHook
	st := docker.ContainerState{
		Exists:  f.running[name] || f.containerImage[name] != "",
		Running: f.running[name],
		ImageID: f.containerImage[name],
	}
	f.mu.Unlock()
	if hook != nil {
		st = hook(name, st)
	}
	return st, nil
}

// ---- fixture ----

var (
	idOld = "sha256:" + strings.Repeat("1", 64)
	idNew = "sha256:" + strings.Repeat("2", 64)
)

// newImageFixture builds a running catalog app "db" on postgres:17.5 (idOld)
// with the gate collapsed to a single probe.
func newImageFixture(t *testing.T) *fixture {
	t.Helper()
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "db", "type": "container", "image": "postgres:17.5", "active": true},
		map[string]any{"id": float64(2), "name": "web", "type": "git", "image": "odac-app-web", "active": true},
	})
	fx.dock.images = map[string]string{"postgres:17.5": idOld}
	fx.dock.pullResult = map[string]string{"postgres:17.6": idNew}
	fx.dock.running["db"] = true
	fx.dock.containerImage = map[string]string{"db": idOld}
	fx.m.imageStableFor = 0
	fx.m.imageHealthTimeout = 0
	return fx
}

func (fx *fixture) dbRecord(t *testing.T) map[string]any {
	t.Helper()
	app := fx.app(0)
	if app == nil || app["name"] != "db" {
		t.Fatalf("db record missing: %v", app)
	}
	return app
}

func (fx *fixture) runImages() []string {
	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	var out []string
	for _, c := range fx.dock.runCalls {
		out = append(out, c.options.Image)
	}
	return out
}

func assertUntouched(t *testing.T, fx *fixture) {
	t.Helper()
	fx.dock.mu.Lock()
	stopped, runs := len(fx.dock.stopped), len(fx.dock.runCalls)
	fx.dock.mu.Unlock()
	if stopped != 0 || runs != 0 {
		t.Fatalf("the running container was touched: stopped=%d runs=%d", stopped, runs)
	}
	rec := fx.dbRecord(t)
	if rec["image"] != "postgres:17.5" || rec[imageChangeKey] != nil {
		t.Fatalf("record changed: %v", rec)
	}
}

// ---- refusals ----

func TestSetImageRefusesWhatIsNotATagChange(t *testing.T) {
	fx := newImageFixture(t)
	cases := []struct{ container, image string }{
		{"db", "mysql:8"},                       // another repository
		{"db", "ghcr.io/library/postgres:17.6"}, // same path, another registry
		{"db", "Postgres::x"},                   // garbage
		{"db", ""},                              // empty
		{"db", "postgres"},                      // no tag
		{"db", "postgres:17.6@sha256:" + strings.Repeat("a", 64)},
		{"db", "postgres:" + strings.Repeat("1", 300)},
		{"web", "odac-app-web:2"}, // git app
		{"nope", "postgres:17.6"}, // unknown app
		{"", "postgres:17.6"},
	}
	for _, c := range cases {
		res := fx.m.SetImage(ImageSetPayload{Container: c.container, Image: c.image})
		if res.Success || res.Reason != ImageInvalid || res.RolledBack || res.Message == "" {
			t.Errorf("%s -> %q: %+v, want invalid", c.container, c.image, res)
		}
	}
	if len(fx.dock.pulls) != 0 {
		t.Fatalf("a refused change pulled: %v", fx.dock.pulls)
	}
	assertUntouched(t, fx)
}

func TestSetImageRefusesALocallyBuiltContainerImage(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "x", "type": "container", "image": "odac-app-x:latest", "active": true},
	})
	res := fx.m.SetImage(ImageSetPayload{Container: "x", Image: "odac-app-x:2"})
	if res.Reason != ImageInvalid {
		t.Fatalf("res = %+v", res)
	}
}

func TestSetImageNormalizesTheRepository(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullResult["docker.io/library/postgres:17.6"] = idNew
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "docker.io/library/postgres:17.6"})
	if !res.Success {
		t.Fatalf("a normalized form of the same repository must be accepted: %+v", res)
	}
}

func TestSetImageRefusesAStoppedApp(t *testing.T) {
	fx := newImageFixture(t)
	fx.m.set(float64(1), map[string]any{"active": false})
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	if res.Reason != ImageInvalid || len(fx.dock.pulls) != 0 {
		t.Fatalf("res = %+v pulls = %v", res, fx.dock.pulls)
	}
}

func TestSetImageIsBusyWhileAnotherOperationRuns(t *testing.T) {
	fx := newImageFixture(t)
	if !fx.m.tryLockProcessing(1) {
		t.Fatal("lock")
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.m.unlockProcessing(1)
	if res.Success || res.Reason != ImageBusy || res.RolledBack {
		t.Fatalf("res = %+v", res)
	}
	if len(fx.dock.pulls) != 0 {
		t.Fatalf("a busy app was pulled for: %v", fx.dock.pulls)
	}
	assertUntouched(t, fx)
}

// ---- pull ----

func TestSetImagePullFailureLeavesTheContainerRunning(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullErr = map[string]error{
		"postgres:17.6": errors.New("Error response from daemon: Get \"https://bob:hunter2@registry.example/v2/\": manifest unknown\nmore"),
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	if res.Success || res.Reason != ImagePullFailed || res.RolledBack {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(res.Message, "hunter2") || strings.Contains(res.Message, "\n") || !strings.Contains(res.Message, "manifest unknown") {
		t.Fatalf("message must be short, real and credential-free: %q", res.Message)
	}
	assertUntouched(t, fx)
	if !fx.m.tryLockProcessing(1) {
		t.Fatal("the processing lock leaked")
	}
}

func TestSetImagePullIsBoundedByItsTimeout(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullWaits = true
	fx.m.imagePullTimeout = 20 * time.Millisecond
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	if res.Reason != ImagePullFailed || !strings.Contains(res.Message, "did not finish in time") {
		t.Fatalf("res = %+v", res)
	}
	assertUntouched(t, fx)
}

func TestSetImageUnchangedImageIsANoOp(t *testing.T) {
	fx := newImageFixture(t)
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.5"})
	if !res.Success || res.Changed {
		t.Fatalf("res = %+v", res)
	}
	if got := res.ReplyFields(); len(got) != 2 {
		t.Fatalf("unchanged reply = %v", got)
	}
	assertUntouched(t, fx)
	if fx.dock.pulls[0] != "postgres:17.5 missing" {
		t.Fatalf("pull: false must only pull a missing tag: %v", fx.dock.pulls)
	}
}

func TestSetImageForcedPullOfARollingTag(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullResult["postgres:17.5"] = idNew // upstream moved the tag
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.5", Pull: true})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Previous != "postgres:17.5" || res.Image != "postgres:17.5" || res.ImageID != idNew {
		t.Fatalf("res = %+v", res)
	}
	if fx.dock.pulls[0] != "postgres:17.5 force" {
		t.Fatalf("pull: true must force: %v", fx.dock.pulls)
	}
	// The previous image was held while the change could still need it,
	// and the hold is all that was removed afterwards.
	if len(fx.dock.tags) == 0 || fx.dock.tags[0] != [2]string{idOld, holdTag(1)} {
		t.Fatalf("previous image not held: %v", fx.dock.tags)
	}
	if len(fx.dock.removedImages) != 1 || fx.dock.removedImages[0] != holdTag(1) {
		t.Fatalf("removed images = %v", fx.dock.removedImages)
	}
}

func TestSetImageForcedPullWithNoNewContentIsANoOp(t *testing.T) {
	fx := newImageFixture(t)
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.5", Pull: true})
	if !res.Success || res.Changed {
		t.Fatalf("res = %+v", res)
	}
	assertUntouched(t, fx)
}

// ---- happy path ----

func TestSetImageHappyPathPersistsOnlyAfterTheGate(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.digests = map[string]string{idNew + "\x00postgres:17.6": "sha256:" + strings.Repeat("d", 64)}

	var duringGate map[string]any
	fx.dock.stateHook = func(name string, st docker.ContainerState) docker.ContainerState {
		if duringGate == nil {
			duringGate = copyMap(fx.dbRecord(t))
		}
		return st
	}

	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Previous != "postgres:17.5" || res.Image != "postgres:17.6" ||
		res.ImageID != idNew || res.ImageDigest != "sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("res = %+v", res)
	}
	if duringGate["image"] != "postgres:17.5" || duringGate[imageChangeKey] == nil {
		t.Fatalf("during the gate the record must still name the previous image, with the marker: %v", duringGate)
	}
	rec := fx.dbRecord(t)
	if rec["image"] != "postgres:17.6" || rec[imageChangeKey] != nil {
		t.Fatalf("record after success = %v", rec)
	}
	if imgs := fx.runImages(); len(imgs) != 1 || imgs[0] != "postgres:17.6" {
		t.Fatalf("recreated on %v", imgs)
	}
	if len(fx.dock.stopped) == 0 || fx.dock.stopped[0] != "db" {
		t.Fatalf("the old container must be stopped gracefully first: %v", fx.dock.stopped)
	}
	for _, img := range fx.dock.removedImages {
		if img == "postgres:17.5" {
			t.Fatal("the previous image must stay local")
		}
	}
	fields := res.ReplyFields()
	keys := make([]string, len(fields))
	for i, f := range fields {
		keys[i] = f.K
	}
	if strings.Join(keys, ",") != "success,changed,previous,image,imageId,imageDigest" {
		t.Fatalf("reply keys = %v", keys)
	}
	// The marker never reaches app.list.
	rows := fx.m.List(true).Data.([]any)
	if _, ok := rows[0].(map[string]any)[imageChangeKey]; ok {
		t.Fatal("marker leaked into app.list")
	}
}

func TestSetImageHealthcheckMustTurnHealthy(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.imageHealth = map[string]bool{idNew: true}
	probes := 0
	fx.m.imageHealthTimeout = 1 << 40 // never times out in this test
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		probes++
		st.Health = "starting"
		if probes >= 3 {
			st.Health = "healthy"
		}
		return st
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if !res.Success || probes < 3 {
		t.Fatalf("res = %+v after %d probes", res, probes)
	}
}

// ---- gate failures ----

func TestSetImageUnhealthyRollsBack(t *testing.T) {
	for name, hook := range map[string]func(docker.ContainerState) docker.ContainerState{
		"unhealthy": func(st docker.ContainerState) docker.ContainerState { st.Health = "unhealthy"; return st },
		"restarted": func(st docker.ContainerState) docker.ContainerState { st.Restarts = 1; return st },
		"oom":       func(st docker.ContainerState) docker.ContainerState { st.OOMKilled = true; return st },
		"exited":    func(st docker.ContainerState) docker.ContainerState { st.Running = false; return st },
		"never healthy": func(st docker.ContainerState) docker.ContainerState {
			st.Health = "starting"
			return st
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newImageFixture(t)
			fx.dock.imageHealth = map[string]bool{idNew: true}
			fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
				if st.ImageID == idNew {
					return hook(st)
				}
				return st
			}
			res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
			fx.waitIdle(t)
			if res.Success || res.Reason != ImageUnhealthy || !res.RolledBack || !strings.Contains(res.Message, "Rolled back to postgres:17.5") {
				t.Fatalf("res = %+v", res)
			}
			if imgs := fx.runImages(); len(imgs) != 2 || imgs[0] != "postgres:17.6" || imgs[1] != "postgres:17.5" {
				t.Fatalf("runs = %v, want new then previous", imgs)
			}
			rec := fx.dbRecord(t)
			if rec["image"] != "postgres:17.5" || rec[imageChangeKey] != nil {
				t.Fatalf("record after rollback = %v", rec)
			}
			if fx.dock.containerImage["db"] != idOld {
				t.Fatalf("app is on %s after rollback", fx.dock.containerImage["db"])
			}
		})
	}
}

func TestSetImageRollbackRestoresAMovedTag(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.pullResult["postgres:17.5"] = idNew
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		if st.ImageID == idNew {
			st.Running = false
		}
		return st
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.5", Pull: true})
	fx.waitIdle(t)
	if res.Reason != ImageUnhealthy || !res.RolledBack {
		t.Fatalf("res = %+v", res)
	}
	if fx.dock.images["postgres:17.5"] != idOld || fx.dock.containerImage["db"] != idOld {
		t.Fatalf("the re-pulled tag must point back at what ran: tag=%s container=%s",
			fx.dock.images["postgres:17.5"], fx.dock.containerImage["db"])
	}
}

func TestSetImageStartFailureRollsBack(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.runImageErr = map[string]error{"postgres:17.6": errors.New("port is already allocated")}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageStartFailed || !res.RolledBack || !strings.Contains(res.Message, "port is already allocated") {
		t.Fatalf("res = %+v", res)
	}
	if fx.dbRecord(t)["image"] != "postgres:17.5" || fx.dock.containerImage["db"] != idOld {
		t.Fatal("not back on the previous image")
	}
}

func TestSetImageRollbackFailure(t *testing.T) {
	fx := newImageFixture(t)
	fx.dock.runImageErr = map[string]error{"postgres:17.5": errors.New("disk full")}
	fx.dock.stateHook = func(_ string, st docker.ContainerState) docker.ContainerState {
		st.Restarts = 3
		return st
	}
	res := fx.m.SetImage(ImageSetPayload{Container: "db", Image: "postgres:17.6"})
	fx.waitIdle(t)
	if res.Success || res.Reason != ImageRollbackFailed || res.RolledBack ||
		!strings.Contains(res.Message, "disk full") || !strings.Contains(res.Message, "not running") {
		t.Fatalf("res = %+v", res)
	}
	rec := fx.dbRecord(t)
	if rec["image"] != "postgres:17.5" || rec[imageChangeKey] != nil {
		t.Fatalf("record after a failed rollback = %v", rec)
	}
	fields := res.ReplyFields()
	if len(fields) != 4 || fields[2].K != "rolledBack" || fields[2].V != false {
		t.Fatalf("failure reply = %v", fields)
	}
}

// ---- crash recovery ----

func TestCheckRecoversAnInterruptedImageChange(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{
			"id": float64(1), "name": "db", "type": "container", "image": "postgres:17.5", "active": true,
			imageChangeKey: map[string]any{
				"image": "postgres:17.6", "previous": "postgres:17.5", "previousId": idOld, "hold": holdTag(1),
			},
		},
	})
	// The agent died mid-gate: the new container runs, the record and the
	// hold still name the previous image.
	fx.dock.images = map[string]string{"postgres:17.5": idOld, "postgres:17.6": idNew, holdTag(1): idOld}
	fx.dock.running["db"] = true
	fx.dock.containerImage = map[string]string{"db": idNew}

	fx.checkAndSettle(t)
	if fx.dbRecord(t)[imageChangeKey] != nil {
		t.Fatal("marker survived the recovery")
	}
	if len(fx.dock.removed) == 0 || fx.dock.removed[0] != "db" {
		t.Fatalf("the container on the new image must go: %v", fx.dock.removed)
	}
	if len(fx.dock.tags) != 1 || fx.dock.tags[0] != [2]string{holdTag(1), "postgres:17.5"} {
		t.Fatalf("tags = %v", fx.dock.tags)
	}
	if fx.runImages() != nil {
		t.Fatal("the recovery pulse itself must not start the app")
	}

	// The next pulse recreates the app from its record.
	fx.checkAndSettle(t)
	if imgs := fx.runImages(); len(imgs) != 1 || imgs[0] != "postgres:17.5" {
		t.Fatalf("recreated on %v", imgs)
	}
	if fx.dock.containerImage["db"] != idOld {
		t.Fatalf("app came back on %s", fx.dock.containerImage["db"])
	}
}

func TestCheckRecoveryKeepsAContainerOnThePreviousImage(t *testing.T) {
	fx := newFixture(t, []any{
		map[string]any{
			"id": float64(1), "name": "db", "type": "container", "image": "postgres:17.5", "active": true,
			imageChangeKey: map[string]any{"image": "postgres:17.6", "previous": "postgres:17.5", "previousId": idOld},
		},
	})
	fx.dock.images = map[string]string{"postgres:17.5": idOld}
	fx.dock.running["db"] = true
	fx.dock.containerImage = map[string]string{"db": idOld}

	fx.checkAndSettle(t)
	if len(fx.dock.removed) != 0 || len(fx.dock.stopped) != 0 {
		t.Fatalf("a container already on the previous image was touched: removed=%v stopped=%v", fx.dock.removed, fx.dock.stopped)
	}
	if fx.dbRecord(t)[imageChangeKey] != nil {
		t.Fatal("marker survived")
	}
}

func TestShortErrorStripsCredentials(t *testing.T) {
	got := shortError(errors.New("Error response from daemon: Head \"https://user:pa55@reg.example/v2/x\": unauthorized\nsecond line"))
	if strings.Contains(got, "pa55") || strings.Contains(got, "user:") || strings.Contains(got, "second") || strings.HasPrefix(got, "Error response") {
		t.Fatalf("shortError = %q", got)
	}
	long := shortError(errors.New(strings.Repeat("x", 500)))
	if len([]rune(long)) > 203 {
		t.Fatalf("not bounded: %d", len(long))
	}
}

func TestImageChangeRefusalGHCRAndQuay(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	allowed := []struct {
		name, current, target string
	}{
		{"ghcr tag change", "ghcr.io/blakeblackshear/frigate:stable", "ghcr.io/blakeblackshear/frigate:0.15.0"},
		{"ghcr multi-segment tag change", "ghcr.io/org/team/app:1.0", "ghcr.io/org/team/app:1.1"},
		{"quay implicit tag to explicit release tag", "quay.io/minio/minio", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"},
		{"quay multi-segment tag change", "quay.io/org/sub/repo:1.0", "quay.io/org/sub/repo:1.1"},
		{"ghcr digest-pinned current to tag", "ghcr.io/org/app@" + d, "ghcr.io/org/app:1.2.3"},
		{"quay digest-pinned current to tag", "quay.io/minio/minio@" + d, "quay.io/minio/minio:latest"},
		{"ghcr uppercase host current to lowercase", "GHCR.IO/open-webui/open-webui:main", "ghcr.io/open-webui/open-webui:v0.6.0"},
		{"quay uppercase host current to lowercase", "QUAY.IO/minio/minio:latest", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"},
		{"ghcr repo starting with odac-app prefix", "ghcr.io/odac-app-x/y:1", "ghcr.io/odac-app-x/y:2"},
	}
	for _, tc := range allowed {
		if got := imageChangeRefusal("container", tc.current, tc.target); got != "" {
			t.Errorf("%s (%s -> %s): unexpected refusal %q", tc.name, tc.current, tc.target, got)
		}
	}

	refused := []struct {
		name, current, target string
	}{
		{"ghcr to quay same path", "ghcr.io/minio/minio:latest", "quay.io/minio/minio:latest"},
		{"quay to docker hub same path", "quay.io/minio/minio:latest", "minio/minio:latest"},
		{"ghcr to docker hub same path", "ghcr.io/org/app:1", "org/app:2"},
		{"ghcr different org", "ghcr.io/org/app:1", "ghcr.io/other/app:1"},
		{"ghcr different segment depth", "ghcr.io/org/app:1", "ghcr.io/org/team/app:1"},
		{"quay untagged target", "quay.io/minio/minio:latest", "quay.io/minio/minio"},
		{"ghcr untagged target", "ghcr.io/org/app:1", "ghcr.io/org/app"},
		{"ghcr digest target", "ghcr.io/org/app:1", "ghcr.io/org/app@" + d},
		{"quay tagged and digested target", "quay.io/minio/minio:latest", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z@" + d},
		{"ghcr uppercase repo path in target", "ghcr.io/org/app:1", "ghcr.io/Org/app:2"},
		{"quay uppercase repo path in target", "quay.io/minio/minio:latest", "quay.io/MinIO/minio:latest"},
		{"ghcr uppercase repo path in current", "ghcr.io/Org/app:1", "ghcr.io/org/app:2"},
	}
	for _, tc := range refused {
		if got := imageChangeRefusal("container", tc.current, tc.target); got == "" {
			t.Errorf("%s (%s -> %s): allowed, want refusal", tc.name, tc.current, tc.target)
		}
	}
}

func TestSameReferenceGHCRAndQuay(t *testing.T) {
	d := "sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"ghcr implicit and explicit latest", "ghcr.io/openclaw/openclaw", "ghcr.io/openclaw/openclaw:latest", true},
		{"ghcr multi-segment implicit and explicit latest", "ghcr.io/org/team/app", "ghcr.io/org/team/app:latest", true},
		{"quay implicit and explicit latest", "quay.io/minio/minio", "quay.io/minio/minio:latest", true},
		{"ghcr uppercase host implicit latest", "GHCR.IO/openclaw/openclaw", "ghcr.io/openclaw/openclaw:latest", true},
		{"quay uppercase host release tag", "QUAY.IO/minio/minio:RELEASE.2025-04-22T22-12-26Z", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", true},
		{"quay tag case stays sensitive", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", "quay.io/minio/minio:release.2025-04-22t22-12-26z", false},
		{"ghcr different tags", "ghcr.io/open-webui/open-webui:main", "ghcr.io/open-webui/open-webui:latest", false},
		{"ghcr digest vs latest tag", "ghcr.io/org/app@" + d, "ghcr.io/org/app:latest", false},
		{"quay digest vs latest tag", "quay.io/minio/minio@" + d, "quay.io/minio/minio:latest", false},
		{"ghcr vs quay same path", "ghcr.io/minio/minio:latest", "quay.io/minio/minio:latest", false},
		{"quay vs docker hub same path", "quay.io/minio/minio:latest", "minio/minio:latest", false},
		{"ghcr uppercase repo path", "ghcr.io/Org/app:1", "ghcr.io/org/app:1", false},
	}
	for _, tc := range cases {
		if got := sameReference(tc.a, tc.b); got != tc.want {
			t.Errorf("%s (%q vs %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSetImageGHCRAndQuay(t *testing.T) {
	digestNew := "sha256:" + strings.Repeat("e", 64)
	fx := newFixture(t, []any{
		map[string]any{"id": float64(1), "name": "app", "type": "container", "image": "ghcr.io/org/team/app:1.0", "active": true},
		map[string]any{"id": float64(2), "name": "s3", "type": "container", "image": "quay.io/minio/minio", "active": true},
	})
	fx.dock.images = map[string]string{
		"ghcr.io/org/team/app:1.0": idOld,
		"quay.io/minio/minio":      idOld,
	}
	fx.dock.pullResult = map[string]string{
		"ghcr.io/org/team/app:1.1":   idNew,
		"quay.io/minio/minio:latest": idOld,
	}
	fx.dock.digests = map[string]string{idNew + "\x00ghcr.io/org/team/app:1.1": digestNew}
	fx.dock.running["app"] = true
	fx.dock.running["s3"] = true
	fx.dock.containerImage = map[string]string{"app": idOld, "s3": idOld}
	fx.m.imageStableFor = 0
	fx.m.imageHealthTimeout = 0

	res := fx.m.SetImage(ImageSetPayload{Container: "app", Image: "ghcr.io/org/team/app:1.1"})
	fx.waitIdle(t)
	if !res.Success || !res.Changed || res.Previous != "ghcr.io/org/team/app:1.0" ||
		res.Image != "ghcr.io/org/team/app:1.1" || res.ImageID != idNew || res.ImageDigest != digestNew {
		t.Fatalf("ghcr SetImage = %+v", res)
	}

	// Implicit latest on quay.io matches explicit :latest with the same ID.
	noop := fx.m.SetImage(ImageSetPayload{Container: "s3", Image: "quay.io/minio/minio:latest"})
	if !noop.Success || noop.Changed {
		t.Fatalf("quay implicit-to-explicit unchanged = %+v", noop)
	}
}
