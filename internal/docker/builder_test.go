package docker

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// detect is detectStrategy with no override, which never fails.
func detect(t *testing.T, dir string) *buildStrategy {
	t.Helper()
	s, err := detectStrategy(dir, "")
	if err != nil {
		t.Fatalf("detectStrategy without override: %v", err)
	}
	return s
}

func TestDetectStrategy(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string // strategy name, "" for nil
	}{
		{"custom dockerfile wins", map[string]string{"Dockerfile": "FROM x", "package.json": "{}"}, "Custom Dockerfile"},
		{"python by requirements", map[string]string{"requirements.txt": ""}, "Python"},
		{"go by go.mod", map[string]string{"go.mod": "module x"}, "Go"},
		{"rust by cargo", map[string]string{"Cargo.toml": ""}, "Rust"},
		{"bun by lockfile", map[string]string{"bun.lockb": "", "package.json": "{}"}, "Bun"},
		{"pnpm before yarn/npm", map[string]string{"pnpm-lock.yaml": "", "yarn.lock": "", "package-lock.json": ""}, "Node.js (pnpm)"},
		{"yarn before npm", map[string]string{"yarn.lock": "", "package-lock.json": ""}, "Node.js (yarn)"},
		{"npm by package-lock", map[string]string{"package-lock.json": ""}, "Node.js (npm)"},
		{"bare package.json falls back to npm", map[string]string{"package.json": "{}"}, "Node.js (npm)"},
		{"python beats go", map[string]string{"pyproject.toml": "", "go.mod": "module x"}, "Python"},
		{"php by composer", map[string]string{"composer.json": "{}"}, "PHP"},
		{"php by index.php", map[string]string{"index.php": "<?php"}, "PHP"},
		{"static by index.html", map[string]string{"index.html": "<html>"}, "Static Web"},
		{"nothing", map[string]string{"README.md": "hi"}, ""},
	}
	for _, c := range cases {
		dir := writeFiles(t, c.files)
		got := detect(t, dir)
		if c.want == "" {
			if got != nil {
				t.Errorf("%s: detected %q, want nil", c.name, got.name)
			}
			continue
		}
		if got == nil || got.name != c.want {
			t.Errorf("%s: detected %v, want %s", c.name, got, c.want)
		}
	}
}

// Interpreted languages run on the image that installed their dependencies:
// a native addon or wheel built for Node 20 or Python 3.12 does not load on
// whatever lts or 3 points at.
func TestResolveImageVersions(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		image   string
		runtime string
	}{
		{"go version from go.mod", map[string]string{"go.mod": "module x\n\ngo 1.22.5\n"}, "golang:1.22-alpine", "alpine:latest"},
		{"go default", map[string]string{"go.mod": "module x\n"}, "golang:alpine", "alpine:latest"},
		{"node engines", map[string]string{"package-lock.json": "", "package.json": `{"engines":{"node":">=20.11"}}`}, "node:20-alpine", "node:20-alpine"},
		{"node default", map[string]string{"package-lock.json": "", "package.json": `{}`}, "node:lts-alpine", "node:lts-alpine"},
		{"pnpm engines", map[string]string{"pnpm-lock.yaml": "", "package.json": `{"engines":{"node":"22"}}`}, "node:22-alpine", "node:22-alpine"},
		{"bun", map[string]string{"bun.lock": "", "package.json": `{}`}, "oven/bun:alpine", "oven/bun:alpine"},
		{"php composer require", map[string]string{"composer.json": `{"require":{"php":"^8.3"}}`}, "composer:lts", "php:8.3-apache"},
		{"php default", map[string]string{"index.php": "<?php"}, "composer:lts", "php:8.2-apache"},
		{"python pyproject", map[string]string{"pyproject.toml": `requires-python = ">=3.12"`}, "python:3.12-slim", "python:3.12-slim"},
		{"python runtime.txt fallback", map[string]string{"requirements.txt": "", "runtime.txt": "python-3.11.4"}, "python:3.11-slim", "python:3.11-slim"},
		{"python default", map[string]string{"requirements.txt": ""}, "python:3-slim", "python:3-slim"},
		{"rust toolchain toml", map[string]string{"Cargo.toml": "", "rust-toolchain.toml": `channel = "1.78.0"`}, "rust:1.78-alpine", "alpine:latest"},
		{"rust bare toolchain", map[string]string{"Cargo.toml": "", "rust-toolchain": "1.75.0\n"}, "rust:1.75-alpine", "alpine:latest"},
		{"static no resolver", map[string]string{"index.html": ""}, "alpine:latest", "nginx:alpine"},
	}
	for _, c := range cases {
		dir := writeFiles(t, c.files)
		got := detect(t, dir)
		if got == nil {
			t.Errorf("%s: no strategy", c.name)
			continue
		}
		if got.image != c.image || got.runtime != c.runtime {
			t.Errorf("%s: images = %q, %q, want %q, %q", c.name, got.image, got.runtime, c.image, c.runtime)
		}
	}
}

