package appmgr

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"odac/internal/api"
	"odac/internal/applog"
	"odac/internal/ports"
)

var (
	scpLikeURL  = regexp.MustCompile(`^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:`)
	commitShaRE = regexp.MustCompile(`^[a-fA-F0-9]{6,40}$`)
	networkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// greenSuffix matches the green container name suffix produced by
	// generateRuntimeID: `<appName>-green-<13+digit-ms>_<8-hex>`.
	greenSuffix = regexp.MustCompile(`-green-\d+_[a-f0-9]{8}$`)
	// scriptExts is SCRIPT_EXTENSIONS: files App.start strips to name apps.
	scriptExts = []string{".js", ".py", ".php", ".sh", ".rb"}
)

// Start ports App.start: register/start a script app from a file path.
func (m *Manager) Start(file string) *api.Result {
	if file == "" {
		return res(false, __("App file not specified."))
	}

	abs, err := filepath.Abs(file)
	if err == nil {
		file = abs
	}

	if _, statErr := os.Stat(file); statErr != nil {
		return res(false, __("App file %s not found.", file))
	}

	var existingID any
	var existingName, existingStatus string
	var appID any
	m.cfg.Mutate(func() {
		for _, app := range m.apps {
			if app["file"] == file {
				existingID = app["id"]
				existingName, _ = app["name"].(string)
				existingStatus, _ = app["status"].(string)
				return
			}
		}
		app := m.addLocked(file, "script")
		appID = app["id"]
	})

	if existingID == nil {
		_ = m.run(appID, nil)
		return res(true, __("App %s added successfully.", file))
	}

	if existingStatus != "running" {
		_ = m.run(existingID, nil)
		return res(true, __("App %s started successfully.", existingName))
	}

	return res(false, __("App %s already exists and is running.", file))
}

// Stop ports App.stop.
func (m *Manager) Stop(id any) *api.Result {
	var name, status string
	var idVal any
	pid := 0
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
			status, _ = app["status"].(string)
			idVal = app["id"]
			if p, ok := app["pid"].(float64); ok {
				pid = int(p)
			}
		}
	})
	if !found {
		return res(false, __("App ID %s not found.", jsString(id)))
	}

	if status == "stopped" {
		return res(true, __("App %s is already stopped.", name))
	}

	if pid > 0 {
		if err := pidTerminate(pid); err != nil {
			return res(false, err.Error())
		}
	}

	if m.deps.Docker.Available() {
		m.deps.Docker.Stop(name)
	}

	m.set(idVal, map[string]any{"status": "stopped", "pid": nil, "active": false})

	// Cleanup log stream.
	m.mu.Lock()
	stream := m.logStreams[name]
	delete(m.logStreams, name)
	m.mu.Unlock()
	if stream != nil {
		stream.end()
	}

	return res(true, __("App %s stopped.", name))
}

// StopAll ports App.stopAll.
func (m *Manager) StopAll() {
	m.log.Log("Stopping all apps...")
	var ids []any
	m.cfg.View(func() {
		for _, app := range m.apps {
			ids = append(ids, app["id"])
		}
	})
	for _, id := range ids {
		m.Stop(id)
	}
}

