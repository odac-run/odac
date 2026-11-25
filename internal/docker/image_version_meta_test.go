package docker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

func imageWithConfig(labels map[string]string, env []string) image.InspectResponse {
	cfg := &dockerspec.DockerOCIImageConfig{}
	cfg.Labels = labels
	cfg.Env = env
	return image.InspectResponse{Config: cfg}
}

func TestDeclaredVersionMetaClickHouse(t *testing.T) {
	got := declaredVersionMeta(map[string]string{
		"com.clickhouse.build.version":     "26.10.4.207",
		"org.opencontainers.image.version": "22.04",
		"org.opencontainers.image.title":   "ubuntu",
		"maintainer":                       "ClickHouse Inc.",
		"build-url":                        "https://example.invalid",
	}, []string{"PATH=/usr/bin", "TZ=UTC", "CLICKHOUSE_CONFIG=/etc/clickhouse-server/config.xml"})
	want := ImageVersionMeta{
		Labels: map[string]string{
			"com.clickhouse.build.version":     "26.10.4.207",
			"org.opencontainers.image.version": "22.04",
			"org.opencontainers.image.title":   "ubuntu",
		},
		Env: map[string]string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDeclaredVersionMetaPostgresEnv(t *testing.T) {
	got := declaredVersionMeta(nil, []string{
		"PATH=/usr/local/sbin:/usr/local/bin",
		"GOSU_VERSION=1.19",
		"LANG=en_US.utf8",
		"PG_MAJOR=18",
		"PG_VERSION=18.7-1.pgdg13+2",
		"PGDATA=/var/lib/postgresql/18/docker",
		"PG_VERSION_EXTRA",      // no value: an env name without '='
		"pg_VERSION=1",          // lowercase
		"_VERSION=1",            // no leading letter
		"1PG_VERSION=1",         // leading digit
		"PG-VERSION=1",          // dash
		"PGVERSIONX=1",          // suffix missing
		"PG_VERSION_NUM=180007", // suffix not last
	})
	want := ImageVersionMeta{
		Labels: map[string]string{},
		Env:    map[string]string{"GOSU_VERSION": "1.19", "PG_VERSION": "18.7-1.pgdg13+2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDeclaredVersionMetaEnvNameLength(t *testing.T) {
	// ^[A-Z][A-Z0-9_]{0,62}_VERSION$: 63 characters before "_VERSION" at most.
	longest := "A" + strings.Repeat("B", 62) + "_VERSION"
	tooLong := "A" + strings.Repeat("B", 63) + "_VERSION"
	got := declaredVersionMeta(nil, []string{longest + "=1", tooLong + "=2", "A_VERSION=3"})
	if _, ok := got.Env[longest]; !ok {
		t.Errorf("the longest legal name was dropped: %v", got.Env)
	}
	if _, ok := got.Env[tooLong]; ok {
		t.Errorf("an overlong name was kept: %v", got.Env)
	}
	if got.Env["A_VERSION"] != "3" {
		t.Errorf("the shortest legal name was dropped: %v", got.Env)
	}
}

func TestDeclaredVersionMetaLaterEnvEntryWins(t *testing.T) {
	got := declaredVersionMeta(nil, []string{"NODE_VERSION=20", "NODE_VERSION=22.1.0", "EMPTY_VERSION="})
	if got.Env["NODE_VERSION"] != "22.1.0" {
		t.Errorf("NODE_VERSION = %q", got.Env["NODE_VERSION"])
	}
	if v, ok := got.Env["EMPTY_VERSION"]; !ok || v != "" {
		t.Errorf("an empty value is still a declared one: %q, %v", v, ok)
	}
}

func TestDeclaredVersionMetaLabelSelection(t *testing.T) {
	got := declaredVersionMeta(map[string]string{
		"org.opencontainers.image.ref.name": "ubuntu",
		"org.opencontainers.image.Title":    "case matters for the exact names",
		"io.Example.VERSION":                "3.1", // version match is case-insensitive
		"versioned":                         "x",
		"org.opencontainers.image.revision": "abc",
		"org.opencontainers.image.source":   "https://github.com/x/y",
	}, nil)
	want := map[string]string{
		"org.opencontainers.image.ref.name": "ubuntu",
		"io.Example.VERSION":                "3.1",
		"versioned":                         "x",
	}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
}

func TestDeclaredVersionMetaKeepsTheFirstSixteenSortedKeys(t *testing.T) {
	labels := map[string]string{}
	var env []string
	for i := 0; i < 20; i++ {
		labels[fmt.Sprintf("l%02d.version", i)] = "1"
		env = append(env, fmt.Sprintf("E%02d_VERSION=1", i))
	}
	for run := 0; run < 5; run++ {
		got := declaredVersionMeta(labels, env)
		if len(got.Labels) != maxVersionMetaKeys || len(got.Env) != maxVersionMetaKeys {
			t.Fatalf("sizes = %d labels, %d env", len(got.Labels), len(got.Env))
		}
		for i := 0; i < 20; i++ {
			_, hasLabel := got.Labels[fmt.Sprintf("l%02d.version", i)]
			_, hasEnv := got.Env[fmt.Sprintf("E%02d_VERSION", i)]
			if hasLabel != (i < 16) || hasEnv != (i < 16) {
				t.Fatalf("run %d: key %d kept = %v/%v", run, i, hasLabel, hasEnv)
			}
		}
	}
}

func TestDeclaredVersionMetaSkipsBadMembersBeforeCounting(t *testing.T) {
	labels := map[string]string{
		"a.version":                           strings.Repeat("9", maxVersionMetaValue+1),
		"b.version":                           "1\n2",
		"c.version":                           "1\x7f",
		"d.version":                           "1\u0085", // C1 control
		"e.version":                           "\xff\xfe",
		strings.Repeat("k", 121) + ".version": "1", // 129-byte key
		"f.version":                           strings.Repeat("9", maxVersionMetaValue),
	}
	var env []string
	for i := 0; i < maxVersionMetaKeys; i++ {
		labels[fmt.Sprintf("z%02d.version", i)] = "1"
	}
	env = append(env, "A_VERSION=1\t2", "B_VERSION="+strings.Repeat("1", 129), "C_VERSION=1.0")
	got := declaredVersionMeta(labels, env)

	if _, ok := got.Labels["f.version"]; !ok {
		t.Error("a value of exactly the limit was dropped")
	}
	for k := range got.Labels {
		if len(k) > maxVersionMetaKey || len(got.Labels[k]) > maxVersionMetaValue {
			t.Errorf("oversized member kept: %q", k)
		}
		if k < "f" {
			t.Errorf("a bad member was kept or truncated: %q = %q", k, got.Labels[k])
		}
	}
	// Skipped members free their slot: f plus the first fifteen z's.
	if len(got.Labels) != maxVersionMetaKeys {
		t.Errorf("labels = %d, want %d", len(got.Labels), maxVersionMetaKeys)
	}
	if !reflect.DeepEqual(got.Env, map[string]string{"C_VERSION": "1.0"}) {
		t.Errorf("env = %v", got.Env)
	}
}

func TestImageVersionMetaRowIsAFreshCopy(t *testing.T) {
	meta := declaredVersionMeta(map[string]string{"x.version": "1"}, nil)
	row := meta.Row()
	row["labels"].(map[string]any)["x.version"] = "2"
	if meta.Labels["x.version"] != "1" {
		t.Error("editing a row reached the cached meta")
	}
	if !reflect.DeepEqual(declaredVersionMeta(nil, nil).Row(), map[string]any{"labels": map[string]any{}, "env": map[string]any{}}) {
		t.Error("an image declaring nothing must report empty maps")
	}
}

func TestImageVersionMetaReadsTheImageConfigOnly(t *testing.T) {
	f := newFakeAPI()
	f.images[testImageID] = imageWithConfig(
		map[string]string{"org.opencontainers.image.version": "17"},
		[]string{"PG_VERSION=17.2", "PATH=/bin"},
	)
	c := newTestClient(t, f)

	meta, ok := c.ImageVersionMeta(testImageID)
	if !ok {
		t.Fatal("a readable image answered no meta")
	}
	want := ImageVersionMeta{
		Labels: map[string]string{"org.opencontainers.image.version": "17"},
		Env:    map[string]string{"PG_VERSION": "17.2"},
	}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("meta = %+v", meta)
	}
}

func TestImageVersionMetaWithoutConfig(t *testing.T) {
	f := newFakeAPI()
	f.images[testImageID] = image.InspectResponse{}
	c := newTestClient(t, f)
	meta, ok := c.ImageVersionMeta(testImageID)
	if !ok || meta.Labels == nil || meta.Env == nil || len(meta.Labels)+len(meta.Env) != 0 {
		t.Errorf("an image with no config = %+v, %v; want empty and known", meta, ok)
	}
}

func TestImageVersionMetaFailures(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	if _, ok := c.ImageVersionMeta(testImageID); ok {
		t.Error("a missing image answered meta")
	}
	if _, ok := c.ImageVersionMeta("postgres:16"); ok {
		t.Error("a non-ID answered meta")
	}
	f.images[testImageID] = imageWithConfig(nil, []string{"PG_VERSION=17"})
	if meta, ok := c.ImageVersionMeta(testImageID); !ok || meta.Env["PG_VERSION"] != "17" {
		t.Errorf("a failed inspect was cached: %+v, %v", meta, ok)
	}

	down := newFakeAPI()
	down.pingErr = notFoundErr{"down"}
	down.images[testImageID] = imageWithConfig(nil, nil)
	if _, ok := newTestClient(t, down).ImageVersionMeta(testImageID); ok {
		t.Error("an unavailable daemon answered meta")
	}
}

func TestImageDigestAndVersionMetaShareOneInspect(t *testing.T) {
	f := newFakeAPI()
	resp := imageWithConfig(map[string]string{"org.opencontainers.image.version": "7.4"}, nil)
	resp.RepoDigests = []string{"redis@" + hubDigest}
	f.images[testImageID] = resp
	c := newTestClient(t, f)

	for i := 0; i < 3; i++ {
		if got := c.ImageDigest(testImageID, "redis:7"); got != hubDigest {
			t.Fatalf("digest = %q", got)
		}
		if meta, ok := c.ImageVersionMeta(testImageID); !ok || meta.Labels["org.opencontainers.image.version"] != "7.4" {
			t.Fatalf("meta = %+v, %v", meta, ok)
		}
	}
	if f.imageInspects != 1 {
		t.Errorf("image inspects = %d, want 1", f.imageInspects)
	}
}

func TestRetainImageInfoDropsImagesNoLongerInUse(t *testing.T) {
	f := newFakeAPI()
	other := "sha256:" + strings.Repeat("b", 64)
	f.images[testImageID] = imageWithConfig(nil, []string{"A_VERSION=1"})
	f.images[other] = imageWithConfig(nil, []string{"B_VERSION=1"})
	c := newTestClient(t, f)

	c.ImageVersionMeta(testImageID)
	c.ImageVersionMeta(other)
	c.RetainImageInfo(map[string]bool{testImageID: true})
	if _, ok := c.images.get(other); ok {
		t.Error("an image no app runs stayed cached")
	}
	if _, ok := c.images.get(testImageID); !ok {
		t.Fatal("an image in use was dropped")
	}
	c.ImageVersionMeta(testImageID)
	c.ImageVersionMeta(other)
	if f.imageInspects != 3 {
		t.Errorf("image inspects = %d, want 3 (only the dropped image is read again)", f.imageInspects)
	}
	c.RetainImageInfo(nil)
	if n := len(c.images.byID); n != 0 {
		t.Errorf("an empty in-use set left %d entries", n)
	}
}

func TestImageVersionMetaNeverReadsTheContainerEnv(t *testing.T) {
	f := newFakeAPI()
	f.images[testImageID] = imageWithConfig(nil, []string{"PG_VERSION=18.7-1.pgdg13+2", "GOSU_VERSION=1.19"})
	f.inspects["db"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Image: testImageID, State: &container.State{Running: true}},
		Config: &container.Config{Env: []string{
			"PG_VERSION=18.7-1.pgdg13+2",
			"GOSU_VERSION=1.19",
			"SECRET_VERSION=hunter2",
			"POSTGRES_PASSWORD=hunter2",
		}},
	}
	c := newTestClient(t, f)

	meta, ok := c.ImageVersionMeta(c.GetStatus("db").ImageID)
	if !ok {
		t.Fatal("no meta for a running container's image")
	}
	want := map[string]string{"PG_VERSION": "18.7-1.pgdg13+2", "GOSU_VERSION": "1.19"}
	if !reflect.DeepEqual(meta.Env, want) {
		t.Errorf("env = %v, want the image's own %v", meta.Env, want)
	}
	for _, v := range meta.Env {
		if strings.Contains(v, "hunter2") {
			t.Fatalf("a container env value leaked: %v", meta.Env)
		}
	}
}
