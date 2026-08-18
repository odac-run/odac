package appmgr

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"odac/internal/netmode"
	"odac/internal/ports"
)

// wellKnownGuess is the no-container-IP fallback order (#pollForPort).
var wellKnownGuess = []int{80, 8080, 3000, 5000}

// hostNetworked reports whether the app runs in the host namespace. Takes
// cfg.View, so it must not be called from inside a cfg.Mutate block.
func (m *Manager) hostNetworked(id any) bool {
	hostNet := false
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			hostNet = netmode.IsHost(app["networkMode"])
		}
	})
	return hostNet
}

// containerAddr resolves the address ODAC probes and routes an app at. A
// host-mode container has no IP of its own — it answers on the host's
// loopback, which ODAC shares (see netmode.LoopbackAddr). Everything else is
// reached by its bridge IP. Returns "" when no address is available yet.
func (m *Manager) containerAddr(id any, containerName string) string {
	if m.hostNetworked(id) {
		return netmode.LoopbackAddr
	}
	ip, _ := m.deps.Docker.GetIP(containerName)
	return ip
}

// wellKnownHTTP is the multi-response preference order (#detectHttpPort).
var wellKnownHTTP = []int{80, 443, 8080, 3000, 5000, 8000}

// pollForPort ports #pollForPort (dev a9af39f semantics): after a start,
// watch the container's listening ports and correct/write the proxy mapping
// ONLY once a port provably answers HTTP. containerName may be a Blue-Green
// green container; id always resolves the real app, whose config receives
// the adoption.
//
// Unlike Node, the adoption re-fetches the app from the current working set
// under cfg.Mutate instead of mutating a map captured before the poll loop —
// Node's captured object may belong to an array a #saveApps already
// replaced, silently dropping the write (STATE.md 2026-07-12 trap).
func (m *Manager) pollForPort(id any, containerName string, expectedPort int) {
	for attempts := 0; attempts < 60; attempts++ { // give up after ~60s (selfhosted apps may take long to bind)
		if attempts > 0 {
			m.sleep(m.pollInterval)
		}

		var primaryContainer int
		var primaryExists, primaryAuto, appGone bool
		m.cfg.View(func() {
			app := m.getLocked(id)
			if app == nil {
				appGone = true
				return
			}
			portList, _ := app["ports"].([]any)
			if primary := ports.Primary(portList); primary != nil {
				primaryExists = true
				primaryAuto = ports.IsAuto(primary)
				c, _ := jsNumber(primary["container"])
				primaryContainer = int(c)
			}
		})
		if appGone {
			return
		}

		listening := m.deps.Docker.GetListeningPorts(containerName)
		if len(listening) == 0 {
			continue
		}

		// The mapping already points at a port the app binds: nothing to
		// discover.
		if primaryExists && containsInt(listening, primaryContainer) {
			return
		}

		// The mapped port is not there (but others are). It might be an
		// ephemeral/debug port while the main one is starting — give the
		// expected port 5 seconds to appear before acting.
		if attempts < 5 {
			continue
		}

		// Auto-discovery exists to correct a port ODAC guessed, or to write
		// the first mapping when there is none — never to touch a declared
		// one. A published binding is already applied on the host, and a
		// proxy entry the user or a recipe wrote is a setting they chose.
		if primaryExists && !primaryAuto {
			m.log.Log(
				"Auto-Discovery: App %s listens on %s but its config declares %s. Keeping the declared port.",
				containerName, joinInts(listening), itoa(primaryContainer),
			)
			return
		}

		preferred := 0

		// HTTP probe: a proxy mapping claims the ODAC proxy routes to that
		// port, so only a port that answers HTTP is worth writing. Probing
		// every open port — including a lone one — is what keeps a database
		// app from being handed a `proxy: 3306` mapping.
		containerIP := m.containerAddr(id, containerName)
		if containerIP != "" {
			preferred = m.detectHTTPPort(containerIP, listening)
			if preferred == 0 {
				// Nothing speaks HTTP yet. It may still come up — a slow app
				// binds its socket before it serves — so keep polling on the
				// remaining budget and leave the config untouched.
				if attempts == 5 {
					m.log.Log(
						"Auto-Discovery: App %s listens on %s but none of them answer HTTP. Leaving its ports alone.",
						containerName, joinInts(listening),
					)
				}
				continue
			}
			m.log.Log("Auto-Discovery: HTTP probe identified port %s for app %s", itoa(preferred), containerName)
		} else {
			// No container IP means the probe cannot run at all. Guess as
			// before: well-known HTTP ports, then the first one open.
			preferred = pickPort(listening, wellKnownGuess)
		}

		m.log.Log("Auto-Discovery: App %s serves HTTP on port %s (expected %s). Updating config...",
			containerName, itoa(preferred), itoa(expectedPort))

		adopted := false
		m.cfg.Mutate(func() {
			app := m.getLocked(id)
			if app == nil {
				return // deleted mid-poll
			}
			portList, _ := app["ports"].([]any)
			if primary := ports.Primary(portList); primary != nil {
				if !ports.IsAuto(primary) {
					return // declared while we probed — keep the user's setting
				}
				// Correct the guessed entry in place; it stays a guess, and
				// every other mapping keeps its position and contents.
				primary["container"] = float64(preferred)
			} else {
				// No mapping at all: now that a port is known to serve HTTP,
				// the proxy entry is written on evidence, not on a guess.
				app["ports"] = []any{ports.Discovered(preferred)}
			}
			// Cache container IP for zero-downtime proxy routing.
			if containerIP != "" {
				app["ip"] = containerIP
			}
			m.saveAppsLocked()
			adopted = true
		})

		if adopted {
			m.proxySync()
		}
		return
	}
}

