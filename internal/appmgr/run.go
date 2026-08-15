package appmgr

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"odac/internal/applog"
	"odac/internal/docker"
	"odac/internal/gpu"
	"odac/internal/ports"
)

// scriptRunner mirrors SCRIPT_RUNNERS.
type scriptRunner struct {
	image string
	cmd   string
	local string
	args  []string
}

var scriptRunners = map[string]scriptRunner{
	".js":  {image: "node:lts-alpine", cmd: "node", local: "node"},
	".py":  {image: "python:alpine", cmd: "python3", local: "python3", args: []string{"-u"}},
	".php": {image: "php:cli-alpine", cmd: "php", local: "php"},
	".rb":  {image: "ruby:alpine", cmd: "ruby", local: "ruby"},
	".sh":  {image: "alpine:latest", cmd: "sh", local: "sh"},
}

func runnerFor(filename string) scriptRunner {
	if r, ok := scriptRunners[filepath.Ext(filename)]; ok {
		return r
	}
	return scriptRunners[".js"]
}

// errNotFound is run/runHeld's "the app vanished mid-flight" (deletion races
// the start path); callers treat it like any other start failure.
var errNotFound = errors.New("app not found")

// run ports #run: the shared start path for every app type.
func (m *Manager) run(id any, logCtrl *applog.BuildControl) error {
	var name string
	var idNum float64
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
			idNum, _ = app["id"].(float64)
		}
	})
	if !found {
		return errNotFound
	}

	// Prevent concurrent runs for the same app.
	if !m.tryLockProcessing(idNum) {
		m.log.Log("App %s is already being processed. Skipping duplicate run.", name)
		return nil
	}
	defer m.unlockProcessing(idNum)

	return m.runHeld(id, logCtrl)
}

// runHeld is run's body once the processing lock is held (Check acquires it
// synchronously before dispatching, like Node's pre-await section of #run).
func (m *Manager) runHeld(id any, logCtrl *applog.BuildControl) error {
	var name, typ string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
		}
	})
	if !found {
		return errNotFound
	}

	m.log.Log("Starting app %s (Type: %s)...", name, typ)
	m.set(id, map[string]any{"status": "starting", "updated": nowMs()})

	var err error
	switch typ {
	case "script":
		err = m.runScript(id)
	case "container":
		err = m.runContainer(id, "", logCtrl)
	case "git":
		err = m.runGitApp(id, "")
	}

	if err != nil {
		m.log.Error("Failed to start app %s: %s", name, err.Error())
		m.set(id, map[string]any{"status": "errored", "updated": nowMs()})
		return err
	}

	m.set(id, map[string]any{"status": "running", "started": nowMs()})

	m.spawn(func() {
		if scanErr := m.scanAndSaveHTTPStatus(id); scanErr != nil {
			m.log.Error("HTTP scan failed for %s: %s", name, scanErr.Error())
		}
	})

	// Trigger Proxy sync after every successful start/restart. Container IP
	// changes on restart; without this the proxy routes to the dead IP.
	m.proxySync()
	return nil
}

func nowMs() float64 { return float64(time.Now().UnixMilli()) }

