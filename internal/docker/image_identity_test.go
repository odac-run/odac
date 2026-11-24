package docker

import (
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

var (
	testImageID = "sha256:" + strings.Repeat("a", 64)
	hubDigest   = "sha256:" + strings.Repeat("1", 64)
	ghcrDigest  = "sha256:" + strings.Repeat("2", 64)
	quayDigest  = "sha256:" + strings.Repeat("3", 64)
	multiDigest = "sha256:" + strings.Repeat("4", 64)
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

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		wantName string
		wantTag  string
		wantErr  bool
	}{
		{"ghcr explicit tag", "ghcr.io/open-webui/open-webui:main", "ghcr.io/open-webui/open-webui", "main", false},
		{"ghcr implicit tag", "ghcr.io/openclaw/openclaw", "ghcr.io/openclaw/openclaw", "", false},
		{"ghcr multi-segment", "ghcr.io/org/team/app:1.2.3", "ghcr.io/org/team/app", "1.2.3", false},
		{"ghcr digest", "ghcr.io/org/app@" + ghcrDigest, "ghcr.io/org/app", "", false},
		{"quay minio release tag preserves case", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", "quay.io/minio/minio", "RELEASE.2025-04-22T22-12-26Z", false},
		{"quay multi-segment", "quay.io/org/sub/repo:1.0", "quay.io/org/sub/repo", "1.0", false},
		{"quay digest", "quay.io/minio/minio@" + quayDigest, "quay.io/minio/minio", "", false},
		{"uppercase ghcr host", "GHCR.IO/open-webui/open-webui:main", "ghcr.io/open-webui/open-webui", "main", false},
		{"mixed-case quay host with uppercase tag", "Quay.IO/minio/minio:RELEASE.2025-04-22T22-12-26Z", "quay.io/minio/minio", "RELEASE.2025-04-22T22-12-26Z", false},
		{"uppercase docker.io host", "DOCKER.IO/postgres:16", "docker.io/library/postgres", "16", false},
		{"uppercase legacy hub host", "INDEX.DOCKER.IO/library/postgres:16", "docker.io/library/postgres", "16", false},
		{"uppercase localhost", "LOCALHOST/org/app:1", "localhost/org/app", "1", false},
		{"uppercase repo path on ghcr rejected", "ghcr.io/Org/app:1", "", "", true},
		{"uppercase host and repo path rejected", "GHCR.IO/Org/app:1", "", "", true},
		{"uppercase repo path on quay rejected", "quay.io/MinIO/minio:latest", "", "", true},
	}
	for _, tc := range cases {
		got, err := ParseImageRef(tc.ref)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s (%q): got %v, want error", tc.name, tc.ref, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s (%q): unexpected error: %v", tc.name, tc.ref, err)
			continue
		}
		if got.Name() != tc.wantName {
			t.Errorf("%s (%q): Name() = %q, want %q", tc.name, tc.ref, got.Name(), tc.wantName)
		}
		var gotTag string
		if tagged, ok := got.(reference.Tagged); ok {
			gotTag = tagged.Tag()
		}
		if gotTag != tc.wantTag {
			t.Errorf("%s (%q): Tag() = %q, want %q", tc.name, tc.ref, gotTag, tc.wantTag)
		}
	}
}