// Delete ports App.delete: force-cleanup regardless of in-flight state.
func (m *Manager) Delete(id any, purge bool) *api.Result {
	var name string
	var idNum float64
	var activeContainerID string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
			idNum, _ = app["id"].(float64)
			activeContainerID, _ = app["activeContainerId"].(string)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	m.log.Log("Deleting app %s (force-cleanup any in-flight blue/green containers)", name)

	// Flag the live object first (app._deleted = true) so in-flight flows
	// abandon at their next appDeleted checkpoint. In-memory only, never
	// persisted — a crash mid-delete must not brick the app on disk (Node
	// could persist the flag if a #saveApps raced the window; not replicated).
	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			app["_deleted"] = true
		}
	})

	// The user has double-confirmed in the UI — delete must succeed
	// regardless of restart/redeploy/create state. Releasing in-flight locks
	// lets concurrent flows abandon gracefully; their set() calls become
	// no-ops once the app leaves the working set below.
	m.mu.Lock()
	delete(m.processing, idNum)
	delete(m.creating, name)
	m.mu.Unlock()

	m.Stop(idNum)

	if m.deps.Docker.Available() {
		m.deps.Docker.Remove(name)
		// Container.js left the built image behind; repeated create/delete
		// then leaked one odac-app-<name> image per app until the disk filled.
		m.deps.Docker.RemoveImage("odac-app-" + name)
	}

	// Sweep Blue-Green companions left behind by an in-flight ZDD deploy —
	// a green container can survive as a ghost under its temporary name (or
	// worse, get renamed to app.name after we already removed it).
	m.SweepGreenContainersFor(name, activeContainerID)

	var unlinked []string
	m.cfg.Mutate(func() {
		filtered := m.apps[:0]
		for _, app := range m.apps {
			if app["name"] == name || app["id"] == idNum {
				continue
			}
			filtered = append(filtered, app)
		}
		m.apps = filtered
		// Cascading delete: drop env links pointing at the removed app.
		unlinked = m.sweepLinkedRefsLocked(name)
		m.saveAppsLocked()
	})
	if len(unlinked) > 0 {
		m.log.Log("Removed env link to %s from %s - restart required to apply", name, strings.Join(unlinked, ", "))
	}

	m.mu.Lock()
	stream := m.logStreams[name]
	delete(m.logStreams, name)
	logger := m.loggers[name]
	delete(m.loggers, name)
	m.mu.Unlock()
	if stream != nil {
		stream.end()
	}
	m.deps.Docker.UnregisterBuildLogger(name)

	if purge {
		if logger != nil {
			if err := logger.Destroy(); err != nil {
				m.log.Error("Failed to remove app logs for %s: %s", name, err.Error())
			}
		}
		appDir := filepath.Join(m.appsPath(), name)
		if err := os.RemoveAll(appDir); err != nil {
			m.log.Error("Failed to remove app directory for %s: %s", name, err.Error())
		}
	}

	// Cascading delete: remove associated domains.
	if m.deps.Domains != nil {
		if err := m.deps.Domains.DeleteByApp(name); err != nil {
			m.log.Error("Failed to delete domains for app %s: %s", name, err.Error())
		}
	}

	m.hubTrigger("app.list")

	return res(true, __("App %s deleted successfully.", name))
}

// hasDomainsFor ports the ZDD gate: any config.domains record whose appId is
// the app's name or id.
func (m *Manager) hasDomainsFor(name string, id float64) bool {
	has := false
	m.cfg.View(func() {
		domains, _ := m.cfg.Get("domains").(map[string]any)
		for _, rec := range domains {
			record, _ := rec.(map[string]any)
			if record == nil {
				continue
			}
			if record["appId"] == name || record["appId"] == id {
				has = true
				return
			}
		}
	})
	return has
}