// runGitApp ports #runGitApp. containerName overrides the app's own name for
// Blue-Green green containers ("" = the app itself); the green container is
// NOT in the working set, so every config effect targets the real app.
func (m *Manager) runGitApp(id any, containerName string) error {
	type snap struct {
		name, identity, image string
		dev                   bool
		hasAPI                bool
		apiPerms              any
		cmd                   []string
		volumes               []docker.Mount
		devices               []docker.Device
		gpu                   *gpu.Spec
		env                   map[string]any
		privileged            string
		networkMode           string
		port                  int
	}
	var s snap
	found := false
	appsRoot := m.appsPath() // before the Mutate block: View inside Mutate deadlocks

	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			return
		}
		found = true

		// Canonical identity for API tokens & resource paths (survives
		// Blue-Green rename; may also arrive persisted on the map).
		s.identity, _ = app["name"].(string)
		if ai, _ := app["_appIdentity"].(string); ai != "" {
			s.identity = ai
		}
		s.name, _ = app["name"].(string)
		if containerName != "" {
			s.name = containerName
		}
		s.image, _ = app["image"].(string)
		s.dev = app["dev"] == true
		s.privileged, _ = app["privileged"].(string)
		s.networkMode = toNetworkMode(app["networkMode"])
		s.cmd = toCmd(app["cmd"])
		s.volumes = toMounts(app["volumes"])
		s.devices = toDevices(app["devices"])
		s.gpu = toGPU(app["gpu"])

		if s.dev {
			// Mount the whole app directory to /app for live development.
			appDir := filepath.Join(appsRoot, s.identity)
			s.volumes = append(s.volumes, docker.Mount{Host: appDir, Container: "/app"})
		}

		// Safety check for legacy apps without ports config.
		s.port = 3000
		portList, _ := app["ports"].([]any)
		if primary := ports.Primary(portList); primary != nil && jsTruthy(primary["container"]) {
			c, _ := jsNumber(primary["container"])
			s.port = int(c)
		} else {
			// Legacy fix: assume 3000 and SAVE IT so the proxy sees it.
			m.log.Log("Legacy App Fix: Assigning default port 3000 to app %s", s.name)
			app["ports"] = []any{ports.Discovered(3000)}
			m.saveAppsLocked()
		}

		s.env = m.resolveEnvLocked(app, true)
		if jsTruthy(app["api"]) {
			s.hasAPI = true
			s.apiPerms = app["api"]
		}
	})
	if !found {
		return errors.New("app not found")
	}

	env := envToStrings(s.env)

	// API permission injection.
	if s.hasAPI && m.deps.Api != nil {
		env["ODAC_API_KEY"] = m.deps.Api.GenerateAppToken(s.identity, s.apiPerms)
		if dir := m.deps.Api.HostSocketDir(); dir != "" {
			s.volumes = append(s.volumes, docker.Mount{Host: dir, Container: "/odac:ro"})
			env["ODAC_API_SOCKET"] = "/odac/api.sock"
		}
	}

	env["PORT"] = strconv.Itoa(s.port)

	runOptions := docker.RunOptions{
		Image:       s.image,
		Ports:       []map[string]any{},
		Volumes:     s.volumes,
		Devices:     s.devices,
		GPU:         s.gpu,
		Env:         env,
		Cmd:         s.cmd,
		NetworkMode: s.networkMode,
	}

	// In dev mode the mounted host directory is owned by the host user/root;
	// run as root to avoid EACCES on writes (acceptable for development).
	if s.dev {
		m.log.Log("Active Dev Mode detected for %s. Forcing container to run as ROOT to handle volume permissions.", s.name)
		runOptions.User = "root"
	}

	m.applyPrivilege(s.name, s.privileged, &runOptions)

	// Fix volume permissions before starting the app container.
	m.fixVolumePermissions(s.name, s.volumes)

	if m.appDeleted(id) {
		return nil
	}

	started, err := m.deps.Docker.RunApp(s.name, runOptions, nil, func() bool { return m.appDeleted(id) })
	if err != nil {
		return err
	}
	if !started {
		return nil
	}

	if err := m.attachLogger(s.name); err != nil {
		m.log.Error("Failed to attach logger to app %s: %s", s.name, err.Error())
	}

	// Runtime port discovery: verify the app actually listens where we told
	// it to (handles apps that ignore the PORT env, like n8n or ComfyUI).
	name := s.name
	port := s.port
	m.spawn(func() { m.pollForPort(id, name, port) })
	return nil
}

