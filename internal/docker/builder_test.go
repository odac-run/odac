package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		got := detectStrategy(dir)
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

func TestResolveImageVersions(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"go version from go.mod", map[string]string{"go.mod": "module x\n\ngo 1.22.5\n"}, "golang:1.22-alpine"},
		{"go default", map[string]string{"go.mod": "module x\n"}, "golang:alpine"},
		{"node engines", map[string]string{"package-lock.json": "", "package.json": `{"engines":{"node":">=20.11"}}`}, "node:20-alpine"},
		{"node default", map[string]string{"package-lock.json": "", "package.json": `{}`}, "node:lts-alpine"},
		{"php composer require", map[string]string{"composer.json": `{"require":{"php":"^8.2"}}`}, "composer:8.2"},
		{"python pyproject", map[string]string{"pyproject.toml": `requires-python = ">=3.12"`}, "python:3.12-slim"},
		{"python runtime.txt fallback", map[string]string{"requirements.txt": "", "runtime.txt": "python-3.11.4"}, "python:3.11-slim"},
		{"python default", map[string]string{"requirements.txt": ""}, "python:3-slim"},
		{"rust toolchain toml", map[string]string{"Cargo.toml": "", "rust-toolchain.toml": `channel = "1.78.0"`}, "rust:1.78-alpine"},
		{"rust bare toolchain", map[string]string{"Cargo.toml": "", "rust-toolchain": "1.75.0\n"}, "rust:1.75-alpine"},
		{"static no resolver", map[string]string{"index.html": ""}, "alpine:latest"},
	}
	for _, c := range cases {
		dir := writeFiles(t, c.files)
		got := detectStrategy(dir)
		if got == nil {
			t.Errorf("%s: no strategy", c.name)
			continue
		}
		if got.image != c.want {
			t.Errorf("%s: image = %q, want %q", c.name, got.image, c.want)
		}
	}
}

func TestGenerateDockerfile(t *testing.T) {
	got := generateDockerfile(buildStrategies["GO"].pkg)
	want := "\nFROM alpine:latest\nWORKDIR /app\nCOPY . .\nUSER root\nRUN chown -R nobody:nobody /app\nUSER nobody\nCMD [\"/app/app\"]\n"
	if got != want {
		t.Errorf("dockerfile = %q, want %q", got, want)
	}

	// PHP: setup commands rendered as RUN lines between chown and USER.
	php := generateDockerfile(buildStrategies["PHP"].pkg)
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
	py := generateDockerfile(buildStrategies["PYTHON"].pkg)
	if !strings.Contains(py, "ENV PYTHONPATH=\"/app/deps\"") {
		t.Errorf("python env missing: %q", py)
	}
}

func TestBuildTwoStagePipeline(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:1.22-alpine"] = image.InspectResponse{}
	f.images[packagerImage] = image.InspectResponse{}
	c := newTestClient(t, f)
	c.hostRoot = "/var/odac" // exercise DooD path mapping

	src := writeFiles(t, map[string]string{"go.mod": "module x\n\ngo 1.22\n"})

	if err := c.Build(src, "odac-app-x", "x", nil); err != nil {
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
	if string(compile.HostConfig.NetworkMode) != "host" {
		t.Errorf("compile network = %q", compile.HostConfig.NetworkMode)
	}
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
	if err := c.Build(src, "custom-img", "x", nil); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d containers, want 1 (custom fast track)", len(f.created))
	}
	if got := f.created[0].Config.Cmd[2]; got != "docker build --progress=plain -t custom-img /app" {
		t.Errorf("custom cmd = %q", got)
	}
}

func TestBuildFailureExitCode(t *testing.T) {
	f := newFakeAPI()
	f.images["golang:alpine"] = image.InspectResponse{}
	c := newTestClient(t, f)
	f.waitCodes["ctr1"] = 2 // compile container fails

	src := writeFiles(t, map[string]string{"go.mod": "module x\n"})
	err := c.Build(src, "img", "x", nil)
	if err == nil || err.Error() != "Compilation failed with exit code 2" {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsUndetectableProject(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	src := writeFiles(t, map[string]string{"README.md": "x"})
	err := c.Build(src, "img", "x", nil)
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
	err := c.Build(src, "img", "x", nil)
	if err == nil || err.Error() != "Build already in progress for img" {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsUnsafeImageName(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)

	src := writeFiles(t, map[string]string{"Dockerfile": "FROM scratch"})
	err := c.Build(src, "img;touch /pwned", "x", nil)
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
	if err := c.Build(src, "img", "myapp", nil); err != nil {
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