// Restart ports App.restart: Blue-Green for git apps with domains, standard
// stop/start otherwise.
func (m *Manager) Restart(id any) *api.Result {
	var name, typ string
	var idNum float64
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
			idNum, _ = app["id"].(float64)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	m.log.Log("Restarting app %s", name)

	if typ == "git" && m.hasDomainsFor(name, idNum) {
		m.log.Log("ZDD enabled for %s (Has Domains). Executing Blue-Green restart.", name)

		if !m.tryLockProcessing(idNum) {
			return res(false, __("App %s is already being processed.", name))
		}
		defer m.unlockProcessing(idNum)

		greenName := name + "-green-" + generateRuntimeID("")
		err := m.performBlueGreenDeploy(idNum, greenName, deployOptions{
			operation:   "Restart",
			setStarting: true,
			runGreenContainer: func() error {
				if typ == "git" {
					return m.runGitApp(idNum, greenName)
				}
				return m.runContainer(idNum, greenName, nil)
			},
		})
		if err != nil {
			m.log.Error("Failed to restart app %s with ZDD: %s", name, err.Error())
			m.set(idNum, map[string]any{"status": "errored", "updated": nowMs()})
			return res(false, __("Failed to restart app %s: %s", name, err.Error()))
		}

		m.hubTrigger("app.list")
		return res(true, __("App %s restarted successfully with zero-downtime.", name))
	}

	// Standard restart (no domains or script app).
	if !m.tryLockProcessing(idNum) {
		return res(false, __("App %s is already being processed.", name))
	}

	m.log.Log("Standard restart for %s (No Domains or Script App). Stopping old container first.", name)

	m.Stop(idNum)

	// Give the container environment a moment to release resources.
	m.sleep(m.restartDelay)

	m.set(idNum, map[string]any{"active": true})

	// Release the lock before run() so it can acquire its own processing
	// lock; the guard above already rejected concurrent callers.
	m.unlockProcessing(idNum)

	if m.run(idNum, nil) == nil {
		m.hubTrigger("app.list")
		return res(true, __("App %s restarted successfully.", name))
	}
	return res(false, __("Failed to restart app %s.", name))
}

func (m *Manager) tryLockProcessing(id float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processing[id] {
		return false
	}
	m.processing[id] = true
	return true
}

func (m *Manager) unlockProcessing(id float64) {
	m.mu.Lock()
	delete(m.processing, id)
	m.mu.Unlock()
}

// RedeployPayload carries app.redeploy's named arguments.
type RedeployPayload struct {
	Container string
	URL       string
	Token     string
	Branch    string
	CommitSha string
}