// realHTTPProbe is the default httpProbe: one request to ip:port, any HTTP
// response (even 4xx/5xx) confirms the port speaks HTTP.
func realHTTPProbe(ip string, port int, timeout time.Duration, method string) bool {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(method, "http://"+ip+":"+strconv.Itoa(port)+"/", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// detectHTTPPort ports #detectHttpPort: parallel HEAD probes. 0 = none found.
func (m *Manager) detectHTTPPort(ip string, portList []int) int {
	results := make([]bool, len(portList))
	done := make(chan int, len(portList))

	for i, port := range portList {
		go func(i, port int) {
			results[i] = m.httpProbe(ip, port, m.probeTimeout, http.MethodHead)
			done <- i
		}(i, port)
	}
	for range portList {
		<-done
	}

	var httpPorts []int
	for i, ok := range results {
		if ok {
			httpPorts = append(httpPorts, portList[i])
		}
	}
	if len(httpPorts) == 0 {
		return 0
	}
	// If multiple ports respond to HTTP, prefer well-known HTTP ports.
	return pickPort(httpPorts, wellKnownHTTP)
}

// scanAndSaveHTTPStatus ports #scanAndSaveHttpStatus: detect whether an app
// serves HTTP at all and persist the detected port (or false) — only for
// apps never probed before (http absent) or probed empty (http === false).
func (m *Manager) scanAndSaveHTTPStatus(id any) error {
	var target string
	var hasPid, skip, appGone bool
	m.cfg.View(func() {
		app := m.getLocked(id)
		if app == nil {
			appGone = true
			return
		}
		httpVal, present := app["http"]
		if present && httpVal != false {
			skip = true
			return
		}
		target, _ = app["name"].(string)
		if acid, _ := app["activeContainerId"].(string); acid != "" {
			target = acid
		}
		hasPid = jsTruthy(app["pid"])
	})
	if appGone || skip {
		return nil
	}

	var newHTTP any = false

	// Docker-less hosts and locally running scripts skip container scanning.
	if m.deps.Docker.Available() && !hasPid {
		var listening []int
		for attempts := 0; attempts < 120; attempts++ {
			listening = m.deps.Docker.GetListeningPorts(target)
			if len(listening) > 0 {
				break
			}
			m.sleep(m.scanPortInterval)
		}

		if len(listening) > 0 {
			// containerAddr, not GetIP: a host-mode app has no container IP
			// and answers on loopback, so probing by IP alone would record it
			// as "serves no HTTP" and cost the proxy its app.http fallback.
			containerIP := m.containerAddr(id, target)
			if containerIP != "" {
				// Retry the probe: some apps open their TCP ports early but
				// take time to actually serve HTTP. Re-fetch listening ports
				// each attempt so newly opened ports join the probe set.
				httpPort := 0
				for probeAttempts := 0; probeAttempts < 30; probeAttempts++ {
					current := m.deps.Docker.GetListeningPorts(target)
					if len(current) == 0 {
						current = listening
					}
					httpPort = m.detectHTTPPort(containerIP, current)
					if httpPort != 0 {
						break
					}
					m.sleep(m.scanProbeInterval)
				}
				if httpPort != 0 {
					newHTTP = float64(httpPort)
				}
			}
		}
	}

	changed := false
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			return
		}
		if app["http"] != newHTTP {
			m.setLocked(id, map[string]any{"http": newHTTP})
			changed = true
		}
	})
	if changed {
		m.hubTrigger("app.list")
	}
	return nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// pickPort returns the first well-known port present in list, else list[0].
func pickPort(list []int, wellKnown []int) int {
	for _, p := range list {
		if containsInt(wellKnown, p) {
			return p
		}
	}
	return list[0]
}

func joinInts(list []int) string {
	parts := make([]string, len(list))
	for i, p := range list {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}
