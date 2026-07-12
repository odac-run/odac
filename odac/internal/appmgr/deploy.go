package appmgr

// Port of server/src/App/Deploy.js: Blue-Green zero-downtime deploys and the
// sweeps for their leftover green artifacts.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"odac/internal/applog"
	"odac/internal/ports"
)

// deployOptions mirrors performBlueGreenDeploy's options object.
type deployOptions struct {
	logCtrl           *applog.BuildControl
	operation         string // "Restart" | "Redeploy" (log/error labels)
	runGreenContainer func() error
	setStarting       bool
}

// performBlueGreenDeploy ports Deploy.performBlueGreenDeploy: start the green
// container, wait for TCP+HTTP readiness, flip the proxy, drain, then retire
// the blue container and rename green into its place.
func (m *Manager) performBlueGreenDeploy(id any, greenName string, options deployOptions) error {
	if options.runGreenContainer == nil {
		return errors.New("Blue-Green deploy requires a runGreenContainer function.")
	}
	operation := options.operation
	if operation == "" {
		operation = "Redeploy"
	}

	var appName string
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			return
		}
		appName, _ = app["name"].(string)
		portList, _ := app["ports"].([]any)
		primary := ports.Primary(portList)
		if primary == nil || !jsTruthy(primary["container"]) {
			m.dlog.Log("Legacy App Fix: Assigning default port 3000 to app %s during %s", appName, strings.ToLower(operation))
			app["ports"] = []any{ports.Discovered(3000)}
			m.saveAppsLocked()
		}
	})
	if appName == "" {
		return errors.New("app not found")
	}

	if options.setStarting {
		m.set(id, map[string]any{"status": "starting"})
	}

	if options.logCtrl != nil {
		options.logCtrl.StartPhase("start_new_container")
	}
	if err := options.runGreenContainer(); err != nil {
		return err
	}
	if options.logCtrl != nil {
		options.logCtrl.EndPhase("start_new_container", true)
	}

	m.set(id, map[string]any{"status": "switching"})

	expectedPort := 3000
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			portList, _ := app["ports"].([]any)
			if primary := ports.Primary(portList); primary != nil && jsTruthy(primary["container"]) {
				c, _ := jsNumber(primary["container"])
				expectedPort = int(c)
			}
		}
	})

	// TCP readiness: the green container must bind the expected port and
	// have a routable IP before traffic can flip.
	isReady := false
	greenIP := ""
	for attempts := 0; attempts < 120; attempts++ {
		listening := m.deps.Docker.GetListeningPorts(greenName)
		if containsInt(listening, expectedPort) {
			if ip, err := m.deps.Docker.GetIP(greenName); err == nil && ip != "" {
				greenIP = ip
				isReady = true
				break
			}
		}
		m.sleep(m.readyInterval)
	}

	if !isReady || greenIP == "" {
		m.deps.Docker.Stop(greenName)
		m.deps.Docker.Remove(greenName)
		m.cleanupGreenArtifacts(greenName)
		return errors.New("New container failed readiness probe (port bind timeout). " + operation + " aborted to maintain uptime.")
	}

	// L7 readiness: TCP listening does not guarantee HTTP serving; probing
	// eliminates the brief 502 window during the switch.
	if !m.httpHealthCheck(greenIP, expectedPort, 30) {
		m.deps.Docker.Stop(greenName)
		m.deps.Docker.Remove(greenName)
		m.cleanupGreenArtifacts(greenName)
		return errors.New("New container failed HTTP readiness probe. " + operation + " aborted to maintain uptime.")
	}

	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			app["ip"] = greenIP
			m.setLocked(id, map[string]any{"status": "running", "activeContainerId": greenName})
		}
	})

	m.spawn(func() {
		if err := m.scanAndSaveHTTPStatus(id); err != nil {
			m.dlog.Error("HTTP scan failed for %s: %s", appName, err.Error())
		}
	})

	if options.logCtrl != nil {
		options.logCtrl.StartPhase("proxy_propagation")
	}
	m.proxySync()
	m.proxyPurge(id)
	if options.logCtrl != nil {
		options.logCtrl.EndPhase("proxy_propagation", true)
	}

	if options.logCtrl != nil {
		options.logCtrl.StartPhase("stop_old_container")
	}
	// Drain: let in-flight requests against the blue container finish.
	m.sleep(m.deploySwitchDelay)

	m.endLogStream(appName)

	m.deps.Docker.Stop(appName)
	m.deps.Docker.Remove(appName)

	m.endLogStream(greenName)

	renameSuccess := false
	for i := 0; i < 5; i++ {
		if err := m.deps.Docker.Rename(greenName, appName); err == nil {
			renameSuccess = true
			break
		} else {
			m.dlog.Log("Docker rename failed, retrying in 2s (Attempt %s/5): %s", itoa(i+1), err.Error())
			m.sleep(m.renameInterval)
		}
	}

	if renameSuccess {
		m.set(id, map[string]any{"activeContainerId": nil})
		// The green name no longer refers to a live container, so its
		// on-disk log dir is orphaned. Skip on rename failure — there the
		// green name IS the live container.
		m.cleanupGreenArtifacts(greenName)
	} else {
		m.dlog.Error(
			"Failed to rename green container %s to %s after 5 attempts. ZDD will persist with activeContainerId.",
			greenName, appName,
		)
	}

	if err := m.attachLogger(appName); err != nil {
		m.dlog.Error("Failed to attach logger to app %s: %s", appName, err.Error())
	}

	if options.logCtrl != nil {
		m.sleep(m.readyInterval)
		options.logCtrl.EndPhase("stop_old_container", true)
	}

	m.set(id, map[string]any{"started": nowMs()})
	return nil
}