// Redeploy ports App.redeploy: fetch + rebuild + (Blue-Green) restart.
func (m *Manager) Redeploy(payload RedeployPayload) *api.Result {
	appName := payload.Container
	if appName == "" {
		return res(false, __("Missing container name"))
	}

	var name, appBranch, appURL, appImage string
	var idNum float64
	var typ string
	var hasGitMeta bool
	var gitMeta map[string]any
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(appName); app != nil {
			found = true
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
			idNum, _ = app["id"].(float64)
			appBranch, _ = app["branch"].(string)
			appURL, _ = app["url"].(string)
			appImage, _ = app["image"].(string)
			if g, _ := app["git"].(map[string]any); g != nil {
				hasGitMeta = true
				gitMeta = copyMap(g)
			}
		}
	})
	if !found {
		return res(false, __("App %s not found.", appName))
	}

	if typ != "git" {
		return res(false, __("Redeploy is only supported for git apps."))
	}

	// Validate overrides (same rules as createFromGit).
	if payload.URL != "" {
		if hasIllegalURLChars(payload.URL) {
			return res(false, __("Invalid Git URL: Contains illegal characters."))
		}
		if !validGitURL(payload.URL, true) {
			return res(false, __("Invalid Git URL: Unsupported protocol."))
		}
	}
	if payload.CommitSha != "" && !commitShaRE.MatchString(payload.CommitSha) {
		return res(false, __("Invalid commit SHA format."))
	}
	if !validBranch(payload.Branch) {
		return res(false, __("Invalid branch name format."))
	}
	targetBranch := payload.Branch
	if targetBranch == "" {
		targetBranch = appBranch
	}
	if targetBranch == "" {
		targetBranch = "main"
	}

	if !m.tryLockProcessing(idNum) {
		return res(false, __("App %s is already being processed.", name))
	}
	defer m.unlockProcessing(idNum)

	// Register the logger IMMEDIATELY to prevent races with Hub requests.
	logger := m.getLoggerInstance(name)
	m.deps.Docker.RegisterBuildLogger(name, logger)
	defer m.deps.Docker.UnregisterBuildLogger(name)

	commitLabel := payload.CommitSha
	if commitLabel == "" {
		commitLabel = "HEAD"
	}
	m.log.Log("Redeploying app %s (branch: %s, commit: %s)", name, targetBranch, commitLabel)

	var logCtrl *applog.BuildControl
	greenName := ""

	fail := func(err error) *api.Result {
		m.log.Error("Redeploy failed for %s: %s", name, err.Error())
		if logCtrl != nil {
			logCtrl.Write([]byte("[Error] " + err.Error() + "\n"))
			logCtrl.Finalize(false)
		}
		// Cleanup a leaked green container if ZDD failed mid-flight.
		if greenName != "" {
			if status := m.deps.Docker.GetStatus(greenName); status.Running {
				m.log.Log("ZDD Cleanup: Removing leaked temporary container %s due to redeploy abort.", greenName)
				m.deps.Docker.Stop(greenName)
				m.deps.Docker.Remove(greenName)
			}
			m.cleanupGreenArtifacts(greenName)
		}
		m.set(idNum, map[string]any{"status": "errored"})
		return res(false, __("Redeploy failed: %s", err.Error()))
	}

	appsPath := m.appsPath()
	appDir := filepath.Join(appsPath, name)

	// Defense-in-depth: prevent path traversal before destructive fs ops.
	cleanBase := filepath.Clean(appsPath)
	if !strings.HasPrefix(filepath.Clean(appDir), cleanBase+string(filepath.Separator)) {
		return res(false, __("Invalid application directory."))
	}

	targetURL := payload.URL
	if targetURL == "" {
		targetURL = appURL
	}
	imageName := appImage
	if imageName == "" {
		imageName = "odac-app-" + name
	}

	// Step 1: fetch the latest code (app still running -> zero downtime).
	m.set(idNum, map[string]any{"status": "updating"})
	_, gitErr := os.Stat(filepath.Join(appDir, ".git"))
	hasGit := gitErr == nil

	if err := logger.Init(); err != nil {
		return fail(err)
	}

	buildID := generateRuntimeID("build")
	var err error
	logCtrl, err = logger.NewBuildStream(buildID, map[string]any{
		"image":    imageName,
		"strategy": "git-app",
	})
	if err != nil {
		return fail(err)
	}

	gitPhase := "git_clone"
	if hasGit {
		gitPhase = "git_pull"
	}
	logCtrl.StartPhase(gitPhase)

	if hasGit {
		// Fast path: incremental fetch (delta download only).
		if err := m.deps.Docker.FetchRepo(targetURL, targetBranch, appDir, payload.Token, payload.CommitSha, logCtrl); err != nil {
			return fail(err)
		}
	} else {
		// Fallback: fresh clone for legacy apps where .git was removed.
		m.log.Log("No .git found in %s, performing fresh clone", name)
		if err := os.RemoveAll(appDir); err != nil {
			return fail(err)
		}
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			return fail(err)
		}
		if err := m.deps.Docker.CloneRepo(targetURL, targetBranch, appDir, payload.Token, logCtrl); err != nil {
			return fail(err)
		}
	}
	logCtrl.EndPhase(gitPhase, true)

	if m.appDeleted(idNum) {
		return res(false, "App was deleted during git fetch phase.")
	}

	// Step 2: rebuild the image (app still running on the old one).
	m.set(idNum, map[string]any{"status": "building"})
	if err := m.deps.Docker.Build(appDir, imageName, name, logCtrl); err != nil {
		return fail(err)
	}

	if m.appDeleted(idNum) {
		return res(false, "App was deleted during build phase.")
	}

	if m.hasDomainsFor(name, idNum) {
		// Zero-Downtime Deployment (Blue-Green).
		m.log.Log("ZDD enabled for %s (Has Domains). Executing Blue-Green switch.", name)
		greenName = name + "-green-" + generateRuntimeID("")
		err := m.performBlueGreenDeploy(idNum, greenName, deployOptions{
			logCtrl:   logCtrl,
			operation: "Redeploy",
			runGreenContainer: func() error {
				return m.runGitApp(idNum, greenName)
			},
		})
		if err != nil {
			return fail(err)
		}
	} else {
		// Standard redeploy (no domains).
		m.log.Log("Standard redeploy for %s (No Domains). Stopping old container first.", name)

		logCtrl.StartPhase("stop_old_container")
		m.Stop(idNum)
		logCtrl.EndPhase("stop_old_container", true)

		m.set(idNum, map[string]any{"active": true, "status": "starting"})

		logCtrl.StartPhase("start_new_container")
		if err := m.runGitApp(idNum, ""); err != nil {
			return fail(err)
		}
		logCtrl.EndPhase("start_new_container", true)

		m.set(idNum, map[string]any{"status": "running", "started": nowMs()})

		m.spawn(func() {
			if scanErr := m.scanAndSaveHTTPStatus(idNum); scanErr != nil {
				m.log.Error("HTTP scan failed for %s: %s", name, scanErr.Error())
			}
		})

		logCtrl.StartPhase("proxy_propagation")
		m.proxySync()
		m.proxyPurge(idNum)
		logCtrl.EndPhase("proxy_propagation", true)
	}

	// Persist updated metadata.
	updates := map[string]any{}
	if payload.CommitSha != "" {
		updates["commitSha"] = payload.CommitSha
	}
	if payload.Branch != "" || payload.URL != "" {
		if payload.Branch != "" {
			updates["branch"] = targetBranch
		}
		if payload.URL != "" {
			updates["url"] = payload.URL
		}
		if hasGitMeta {
			repo, provider := gitMetadata(targetURL)
			gitMeta["repo"] = repo
			gitMeta["branch"] = targetBranch
			gitMeta["provider"] = provider
			updates["git"] = gitMeta
		}
	}
	if len(updates) > 0 {
		m.set(idNum, updates)
	}

	// The rebuild retagged odac-app-<name> onto fresh layers, orphaning the
	// previous image as an untagged (<none>) dangling entry. Sweep those now
	// that the new container is live — Docker skips any image still in use, so
	// this never touches a running app.
	m.deps.Docker.PruneDanglingImages()

	m.hubTrigger("app.list")
	logCtrl.Finalize(true)
	return res(true, __("App %s redeployed successfully.", name))
}