// runContainer ports #runContainer (recipe/template apps). containerName
// overrides for Blue-Green ("" = the app itself).
func (m *Manager) runContainer(id any, containerName string, logCtrl *applog.BuildControl) error {
	if !m.deps.Docker.Available() {
		return errors.New("Docker is not available via Container service.")
	}

	type snap struct {
		name, identity, image string
		hasAPI                bool
		apiPerms              any
		cmd                   []string
		volumes               []docker.Mount
		devices               []docker.Device
		gpu                   *gpu.Spec
		published             []map[string]any
		env                   map[string]any
		privileged            string
		networkMode           string
	}
	var s snap
	found := false

	m.cfg.View(func() {
		app := m.getLocked(id)
		if app == nil {
			return
		}
		found = true
		s.identity, _ = app["name"].(string)
		if ai, _ := app["_appIdentity"].(string); ai != "" {
			s.identity = ai
		}
		s.name, _ = app["name"].(string)
		if containerName != "" {
			s.name = containerName
		}
		s.image, _ = app["image"].(string)
		s.privileged, _ = app["privileged"].(string)
		s.networkMode = toNetworkMode(app["networkMode"])
		s.cmd = toCmd(app["cmd"])
		s.volumes = toMounts(app["volumes"])
		s.devices = toDevices(app["devices"])
		s.gpu = toGPU(app["gpu"])
		s.env = m.resolveEnvLocked(app, true)
		if jsTruthy(app["api"]) {
			s.hasAPI = true
			s.apiPerms = app["api"]
		}
		// Only published ports go to Docker; proxy-routed entries are
		// routing metadata, not PortBindings.
		if portList, _ := app["ports"].([]any); portList != nil {
			for _, p := range portList {
				if pm, _ := p.(map[string]any); pm != nil && ports.IsPublished(pm) {
					s.published = append(s.published, copyMap(pm))
				}
			}
		}
	})
	if !found {
		return errors.New("app not found")
	}

	// Pull the image FIRST so subsequent inspections (port, user) have
	// metadata available.
	var pullLog io.Writer
	if logCtrl != nil {
		pullLog = logCtrl
	}
	if err := m.deps.Docker.EnsureImage(s.image, pullLog); err != nil {
		return err
	}

	// Port resolution: config primary > image EXPOSE > default. Read-only —
	// #pollForPort writes the mapping once a port provably answers HTTP.
	port := m.resolveContainerPort(id, s.name, s.image)

	env := envToStrings(s.env)
	if port != 0 {
		env["PORT"] = strconv.Itoa(port)
	}

	if s.hasAPI && m.deps.Api != nil {
		env["ODAC_API_KEY"] = m.deps.Api.GenerateAppToken(s.identity, s.apiPerms)
		if dir := m.deps.Api.HostSocketDir(); dir != "" {
			s.volumes = append(s.volumes, docker.Mount{Host: dir, Container: "/odac:ro"})
			env["ODAC_API_SOCKET"] = "/odac/api.sock"
		}
	}

	m.fixVolumePermissions(s.name, s.volumes)

	runOptions := docker.RunOptions{
		Image:       s.image,
		Ports:       s.published,
		Volumes:     s.volumes,
		Devices:     s.devices,
		GPU:         s.gpu,
		Env:         env,
		Cmd:         s.cmd,
		NetworkMode: s.networkMode,
	}
	m.applyPrivilege(s.name, s.privileged, &runOptions)

	var buildLog docker.BuildLog
	if logCtrl != nil {
		buildLog = logCtrl
	}

	if m.appDeleted(id) {
		return nil
	}

	started, err := m.deps.Docker.RunApp(s.name, runOptions, buildLog, func() bool { return m.appDeleted(id) })
	if err != nil {
		return err
	}
	if !started {
		return nil
	}

	if err := m.attachLogger(s.name); err != nil {
		m.log.Error("Failed to attach logger to app %s: %s", s.name, err.Error())
	}

	expected := port
	if expected == 0 {
		expected = 3000
	}
	name := s.name
	m.spawn(func() { m.pollForPort(id, name, expected) })
	return nil
}

// runScript ports #runScript: containerized when Docker is available,
// local process otherwise.
func (m *Manager) runScript(id any) error {
	if !m.deps.Docker.Available() {
		return m.runScriptLocal(id)
	}
	return m.runScriptContainer(id)
}