func TestDockerfileBaseImages(t *testing.T) {
	got := dockerfileBaseImages(`# syntax=docker/dockerfile:1
ARG GO=1.26
FROM golang:${GO}-alpine AS build
from node:22-alpine as assets
FROM --platform=linux/amd64 alpine:3.22 AS cross
FROM build AS test
FROM ASSETS
FROM scratch
FROM gcr.io/distroless/static:nonroot
FROM node:22-alpine
`)
	want := []string{"node:22-alpine", "gcr.io/distroless/static:nonroot"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bases = %v, want %v", got, want)
	}

	many := strings.Repeat("FROM a\nFROM b\nFROM c\nFROM d\nFROM e\nFROM f\nFROM g\nFROM h\nFROM i\n", 2)
	if n := len(dockerfileBaseImages(many)); n != maxDockerfileBases {
		t.Errorf("refreshed %d bases, want the cap %d", n, maxDockerfileBases)
	}
}

func TestGenerateDockerfile(t *testing.T) {
	got := generateDockerfile("alpine:latest", buildStrategies["GO"].pkg)
	want := "\nFROM alpine:latest\nWORKDIR /app\nCOPY . .\nUSER root\nRUN chown -R nobody:nobody /app\nUSER nobody\nCMD [\"/app/app\"]\n"
	if got != want {
		t.Errorf("dockerfile = %q, want %q", got, want)
	}

	// PHP: setup commands rendered as RUN lines between chown and USER.
	php := generateDockerfile("php:8.2-apache", buildStrategies["PHP"].pkg)
	if !strings.Contains(php, "RUN sed -ri -e \"s!/var/www/html!/app!g\"") {
		t.Errorf("php setup missing: %q", php)
	}
	if strings.Index(php, "RUN sed") < strings.Index(php, "RUN chown") {
		t.Error("setup must come after chown")
	}
	if strings.Index(php, "USER www-data") < strings.Index(php, "RUN sed") {
		t.Error("USER must come after setup")
	}

	// PYTHON: env rendered.
	py := generateDockerfile("python:3-slim", buildStrategies["PYTHON"].pkg)
	if !strings.Contains(py, "ENV PYTHONPATH=\"/app/deps\"") {
		t.Errorf("python env missing: %q", py)
	}
	// python:3-slim is Debian, where nobody's group is nogroup.
	if !strings.Contains(py, "RUN chown -R nobody:nogroup /app\n") {
		t.Errorf("python chown: %q", py)
	}
}