// GetBuildStats ports App.getBuildStats.
func (m *Manager) GetBuildStats(id any) *api.Result {
	var name string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	logger := applog.New(m.logsRoot, name)
	return res(true, logger.GetDailySummary())
}

// SubscribeToLogs ports App.subscribeToLogs: realtime runtime log feed.
// Returns nil when the app is unknown.
func (m *Manager) SubscribeToLogs(appName string, cb func(applog.Entry)) func() {
	m.mu.Lock()
	logger := m.loggers[appName]
	hasStream := m.logStreams[appName] != nil
	m.mu.Unlock()

	if logger == nil {
		if !hasStream {
			exists := false
			m.cfg.View(func() {
				for _, app := range m.apps {
					if app["name"] == appName {
						exists = true
						break
					}
				}
			})
			if !exists {
				var active []string
				m.mu.Lock()
				for name := range m.logStreams {
					active = append(active, name)
				}
				m.mu.Unlock()
				m.log.Log("No log stream found for %s. Active: %s", appName, strings.Join(active, ","))
				return nil
			}
		}
		// Create a logger for the known app (even if stopped); init is
		// fire-and-forget, not needed for subscribing.
		logger = m.getLoggerInstance(appName)
		m.spawn(func() { _ = logger.Init() })
	}

	return logger.Subscribe(cb, applog.Runtime)
}

