package docker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

// TestRuntimeStrategiesCoverEveryBuilder pins the wire enum to the builder:
// every strategy maps onto it, and nothing in it is unreachable.
func TestRuntimeStrategiesCoverEveryBuilder(t *testing.T) {
	reached := map[string]bool{"dockerfile": true}
	for key := range buildStrategies {
		identity, ok := runtimeIdentities[key]
		if !ok {
			t.Errorf("strategy %s has no runtime identity", key)
			continue
		}
		if !RuntimeStrategies[identity.strategy] {
			t.Errorf("strategy %s maps to %q, outside the wire enum", key, identity.strategy)
		}
		reached[identity.strategy] = true
	}
	for key := range runtimeIdentities {
		if _, ok := buildStrategies[key]; !ok {
			t.Errorf("runtime identity %s names no strategy", key)
		}
	}
	if !reflect.DeepEqual(reached, RuntimeStrategies) {
		t.Errorf("reachable = %v, enum = %v", reached, RuntimeStrategies)
	}
}

func TestDockerfileRuntimeImage(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"single", "FROM node:20-alpine\nCMD [\"node\"]\n", "node:20-alpine"},
		{"last stage wins", "FROM golang:1.22 AS build\nRUN go build\nFROM alpine:3.20\nCOPY --from=build /app /app\n", "alpine:3.20"},
		{"as suffix stripped", "FROM python:3.12-slim AS runner\n", "python:3.12-slim"},
		{"lowercase keywords", "from node:20 as base\n", "node:20"},
		{"platform flag stripped", "FROM --platform=linux/amd64 nginx:1.27-alpine\n", "nginx:1.27-alpine"},
		{"platform variable is only a flag", "FROM --platform=$BUILDPLATFORM golang:1.22 AS build\nFROM --platform=$TARGETPLATFORM node:20\n", "node:20"},
		{"stage alias inherits", "FROM node:20-alpine AS base\nRUN npm ci\nFROM base AS runner\n", "node:20-alpine"},
		{"stage alias case-insensitive", "FROM node:20 AS Base\nFROM base\n", "node:20"},
		{"arg ref omitted", "ARG NODE=20\nFROM node:${NODE}-alpine\n", ""},
		{"bare arg omitted", "ARG BASE\nFROM $BASE\n", ""},
		{"alias of arg omitted", "FROM ${BASE} AS b\nFROM b\n", ""},
		{"arg in earlier stage only", "FROM ${BUILDER} AS build\nFROM node:20\n", "node:20"},
		{"scratch omitted", "FROM golang:1.22 AS build\nFROM scratch\n", ""},
		{"digest ref kept", "FROM node@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n", "node@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"registry ref kept", "FROM ghcr.io/acme/base:1.2\n", "ghcr.io/acme/base:1.2"},
		{"comments ignored", "# FROM evil:1\nFROM node:20\n# FROM other:2\n", "node:20"},
		{"shell metacharacters omitted", "FROM node:20;rm\n", ""},
		{"no from", "RUN echo hi\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := dockerfileRuntimeImage(tc.content); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEnvVersion(t *testing.T) {
	env := []string{"PATH=/usr/bin", "NODE_VERSION=20.11.1", "YARN_VERSION=1.22.19", "PYTHON_VERSION=3.12.2rc1", "PHP_VERSION=v8.3", "GOLANG_VERSION=1.22.5; rm -rf /", "RUST_VERSION="}
	cases := map[string]string{
		"NODE_VERSION":   "20.11.1",
		"PYTHON_VERSION": "3.12.2rc1",
		"PHP_VERSION":    "", // must start with a digit
		"GOLANG_VERSION": "", // shell characters
		"RUST_VERSION":   "", // empty
		"BUN_VERSION":    "", // absent
		"NODE":           "", // a key prefix is not the key
		"YARN_VERSION":   "1.22.19",
	}
	for key, want := range cases {
		if got := envVersion(env, key); got != want {
			t.Errorf("%s: got %q, want %q", key, got, want)
		}
	}
	if edge := "1" + strings.Repeat("0", 31); envVersion([]string{"NODE_VERSION=" + edge}, "NODE_VERSION") != edge {
		t.Errorf("a 32-char version was refused")
	}
	if long := "1" + strings.Repeat("0", 32); envVersion([]string{"NODE_VERSION=" + long}, "NODE_VERSION") != "" {
		t.Errorf("a 33-char version passed")
	}
	if got := envVersion([]string{"NODE_VERSION=1.0.0+build-7"}, "NODE_VERSION"); got != "1.0.0+build-7" {
		t.Errorf("build metadata rejected: %q", got)
	}
}

func TestSoleEnvVersion(t *testing.T) {
	if got := soleEnvVersion([]string{"NODE_VERSION=20.11.1", "YARN_VERSION=1.22.19"}); got != "20.11.1" {
		t.Errorf("one language: %q", got)
	}
	if got := soleEnvVersion([]string{"NODE_VERSION=20.11.1", "PYTHON_VERSION=3.12.2"}); got != "" {
		t.Errorf("two languages must have no answer: %q", got)
	}
	if got := soleEnvVersion([]string{"PATH=/bin"}); got != "" {
		t.Errorf("no language: %q", got)
	}
}

func TestRuntimeMapOmitsEmptyMembers(t *testing.T) {
	if got := (Runtime{Strategy: "go"}).Map(); !reflect.DeepEqual(got, map[string]any{"strategy": "go"}) {
		t.Errorf("strategy only: %v", got)
	}
	full := Runtime{Strategy: "node-npm", Image: "node:20-alpine", Version: "20.11.1"}.Map()
	if !reflect.DeepEqual(full, map[string]any{"strategy": "node-npm", "image": "node:20-alpine", "version": "20.11.1"}) {
		t.Errorf("full: %v", full)
	}
	if got := (Runtime{Strategy: "NODE_NPM", Image: "node:20"}).Map(); got != nil {
		t.Errorf("internal key leaked: %v", got)
	}
	if got := (Runtime{}).Map(); got != nil {
		t.Errorf("zero runtime: %v", got)
	}
	if got := (Runtime{Strategy: "static", Image: "nginx alpine", Version: "x"}).Map(); !reflect.DeepEqual(got, map[string]any{"strategy": "static"}) {
		t.Errorf("invalid members kept: %v", got)
	}
}

func TestRuntimeRowRevalidates(t *testing.T) {
	stored := map[string]any{"strategy": "python", "image": "python:3.12-slim", "version": "3.12.2", "extra": "x"}
	want := map[string]any{"strategy": "python", "image": "python:3.12-slim", "version": "3.12.2"}
	if got := RuntimeRow(stored); !reflect.DeepEqual(got, want) {
		t.Errorf("row = %v", got)
	}
	for _, bad := range []any{nil, "node", map[string]any{"strategy": "cobol"}, map[string]any{"image": "node:20"}, map[string]any{"strategy": float64(1)}} {
		if got := RuntimeRow(bad); got != nil {
			t.Errorf("RuntimeRow(%v) = %v", bad, got)
		}
	}
}

func withEnv(env ...string) image.InspectResponse {
	cfg := &dockerspec.DockerOCIImageConfig{}
	cfg.Env = env
	return image.InspectResponse{Config: cfg}
}

func TestBuildReportsInterpreterRuntime(t *testing.T) {
	f := newFakeAPI()
	f.images["node:20-alpine"] = withEnv("NODE_VERSION=20.11.1", "YARN_VERSION=1.22.19")
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"package.json": `{"engines":{"node":">=20"}}`, "pnpm-lock.yaml": ""})
	rt, err := c.Build(src, "odac-app-x", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Runtime{Strategy: "node-pnpm", Image: "node:20-alpine", Version: "20.11.1"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

// TestBuildReportsCompilerRuntime pins compiled languages to the compiler
// image: the runtime base is a plain alpine with no toolchain version.
func TestBuildReportsCompilerRuntime(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:1.22-alpine"] = withEnv("GOLANG_VERSION=1.22.5")
	f.images["alpine:latest"] = withEnv("PATH=/bin")
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"go.mod": "module x\n\ngo 1.22\n"})
	rt, err := c.Build(src, "odac-app-x", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Runtime{Strategy: "go", Image: "golang:1.22-alpine", Version: "1.22.5"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

func TestBuildReportsVersionedRuntimeBase(t *testing.T) {
	f := newFakeAPI()
	f.images["composer:lts"] = withEnv("PHP_VERSION=8.4.1", "COMPOSER_VERSION=2.8.0")
	f.images["php:8.3-apache"] = withEnv("PHP_VERSION=8.3.14")
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"composer.json": `{"require":{"php":"^8.3"}}`})
	rt, err := c.Build(src, "odac-app-x", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Runtime{Strategy: "php", Image: "php:8.3-apache", Version: "8.3.14"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

func TestBuildFailureStillNamesRuntime(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:alpine"] = withEnv("GOLANG_VERSION=1.23.0")
	c := newTestClient(t, f)
	f.waitCodes["ctr1"] = 2

	src := writeFiles(t, map[string]string{"go.mod": "module x\n"})
	rt, err := c.Build(src, "img", "x", nil)
	if err == nil {
		t.Fatal("compile failure not reported")
	}
	if want := (Runtime{Strategy: "go", Image: "golang:alpine", Version: "1.23.0"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

func TestBuildReportsDockerfileRuntime(t *testing.T) {
	f := newFakeAPI()
	f.images[packagerImage] = image.InspectResponse{}
	// The built image inherits the base's Env.
	f.images["custom-img"] = withEnv("NODE_VERSION=22.1.0")
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM node:22-alpine AS base\nFROM base\n"})
	rt, err := c.Build(src, "custom-img", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Runtime{Strategy: "dockerfile", Image: "node:22-alpine", Version: "22.1.0"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

// TestFailedDockerfileBuildReadsNoVersion pins the tag rule: after a failed
// build the image name still points at the previous build, whose Env would
// be reported as this one's.
func TestFailedDockerfileBuildReadsNoVersion(t *testing.T) {
	f := newFakeAPI()
	f.images[packagerImage] = image.InspectResponse{}
	f.images["custom-img"] = withEnv("NODE_VERSION=18.0.0")
	f.waitCodes["ctr1"] = 1
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM node:22-alpine\n"})
	rt, err := c.Build(src, "custom-img", "x", nil)
	if err == nil {
		t.Fatal("build failure not reported")
	}
	if want := (Runtime{Strategy: "dockerfile", Image: "node:22-alpine"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

func TestBuildUndetectableHasNoRuntime(t *testing.T) {
	c := newTestClient(t, newFakeAPI())
	src := writeFiles(t, map[string]string{"README.md": "x"})
	if rt, err := c.Build(src, "img", "x", nil); err == nil || rt != (Runtime{}) {
		t.Errorf("rt = %+v, err = %v", rt, err)
	}
}
