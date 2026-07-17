package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"odac/internal/applog"
)

// This file ports server/src/Container/Builder.js: the ODAC native builder.
// Secure two-stage build (compile → package) that avoids DinD and
// privileged mode entirely — compile runs in an unprivileged runner with
// the source bind-mounted, packaging talks to the host daemon through a
// socket-mounted docker:cli container.

// safeImageRef guards the `docker build -t <imageName>` shell interpolation
// in packageImage/packageCustom. It permits the full image-reference set
// (registry/repo:tag) but rejects every shell metacharacter and whitespace,
// so imageName can never break out of the build command.
var safeImageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)

// versionResolver extracts a Docker tag suffix from a project version file.
type versionResolver struct {
	file         string
	fallbackFile string
	parse        func(content, filename string) string
}

// parseMajorMinor returns the "1.22" of "1.22.5" / "^3.11" / ">=20.0".
var majorMinorRe = regexp.MustCompile(`(\d+\.\d+)`)

func parseMajorMinor(raw string) string {
	return majorMinorRe.FindString(raw)
}

var (
	goDirectiveRe    = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	nodeMajorRe      = regexp.MustCompile(`(\d+)`)
	pythonRuntimeRe  = regexp.MustCompile(`python-(\d+\.\d+)`)
	requiresPythonRe = regexp.MustCompile(`requires-python\s*=\s*["']([^"']+)["']`)
	rustChannelRe    = regexp.MustCompile(`channel\s*=\s*["']([^"']+)["']`)
	versionResolvers = map[string]versionResolver{
		"GO": {file: "go.mod", parse: func(content, _ string) string {
			if m := goDirectiveRe.FindStringSubmatch(content); m != nil {
				return m[1] + "-alpine"
			}
			return ""
		}},
		"NODE": {file: "package.json", parse: func(content, _ string) string {
			var pkg struct {
				Engines struct {
					Node string `json:"node"`
				} `json:"engines"`
			}
			if json.Unmarshal([]byte(content), &pkg) != nil || pkg.Engines.Node == "" {
				return ""
			}
			if m := nodeMajorRe.FindString(pkg.Engines.Node); m != "" {
				return m + "-alpine"
			}
			return ""
		}},
		"PHP": {file: "composer.json", parse: func(content, _ string) string {
			var composer struct {
				Require map[string]string `json:"require"`
			}
			if json.Unmarshal([]byte(content), &composer) != nil {
				return ""
			}
			return parseMajorMinor(composer.Require["php"])
		}},
		"PYTHON": {file: "pyproject.toml", fallbackFile: "runtime.txt", parse: func(content, filename string) string {
			if filename == "runtime.txt" {
				if m := pythonRuntimeRe.FindStringSubmatch(content); m != nil {
					return m[1] + "-slim"
				}
				return ""
			}
			m := requiresPythonRe.FindStringSubmatch(content)
			if m == nil {
				return ""
			}
			if v := parseMajorMinor(m[1]); v != "" {
				return v + "-slim"
			}
			return ""
		}},
		"RUST": {file: "rust-toolchain.toml", fallbackFile: "rust-toolchain", parse: func(content, filename string) string {
			if filename == "rust-toolchain" {
				if v := parseMajorMinor(strings.TrimSpace(content)); v != "" {
					return v + "-alpine"
				}
				return ""
			}
			m := rustChannelRe.FindStringSubmatch(content)
			if m == nil {
				return ""
			}
			if v := parseMajorMinor(m[1]); v != "" {
				return v + "-alpine"
			}
			return ""
		}},
	}
)

// packageSpec describes the final-image Dockerfile of a strategy.
type packageSpec struct {
	baseImage string
	user      string
	cmd       []string
	setup     []string
	env       [][2]string // ordered KEY,VALUE pairs
}

// buildStrategy is one entry of Node's BUILD_STRATEGIES.
type buildStrategy struct {
	key             string
	name            string
	triggers        []string
	imageBase       string
	imageDefault    string
	versionResolver string
	installCmd      string
	buildCmd        string
	cleanupCmd      string
	pkg             *packageSpec
	custom          bool
	image           string // resolved compiler image
}

