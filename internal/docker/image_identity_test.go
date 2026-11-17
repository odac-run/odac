package docker

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

var (
	testImageID = "sha256:" + strings.Repeat("a", 64)
	hubDigest   = "sha256:" + strings.Repeat("1", 64)
	ghcrDigest  = "sha256:" + strings.Repeat("2", 64)
)

func TestMatchRepoDigest(t *testing.T) {
	digests := []string{"ghcr.io/acme/postgres@" + ghcrDigest, "postgres@" + hubDigest}
	cases := []struct {
		name, ref string
		digests   []string
		want      string
	}{
		{"short hub name", "postgres:16", digests, hubDigest},
		{"untagged", "postgres", digests, hubDigest},
		{"canonical hub name", "docker.io/library/postgres:16", digests, hubDigest},
		{"legacy hub host", "index.docker.io/library/postgres", digests, hubDigest},
		{"digest-pinned ref", "postgres@" + hubDigest, digests, hubDigest},
		{"other registry", "ghcr.io/acme/postgres:1", digests, ghcrDigest},
		{"hub entry in long form", "postgres", []string{"docker.io/library/postgres@" + hubDigest}, hubDigest},
		{"same name, other namespace", "acme/postgres", digests, ""},
		{"locally built", "odac-app-web", nil, ""},
		{"tag-only entry", "postgres", []string{"postgres:16"}, ""},
		{"malformed entry", "postgres", []string{"postgres@sha256:xyz", "::"}, ""},
		{"non-sha256 digest", "postgres", []string{"postgres@sha512:" + strings.Repeat("3", 128)}, ""},
		{"unparsable ref", "Not A Ref", digests, ""},
	}
	for _, tc := range cases {
		if got := matchRepoDigest(tc.digests, tc.ref); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestImageDigestCachesByImageID(t *testing.T) {
	f := newFakeAPI()
	f.images[testImageID] = image.InspectResponse{RepoDigests: []string{"postgres@" + hubDigest}}
	c := newTestClient(t, f)

	if got := c.ImageDigest(testImageID, "postgres:16"); got != hubDigest {
		t.Fatalf("digest = %q", got)
	}
	// Another app on the same image, and the next list: no second inspect.
	if got := c.ImageDigest(testImageID, "docker.io/library/postgres:latest"); got != hubDigest {
		t.Fatalf("cached digest = %q", got)
	}
	if got := c.ImageDigest(testImageID, "redis"); got != "" {
		t.Fatalf("other repo matched: %q", got)
	}
	if f.imageInspects != 1 {
		t.Errorf("image inspects = %d, want 1", f.imageInspects)
	}
}

func TestImageDigestDoesNotCacheAMiss(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)

	if got := c.ImageDigest(testImageID, "postgres"); got != "" {
		t.Fatalf("missing image answered %q", got)
	}
	f.images[testImageID] = image.InspectResponse{RepoDigests: []string{"postgres@" + hubDigest}}
	if got := c.ImageDigest(testImageID, "postgres"); got != hubDigest {
		t.Errorf("a failed inspect was cached: %q", got)
	}
}

func TestImageDigestRefusesMalformedID(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	for _, id := range []string{"", "postgres:16", "sha256:ABC", "sha256:" + strings.Repeat("a", 63)} {
		if got := c.ImageDigest(id, "postgres"); got != "" {
			t.Errorf("ImageDigest(%q) = %q", id, got)
		}
	}
	if f.imageInspects != 0 {
		t.Errorf("malformed ids reached the daemon: %d inspects", f.imageInspects)
	}
}

func TestImageDigestCacheIsBounded(t *testing.T) {
	var cache imageInfoCache
	for i := 0; i < maxCachedImages+10; i++ {
		cache.put(strings.Repeat("x", i+1), imageInfo{})
	}
	if n := len(cache.byID); n > maxCachedImages {
		t.Errorf("cache holds %d entries, bound is %d", n, maxCachedImages)
	}
}

func TestGetStatusReportsImageID(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Image: testImageID, State: &container.State{Running: true}},
	}
	f.inspects["odd"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Image: "postgres:16", State: &container.State{}},
	}
	c := newTestClient(t, f)

	if got := c.GetStatus("web").ImageID; got != testImageID {
		t.Errorf("image id = %q", got)
	}
	if got := c.GetStatus("odd").ImageID; got != "" {
		t.Errorf("a non-ID image field was reported: %q", got)
	}
	if got := c.GetStatus("gone").ImageID; got != "" {
		t.Errorf("missing container reported %q", got)
	}
}