// runScriptLocal ports #runScriptLocal: spawn the interpreter directly.
func (m *Manager) runScriptLocal(id any) error {
	var name, file string
	var privileged string
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			name, _ = app["name"].(string)
			file, _ = app["file"].(string)
			privileged, _ = app["privileged"].(string)
		}
	})
	_ = privileged // local scripts run as the server user, like Node

	dir := filepath.Dir(file)
	filename := filepath.Base(file)
	runner := runnerFor(filename)

	logger, err := m.getLogger(name)
	if err != nil {
		return err
	}
	ctrl, err := logger.NewRuntimeStream()
	if err != nil {
		return err
	}
	stream := &runtimeStream{ctrl: ctrl}
	m.mu.Lock()
	m.logStreams[name] = stream
	m.mu.Unlock()

	args := append(append([]string{}, runner.args...), filename)
	m.log.Log("Spawning local process for " + name + ": " + runner.local + " " + strings.Join(args, " "))

	cmd := exec.Command(runner.local, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ODAC_APP=true")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		m.log.Error("Failed to start local app " + name + ": " + err.Error())
		ctrl.Error([]byte("[ERR] [" + nowMsString() + "] Failed to start app: " + err.Error() + "\n"))
		stream.end()
		m.set(id, map[string]any{"status": "errored", "pid": nil})
		return err
	}

	m.set(id, map[string]any{"pid": float64(cmd.Process.Pid)})

	go pump(stdout, func(chunk []byte) {
		ctrl.Write([]byte("[LOG] [" + nowMsString() + "] " + string(chunk)))
	})
	go pump(stderr, func(chunk []byte) {
		ctrl.Error([]byte("[ERR] [" + nowMsString() + "] " + string(chunk)))
	})

	go func() {
		state := "exited"
		if err := cmd.Wait(); err != nil {
			state = err.Error()
		}
		m.log.Log("App " + name + " exited (" + state + ")")
		ctrl.Write([]byte("[LOG] [" + nowMsString() + "] App exited (" + state + ")\n"))
		stream.end()
		m.mu.Lock()
		if m.logStreams[name] == stream {
			delete(m.logStreams, name)
		}
		m.mu.Unlock()
		m.set(id, map[string]any{"status": "stopped", "pid": nil, "active": false})
	}()

	return nil
}

func nowMsString() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// pump forwards reader chunks to sink until EOF.
func pump(r interface{ Read([]byte) (int, error) }, sink func([]byte)) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sink(chunk)
		}
		if err != nil {
			return
		}
	}
}

// runScriptContainer ports #runScriptContainer.
func (m *Manager) runScriptContainer(id any) error {
	type snap struct {
		name, identity, file string
		hasAPI               bool
		apiPerms             any
		devices              []docker.Device
		gpu                  *gpu.Spec
		privileged           string
		networkMode          string
	}
	var s snap
	found := false
	m.cfg.View(func() {
		app := m.getLocked(id)
		if app == nil {
			return
		}
		found = true
		s.identity, _ = app["name"].(string)
		if ai, _ := app["_appIdentity"].(string); ai != "" {
			s.identity = ai
		}
		s.name, _ = app["name"].(string)
		s.file, _ = app["file"].(string)
		s.privileged, _ = app["privileged"].(string)
		s.networkMode = toNetworkMode(app["networkMode"])
		s.devices = toDevices(app["devices"])
		s.gpu = toGPU(app["gpu"])
		if jsTruthy(app["api"]) {
			s.hasAPI = true
			s.apiPerms = app["api"]
		}
	})
	if !found {
		return errors.New("app not found")
	}

	filename := filepath.Base(s.file)
	dir := filepath.Dir(s.file)
	runner := runnerFor(filename)

	env := map[string]string{"ODAC_APP": "true"}
	volumes := []docker.Mount{{Host: dir, Container: "/app"}}

	if s.hasAPI && m.deps.Api != nil {
		env["ODAC_API_KEY"] = m.deps.Api.GenerateAppToken(s.identity, s.apiPerms)
		if dirPath := m.deps.Api.HostSocketDir(); dirPath != "" {
			volumes = append(volumes, docker.Mount{Host: dirPath, Container: "/odac:ro"})
			env["ODAC_API_SOCKET"] = "/odac/api.sock"
		}
	}

	runOptions := docker.RunOptions{
		Image:       runner.image,
		Cmd:         append(append([]string{runner.cmd}, runner.args...), filename),
		Volumes:     volumes,
		Devices:     s.devices,
		GPU:         s.gpu,
		Env:         env,
		NetworkMode: s.networkMode,
	}
	m.applyPrivilege(s.name, s.privileged, &runOptions)

	_, err := m.deps.Docker.RunApp(s.name, runOptions, nil, nil)
	return err
}

