package appmgr

// Port of App.test.js "runtime port auto-discovery" (dev a9af39f: adoption is
// HTTP-probe gated) plus the migrated-legacy-entry correction case.

import (
	"sync/atomic"
	"testing"
	"time"
)

// discover runs one check pulse for a single app whose container listens on
// `listening`; `httpPorts` names the ports that answer the probe. Returns
// the app's persisted ports afterwards.
func discover(t *testing.T, app map[string]any, listening, httpPorts []int) (*fixture, []any) {
	t.Helper()
	name, _ := app["name"].(string)
	fx := newFixture(t, []any{app})
	fx.dock.setListening(name, listening)
	fx.setHTTPPorts(httpPorts...)

	fx.checkAndSettle(t)

	p, _ := fx.app(0)["ports"].([]any)
	return fx, p
}

func TestDiscoveryAdoptsObservedPortForGuessedEntry(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
}

// Isolation blocks traffic routed off the host, not ODAC's own probe: the
// container keeps an IP, /proc is read over the Docker socket rather than the
// network, and ODAC shares the host namespace. Auto-discovery must therefore
// keep correcting an isolated app's proxy port like any other app's — losing
// it would leave the proxy pointed at a port the app never binds.
func TestDiscoveryAdoptsPortForIsolatedApp(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"isolated": true,
		"ports":    []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
}

// app.http is the proxy's fallback routing source, so the HTTP scan has to
// resolve an address the same way everything else does. A host-mode app has
// no container IP; probing by IP alone recorded it as serving nothing.
func TestHTTPScanResolvesAddressPerNetworkMode(t *testing.T) {
	// hasIP mirrors what Docker would report: bridge and isolated containers
	// have an address, a host-networked one does not.
	scan := func(t *testing.T, extra map[string]any, hasIP bool) any {
		t.Helper()
		app := map[string]any{
			"id": float64(1), "name": "web", "type": "container",
			"image": "n8n", "active": true,
		}
		for k, v := range extra {
			app[k] = v
		}
		fx := newFixture(t, []any{app})
		fx.dock.setListening("web", []int{8080})
		fx.setHTTPPorts(8080)
		if !hasIP {
			fx.dock.dropIPs()
		}

		if err := fx.m.scanAndSaveHTTPStatus(float64(1)); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		return fx.app(0)["http"]
	}

	if got := scan(t, nil, true); got != float64(8080) {
		t.Errorf("bridge app: http = %v, want 8080", got)
	}
	if got := scan(t, map[string]any{"isolated": true}, true); got != float64(8080) {
		t.Errorf("isolated app: http = %v, want 8080", got)
	}
	if got := scan(t, map[string]any{"networkMode": "host"}, false); got != float64(8080) {
		t.Errorf("host app: http = %v, want 8080 (probed on loopback)", got)
	}
	// A bridge app with no address yet genuinely cannot be probed.
	if got := scan(t, nil, false); got != false {
		t.Errorf("bridge app without an IP: http = %v, want false", got)
	}
}

func TestDiscoveryKeepsCorrectingACorrectedGuess(t *testing.T) {
	// The entry stays a guess after a correction, or a wrong first guess
	// would freeze the app on a port it never binds.
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(5678), "auto": true}},
	}, []int{8080}, []int{8080})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(8080), "auto": true})
}

func TestDiscoveryDoesNotClobberPublishedBinding(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": float64(8080), "container": float64(3000)}},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": float64(8080), "container": float64(3000)})
}

func TestDiscoveryPreservesSecondaryMappings(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{
			map[string]any{"host": "proxy", "container": float64(3000), "auto": true},
			map[string]any{"host": float64(9000), "container": float64(9000)},
		},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
	portEq(t, p[1], map[string]any{"host": float64(9000), "container": float64(9000)})
}

func TestDiscoveryCorrectsProxyEntryAnywhereInList(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{
			map[string]any{"host": float64(9000), "container": float64(9000)},
			map[string]any{"host": "proxy", "container": float64(3000), "auto": true},
		},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": float64(9000), "container": float64(9000)})
	portEq(t, p[1], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
}

func TestDiscoveryLeavesMatchingConfigAlone(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000)}},
	}, []int{3000}, []int{3000})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000)})
}

func TestDiscoveryHonoursDeclaredPortTheAppBinds(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(5000)}},
	}, []int{3000, 5000}, []int{3000, 5000})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5000)})
}

func TestDiscoveryNeverOverwritesDeclaredPort(t *testing.T) {
	// Unroutable, but the user's choice: silently healing it would undo the
	// port they just saved from the dashboard.
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(5000)}},
	}, []int{3000}, []int{3000})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5000)})
}

func TestDiscoverySkipsRecipeDeclaredEntry(t *testing.T) {
	// Recipe ports go through preparePorts, which never stamps `auto`.
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000)}},
	}, []int{5678}, []int{5678})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000)})
}

