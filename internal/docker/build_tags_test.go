package docker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/system"
)

func TestImageTagsListsEveryRepositoryTagOnce(t *testing.T) {
	f := newFakeAPI()
	f.images["odac-app-web:latest"] = image.InspectResponse{ID: testIDNew}
	f.images["odac-app-web:build_2_b"] = image.InspectResponse{ID: testIDNew}
	f.images["odac-app-web:build_1_a"] = image.InspectResponse{ID: testIDOld}
	f.images["bogus"] = image.InspectResponse{ID: "not-an-id"}
	c := newTestClient(t, f)

	tags, err := c.ImageTags("odac-app-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.imageListFilters) != 1 || f.imageListFilters[0] != "odac-app-*" {
		t.Fatalf("filter = %v, the pattern must reach the engine", f.imageListFilters)
	}
	got := map[string]string{}
	for _, tag := range tags {
		if tag.Repository != "odac-app-web" {
			t.Fatalf("repository = %q, want the familiar name", tag.Repository)
		}
		got[tag.Tag] = tag.ImageID
	}
	want := map[string]string{"latest": testIDNew, "build_2_b": testIDNew, "build_1_a": testIDOld}
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v (an invalid image ID is dropped)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %s, want %s", k, got[k], v)
		}
	}
}

func TestImageTagsFailureIsNotAnEmptyList(t *testing.T) {
	f := newFakeAPI()
	f.imageListErr = errors.New("daemon stalled")
	if tags, err := newTestClient(t, f).ImageTags("odac-app-*"); err == nil || tags != nil {
		t.Fatalf("tags=%v err=%v, a failed list must be an error", tags, err)
	}

	down := newFakeAPI()
	down.pingErr = errors.New("down")
	if _, err := newTestClient(t, down).ImageTags("odac-app-*"); err == nil {
		t.Fatal("an unavailable daemon must fail the list")
	}
}

func TestUntagImageIgnoresAMissingReference(t *testing.T) {
	f := newFakeAPI()
	f.images["odac-app-web:build_1_a"] = image.InspectResponse{ID: testIDOld}
	c := newTestClient(t, f)
	if err := c.UntagImage("odac-app-web:build_1_a"); err != nil {
		t.Fatal(err)
	}
	if err := c.UntagImage("odac-app-web:build_1_a"); err != nil {
		t.Fatalf("a missing tag must not be an error: %v", err)
	}
	if len(f.removedImages) != 1 {
		t.Fatalf("removed = %v", f.removedImages)
	}
}

func TestDataRootSpaceReadsTheEngineRootThroughAView(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("statfs is measured on linux and darwin only")
	}
	view := t.TempDir()
	if err := os.MkdirAll(filepath.Join(view, "var/lib/docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := dataRootViews
	dataRootViews = []string{filepath.Join(view, "absent"), view}
	t.Cleanup(func() { dataRootViews = previous })

	f := newFakeAPI()
	f.info = system.Info{DockerRootDir: "/var/lib/docker"}
	c := newTestClient(t, f)
	free, total, ok := c.DataRootSpace()
	if !ok || total == 0 || free > total {
		t.Fatalf("free=%d total=%d ok=%t", free, total, ok)
	}

	// A root this host cannot see is unknown, never an ancestor's space.
	f.info = system.Info{DockerRootDir: "/nowhere/docker"}
	if _, _, ok := newTestClient(t, f).DataRootSpace(); ok {
		t.Fatal("an unreachable data root must read as unknown")
	}
	f.info = system.Info{DockerRootDir: "relative"}
	if _, _, ok := newTestClient(t, f).DataRootSpace(); ok {
		t.Fatal("a relative data root must read as unknown")
	}
}