// applyPrivilege ports #applyPrivilege.
func (m *Manager) applyPrivilege(name, privileged string, runOptions *docker.RunOptions) {
	switch privileged {
	case "full":
		m.log.Log("App %s configured as FULL privileged. Enabling Docker Privileged mode + root.", name)
		runOptions.Privileged = true
		runOptions.User = "root"
	case "root":
		m.log.Log("App %s configured to run as root.", name)
		runOptions.User = "root"
	}
}

// mountIsFile decides whether the host side of a volume must be materialized
// as a single file rather than a directory, so Docker bind-mounts a file (a
// missing host path is otherwise auto-created as a directory). Signals, in
// order: (1) a trailing '/' on the container path means directory; (2) an
// already-materialized host path mirrors its own type; (3) the app's live
// container mirrors whatever exists at that path; (4) otherwise fall back to
// an extension heuristic (a basename with an extension is a file). name is
// the container to probe in step 3; fsPath is the orchestrator-visible host
// path to stat in step 2.
func (m *Manager) mountIsFile(name, fsPath, container string) bool {
	cPath := strings.TrimSuffix(container, ":ro")

	// (1) explicit directory intent.
	if strings.HasSuffix(cPath, "/") {
		return false
	}

	// (2) host already exists: mirror its type (stable across redeploys).
	if fi, err := os.Stat(fsPath); err == nil {
		return !fi.IsDir()
	}

	// (3) the app's currently running container: mirror what lives there.
	if m.deps.Docker != nil && name != "" {
		if isDir, ok := m.deps.Docker.StatPathIsDir(name, cPath); ok {
			return !isDir
		}
	}

	// (4) create-time / no container: a basename with an extension is a file.
	return path.Ext(path.Base(cPath)) != ""
}

// fixVolumePermissions ports #fixVolumePermissions: materialize each volume
// host under the apps path and chmod it wide open so any container user can
// write. Host-native paths (ODAC_HOST_ROOT-prefixed) are normalized to
// container-internal /app/... so the fs ops go through the existing bind
// mount. File-typed volumes (see mountIsFile) are created as an empty file
// under an existing parent so Docker binds a file, not a directory. name is
// the app's container, consulted to classify ambiguous mounts.
func (m *Manager) fixVolumePermissions(name string, volumes []docker.Mount) {
	if len(volumes) == 0 {
		return
	}

	appsPath := filepath.Clean(m.appsPath())
	hostRoot := os.Getenv("ODAC_HOST_ROOT")

	for _, vol := range volumes {
		if strings.HasSuffix(vol.Container, ":ro") {
			continue // read-only mounts
		}
		if vol.Host == "" || !filepath.IsAbs(vol.Host) {
			continue // unresolved or non-absolute host paths
		}

		fsPath := vol.Host
		if hostRoot != "" && strings.HasPrefix(fsPath, hostRoot) {
			fsPath = filepath.Join("/app", fsPath[len(hostRoot):])
		}

		if !strings.HasPrefix(filepath.Clean(fsPath), appsPath) {
			m.log.Error("FixVolPerms: Skipping chmod on path outside app directory for security: %s", vol.Host)
			continue
		}

		if m.mountIsFile(name, fsPath, vol.Container) {
			if err := os.MkdirAll(filepath.Dir(fsPath), 0o755); err != nil {
				m.log.Error("FixVolPerms: mkdir parent failed for %s: %s", fsPath, err.Error())
				continue
			}
			if _, err := os.Stat(fsPath); os.IsNotExist(err) {
				f, err := os.OpenFile(fsPath, os.O_CREATE, 0o666)
				if err != nil {
					m.log.Error("FixVolPerms: create file failed for %s: %s", fsPath, err.Error())
					continue
				}
				f.Close()
			}
			if err := os.Chmod(fsPath, 0o666); err != nil {
				m.log.Error("FixVolPerms: chmod failed for %s: %s", fsPath, err.Error())
				continue
			}
			m.log.Log("FixVolPerms: Set 0666 on file %s (from %s)", fsPath, vol.Host)
			continue
		}

		if err := os.MkdirAll(fsPath, 0o755); err != nil {
			m.log.Error("FixVolPerms: chmod failed for %s: %s", fsPath, err.Error())
			continue
		}
		if err := os.Chmod(fsPath, 0o777); err != nil {
			m.log.Error("FixVolPerms: chmod failed for %s: %s", fsPath, err.Error())
			continue
		}
		m.log.Log("FixVolPerms: Set 0777 on %s (from %s)", fsPath, vol.Host)
	}
}