// SetPorts ports App.setPorts: replace the port mappings after validation.
func (m *Manager) SetPorts(id any, portsPayload []any, payloadOK bool) *api.Result {
	var name string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	if !payloadOK {
		return res(false, __("Invalid ports payload. Expected an array."))
	}

	// Validate each mapping. Both sides are mandatory: an entry with no host
	// is ambiguous now that proxy ownership has an explicit spelling.
	entries := make([]map[string]any, 0, len(portsPayload))
	for _, raw := range portsPayload {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			return res(false, __("Invalid port entry. Expected {host, container}."))
		}
		cv, present := entry["container"]
		if !present || cv == nil || cv == "" {
			return res(false, __("Each port entry must have a container port."))
		}
		containerPort, ok := jsNumber(cv)
		if !ok || !isInt(containerPort) || containerPort < 1 || containerPort > 65535 {
			return res(false, __("Invalid container port: %s. Must be 1-65535.", jsString(cv)))
		}
		hv, present := entry["host"]
		if !present || hv == nil || hv == "" {
			return res(false, __("Each port entry must have a host port, \"auto\", or \"%s\".", ports.Proxy))
		}
		if hv != "auto" && hv != ports.Proxy {
			hostPort, ok := jsNumber(hv)
			if !ok || !isInt(hostPort) || hostPort < 1 || hostPort > 65535 {
				return res(false, __("Invalid host port: %s. Must be 1-65535, \"auto\", or \"%s\".", jsString(hv), ports.Proxy))
			}
		}

		// A public entry reaches the internet, so an unparseable flag must
		// fail loudly rather than fall back to either interpretation.
		isPublic, ok := ports.ParsePublic(entry["public"])
		if !ok {
			return res(false, __("Invalid public flag: %s. Must be true or false.", jsString(entry["public"])))
		}
		if isPublic && hv == ports.Proxy {
			return res(false, __("A \"%s\" port cannot be public. Give it a host port to publish it.", ports.Proxy))
		}
		entries = append(entries, entry)
	}

	// Reject sets that would collide instead of silently dropping an entry.
	if collision := findPortCollision(entries); collision != "" {
		return res(false, collision)
	}

	// Resolve 'auto' host ports, then persist.
	resolved := m.preparePorts(entries)
	list := make([]any, len(resolved))
	for i, e := range resolved {
		list[i] = e
	}

	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			app["ports"] = list
			m.saveAppsLocked()
		}
	})

	return res(true, __("Ports updated for %s. Restart required to apply.", name))
}

// SetVolumes ports App.setVolumes.
func (m *Manager) SetVolumes(id any, volumes []any, payloadOK bool) *api.Result {
	var name string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	if !payloadOK {
		return res(false, __("Invalid volumes payload. Expected an array."))
	}

	entries := make([]map[string]any, 0, len(volumes))
	for _, raw := range volumes {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			return res(false, __("Invalid volume entry. Expected {host, container}."))
		}
		if c, _ := entry["container"].(string); c == "" {
			return res(false, __("Each volume entry must have a container path string."))
		}
		if h, _ := entry["host"].(string); h == "" {
			return res(false, __("Each volume entry must have a host path string."))
		}
		entries = append(entries, entry)
	}

	// Resolve named volumes relative to the app directory.
	appDir := filepath.Join(m.appsPath(), name)
	prepared := m.prepareVolumes(entries, appDir)

	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			app["volumes"] = prepared
			m.saveAppsLocked()
		}
	})

	return res(true, __("Volumes updated for %s. Restart required to apply.", name))
}

// prepareVolumes ports #prepareVolumes: normalize host-native paths back to
// container-internal /app/... (DooD) and resolve named volumes under the app
// dir. It only resolves paths — materialization (creating the host dir or
// file) is deferred to fixVolumePermissions at run time, which classifies
// each mount as file-vs-directory; pre-creating a named volume as a directory
// here would defeat that classification (an existing host dir always reads as
// a directory).
func (m *Manager) prepareVolumes(recipeVolumes []map[string]any, appDir string) []any {
	out := make([]any, 0, len(recipeVolumes))
	hostRoot := os.Getenv("ODAC_HOST_ROOT")

	for _, vol := range recipeVolumes {
		host, _ := vol["host"].(string)

		if host != "" && hostRoot != "" && filepath.IsAbs(host) && strings.HasPrefix(host, hostRoot) {
			host = filepath.Join("/app", host[len(hostRoot):])
		}

		// Named volumes (non-absolute paths like 'data', 'config') are
		// resolved under the app's dedicated directory for isolation.
		if host != "" && !filepath.IsAbs(host) {
			host = filepath.Join(appDir, host)
		}

		out = append(out, map[string]any{"host": host, "container": vol["container"]})
	}
	return out
}

