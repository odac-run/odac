package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

const (
	testIDOld = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testIDNew = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestPullImageSkipsALocalTagUnlessForced(t *testing.T) {
	f := newFakeAPI()
	f.images["postgres:17"] = image.InspectResponse{ID: testIDOld}
	c := newTestClient(t, f)

	if err := c.PullImage(context.Background(), "postgres:17", false); err != nil {
		t.Fatal(err)
	}
	if len(f.pulled) != 0 {
		t.Fatalf("a local tag must not be pulled without force: %v", f.pulled)
	}
	if err := c.PullImage(context.Background(), "postgres:17", true); err != nil {
		t.Fatal(err)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "postgres:17" {
		t.Fatalf("force must re-pull the tag: %v", f.pulled)
	}
	if err := c.PullImage(context.Background(), "postgres:18", false); err != nil {
		t.Fatal(err)
	}
	if len(f.pulled) != 2 {
		t.Fatalf("a missing tag must be pulled: %v", f.pulled)
	}
}

func TestPullImageFailsWhenTheForcedPullFails(t *testing.T) {
	f := newFakeAPI()
	f.images["postgres:17"] = image.InspectResponse{ID: testIDOld}
	f.pullBodies["postgres:17"] = `{"error":"toomanyrequests: rate limited"}` + "\n"
	c := newTestClient(t, f)

	err := c.PullImage(context.Background(), "postgres:17", true)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, the local copy must not stand in for a forced pull", err)
	}
}

func TestPullImageNeedsDocker(t *testing.T) {
	f := newFakeAPI()
	f.pingErr = errors.New("down")
	if err := newTestClient(t, f).PullImage(context.Background(), "x:1", false); err == nil {
		t.Fatal("an unavailable daemon must fail the pull")
	}
}

func TestImageMetaReadsTheHealthcheck(t *testing.T) {
	f := newFakeAPI()
	withCheck := &dockerspec.DockerOCIImageConfig{}
	withCheck.Healthcheck = &dockerspec.HealthcheckConfig{Test: []string{"CMD", "pg_isready"}}
	disabled := &dockerspec.DockerOCIImageConfig{}
	disabled.Healthcheck = &dockerspec.HealthcheckConfig{Test: []string{"NONE"}}
	f.images["a:1"] = image.InspectResponse{ID: testIDOld, Config: withCheck}
	f.images["b:1"] = image.InspectResponse{ID: testIDNew, Config: disabled}
	f.images["c:1"] = image.InspectResponse{ID: "not-an-id"}
	c := newTestClient(t, f)

	if meta, err := c.ImageMeta("a:1"); err != nil || meta.ID != testIDOld || !meta.Healthcheck {
		t.Errorf("a:1 = %+v, %v", meta, err)
	}
	if meta, err := c.ImageMeta("b:1"); err != nil || meta.Healthcheck {
		t.Errorf("HEALTHCHECK NONE must read as none: %+v, %v", meta, err)
	}
	if meta, err := c.ImageMeta("c:1"); err != nil || meta.ID != "" {
		t.Errorf("a malformed ID must not be reported: %+v, %v", meta, err)
	}
	if _, err := c.ImageMeta("missing:1"); err == nil {
		t.Error("a missing image must be an error")
	}
}

func TestTagImage(t *testing.T) {
	f := newFakeAPI()
	f.images["postgres:17"] = image.InspectResponse{ID: testIDOld}
	c := newTestClient(t, f)

	if err := c.TagImage(testIDOld, "odac-previous:app-1"); err != nil {
		t.Fatal(err)
	}
	if f.images["odac-previous:app-1"].ID != testIDOld {
		t.Fatalf("tag not applied: %v", f.tagged)
	}
}

func TestInspectState(t *testing.T) {
	f := newFakeAPI()
	f.inspects["db"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Image:        testIDNew,
			RestartCount: 2,
			State: &container.State{
				Running:   true,
				OOMKilled: true,
				Health:    &container.Health{Status: "unhealthy"},
			},
		},
	}
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State: &container.State{Running: true, Health: &container.Health{Status: "bogus"}},
		},
	}
	c := newTestClient(t, f)

	st, err := c.InspectState("db")
	if err != nil {
		t.Fatal(err)
	}
	want := ContainerState{Exists: true, Running: true, Restarts: 2, OOMKilled: true, Health: "unhealthy", ImageID: testIDNew}
	if st != want {
		t.Errorf("db = %+v, want %+v", st, want)
	}
	if st, _ := c.InspectState("web"); st.Health != "" {
		t.Errorf("an unknown health value must read as none: %+v", st)
	}
	if st, err := c.InspectState("gone"); err != nil || st.Exists {
		t.Errorf("a 404 is an answer, not an error: %+v, %v", st, err)
	}
}