func TestMatchRepoDigestGHCRAndQuay(t *testing.T) {
	digests := []string{
		"minio/minio@" + hubDigest,
		"ghcr.io/minio/minio@" + ghcrDigest,
		"quay.io/minio/minio@" + quayDigest,
		"ghcr.io/org/team/app@" + multiDigest,
		"quay.io/org/sub/repo@" + quayDigest,
	}
	cases := []struct {
		name, ref string
		digests   []string
		want      string
	}{
		{"ghcr explicit tag", "ghcr.io/minio/minio:latest", digests, ghcrDigest},
		{"ghcr implicit tag", "ghcr.io/minio/minio", digests, ghcrDigest},
		{"ghcr digest ref", "ghcr.io/minio/minio@" + ghcrDigest, digests, ghcrDigest},
		{"ghcr tagged and digested ref", "ghcr.io/minio/minio:latest@" + ghcrDigest, digests, ghcrDigest},
		{"ghcr multi-segment explicit tag", "ghcr.io/org/team/app:1.2.3", digests, multiDigest},
		{"ghcr multi-segment implicit tag", "ghcr.io/org/team/app", digests, multiDigest},
		{"ghcr multi-segment digest ref", "ghcr.io/org/team/app@" + multiDigest, digests, multiDigest},
		{"ghcr segment prefix mismatch", "ghcr.io/org/app:1.2.3", digests, ""},
		{"quay explicit tag", "quay.io/minio/minio:latest", digests, quayDigest},
		{"quay implicit tag", "quay.io/minio/minio", digests, quayDigest},
		{"quay uppercase release tag", "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", digests, quayDigest},
		{"quay digest ref", "quay.io/minio/minio@" + quayDigest, digests, quayDigest},
		{"quay multi-segment", "quay.io/org/sub/repo:1.0", digests, quayDigest},
		{"docker hub same path isolated", "minio/minio:latest", digests, hubDigest},
		{"ghcr uppercase host in ref", "GHCR.IO/org/team/app:1.2.3", digests, multiDigest},
		{"quay uppercase host in ref", "Quay.IO/minio/minio:RELEASE.2025-04-22T22-12-26Z", digests, quayDigest},
		{"ghcr uppercase host in RepoDigests", "ghcr.io/org/app:1", []string{"GHCR.IO/org/app@" + ghcrDigest}, ghcrDigest},
		{"quay uppercase host in RepoDigests", "quay.io/minio/minio:latest", []string{"QUAY.IO/minio/minio@" + quayDigest}, quayDigest},
		{"ghcr uppercase repo path rejected", "ghcr.io/Org/team/app:1.2.3", digests, ""},
		{"quay uppercase repo path rejected", "quay.io/MinIO/minio:latest", digests, ""},
		{"uppercase hex digest rejected", "ghcr.io/org/app:1", []string{"ghcr.io/org/app@sha256:" + strings.Repeat("A", 64)}, ""},
	}
	for _, tc := range cases {
		if got := matchRepoDigest(tc.digests, tc.ref); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestImageDigestGHCRAndQuay(t *testing.T) {
	f := newFakeAPI()
	f.images[testImageID] = image.InspectResponse{
		RepoDigests: []string{
			"ghcr.io/org/team/app@" + ghcrDigest,
			"quay.io/minio/minio@" + quayDigest,
		},
	}
	c := newTestClient(t, f)

	if got := c.ImageDigest(testImageID, "ghcr.io/org/team/app:1.2.3"); got != ghcrDigest {
		t.Fatalf("ghcr multi-segment digest = %q, want %q", got, ghcrDigest)
	}
	if got := c.ImageDigest(testImageID, "GHCR.IO/org/team/app"); got != ghcrDigest {
		t.Fatalf("ghcr uppercase host implicit tag digest = %q, want %q", got, ghcrDigest)
	}
	if got := c.ImageDigest(testImageID, "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"); got != quayDigest {
		t.Fatalf("quay digest = %q, want %q", got, quayDigest)
	}
	if got := c.ImageDigest(testImageID, "quay.io/minio/minio@"+quayDigest); got != quayDigest {
		t.Fatalf("quay digest-pinned ref = %q, want %q", got, quayDigest)
	}
	if got := c.ImageDigest(testImageID, "minio/minio:latest"); got != "" {
		t.Fatalf("cross-registry hub ref matched: %q", got)
	}
	if f.imageInspects != 1 {
		t.Errorf("image inspects = %d, want 1", f.imageInspects)
	}
}