// resolveContainerPort ports #resolveContainerPort: the port a container app
// is told to listen on via the PORT env. Priority: config primary > image
// EXPOSE > 3000. Nothing is persisted — a proxy mapping asserts the proxy
// routes there, and neither EXPOSE nor a default is evidence of HTTP
// (dev a9af39f).
func (m *Manager) resolveContainerPort(id any, name, image string) int {
	port := 0
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			portList, _ := app["ports"].([]any)
			if primary := ports.Primary(portList); primary != nil && jsTruthy(primary["container"]) {
				c, _ := jsNumber(primary["container"])
				port = int(c)
			}
		}
	})
	if port != 0 {
		return port
	}

	if image != "" {
		if exposed := m.deps.Docker.GetImageExposedPorts(image); len(exposed) > 0 {
			m.log.Log("Port Auto-Detect: Image EXPOSE suggests port %s for app %s", itoa(exposed[0]), name)
			return exposed[0]
		}
	}

	m.log.Log("Port Auto-Detect: No port info available for app %s. Defaulting PORT to 3000.", name)
	return 3000
}

// attachLogger ports #attachLogger: create a runtime log stream and pipe the
// container's demuxed output into it. No-op when a stream already exists.
func (m *Manager) attachLogger(name string) error {
	m.mu.Lock()
	if m.logStreams[name] != nil {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	logger, err := m.getLogger(name)
	if err != nil {
		return err
	}
	ctrl, err := logger.NewRuntimeStream()
	if err != nil {
		return err
	}
	stream := &runtimeStream{ctrl: ctrl}

	stop, err := m.deps.Docker.StreamLogs(name, writerFunc(ctrl.Write), writerFunc(ctrl.Error))
	if err == nil {
		stream.stopStream = stop
	}

	m.mu.Lock()
	if m.logStreams[name] != nil {
		// Raced with another attach; keep the existing one.
		m.mu.Unlock()
		stream.end()
		return nil
	}
	m.logStreams[name] = stream
	m.mu.Unlock()

	m.log.Log("Attached log stream to active app: %s", name)
	return nil
}

// writerFunc adapts applog's no-return write methods to io.Writer.
type writerFunc func([]byte)

func (w writerFunc) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}

// getLoggerInstance ports #getLoggerInstance.
func (m *Manager) getLoggerInstance(appName string) *applog.Logger {
	m.mu.Lock()
	defer m.mu.Unlock()
	logger := m.loggers[appName]
	if logger == nil {
		logger = applog.New(m.logsRoot, appName)
		m.loggers[appName] = logger
	}
	return logger
}

// getLogger ports #getLogger: instance + idempotent init.
func (m *Manager) getLogger(appName string) (*applog.Logger, error) {
	logger := m.getLoggerInstance(appName)
	if err := logger.Init(); err != nil {
		return nil, err
	}
	return logger, nil
}

// isAppRunning ports #isAppRunning.
func (m *Manager) isAppRunning(id any) bool {
	var name, typ string
	pid := 0
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
			if p, ok := app["pid"].(float64); ok {
				pid = int(p)
			}
		}
	})

	if typ == "container" || m.deps.Docker.Available() {
		return m.deps.Docker.IsRunning(name)
	}
	if pid > 0 {
		return pidAlive(pid)
	}
	return false
}