var buildStrategies = map[string]buildStrategy{
	"BUN": {
		key: "BUN", name: "Bun", triggers: []string{"bun.lock", "bun.lockb"},
		imageBase: "oven/bun", imageDefault: "alpine",
		installCmd: "bun install --frozen-lockfile",
		buildCmd:   "bun run build --if-present",
		cleanupCmd: "bun install --production && rm -rf test tests",
		pkg:        &packageSpec{baseImage: "oven/bun:alpine", user: "bun", cmd: []string{"bun", "run", "start"}},
	},
	"GO": {
		key: "GO", name: "Go", triggers: []string{"go.mod"},
		imageBase: "golang", imageDefault: "alpine", versionResolver: "GO",
		installCmd: "go mod download",
		buildCmd:   `PKG=$(go list -f "{{.Name}} {{.ImportPath}}" ./... | grep "^main " | head -n 1 | cut -d" " -f2); if [ -z "$PKG" ]; then PKG="."; fi; go build -o app $PKG`,
		pkg:        &packageSpec{baseImage: "alpine:latest", user: "nobody", cmd: []string{"/app/app"}},
	},
	"NODE_NPM": {
		key: "NODE_NPM", name: "Node.js (npm)", triggers: []string{"package-lock.json"},
		imageBase: "node", imageDefault: "lts-alpine", versionResolver: "NODE",
		installCmd: "if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; else npm install --no-audit --no-fund; fi",
		buildCmd:   "npm run build --if-present",
		cleanupCmd: "npm prune --production && rm -rf test tests",
		pkg:        &packageSpec{baseImage: "node:lts-alpine", user: "node", cmd: []string{"npm", "start"}},
	},
	"NODE_PNPM": {
		key: "NODE_PNPM", name: "Node.js (pnpm)", triggers: []string{"pnpm-lock.yaml"},
		imageBase: "node", imageDefault: "lts-alpine", versionResolver: "NODE",
		installCmd: "corepack enable && corepack prepare pnpm@latest --activate && pnpm install --frozen-lockfile",
		buildCmd:   "pnpm run build --if-present",
		cleanupCmd: "pnpm prune --prod && rm -rf test tests",
		pkg:        &packageSpec{baseImage: "node:lts-alpine", user: "node", cmd: []string{"npm", "start"}},
	},
	"NODE_YARN": {
		key: "NODE_YARN", name: "Node.js (yarn)", triggers: []string{"yarn.lock"},
		imageBase: "node", imageDefault: "lts-alpine", versionResolver: "NODE",
		installCmd: "corepack enable && yarn install --frozen-lockfile",
		buildCmd:   "yarn run build --if-present",
		cleanupCmd: "yarn install --production --frozen-lockfile && rm -rf test tests",
		pkg:        &packageSpec{baseImage: "node:lts-alpine", user: "node", cmd: []string{"npm", "start"}},
	},
	"PHP": {
		key: "PHP", name: "PHP", triggers: []string{"composer.json", "index.php"},
		imageBase: "composer", imageDefault: "lts", versionResolver: "PHP",
		installCmd: "if [ -f composer.json ]; then composer install --no-dev --ignore-platform-reqs; fi",
		buildCmd:   "true",
		pkg: &packageSpec{
			baseImage: "php:8.2-apache", user: "www-data", cmd: []string{"apache2-foreground"},
			setup: []string{
				`sed -ri -e "s!/var/www/html!/app!g" /etc/apache2/sites-available/*.conf`,
				`sed -ri -e "s!/var/www/!/app!g" /etc/apache2/apache2.conf /etc/apache2/conf-available/*.conf`,
				`chown -R www-data:www-data /var/run/apache2 /var/log/apache2 /var/lock/apache2 /var/lib/apache2`,
			},
		},
	},
	"PYTHON": {
		key: "PYTHON", name: "Python", triggers: []string{"requirements.txt", "pyproject.toml"},
		imageBase: "python", imageDefault: "3-slim", versionResolver: "PYTHON",
		installCmd: "[ ! -f requirements.txt ] || pip install --no-cache-dir -r requirements.txt --target /app/deps",
		buildCmd:   "rm -rf __pycache__",
		pkg: &packageSpec{
			baseImage: "python:3-slim", user: "nobody",
			cmd: []string{"sh", "-c", "if [ -f main.py ]; then python main.py; elif [ -f run.py ]; then python run.py; else python app.py; fi"},
			env: [][2]string{{"PYTHONPATH", "/app/deps"}},
		},
	},
	"RUST": {
		key: "RUST", name: "Rust", triggers: []string{"Cargo.toml", "Cargo.lock"},
		imageBase: "rust", imageDefault: "alpine", versionResolver: "RUST",
		installCmd: "apk add --no-cache musl-dev",
		buildCmd:   `cargo build --release && find target/release -maxdepth 1 -type f -executable -not -name "*.*" | head -n 1 | xargs -I {} cp {} /app/app`,
		cleanupCmd: "rm -rf target src",
		pkg:        &packageSpec{baseImage: "alpine:latest", user: "nobody", cmd: []string{"/app/app"}},
	},
	"STATIC": {
		key: "STATIC", name: "Static Web", triggers: []string{"index.html"},
		imageBase: "alpine", imageDefault: "latest",
		installCmd: "true",
		buildCmd:   "true",
		pkg: &packageSpec{
			baseImage: "nginx:alpine", user: "nginx", cmd: []string{"nginx", "-g", "daemon off;"},
			setup: []string{
				`chown -R nginx:nginx /var/cache/nginx /var/log/nginx /etc/nginx/conf.d`,
				`touch /var/run/nginx.pid && chown nginx:nginx /var/run/nginx.pid`,
				`sed -i "/user  nginx;/d" /etc/nginx/nginx.conf`,
				`sed -i "s|root   /usr/share/nginx/html;|root   /app;|g" /etc/nginx/conf.d/default.conf`,
			},
		},
	},
}