func TestBuildTwoStagePipeline(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:1.22-alpine"] = image.InspectResponse{}
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)
	c.hostRoot = "/var/odac" // exercise DooD path mapping

	src := writeFiles(t, map[string]string{"go.mod": "module x\n\ngo 1.22\n"})

	if _, err := c.Build(src, "odac-app-x", "x", "", nil); err != nil {
		t.Fatal(err)
	}

	if len(f.created) != 2 {
		t.Fatalf("created %d containers, want compile+package", len(f.created))
	}

	compile := f.created[0]
	if compile.Config.Image != "golang:1.22-alpine" {
		t.Errorf("compile image = %q", compile.Config.Image)
	}
	if !compile.HostConfig.AutoRemove || compile.HostConfig.Privileged {
		t.Errorf("compile hostcfg = %+v", compile.HostConfig)
	}
	assertSandboxed(t, f, compile.HostConfig)
	if compile.Config.WorkingDir != "/app" {
		t.Errorf("compile workdir = %q", compile.Config.WorkingDir)
	}
	if compile.HostConfig.Binds[0] != src+":/app" {
		t.Errorf("compile bind = %v (src under temp is outside /app, must pass through)", compile.HostConfig.Binds)
	}
	joined := compile.Config.Cmd[2]
	if !strings.Contains(joined, "go mod download") || !strings.Contains(joined, "go build -o app") {
		t.Errorf("compile cmd = %q", joined)
	}
	env := strings.Join(compile.Config.Env, ",")
	if !strings.Contains(env, "CI=true") || !strings.Contains(env, "TERM=dumb") {
		t.Errorf("compile env = %v", compile.Config.Env)
	}

	pack := f.created[1]
	if pack.Config.Image != packagerImage {
		t.Errorf("packager image = %q", pack.Config.Image)
	}
	if pack.HostConfig.Binds[0] != "/var/run/docker.sock:/var/run/docker.sock" {
		t.Errorf("packager binds = %v", pack.HostConfig.Binds)
	}
	if got := pack.Config.Cmd[2]; got != "docker build --progress=plain -f /app/Dockerfile.odac -t odac-app-x /app" {
		t.Errorf("package cmd = %q", got)
	}

	// Floating tags are pulled although cached; the digest-pinned packager is not.
	if want := []string{"golang:1.22-alpine", "alpine:latest"}; !reflect.DeepEqual(f.pulled, want) {
		t.Errorf("pulled = %v, want %v", f.pulled, want)
	}

	// Ephemeral build files cleaned up.
	if _, err := os.Stat(filepath.Join(src, "Dockerfile.odac")); !os.IsNotExist(err) {
		t.Error("Dockerfile.odac not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(src, ".dockerignore")); !os.IsNotExist(err) {
		t.Error(".dockerignore not cleaned up")
	}
}

func TestBuildCustomDockerfile(t *testing.T) {
	f := newFakeAPI()
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch"})
	if _, err := c.Build(src, "custom-img", "x", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d containers, want 1 (custom fast track)", len(f.created))
	}
	if got := f.created[0].Config.Cmd[2]; got != "docker build --progress=plain -t custom-img /app" {
		t.Errorf("custom cmd = %q", got)
	}
}

func TestBuildCustomDockerfileRefreshesBases(t *testing.T) {
	f := newFakeAPI()
	f.images[packagerImage] = image.InspectResponse{}
	f.pullBodies["local-only:1"] = `{"error":"pull access denied"}` + "\n"
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM golang:1.26-alpine AS build\nFROM local-only:1\n"})
	if _, err := c.Build(src, "custom-img", "x", "", nil); err != nil {
		t.Fatalf("a base the registry refuses must be left to docker build: %v", err)
	}
	if want := []string{"golang:1.26-alpine", "local-only:1"}; !reflect.DeepEqual(f.pulled, want) {
		t.Errorf("pulled = %v, want %v", f.pulled, want)
	}
	if len(f.created) != 1 {
		t.Errorf("created %d containers, want the build", len(f.created))
	}
}