// DeviceAdd ports App.deviceAdd.
func (m *Manager) DeviceAdd(id any, hostPath, containerPath string) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}
		if hostPath == "" {
			result = res(false, __("Missing host device path."))
			return
		}

		mapped := containerPath
		if mapped == "" {
			mapped = hostPath
		}

		devices, _ := app["devices"].([]any)
		updated := false
		for _, d := range devices {
			if dev, _ := d.(map[string]any); dev != nil && dev["host"] == hostPath {
				dev["container"] = mapped
				updated = true
				break
			}
		}
		if !updated {
			devices = append(devices, map[string]any{"host": hostPath, "container": mapped})
		}
		app["devices"] = devices

		m.saveAppsLocked()
		result = res(true, __("Device %s added to %s. Restart required.", hostPath, app["name"]))
	})
	return result
}

// DeviceDelete ports App.deviceDelete.
func (m *Manager) DeviceDelete(id any, hostPath string) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}

		devices, hasDevices := app["devices"].([]any)
		if !hasDevices {
			result = res(true, __("No devices connected to %s.", app["name"]))
			return
		}

		filtered := make([]any, 0, len(devices))
		for _, d := range devices {
			if dev, _ := d.(map[string]any); dev != nil && dev["host"] == hostPath {
				continue
			}
			filtered = append(filtered, d)
		}
		app["devices"] = filtered

		m.saveAppsLocked()
		result = res(true, __("Device %s removed from %s. Restart required.", hostPath, app["name"]))
	})
	return result
}

// SetNetworks ports App.setNetworks.
func (m *Manager) SetNetworks(id any, networks []any, payloadOK bool) *api.Result {
	var name string
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			found = true
			name, _ = app["name"].(string)
		}
	})
	if !found {
		return res(false, __("App %s not found.", jsString(id)))
	}

	if !payloadOK {
		return res(false, __("Invalid networks payload. Expected an array of network names."))
	}

	names := make([]string, 0, len(networks))
	for _, raw := range networks {
		netName, _ := raw.(string)
		if netName == "" || !networkName.MatchString(netName) {
			return res(false, __("Invalid network name: %s", jsString(raw)))
		}
		names = append(names, netName)
	}

	status := m.deps.Docker.GetStatus(name)
	if !status.Running {
		return res(false, __("App %s is not running. Start the app first.", name))
	}

	result := m.deps.Docker.SetNetworks(name, names)
	if !result.Success {
		msg := result.Message
		if msg == "" {
			msg = __("Failed to update networks for %s.", name)
		}
		return res(false, msg)
	}

	return res(true, __("Networks updated for %s: %s", name, strings.Join(result.Networks, ", ")))
}

// SetPrivileged ports App.setPrivileged — CLI-only escalation path,
// intentionally NOT exposed via Hub.
func (m *Manager) SetPrivileged(id any, mode string) *api.Result {
	if mode == "" {
		mode = "root"
	}

	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}

		if mode != "root" && mode != "full" && mode != "off" {
			result = res(false, __("Invalid mode: %s. Expected root, full, or off.", mode))
			return
		}

		if mode == "off" {
			delete(app, "privileged")
			m.saveAppsLocked()
			result = res(true, __("Elevated access removed from %s. Restart required to apply.", app["name"]))
			return
		}

		app["privileged"] = mode
		m.saveAppsLocked()

		label := __("root user")
		if mode == "full" {
			label = __("FULL privileged (Docker Privileged + root)")
		}
		result = res(true, __("%s now runs with %s. Restart required to apply.", app["name"], label))
	})
	return result
}