// packagerImage is the socket-mounted docker client used for image builds.
const packagerImage = "docker:cli"

// compileEnv silences interactive/progress output in runner containers.
var compileEnv = []string{
	"CI=true",
	"NPM_CONFIG_SPIN=false",
	"NPM_CONFIG_PROGRESS=false",
	"HUSKY=0",
	"TERM=dumb",
	"PIP_PROGRESS_BAR=off",
	"COMPOSER_NO_INTERACTION=1",
}

// BuildContext carries the two views of the source directory (Builder.js's
// context object): InternalPath for this process's own FS reads/writes,
// HostPath for bind mounts handed to the host's Docker daemon (DooD).
type BuildContext struct {
	InternalPath string
	HostPath     string
	AppName      string
}

// Build ports Container.build + Builder.build: detects the project type,
// then either forwards a custom Dockerfile build or runs the two-stage
// compile/package pipeline, producing imageName. Parallel builds of the
// same image are rejected. When buildLog is nil and AppName is set, the
// builder creates (and finalizes) its own applog build stream; a
// caller-provided buildLog is never finalized here (App finalizes after
// deployment completes).
func (c *Client) Build(sourceDir, imageName, appName string, buildLog BuildLog) error {
	if !c.available {
		return fmt.Errorf("Docker is not available")
	}
	// Defense-in-depth: imageName is interpolated into the `docker build -t`
	// shell command in packageImage/packageCustom, so reject any value
	// carrying shell metacharacters or whitespace before it reaches the
	// builder. Callers (appmgr) also validate app names at their source.
	if !safeImageRef.MatchString(imageName) {
		return fmt.Errorf("Invalid image name: %q", imageName)
	}

	c.mu.Lock()
	if c.activeBuilds[imageName] {
		c.mu.Unlock()
		return fmt.Errorf("Build already in progress for %s", imageName)
	}
	c.activeBuilds[imageName] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.activeBuilds, imageName)
		c.mu.Unlock()
	}()

	name := appName
	if name == "" {
		name = filepath.Base(sourceDir)
	}
	bctx := BuildContext{InternalPath: sourceDir, HostPath: c.ResolveHostPath(sourceDir), AppName: name}
	return c.builderRun(bctx, imageName, buildLog)
}

// builderRun is Builder.build.
func (c *Client) builderRun(bctx BuildContext, imageName string, buildLog BuildLog) error {
	// Self-created logger when the caller did not pass one (standalone
	// builder path). Only this path finalizes the log.
	var ownCtrl *applog.BuildControl
	if buildLog == nil && bctx.AppName != "" && c.logsRoot != "" {
		logger := applog.New(c.logsRoot, bctx.AppName)
		if err := logger.Init(); err == nil {
			ctrl, err := logger.NewBuildStream(fmt.Sprintf("build_%d", nowMs()), map[string]any{
				"image": imageName, "strategy": "detecting...",
			})
			if err == nil {
				ownCtrl = ctrl
				buildLog = ctrl
			}
		} else {
			c.log.Error("Failed to initialize build logger: %s", err.Error())
		}
	}

	if buildLog != nil {
		buildLog.StartPhase("analysis")
	}
	strategy := detectStrategy(bctx.InternalPath)
	if buildLog != nil {
		buildLog.EndPhase("analysis", true)
		if strategy != nil {
			fmt.Fprintf(buildLog, "[Builder] Detected project type: %s\n", strategy.name)
		}
	}
	if strategy == nil {
		err := fmt.Errorf("Could not detect project type (no package.json, requirements.txt, etc. found)")
		if ownCtrl != nil {
			ownCtrl.Finalize(false)
		}
		return err
	}

	run := func() error {
		if strategy.custom {
			// FAST TRACK: custom Dockerfile.
			if buildLog != nil {
				buildLog.StartPhase("custom")
			}
			if err := c.packageCustom(bctx, imageName, buildLog); err != nil {
				return err
			}
			if buildLog != nil {
				buildLog.EndPhase("custom", true)
			}
			return nil
		}
		if buildLog != nil {
			buildLog.StartPhase("compile")
		}
		if err := c.compile(strategy, bctx, buildLog); err != nil {
			return err
		}
		if buildLog != nil {
			buildLog.EndPhase("compile", true)
			buildLog.StartPhase("package")
		}
		if err := c.packageImage(strategy, bctx, imageName, buildLog); err != nil {
			return err
		}
		if buildLog != nil {
			buildLog.EndPhase("package", true)
		}
		return nil
	}

	if err := run(); err != nil {
		c.log.Error("Build failed: %s", err.Error())
		if ownCtrl != nil {
			ownCtrl.Finalize(false)
		}
		return err
	}
	c.log.Log("Build completed successfully: %s", imageName)
	if ownCtrl != nil {
		ownCtrl.Finalize(true)
	}
	return nil
}