func TestBuildFailureExitCode(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:alpine"] = image.InspectResponse{}
	c := newTestClient(t, f)
	f.waitCodes["ctr1"] = 2 // compile container fails

	src := writeFiles(t, map[string]string{"go.mod": "module x\n"})
	_, err := c.Build(src, "img", "x", "", nil)
	if err == nil || err.Error() != "Compilation failed with exit code 2" {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsUndetectableProject(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	src := writeFiles(t, map[string]string{"README.md": "x"})
	_, err := c.Build(src, "img", "x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "Could not detect project type") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsParallelSameImage(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	c.mu.Lock()
	c.activeBuilds["img"] = true
	c.mu.Unlock()

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch"})
	_, err := c.Build(src, "img", "x", "", nil)
	if err == nil || err.Error() != "Build already in progress for img" {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsUnsafeImageName(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch"})
	_, err := c.Build(src, "img;touch /pwned", "x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "Invalid image name") {
		t.Errorf("err = %v, want Invalid image name", err)
	}
	// A shell-injecting name must never reach the daemon.
	if len(f.created) != 0 {
		t.Errorf("created %d containers, want 0", len(f.created))
	}
}

func TestBuildSelfCreatedLoggerFinalizes(t *testing.T) {
	f := newFakeAPI()
	f.images[packagerImage] = image.InspectResponse{}
	logsRoot := t.TempDir()
	c := New(f, Options{LogsRoot: logsRoot})

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch"})
	if _, err := c.Build(src, "img", "myapp", "", nil); err != nil {
		t.Fatal(err)
	}

	// The builder created and finalized its own build summary.
	builds, err := os.ReadDir(filepath.Join(logsRoot, "myapp", "builds"))
	if err != nil {
		t.Fatal(err)
	}
	var haveJSON bool
	for _, de := range builds {
		if strings.HasSuffix(de.Name(), ".json") {
			haveJSON = true
		}
	}
	if !haveJSON {
		t.Error("self-created build logger did not write a summary")
	}
}

// assertSandboxed checks the build sandbox: never the host's network stack,
// every capability that reaches past the container's own files dropped, no
// setuid escalation, a process limit, and a build network that keeps
// concurrent builds apart.
func assertSandboxed(t *testing.T, f *fakeAPI, hc *container.HostConfig) {
	t.Helper()
	if string(hc.NetworkMode) != BuildNetwork {
		t.Errorf("network = %q, want %s", hc.NetworkMode, BuildNetwork)
	}
	if hc.Privileged {
		t.Error("build sandbox must not be privileged")
	}
	if !reflect.DeepEqual([]string(hc.CapDrop), []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	for _, c := range hc.CapAdd {
		if c == "NET_RAW" || c == "NET_BIND_SERVICE" || c == "NET_ADMIN" || c == "SYS_ADMIN" {
			t.Errorf("CapAdd carries %s", c)
		}
	}
	if !reflect.DeepEqual(hc.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("SecurityOpt = %v", hc.SecurityOpt)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit <= 0 {
		t.Errorf("PidsLimit = %v, want a positive limit", hc.PidsLimit)
	}
	opts, ok := f.netCreateOpts[BuildNetwork]
	if !ok {
		t.Fatalf("%s was not created", BuildNetwork)
	}
	if opts.Internal || opts.Options[iccOption] != "false" {
		t.Errorf("%s options = %+v, want egress with icc off", BuildNetwork, opts)
	}
}

func TestBuildDoesNotWriteThroughPlantedSymlinks(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:alpine"] = image.InspectResponse{}
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	outside := t.TempDir()
	victims := map[string]string{
		".dockerignore":   filepath.Join(outside, "certs.db"),
		"Dockerfile.odac": filepath.Join(outside, "config.json"),
	}
	src := writeFiles(t, map[string]string{"go.mod": "module x\n"})
	for name, target := range victims {
		if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.Build(src, "img", "x", "", nil); err != nil {
		t.Fatal(err)
	}
	for name, target := range victims {
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "precious" {
			t.Errorf("%s was written through its symlink: %q, %v", name, got, err)
		}
	}
}

func TestResolveImageReadsOnlyRegularFiles(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "package.json")
	if err := os.WriteFile(outside, []byte(`{"engines":{"node":"18"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := writeFiles(t, map[string]string{"package-lock.json": ""})
	if err := os.Symlink(outside, filepath.Join(src, "package.json")); err != nil {
		t.Fatal(err)
	}
	if got := detect(t, src); got == nil || got.image != "node:lts-alpine" {
		t.Errorf("symlinked package.json was followed: %+v", got)
	}

	// /dev/zero never ends, so following this link would never return.
	zero := writeFiles(t, map[string]string{"package-lock.json": ""})
	if err := os.Symlink("/dev/zero", filepath.Join(zero, "package.json")); err != nil {
		t.Fatal(err)
	}
	if got := detect(t, zero); got == nil || got.image != "node:lts-alpine" {
		t.Errorf("image = %+v, want the default", got)
	}

	big := writeFiles(t, map[string]string{"go.mod": strings.Repeat("\n", maxProjectFile) + "go 1.22\n"})
	if got := detect(t, big); got == nil || got.image != "golang:alpine" {
		t.Errorf("read past maxProjectFile: %+v", got)
	}
}

func TestVersionFileParsing(t *testing.T) {
	node := []struct{ in, want string }{
		{"20", "20"},
		{"v20.11.0\n", "20"},
		{"20.11", "20"},
		{"  22  \r\n", "22"},
		{"# pinned for prod\n\nv18.19.1 # LTS\n", "18"},
		{"lts/*", ""},
		{"lts/iron", ""},
		{"node", ""},
		{"stable", ""},
		{"20.x", ""},
		{"123", ""},
		{"v20.1.2.3", ""},
		{"20; rm -rf /", ""},
		{"", ""},
		{"\x00\xff", ""},
	}
	for _, c := range node {
		if got := parseNodeVersionFile(c.in); got != c.want {
			t.Errorf("node %q = %q, want %q", c.in, got, c.want)
		}
	}
	python := []struct{ in, want string }{
		{"3.12.1\n", "3.12"},
		{"3.12", "3.12"},
		{" 3.11.9 \r\n3.10\n", "3.11"},
		{"# comment\n3.13", "3.13"},
		{"system", ""},
		{"pypy3.10-7.3.17", ""},
		{"3.12/envs/web", ""},
		{"3", ""},
		{"2.7.18", ""},
		{"3.12.0rc1", ""},
		{"", ""},
	}
	for _, c := range python {
		if got := parsePythonVersionFile(c.in); got != c.want {
			t.Errorf("python %q = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVersionSourcePrecedence pins which file decides a project's language
// version when several name one, and that the deciding file is reported.
func TestVersionSourcePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		image  string
		source string
	}{
		{"nvmrc first", map[string]string{"package-lock.json": "", ".nvmrc": "v20.11.0\n", ".node-version": "18", "package.json": `{"engines":{"node":"16"}}`}, "node:20-alpine", ".nvmrc"},
		{"nvmrc alias falls to node-version", map[string]string{"package-lock.json": "", ".nvmrc": "lts/iron", ".node-version": "18.19.0", "package.json": `{"engines":{"node":"16"}}`}, "node:18-alpine", ".node-version"},
		{"aliases fall to engines", map[string]string{"yarn.lock": "", ".nvmrc": "lts/*", ".node-version": "node", "package.json": `{"engines":{"node":">=16"}}`}, "node:16-alpine", "package.json"},
		{"garbage falls to engines", map[string]string{"pnpm-lock.yaml": "", ".nvmrc": "please use 20", "package.json": `{"engines":{"node":"22"}}`}, "node:22-alpine", "package.json"},
		{"node default", map[string]string{"package-lock.json": "", ".nvmrc": "lts/*", "package.json": `{}`}, "node:lts-alpine", "default"},
		{"python-version first", map[string]string{".python-version": "3.12.1\n", "pyproject.toml": `requires-python = ">=3.11"`, "runtime.txt": "python-3.10.0"}, "python:3.12-slim", ".python-version"},
		{"python-version system falls to pyproject", map[string]string{".python-version": "system", "pyproject.toml": `requires-python = ">=3.11"`}, "python:3.11-slim", "pyproject.toml"},
		{"pyproject without version falls to runtime.txt", map[string]string{"requirements.txt": "", "pyproject.toml": "[project]", "runtime.txt": "python-3.10.4"}, "python:3.10-slim", "runtime.txt"},
		{"python default", map[string]string{"requirements.txt": "", ".python-version": "pypy3.10"}, "python:3-slim", "default"},
		{"go.mod", map[string]string{"go.mod": "module x\n\ngo 1.22.5\n"}, "golang:1.22-alpine", "go.mod"},
		{"go default", map[string]string{"go.mod": "module x\n"}, "golang:alpine", "default"},
		{"rust toml first", map[string]string{"Cargo.toml": "", "rust-toolchain.toml": `channel = "1.78.0"`, "rust-toolchain": "1.70"}, "rust:1.78-alpine", "rust-toolchain.toml"},
		{"rust bare file", map[string]string{"Cargo.toml": "", "rust-toolchain": "1.75.0\n"}, "rust:1.75-alpine", "rust-toolchain"},
		{"composer", map[string]string{"composer.json": `{"require":{"php":"^8.3"}}`}, "composer:lts", "composer.json"},
		{"bun has no resolver", map[string]string{"bun.lock": "", ".nvmrc": "20", "package.json": `{"engines":{"node":"20"}}`}, "oven/bun:alpine", "default"},
		{"static has no resolver", map[string]string{"index.html": ""}, "alpine:latest", "default"},
		{"dockerfile", map[string]string{"Dockerfile": "FROM node:20", ".nvmrc": "22"}, "", "dockerfile"},
	}
	for _, c := range cases {
		got := detect(t, writeFiles(t, c.files))
		if got == nil {
			t.Errorf("%s: no strategy", c.name)
			continue
		}
		if got.image != c.image || got.source != c.source {
			t.Errorf("%s: image %q from %q, want %q from %q", c.name, got.image, got.source, c.image, c.source)
		}
	}
}

func TestNewVersionFilesRespectReadBounds(t *testing.T) {
	big := writeFiles(t, map[string]string{
		"package-lock.json": "",
		".nvmrc":            strings.Repeat("\n", maxProjectFile) + "20\n",
		"package.json":      `{"engines":{"node":"18"}}`,
	})
	if got := detect(t, big); got.image != "node:18-alpine" || got.source != "package.json" {
		t.Errorf("read past maxProjectFile: %q from %q", got.image, got.source)
	}

	zero := writeFiles(t, map[string]string{"requirements.txt": ""})
	if err := os.Symlink("/dev/zero", filepath.Join(zero, ".python-version")); err != nil {
		t.Fatal(err)
	}
	if got := detect(t, zero); got.image != "python:3-slim" || got.source != "default" {
		t.Errorf("symlinked .python-version: %q from %q", got.image, got.source)
	}
}

func TestRuntimeOverride(t *testing.T) {
	node := map[string]string{"package-lock.json": "", ".nvmrc": "18", "package.json": `{"engines":{"node":"16"}}`}
	cases := []struct {
		name     string
		files    map[string]string
		override string
		image    string
		runtime  string
		source   string
		bad      bool
	}{
		{"node major beats nvmrc", node, "22", "node:22-alpine", "node:22-alpine", "override", false},
		{"node minor refused", node, "22.1", "", "", "", true},
		{"node v-prefix refused", node, "v22", "", "", "", true},
		{"node three digits refused", node, "220", "", "", "", true},
		{"python family on node refused", node, "3.12", "", "", "", true},
		{"python", map[string]string{"requirements.txt": "", ".python-version": "3.11"}, "3.12", "python:3.12-slim", "python:3.12-slim", "override", false},
		{"python major only refused", map[string]string{"requirements.txt": ""}, "3", "", "", "", true},
		{"python 2 refused", map[string]string{"requirements.txt": ""}, "2.7", "", "", "", true},
		{"go", map[string]string{"go.mod": "module x\n\ngo 1.21\n"}, "1.23", "golang:1.23-alpine", "alpine:latest", "override", false},
		{"go patch refused", map[string]string{"go.mod": "module x\n"}, "1.23.1", "", "", "", true},
		{"rust", map[string]string{"Cargo.toml": ""}, "1.80", "rust:1.80-alpine", "alpine:latest", "override", false},
		{"rust four digits refused", map[string]string{"Cargo.toml": ""}, "1.8000", "", "", "", true},
		{"php keeps composer", map[string]string{"composer.json": `{"require":{"php":"^8.2"}}`}, "8.3", "composer:lts", "php:8.3-apache", "override", false},
		{"php major only refused", map[string]string{"index.php": ""}, "8", "", "", "", true},
		{"shell metacharacters refused", node, "22;id", "", "", "", true},
		{"bun ignores", map[string]string{"bun.lock": "", "package.json": `{}`}, "22", "oven/bun:alpine", "oven/bun:alpine", "default", false},
		{"bun ignores garbage too", map[string]string{"bun.lock": "", "package.json": `{}`}, "$(id)", "oven/bun:alpine", "oven/bun:alpine", "default", false},
		{"static ignores", map[string]string{"index.html": ""}, "22", "alpine:latest", "nginx:alpine", "default", false},
		{"dockerfile ignores", map[string]string{"Dockerfile": "FROM node:20"}, "22", "", "", "dockerfile", false},
	}
	for _, c := range cases {
		got, err := detectStrategy(writeFiles(t, c.files), c.override)
		if got == nil {
			t.Errorf("%s: no strategy", c.name)
			continue
		}
		if c.bad {
			if !errors.Is(err, ErrUnsupportedRuntimeVersion) || got.image != "" || got.runtime != "" {
				t.Errorf("%s: err %v, images %q %q", c.name, err, got.image, got.runtime)
			}
			continue
		}
		if err != nil || got.image != c.image || got.runtime != c.runtime || got.source != c.source {
			t.Errorf("%s: err %v, images %q %q from %q, want %q %q from %q", c.name, err, got.image, got.runtime, got.source, c.image, c.runtime, c.source)
		}
	}
}

func TestValidRuntimeOverride(t *testing.T) {
	for _, v := range []string{"22", "8", "3.12", "1.23", "1.800", "8.4"} {
		if !ValidRuntimeOverride(v) {
			t.Errorf("%q refused", v)
		}
	}
	for _, v := range []string{"", "v22", "220", "22.1.1", "lts", "3.12-slim", "1.2345", "22 ", " 22", "22\n", "../22", "22;id"} {
		if ValidRuntimeOverride(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

// TestBuildRefusesMalformedOverride pins the order: a version the language
// cannot take fails before anything is pulled or run, so nothing about the
// running app is touched.
func TestBuildRefusesMalformedOverride(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	src := writeFiles(t, map[string]string{"package-lock.json": "", "package.json": `{}`})

	rt, err := c.Build(src, "odac-app-x", "x", "3.12", nil)
	if !errors.Is(err, ErrUnsupportedRuntimeVersion) || err.Error() != "unsupported runtime version" {
		t.Fatalf("err = %v", err)
	}
	if len(f.pulled) != 0 || len(f.created) != 0 {
		t.Errorf("pulled %v, created %d containers", f.pulled, len(f.created))
	}
	if want := (Runtime{Strategy: "node-npm"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}

func TestBuildUsesOverrideImage(t *testing.T) {
	f := newFakeAPI()
	f.images["node:22-alpine"] = withEnv("NODE_VERSION=22.11.0")
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)
	src := writeFiles(t, map[string]string{"package-lock.json": "", ".nvmrc": "18", "package.json": `{}`})

	rt, err := c.Build(src, "odac-app-x", "x", "22", nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.created[0].Config.Image != "node:22-alpine" {
		t.Errorf("compile image = %q", f.created[0].Config.Image)
	}
	if want := (Runtime{Strategy: "node-npm", Image: "node:22-alpine", Version: "22.11.0", Source: "override"}); rt != want {
		t.Errorf("runtime = %+v, want %+v", rt, want)
	}
}