// List ports App.list: the dashboard/CLI app table.
func (m *Manager) List(detailed bool) *api.Result {
	type row struct {
		app      map[string]any
		name     string
		identity string
		file     string
	}
	var rows []row
	m.cfg.View(func() {
		if len(m.apps) == 0 {
			m.apps = m.loadAppsFromConfig()
		}
		for _, app := range m.apps {
			r := row{app: copyMap(app)}
			r.name, _ = app["name"].(string)
			r.identity = r.name
			if ai, _ := app["_appIdentity"].(string); ai != "" {
				r.identity = ai
			}
			r.file, _ = app["file"].(string)
			rows = append(rows, r)
		}
	})

	cleanApps := make([]any, 0, len(rows))
	for _, r := range rows {
		cp := r.app
		statusInfo := m.deps.Docker.GetStatus(r.name)
		isRunning := statusInfo.Running

		logger := applog.New(m.logsRoot, r.name)
		if lastBuild := logger.GetLastBuild(); lastBuild != nil {
			phases := make([]any, len(lastBuild.Phases))
			for i, p := range lastBuild.Phases {
				switch {
				case p.Status == "failed" || p.Errors > 0:
					phases[i] = 2
				case p.Warnings > 0:
					phases[i] = 1
				default:
					phases[i] = 0
				}
			}
			cp["build"] = map[string]any{
				"id":       lastBuild.ID,
				"status":   lastBuild.Status,
				"duration": lastBuild.Duration,
				"errors":   lastBuild.Errors,
				"warnings": lastBuild.Warnings,
				"phases":   phases,
			}
		}
		health := logger.GetHealth()
		if health == nil {
			health = []int{}
		}
		cp["health"] = health

		if isRunning {
			cp["status"] = "running"
		} else {
			cp["status"] = "stopped"
		}
		if len(statusInfo.Networks) > 0 {
			cp["networks"] = statusInfo.Networks
		}
		if isRunning && statusInfo.StartTime != "" {
			if t, err := time.Parse(time.RFC3339Nano, statusInfo.StartTime); err == nil {
				cp["started"] = float64(t.UnixMilli())
			}
		}

		if detailed {
			delete(cp, "pid")
			delete(cp, "ip")
			delete(cp, "uptime")

			internalAppDir := filepath.Join(m.appsPath(), r.identity)
			if r.file != "" {
				internalAppDir = filepath.Dir(r.file)
			}
			cp["path"] = m.deps.Docker.ResolveHostPath(internalAppDir)

			// Security: expose only env KEYS, never values.
			rawEnv, _ := cp["env"].(map[string]any)
			manualKeys := []any{}
			linked := []any{}
			if isNewEnvStructure(rawEnv) {
				for k := range getManualEnv(rawEnv) {
					manualKeys = append(manualKeys, k)
				}
				if l, _ := rawEnv["linked"].([]any); l != nil {
					linked = l
				}
			} else {
				for k := range rawEnv {
					manualKeys = append(manualKeys, k)
				}
			}
			sortAnyStrings(manualKeys)
			cp["env"] = map[string]any{"manual": manualKeys, "linked": linked}

			// Container-path to host-path resolution for volumes.
			if vols, _ := cp["volumes"].([]any); vols != nil {
				mapped := make([]any, 0, len(vols))
				for _, v := range vols {
					vol, _ := v.(map[string]any)
					if vol == nil {
						continue
					}
					host, _ := vol["host"].(string)
					mapped = append(mapped, map[string]any{
						"host":      m.deps.Docker.ResolveHostPath(host),
						"container": vol["container"],
					})
				}
				cp["volumes"] = mapped
			}

			cleanApps = append(cleanApps, cp)
		} else {
			cleanApps = append(cleanApps, map[string]any{
				"name":   cp["name"],
				"image":  cp["image"],
				"status": cp["status"],
			})
		}
	}

	return res(true, cleanApps)
}

// sortAnyStrings sorts a []any of strings in place. Node's Object.keys keeps
// insertion order, which decoded-JSON maps cannot reproduce — sorted output
// is the same deviation the 3.3 handlers documented.
func sortAnyStrings(list []any) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, _ := list[j-1].(string)
			b, _ := list[j].(string)
			if a <= b {
				break
			}
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}