// detectStrategy ports Builder.#detect: custom Dockerfile first, then the
// fixed priority order, resolving the compiler image tag from project
// version files. Nil when nothing matches.
func detectStrategy(internalPath string) *buildStrategy {
	if fileExists(filepath.Join(internalPath, "Dockerfile")) {
		return &buildStrategy{name: "Custom Dockerfile", custom: true}
	}

	order := []string{"PYTHON", "GO", "RUST", "BUN", "NODE_PNPM", "NODE_YARN", "NODE_NPM"}
	var chosen *buildStrategy
	for _, key := range order {
		s := buildStrategies[key]
		if strategyTriggered(internalPath, s) {
			chosen = &s
			break
		}
	}
	if chosen == nil && fileExists(filepath.Join(internalPath, "package.json")) {
		s := buildStrategies["NODE_NPM"]
		chosen = &s
	}
	if chosen == nil {
		for _, key := range []string{"PHP", "STATIC"} {
			s := buildStrategies[key]
			if strategyTriggered(internalPath, s) {
				chosen = &s
				break
			}
		}
	}
	if chosen == nil {
		return nil
	}
	chosen.image = resolveImage(internalPath, chosen)
	return chosen
}

func strategyTriggered(internalPath string, s buildStrategy) bool {
	for _, trigger := range s.triggers {
		if fileExists(filepath.Join(internalPath, trigger)) {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// resolveImage ports Builder.#resolveImage: version-file lookup with the
// strategy default as fallback.
func resolveImage(internalPath string, s *buildStrategy) string {
	fallback := s.imageBase + ":" + s.imageDefault
	resolver, ok := versionResolvers[s.versionResolver]
	if !ok {
		return fallback
	}
	files := []string{resolver.file}
	if resolver.fallbackFile != "" {
		files = append(files, resolver.fallbackFile)
	}
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(internalPath, file))
		if err != nil {
			continue
		}
		if tag := resolver.parse(string(content), file); tag != "" {
			return s.imageBase + ":" + tag
		}
	}
	return fallback
}

// compile ports Builder.#compile: run install/build/cleanup in an
// unprivileged runner with the source bind-mounted and host networking
// (speed + registry caching).
func (c *Client) compile(strategy *buildStrategy, bctx BuildContext, buildLog BuildLog) error {
	c.log.Log("[Phase 1] Compiling artifacts using %s...", strategy.image)

	var cmds []string
	for _, cmd := range []string{strategy.installCmd, strategy.buildCmd, strategy.cleanupCmd} {
		if cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	commands := strings.Join(cmds, " && ")

	if buildLog != nil {
		buildLog.StartPhase("pull_compiler")
	}
	if err := c.EnsureImage(strategy.image, nil); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_compiler", true)
		buildLog.StartPhase("run_compile")
	}

	status, err := c.runBuilderContainer(strategy.image, []string{"sh", "-c", commands}, &container.HostConfig{
		Binds:       []string{bctx.HostPath + ":/app"},
		AutoRemove:  true,
		Privileged:  false, // SECURITY: strict no to privileged
		NetworkMode: "host",
	}, "/app", compileEnv, buildLog)
	if err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("run_compile", true)
	}
	if status != 0 {
		return fmt.Errorf("Compilation failed with exit code %d", status)
	}
	c.log.Log("[Phase 1] Compilation successful.")
	return nil
}