func TestDiscoveryIgnoresLoneNonHTTPPort(t *testing.T) {
	// A database app binds 5432 and nothing else; adopting it would hand the
	// proxy a backend that can never serve a request.
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "db", "type": "container", "image": "postgres", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}, []int{5432}, nil)

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000), "auto": true})
}

func TestDiscoveryAdoptsHTTPPortNextToDatabasePort(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}, []int{5432, 8000}, []int{8000})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(8000), "auto": true})
}

func TestDiscoveryKeepsPollingUntilHTTPAnswers(t *testing.T) {
	// The app binds its socket before it serves: the first probes fail, the
	// poller must retry on its budget instead of adopting or giving up.
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}})
	fx.dock.setListening("web", []int{5678})

	var probes atomic.Int64
	fx.m.httpProbe = func(_ string, port int, _ time.Duration, _ string) bool {
		// Answer only after several failed probe rounds.
		return probes.Add(1) > 8 && port == 5678
	}

	fx.checkAndSettle(t)

	p, _ := fx.app(0)["ports"].([]any)
	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
	if probes.Load() <= 8 {
		t.Fatalf("expected repeated probing, got %d probes", probes.Load())
	}
}

func TestDiscoveryGuessesWellKnownPortWithoutContainerIP(t *testing.T) {
	// No IP means no probe. Guessing beats leaving the app unroutable.
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000), "auto": true}},
	}})
	fx.dock.setListening("web", []int{5432, 8080})
	fx.dock.mu.Lock()
	fx.dock.ips = map[string]string{} // GetIP fails for every container
	fx.dock.mu.Unlock()
	fx.setHTTPPorts()

	fx.checkAndSettle(t)

	p, _ := fx.app(0)["ports"].([]any)
	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(8080), "auto": true})
}

func TestDiscoveryGivesDatabaseAppNoMapping(t *testing.T) {
	// MariaDB EXPOSEs 3306 and binds it, but nothing there speaks HTTP: the
	// proxy has no business claiming it. PORT still comes from EXPOSE.
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "db", "type": "container", "image": "mariadb", "active": true,
	}})
	fx.dock.mu.Lock()
	fx.dock.exposed["mariadb"] = []int{3306}
	fx.dock.mu.Unlock()
	fx.dock.setListening("db", []int{3306})
	fx.setHTTPPorts()

	fx.checkAndSettle(t)

	if _, present := fx.app(0)["ports"]; present {
		t.Fatalf("ports should stay absent: %v", fx.app(0)["ports"])
	}
	if env := fx.dock.runCallAt(0).options.Env; env["PORT"] != "3306" {
		t.Fatalf("PORT = %q", env["PORT"])
	}
}

func TestDiscoveryWritesMappingForPortlessAppOnEvidence(t *testing.T) {
	_, p := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
	}, []int{8080}, []int{8080})

	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(8080), "auto": true})
}

func TestDiscoveryNeverWritesMappingFromExposeAlone(t *testing.T) {
	// EXPOSE is metadata, not evidence: the mapping waits for the probe.
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
	}})
	fx.dock.mu.Lock()
	fx.dock.exposed["app"] = []int{8080}
	fx.dock.mu.Unlock()
	fx.dock.setListening("web", nil)
	fx.setHTTPPorts()

	fx.checkAndSettle(t)

	if _, present := fx.app(0)["ports"]; present {
		t.Fatalf("ports should stay absent: %v", fx.app(0)["ports"])
	}
}

func TestDiscoveryCorrectsMigratedLegacyEntry(t *testing.T) {
	// Pre-sentinel legacy entries get {host:'proxy', auto:true} on load and
	// must stay correctable — migration must not freeze them on a port the
	// app never binds.
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "n8n", "active": true,
		"ports": []any{map[string]any{"container": float64(3000)}},
	}})
	fx.dock.setListening("web", []int{5678})
	fx.setHTTPPorts(5678)

	fx.checkAndSettle(t)

	p, _ := fx.app(0)["ports"].([]any)
	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(5678), "auto": true})
}

func TestRunHandsDockerOnlyPublishedPorts(t *testing.T) {
	fx, _ := discover(t, map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "nginx", "active": true,
		"ports": []any{
			map[string]any{"host": "proxy", "container": float64(3000)},
			map[string]any{"host": float64(9000), "container": float64(9000)},
		},
	}, []int{3000}, []int{3000})

	opts := fx.dock.runCallAt(0).options
	if len(opts.Ports) != 1 {
		t.Fatalf("published ports = %v", opts.Ports)
	}
	portEq(t, opts.Ports[0], map[string]any{"host": float64(9000), "container": float64(9000)})
}

func TestRunCarriesPublicFlagToDocker(t *testing.T) {
	fx, _ := discover(t, map[string]any{
		"id": float64(1), "name": "db", "type": "container", "image": "mysql", "active": true,
		"ports": []any{map[string]any{"host": float64(3307), "container": float64(3306), "public": true}},
	}, []int{3306}, []int{3306})

	opts := fx.dock.runCallAt(0).options
	portEq(t, opts.Ports[0], map[string]any{"host": float64(3307), "container": float64(3306), "public": true})
}