// endLogStream ends and forgets an app's runtime log stream.
func (m *Manager) endLogStream(name string) {
	m.mu.Lock()
	stream := m.logStreams[name]
	delete(m.logStreams, name)
	m.mu.Unlock()
	if stream != nil {
		stream.end()
	}
}

// httpHealthCheck ports Deploy's #httpHealthCheck: repeated GETs until one
// answers (any status code counts).
func (m *Manager) httpHealthCheck(ip string, port, maxAttempts int) bool {
	for i := 0; i < maxAttempts; i++ {
		if m.httpProbe(ip, port, m.healthTimeout, http.MethodGet) {
			m.dlog.Log("HTTP health check passed for %s:%s (attempt %s)", ip, itoa(port), itoa(i+1))
			return true
		}
		m.sleep(m.readyInterval)
	}
	return false
}

// cleanupGreenArtifacts ports Deploy.cleanupGreenArtifacts: remove the
// on-disk log dir + in-memory bookkeeping of a green container that is no
// longer live. No-op for non-green names.
func (m *Manager) cleanupGreenArtifacts(greenName string) {
	if greenName == "" || !greenSuffix.MatchString(greenName) {
		return
	}
	logDir := filepath.Join(m.logsRoot, greenName)
	if err := os.RemoveAll(logDir); err != nil {
		m.dlog.Log("Failed to remove stale green log dir for %s: %s", greenName, err.Error())
	}
	m.mu.Lock()
	delete(m.loggers, greenName)
	delete(m.logStreams, greenName)
	m.mu.Unlock()
	m.deps.Docker.UnregisterBuildLogger(greenName)
}

// CleanupStaleGreenLogs ports Deploy.cleanupStaleGreenLogs: startup sweep for
// green log dirs orphaned by past Blue-Green deploys — the names are
// short-lived by contract, so any dir found at startup is an orphan.
func (m *Manager) CleanupStaleGreenLogs() error {
	entries, err := os.ReadDir(m.logsRoot)
	if err != nil {
		return nil // no logs root yet
	}
	removed := 0
	for _, ent := range entries {
		if !ent.IsDir() || !greenSuffix.MatchString(ent.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.logsRoot, ent.Name())); err != nil {
			m.dlog.Log("Failed to remove stale green log dir %s: %s", ent.Name(), err.Error())
			continue
		}
		removed++
	}
	if removed > 0 {
		m.dlog.Log("Cleaned up %s stale green log dir(s)", itoa(removed))
	}
	return nil
}

// SweepGreenContainersFor ports Deploy.sweepGreenContainersFor: stop/remove
// every green companion of an app (the recorded activeContainerId plus any
// Docker container matching the green name pattern).
func (m *Manager) SweepGreenContainersFor(appName, activeContainerID string) {
	if !m.deps.Docker.Available() {
		return
	}

	candidates := map[string]bool{}
	if activeContainerID != "" && activeContainerID != appName {
		candidates[activeContainerID] = true
	}

	greenPrefix := appName + "-green-"
	for _, c := range m.deps.Docker.List() {
		for _, rawName := range c.Names {
			name := strings.TrimPrefix(rawName, "/")
			if strings.HasPrefix(name, greenPrefix) && greenSuffix.MatchString(name) {
				candidates[name] = true
			}
		}
	}

	// Deterministic order (Node's Set preserves insertion; nothing observes
	// the order beyond log lines).
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sortStrings(names)

	for _, name := range names {
		m.dlog.Log("Delete[%s]: sweeping green companion %s", appName, name)
		m.deps.Docker.Stop(name)
		m.deps.Docker.Remove(name)
		m.cleanupGreenArtifacts(name)
	}
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}