// packageImage ports Builder.#package: write an ephemeral Dockerfile.odac
// + .dockerignore into the source, then build the final image through a
// socket-mounted docker:cli container (DooD — no DinD, no privilege).
func (c *Client) packageImage(strategy *buildStrategy, bctx BuildContext, imageName string, buildLog BuildLog) error {
	c.log.Log("[Phase 2] Packaging final image %s...", imageName)

	dockerfile := generateDockerfile(strategy.pkg)
	dockerfilePath := filepath.Join(bctx.InternalPath, "Dockerfile.odac")
	dockerignorePath := filepath.Join(bctx.InternalPath, ".dockerignore")

	if buildLog != nil {
		buildLog.StartPhase("prepare_context")
	}
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(dockerignorePath, []byte(".git\n.github\nDockerfile.odac\n"), 0o644); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("prepare_context", true)
	}
	defer func() {
		os.Remove(dockerfilePath)
		os.Remove(dockerignorePath)
	}()

	buildCmd := fmt.Sprintf("docker build --progress=plain -f /app/Dockerfile.odac -t %s /app", imageName)

	if buildLog != nil {
		buildLog.StartPhase("pull_packager")
	}
	if err := c.EnsureImage(packagerImage, nil); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_packager", true)
		buildLog.StartPhase("run_package")
	}

	status, err := c.runBuilderContainer(packagerImage, []string{"sh", "-c", buildCmd}, &container.HostConfig{
		Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock", // access host Docker
			bctx.HostPath + ":/app",                     // access source context
		},
		AutoRemove: true,
		Privileged: false, // not needed, just socket access
	}, "", nil, buildLog)
	if err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("run_package", true)
	}
	if status != 0 {
		return fmt.Errorf("Packaging failed with exit code %d", status)
	}
	c.log.Log("[Phase 2] Packaging successful.")
	return nil
}

// packageCustom ports Builder.#packageCustom: forward the user's own
// Dockerfile to the host daemon through docker:cli.
func (c *Client) packageCustom(bctx BuildContext, imageName string, buildLog BuildLog) error {
	c.log.Log("[Builder] Building from Custom Dockerfile for %s...", imageName)

	buildCmd := fmt.Sprintf("docker build --progress=plain -t %s /app", imageName)

	if buildLog != nil {
		buildLog.StartPhase("pull_builder")
	}
	if err := c.EnsureImage(packagerImage, nil); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_builder", true)
		buildLog.StartPhase("run_custom_build")
	}

	status, err := c.runBuilderContainer(packagerImage, []string{"sh", "-c", buildCmd}, &container.HostConfig{
		Binds:      []string{"/var/run/docker.sock:/var/run/docker.sock", bctx.HostPath + ":/app"},
		AutoRemove: true,
		Privileged: false,
	}, "", nil, buildLog)
	if err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("run_custom_build", true)
	}
	if status != 0 {
		return fmt.Errorf("Custom build failed with exit code %d", status)
	}
	c.log.Log("[Builder] Custom build successful.")
	return nil
}

// runBuilderContainer creates + runs one ephemeral build container and
// returns its exit code, streaming output into buildLog.
func (c *Client) runBuilderContainer(image string, cmd []string, hostCfg *container.HostConfig, workDir string, env []string, buildLog BuildLog) (int64, error) {
	ctx := context.Background()
	created, err := c.api.ContainerCreate(ctx, &container.Config{
		Image:      image,
		Cmd:        cmd,
		WorkingDir: workDir,
		Env:        env,
	}, hostCfg, nil, nil, "")
	if err != nil {
		return 0, err
	}
	return c.runToCompletion(ctx, created.ID, writerOrNil(buildLog), "")
}

// generateDockerfile renders the ephemeral packaging Dockerfile exactly as
// Node's template does.
func generateDockerfile(pkg *packageSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nFROM %s\nWORKDIR /app\nCOPY . .\nUSER root\nRUN chown -R %s:%s /app\n", pkg.baseImage, pkg.user, pkg.user)
	for _, cmd := range pkg.setup {
		fmt.Fprintf(&b, "RUN %s\n", cmd)
	}
	fmt.Fprintf(&b, "USER %s\n", pkg.user)
	for _, kv := range pkg.env {
		fmt.Fprintf(&b, "ENV %s=%q\n", kv[0], kv[1])
	}
	cmdJSON, _ := json.Marshal(pkg.cmd)
	fmt.Fprintf(&b, "CMD %s\n", cmdJSON)
	return b.String()
}

// timeNow is a test hook for build ids.
var timeNow = time.Now

func nowMs() int64 {
	return timeNow().UnixMilli()
}
