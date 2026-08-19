package appmgr

// Ports of App.test.js "privileged access", "setPorts()", "legacy port
// migration" plus deviceAdd.test.js / deviceDelete.test.js.

import (
	"strings"
	"testing"
)

// ---- privileged access ----

func TestSetPrivileged(t *testing.T) {
	newPriv := func(t *testing.T, extra map[string]any) *fixture {
		app := map[string]any{"id": float64(1), "name": "priv-app", "type": "container"}
		for k, v := range extra {
			app[k] = v
		}
		return newFixture(t, []any{app})
	}

	t.Run("defaults to root and persists", func(t *testing.T) {
		fx := newPriv(t, nil)
		if r := fx.m.SetPrivileged("priv-app", ""); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if fx.app(0)["privileged"] != "root" {
			t.Fatalf("privileged = %v", fx.app(0)["privileged"])
		}
	})

	t.Run("accepts full", func(t *testing.T) {
		fx := newPriv(t, nil)
		fx.m.SetPrivileged("priv-app", "full")
		if fx.app(0)["privileged"] != "full" {
			t.Fatalf("privileged = %v", fx.app(0)["privileged"])
		}
	})

	t.Run("off removes the flag", func(t *testing.T) {
		fx := newPriv(t, map[string]any{"privileged": "full"})
		if r := fx.m.SetPrivileged("priv-app", "off"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if _, present := fx.app(0)["privileged"]; present {
			t.Fatalf("flag not removed: %v", fx.app(0))
		}
	})

	t.Run("rejects invalid mode without touching the app", func(t *testing.T) {
		fx := newPriv(t, nil)
		if r := fx.m.SetPrivileged("priv-app", "superuser"); r.Status {
			t.Fatal("invalid mode accepted")
		}
		if _, present := fx.app(0)["privileged"]; present {
			t.Fatalf("app touched: %v", fx.app(0))
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newFixture(t, []any{})
		if r := fx.m.SetPrivileged("ghost", ""); r.Status {
			t.Fatal("expected failure")
		}
	})
}

func TestPrivilegeAppliedToRunOptions(t *testing.T) {
	runWith := func(t *testing.T, privileged string) runCall {
		app := map[string]any{
			"id": float64(1), "name": "p-app", "active": true,
			"type": "container", "image": "test:latest",
		}
		if privileged != "" {
			app["privileged"] = privileged
		}
		fx := newFixture(t, []any{app})
		fx.checkAndSettle(t)
		if fx.dock.runCallCount() == 0 {
			t.Fatal("runApp not called")
		}
		return fx.dock.runCallAt(0)
	}

	t.Run("full: Docker Privileged + root", func(t *testing.T) {
		opts := runWith(t, "full").options
		if !opts.Privileged || opts.User != "root" {
			t.Fatalf("opts = %+v", opts)
		}
	})
	t.Run("root: root user only", func(t *testing.T) {
		opts := runWith(t, "root").options
		if opts.Privileged || opts.User != "root" {
			t.Fatalf("opts = %+v", opts)
		}
	})
	t.Run("plain: no elevation", func(t *testing.T) {
		opts := runWith(t, "").options
		if opts.Privileged || opts.User != "" {
			t.Fatalf("opts = %+v", opts)
		}
	})
}

// ---- network mode ----

func TestSetNetworkMode(t *testing.T) {
	newNet := func(t *testing.T, extra map[string]any) *fixture {
		app := map[string]any{"id": float64(1), "name": "net-app", "type": "container"}
		for k, v := range extra {
			app[k] = v
		}
		return newFixture(t, []any{app})
	}

	t.Run("host persists and drops the stale address", func(t *testing.T) {
		fx := newNet(t, map[string]any{"ip": "172.20.0.5"})
		if r := fx.m.SetNetworkMode("net-app", "host"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if fx.app(0)["networkMode"] != "host" {
			t.Fatalf("networkMode = %v", fx.app(0)["networkMode"])
		}
		if _, present := fx.app(0)["ip"]; present {
			t.Fatalf("stale bridge ip kept: %v", fx.app(0))
		}
	})

	t.Run("bridge removes the key rather than storing a default", func(t *testing.T) {
		fx := newNet(t, map[string]any{"networkMode": "host"})
		if r := fx.m.SetNetworkMode("net-app", "bridge"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if _, present := fx.app(0)["networkMode"]; present {
			t.Fatalf("key not removed: %v", fx.app(0))
		}
	})

	t.Run("empty mode means bridge", func(t *testing.T) {
		fx := newNet(t, map[string]any{"networkMode": "host"})
		if r := fx.m.SetNetworkMode("net-app", ""); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if _, present := fx.app(0)["networkMode"]; present {
			t.Fatalf("key not removed: %v", fx.app(0))
		}
	})

	t.Run("rejects invalid mode without touching the app", func(t *testing.T) {
		fx := newNet(t, nil)
		if r := fx.m.SetNetworkMode("net-app", "macvlan"); r.Status {
			t.Fatal("invalid mode accepted")
		}
		if _, present := fx.app(0)["networkMode"]; present {
			t.Fatalf("app touched: %v", fx.app(0))
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newFixture(t, []any{})
		if r := fx.m.SetNetworkMode("ghost", "host"); r.Status {
			t.Fatal("expected failure")
		}
	})

	// Host mode costs zero-downtime deploys, so a routed app may not take it.
	// The gate lives in SetNetworkMode, not the CLI action, so every transport
	// (CLI and Hub alike) gets the same answer.
	t.Run("refuses host mode for an app with domains", func(t *testing.T) {
		fx := newNet(t, nil)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "net-app"}})
		})

		r := fx.m.SetNetworkMode("net-app", "host")
		if r.Status {
			t.Fatal("host mode accepted for a routed app")
		}
		if !strings.Contains(jsString(r.Message), "domains") {
			t.Fatalf("message = %q", jsString(r.Message))
		}
		if _, present := fx.app(0)["networkMode"]; present {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	// The domain gate must not block the safe direction.
	t.Run("bridge is always allowed, domains or not", func(t *testing.T) {
		fx := newNet(t, map[string]any{"networkMode": "host"})
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "net-app"}})
		})

		if r := fx.m.SetNetworkMode("net-app", "bridge"); !r.Status {
			t.Fatalf("bridge refused for a routed app: %v", r.Message)
		}
		if _, present := fx.app(0)["networkMode"]; present {
			t.Fatalf("key not removed: %v", fx.app(0))
		}
	})

	// The domain record may key on the numeric id instead of the name.
	t.Run("refuses when the domain references the app by id", func(t *testing.T) {
		fx := newNet(t, nil)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": float64(1)}})
		})

		if r := fx.m.SetNetworkMode("net-app", "host"); r.Status {
			t.Fatal("host mode accepted for an id-routed app")
		}
	})

	// Docker refuses to attach a host-namespace container to a bridge, so the
	// extra-networks command must answer before reaching the daemon.
	t.Run("host mode rejects extra networks", func(t *testing.T) {
		fx := newNet(t, map[string]any{"networkMode": "host"})
		r := fx.m.SetNetworks("net-app", []any{"other-net"}, true)
		if r.Status {
			t.Fatal("extra networks accepted in host mode")
		}
		if !strings.Contains(jsString(r.Message), "host networking") {
			t.Fatalf("message = %q", jsString(r.Message))
		}
	})
}

// ---- egress isolation (its own axis, not a network mode) ----

func TestSetIsolated(t *testing.T) {
	newIso := func(t *testing.T, extra map[string]any) *fixture {
		app := map[string]any{"id": float64(1), "name": "iso-app", "type": "container"}
		for k, v := range extra {
			app[k] = v
		}
		return newFixture(t, []any{app})
	}

	t.Run("persists and drops the stale address", func(t *testing.T) {
		fx := newIso(t, map[string]any{"ip": "172.20.0.5"})
		if r := fx.m.SetIsolated("iso-app", true); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if fx.app(0)["isolated"] != true {
			t.Fatalf("isolated = %v", fx.app(0)["isolated"])
		}
		if _, present := fx.app(0)["ip"]; present {
			t.Fatalf("stale address kept: %v", fx.app(0))
		}
	})

	t.Run("off removes the key", func(t *testing.T) {
		fx := newIso(t, map[string]any{"isolated": true})
		if r := fx.m.SetIsolated("iso-app", false); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if _, present := fx.app(0)["isolated"]; present {
			t.Fatalf("key not removed: %v", fx.app(0))
		}
	})

	// Isolation is orthogonal to the network mode, but not to host mode: a
	// host-namespace container has no bridge of its own to cut off. Both
	// directions must refuse, or an operator ends up with an app that reports
	// itself isolated while sharing the host's stack.
	t.Run("refuses on a host-networked app", func(t *testing.T) {
		fx := newIso(t, map[string]any{"networkMode": "host"})
		r := fx.m.SetIsolated("iso-app", true)
		if r.Status {
			t.Fatal("isolation accepted for a host-networked app")
		}
		if _, present := fx.app(0)["isolated"]; present {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	t.Run("host mode refused while isolated", func(t *testing.T) {
		fx := newIso(t, map[string]any{"isolated": true})
		r := fx.m.SetNetworkMode("iso-app", "host")
		if r.Status {
			t.Fatal("host mode accepted for an isolated app")
		}
		if fx.app(0)["networkMode"] != nil {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	// Turning isolation off must always be possible, whatever the state.
	t.Run("off is allowed even on a host-networked app", func(t *testing.T) {
		fx := newIso(t, map[string]any{"networkMode": "host"})
		if r := fx.m.SetIsolated("iso-app", false); !r.Status {
			t.Fatalf("clearing isolation refused: %v", r.Message)
		}
	})

	// Isolated apps keep a container IP, so the proxy reaches them and
	// Blue-Green still works — the domain restriction is host-mode only.
	t.Run("allowed for an app with domains", func(t *testing.T) {
		fx := newIso(t, nil)
		fx.cfg.Mutate(func() {
			fx.cfg.Set("domains", map[string]any{"example.com": map[string]any{"appId": "iso-app"}})
		})
		if r := fx.m.SetIsolated("iso-app", true); !r.Status {
			t.Fatalf("isolation refused for a routed app: %v", r.Message)
		}
	})

	// Attaching an isolated app to an ordinary bridge hands it egress again.
	t.Run("rejects extra networks", func(t *testing.T) {
		fx := newIso(t, map[string]any{"isolated": true})
		r := fx.m.SetNetworks("iso-app", []any{"other-net"}, true)
		if r.Status {
			t.Fatal("extra networks accepted for an isolated app")
		}
		if !strings.Contains(jsString(r.Message), "isolated") {
			t.Fatalf("message = %q", jsString(r.Message))
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newFixture(t, []any{})
		if r := fx.m.SetIsolated("ghost", true); r.Status {
			t.Fatal("expected failure")
		}
	})
}

func TestIsolationAppliedToRunOptions(t *testing.T) {
	runWith := func(t *testing.T, isolated bool) runCall {
		app := map[string]any{
			"id": float64(1), "name": "i-app", "active": true,
			"type": "container", "image": "test:latest",
		}
		if isolated {
			app["isolated"] = true
		}
		fx := newFixture(t, []any{app})
		fx.checkAndSettle(t)
		if fx.dock.runCallCount() == 0 {
			t.Fatal("runApp not called")
		}
		return fx.dock.runCallAt(0)
	}

	if !runWith(t, true).options.Isolated {
		t.Error("isolation lost on the way to RunOptions")
	}
	if runWith(t, false).options.Isolated {
		t.Error("isolation set for a plain app")
	}
}

func TestNetworkModeAppliedToRunOptions(t *testing.T) {
	runWith := func(t *testing.T, mode string) runCall {
		app := map[string]any{
			"id": float64(1), "name": "n-app", "active": true,
			"type": "container", "image": "test:latest",
		}
		if mode != "" {
			app["networkMode"] = mode
		}
		fx := newFixture(t, []any{app})
		fx.checkAndSettle(t)
		if fx.dock.runCallCount() == 0 {
			t.Fatal("runApp not called")
		}
		return fx.dock.runCallAt(0)
	}

	if got := runWith(t, "host").options.NetworkMode; got != "host" {
		t.Errorf("host: NetworkMode = %q", got)
	}
	if got := runWith(t, "").options.NetworkMode; got != "bridge" {
		t.Errorf("unset: NetworkMode = %q, want the canonical bridge default", got)
	}
	// A hand-edited config must degrade to the isolated default, not fail.
	if got := runWith(t, "macvlan").options.NetworkMode; got != "bridge" {
		t.Errorf("garbage: NetworkMode = %q", got)
	}
}

// ---- devices (deviceAdd.test.js / deviceDelete.test.js) ----

func TestDeviceAdd(t *testing.T) {
	newDev := func(t *testing.T) *fixture {
		return newFixture(t, []any{map[string]any{"id": float64(1), "name": "test-app", "devices": []any{}}})
	}
	devices := func(fx *fixture) []any {
		d, _ := fx.app(0)["devices"].([]any)
		return d
	}

	t.Run("adds a new device", func(t *testing.T) {
		fx := newDev(t)
		if r := fx.m.DeviceAdd(float64(1), "/dev/ttyACM0", ""); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		d := devices(fx)
		if len(d) != 1 {
			t.Fatalf("devices = %v", d)
		}
		dev, _ := d[0].(map[string]any)
		if dev["host"] != "/dev/ttyACM0" || dev["container"] != "/dev/ttyACM0" {
			t.Fatalf("device = %v", dev)
		}
	})

	t.Run("custom container path", func(t *testing.T) {
		fx := newDev(t)
		fx.m.DeviceAdd(float64(1), "/dev/ttyACM0", "/dev/arduino")
		dev, _ := devices(fx)[0].(map[string]any)
		if dev["container"] != "/dev/arduino" {
			t.Fatalf("device = %v", dev)
		}
	})

	t.Run("updates existing mapping in place", func(t *testing.T) {
		fx := newDev(t)
		fx.m.DeviceAdd(float64(1), "/dev/ttyACM0", "/dev/old")
		if r := fx.m.DeviceAdd(float64(1), "/dev/ttyACM0", "/dev/new"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		d := devices(fx)
		if len(d) != 1 {
			t.Fatalf("devices = %v", d)
		}
		dev, _ := d[0].(map[string]any)
		if dev["container"] != "/dev/new" {
			t.Fatalf("device = %v", dev)
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newDev(t)
		if r := fx.m.DeviceAdd(float64(999), "/dev/ttyACM0", ""); r.Status {
			t.Fatal("expected failure")
		}
	})

	t.Run("missing host path", func(t *testing.T) {
		fx := newDev(t)
		r := fx.m.DeviceAdd(float64(1), "", "")
		if r.Status || !strings.Contains(jsString(r.Message), "Missing host") {
			t.Fatalf("r = %+v", r)
		}
	})
}

func TestDeviceDelete(t *testing.T) {
	t.Run("removes the mapping", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "test-app",
			"devices": []any{
				map[string]any{"host": "/dev/ttyACM0", "container": "/dev/ttyACM0"},
				map[string]any{"host": "/dev/video0", "container": "/dev/video0"},
			},
		}})
		if r := fx.m.DeviceDelete(float64(1), "/dev/ttyACM0"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		d, _ := fx.app(0)["devices"].([]any)
		if len(d) != 1 {
			t.Fatalf("devices = %v", d)
		}
		dev, _ := d[0].(map[string]any)
		if dev["host"] != "/dev/video0" {
			t.Fatalf("wrong device removed: %v", d)
		}
	})

	t.Run("no devices at all", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "test-app"}})
		r := fx.m.DeviceDelete(float64(1), "/dev/ttyACM0")
		if !r.Status || !strings.Contains(jsString(r.Message), "No devices connected") {
			t.Fatalf("r = %+v", r)
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newFixture(t, []any{})
		if r := fx.m.DeviceDelete(float64(999), "/dev/x"); r.Status {
			t.Fatal("expected failure")
		}
	})
}

// ---- setPorts() ----

func portsFixture(t *testing.T) *fixture {
	return newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "web", "type": "container", "image": "nginx",
		"ports": []any{map[string]any{"host": "proxy", "container": float64(3000)}},
	}})
}

func portsOf(fx *fixture) []any {
	p, _ := fx.app(0)["ports"].([]any)
	return p
}

func portEq(t *testing.T, got any, want map[string]any) {
	t.Helper()
	gm, _ := got.(map[string]any)
	if gm == nil || len(gm) != len(want) {
		t.Fatalf("port entry = %v, want %v", got, want)
	}
	for k, v := range want {
		if gm[k] != v {
			t.Fatalf("port entry[%s] = %v, want %v (%v)", k, gm[k], v, gm)
		}
	}
}

func TestSetPortsValidation(t *testing.T) {
	fail := func(t *testing.T, payload []any, ok bool, wantMsg string) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", payload, ok)
		if r.Status {
			t.Fatalf("expected failure, got success")
		}
		if !strings.Contains(jsString(r.Message), wantMsg) {
			t.Fatalf("message %q does not contain %q", jsString(r.Message), wantMsg)
		}
		// Validation failures must not mutate the app.
		portEq(t, portsOf(fx)[0], map[string]any{"host": "proxy", "container": float64(3000)})
	}

	t.Run("unknown app", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("ghost", []any{}, true)
		if r.Status || !strings.Contains(jsString(r.Message), "not found") {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("non-array payload", func(t *testing.T) {
		fail(t, nil, false, "Expected an array")
	})
	t.Run("missing container port", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(8080)}}, true, "must have a container port")
	})
	t.Run("empty container port", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(8080), "container": ""}}, true, "must have a container port")
	})
	t.Run("missing host", func(t *testing.T) {
		fail(t, []any{map[string]any{"container": float64(3000)}}, true, "must have a host port")
	})
	t.Run("empty host", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": "", "container": float64(3000)}}, true, "must have a host port")
	})
	t.Run("out-of-range host port", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(70000), "container": float64(3000)}}, true, "Invalid host port")
	})
	t.Run("out-of-range container port", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(8080), "container": float64(0)}}, true, "Invalid container port")
	})
	t.Run("garbage host value", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": "internal", "container": float64(3000)}}, true, "Invalid host port")
	})
	t.Run("second proxy entry", func(t *testing.T) {
		fail(t, []any{
			map[string]any{"host": "proxy", "container": float64(3000)},
			map[string]any{"host": "proxy", "container": float64(4000)},
		}, true, "Only one port may be routed by the proxy")
	})
	t.Run("duplicate host ports", func(t *testing.T) {
		fail(t, []any{
			map[string]any{"host": float64(8080), "container": float64(3000)},
			map[string]any{"host": float64(8080), "container": float64(4000)},
		}, true, "Duplicate host port")
	})
	t.Run("public proxy port", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": "proxy", "container": float64(3000), "public": true}}, true, "cannot be public")
	})
	t.Run("non-boolean public flag", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(3307), "container": float64(3306), "public": "yes"}}, true, "Invalid public flag")
	})
	t.Run("numeric public flag", func(t *testing.T) {
		fail(t, []any{map[string]any{"host": float64(3307), "container": float64(3306), "public": float64(1)}}, true, "Invalid public flag")
	})
}

func TestSetPortsAccepts(t *testing.T) {
	t.Run("proxy sentinel", func(t *testing.T) {
		fx := portsFixture(t)
		if r := fx.m.SetPorts("web", []any{map[string]any{"host": "proxy", "container": float64(8000)}}, true); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		portEq(t, portsOf(fx)[0], map[string]any{"host": "proxy", "container": float64(8000)})
	})

	t.Run("published host binding", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": float64(8080), "container": float64(3000)}}, true)
		portEq(t, portsOf(fx)[0], map[string]any{"host": float64(8080), "container": float64(3000)})
	})

	t.Run("coerces string ports to numbers", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": "8080", "container": "3000"}}, true)
		portEq(t, portsOf(fx)[0], map[string]any{"host": float64(8080), "container": float64(3000)})
	})

	t.Run("resolves an auto host port", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": "auto", "container": float64(3000)}}, true)
		entry, _ := portsOf(fx)[0].(map[string]any)
		host, _ := entry["host"].(float64)
		if host < 30000 {
			t.Fatalf("auto host = %v", entry["host"])
		}
		if entry["container"] != float64(3000) {
			t.Fatalf("container = %v", entry["container"])
		}
	})

	t.Run("mixed published and proxy entries", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", []any{
			map[string]any{"host": "proxy", "container": float64(3000)},
			map[string]any{"host": float64(9000), "container": float64(9000)},
		}, true)
		if !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		p := portsOf(fx)
		portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000)})
		portEq(t, p[1], map[string]any{"host": float64(9000), "container": float64(9000)})
	})

	t.Run("one container port on two host ports", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", []any{
			map[string]any{"host": float64(8080), "container": float64(3000)},
			map[string]any{"host": float64(9090), "container": float64(3000)},
		}, true)
		if !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
	})

	t.Run("same container port proxied and published", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", []any{
			map[string]any{"host": "proxy", "container": float64(3000)},
			map[string]any{"host": float64(8080), "container": float64(3000)},
		}, true)
		if !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
	})

	t.Run("two auto ports resolve distinct", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", []any{
			map[string]any{"host": "auto", "container": float64(3000)},
			map[string]any{"host": "auto", "container": float64(4000)},
		}, true)
		if !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		p := portsOf(fx)
		h0, _ := p[0].(map[string]any)
		h1, _ := p[1].(map[string]any)
		if h0["host"] == h1["host"] {
			t.Fatalf("auto hosts collided: %v vs %v", h0["host"], h1["host"])
		}
	})

	t.Run("Hub-shaped string payload", func(t *testing.T) {
		fx := portsFixture(t)
		r := fx.m.SetPorts("web", []any{
			map[string]any{"host": "proxy", "container": "3000"},
			map[string]any{"host": "8080", "container": "8080"},
		}, true)
		if !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		p := portsOf(fx)
		portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000)})
		portEq(t, p[1], map[string]any{"host": float64(8080), "container": float64(8080)})
	})

	t.Run("persists the public flag", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": float64(3307), "container": float64(3306), "public": true}}, true)
		portEq(t, portsOf(fx)[0], map[string]any{"host": float64(3307), "container": float64(3306), "public": true})
	})

	t.Run("omits a false public flag", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{
			map[string]any{"host": float64(3307), "container": float64(3306), "public": false},
			map[string]any{"host": float64(8080), "container": float64(8080)},
		}, true)
		p := portsOf(fx)
		portEq(t, p[0], map[string]any{"host": float64(3307), "container": float64(3306)})
		portEq(t, p[1], map[string]any{"host": float64(8080), "container": float64(8080)})
	})

	t.Run("auto host port can be public", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": "auto", "container": float64(3306), "public": true}}, true)
		entry, _ := portsOf(fx)[0].(map[string]any)
		if entry["public"] != true {
			t.Fatalf("public = %v", entry["public"])
		}
		if _, isNum := entry["host"].(float64); !isNum {
			t.Fatalf("host = %v", entry["host"])
		}
	})

	t.Run("string true public flag", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": "3307", "container": "3306", "public": "true"}}, true)
		portEq(t, portsOf(fx)[0], map[string]any{"host": float64(3307), "container": float64(3306), "public": true})
	})

	t.Run("string false is not public", func(t *testing.T) {
		fx := portsFixture(t)
		fx.m.SetPorts("web", []any{map[string]any{"host": "3307", "container": "3306", "public": "false"}}, true)
		entry, _ := portsOf(fx)[0].(map[string]any)
		if _, present := entry["public"]; present {
			t.Fatalf("public should be absent: %v", entry)
		}
	})
}

// ---- legacy port migration ----

func TestLegacyPortMigration(t *testing.T) {
	t.Run("stamps sentinel + auto onto host-less entries on load", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "web", "type": "container",
			"ports": []any{map[string]any{"container": float64(3000)}},
		}})
		fx.m.Check() // any reload normalizes; init already did
		r := fx.m.List(true)
		data, _ := r.Data.([]any)
		app, _ := data[0].(map[string]any)
		p, _ := app["ports"].([]any)
		portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000), "auto": true})
	})

	t.Run("leaves published entries alone", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "web", "type": "container",
			"ports": []any{map[string]any{"host": float64(8080), "container": float64(3000)}},
		}})
		r := fx.m.List(true)
		data, _ := r.Data.([]any)
		app, _ := data[0].(map[string]any)
		p, _ := app["ports"].([]any)
		portEq(t, p[0], map[string]any{"host": float64(8080), "container": float64(3000)})
	})

	t.Run("tolerates apps with no ports", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "web", "type": "script"}})
		fx.m.Check() // must not panic
	})
}

// ---- API access grants ----

func TestSetAPI(t *testing.T) {
	newAPIApp := func(t *testing.T, extra map[string]any) *fixture {
		app := map[string]any{"id": float64(1), "name": "api-app", "type": "container"}
		for k, v := range extra {
			app[k] = v
		}
		return newFixture(t, []any{app})
	}

	t.Run("persists a validated action list", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("api-app", []any{"app.list", "mail.send"}); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		got, _ := fx.app(0)["api"].([]any)
		if len(got) != 2 || got[0] != "app.list" || got[1] != "mail.send" {
			t.Fatalf("api = %v", fx.app(0)["api"])
		}
	})

	t.Run("splits and dedupes a comma-separated string", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("api-app", "app.list, mail.send,app.list"); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		got, _ := fx.app(0)["api"].([]any)
		if len(got) != 2 {
			t.Fatalf("api = %v", fx.app(0)["api"])
		}
	})

	t.Run("true grants every action", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("api-app", true); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if fx.app(0)["api"] != true {
			t.Fatalf("api = %v", fx.app(0)["api"])
		}
	})

	t.Run("wildcard normalizes to true", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("api-app", []any{"app.list", "*"}); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if fx.app(0)["api"] != true {
			t.Fatalf("api = %v", fx.app(0)["api"])
		}
	})

	t.Run("false removes the key", func(t *testing.T) {
		fx := newAPIApp(t, map[string]any{"api": true})
		if r := fx.m.SetAPI("api-app", false); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		if _, present := fx.app(0)["api"]; present {
			t.Fatalf("key not removed: %v", fx.app(0))
		}
	})

	// The shapes that used to persist happily and then answer
	// permission_denied on every request with no way to tell why.
	t.Run("refuses unknown actions", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		r := fx.m.SetAPI("api-app", []any{"app.list", "app.lst"})
		if r.Status {
			t.Fatal("unknown action accepted")
		}
		if msg, _ := r.Message.(string); !strings.Contains(msg, "app.lst") {
			t.Fatalf("message does not name the typo: %v", r.Message)
		}
		if _, present := fx.app(0)["api"]; present {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	// Server control is not a permission an operator can hand out by mistake.
	t.Run("refuses server-control actions", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		r := fx.m.SetAPI("api-app", []any{"app.list", "server.stop"})
		if r.Status {
			t.Fatal("server.stop grant accepted")
		}
		if msg, _ := r.Message.(string); !strings.Contains(msg, "server.stop") {
			t.Fatalf("message does not name the action: %v", r.Message)
		}
		if _, present := fx.app(0)["api"]; present {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	t.Run("refuses an empty list", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("api-app", []any{}); r.Status {
			t.Fatal("empty grant accepted")
		}
		if _, present := fx.app(0)["api"]; present {
			t.Fatalf("app touched after refusal: %v", fx.app(0))
		}
	})

	t.Run("refuses a non-action shape", func(t *testing.T) {
		fx := newAPIApp(t, map[string]any{"api": []any{"app.list"}})
		if r := fx.m.SetAPI("api-app", map[string]any{"all": true}); r.Status {
			t.Fatal("object grant accepted")
		}
		got, _ := fx.app(0)["api"].([]any)
		if len(got) != 1 {
			t.Fatalf("existing grant clobbered: %v", fx.app(0)["api"])
		}
	})

	t.Run("unknown app", func(t *testing.T) {
		fx := newAPIApp(t, nil)
		if r := fx.m.SetAPI("nope", true); r.Status {
			t.Fatal("unknown app accepted")
		}
	})
}

// ---- transient status in List ----

// listStatus returns the status List reports for one app.
func (fx *fixture) listStatus(name string) any {
	data, _ := fx.m.List(true).Data.([]any)
	for _, entry := range data {
		if app, _ := entry.(map[string]any); app != nil && app["name"] == name {
			return app["status"]
		}
	}
	return nil
}

func TestListTransientStatus(t *testing.T) {
	newApp := func(t *testing.T, status string) *fixture {
		return newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "web", "type": "container",
			"active": true, "status": status,
		}})
	}

	t.Run("keeps installing while the create holds the app", func(t *testing.T) {
		fx := newApp(t, "installing")
		fx.m.tryLockCreating("web")
		defer fx.m.unlockCreating("web")
		if got := fx.listStatus("web"); got != "installing" {
			t.Fatalf("status = %v, want installing", got)
		}
	})

	t.Run("keeps building while an operation holds the app", func(t *testing.T) {
		fx := newApp(t, "building")
		fx.m.tryLockProcessing(1)
		defer fx.m.unlockProcessing(1)
		if got := fx.listStatus("web"); got != "building" {
			t.Fatalf("status = %v, want building", got)
		}
	})

	t.Run("a stale transient status decays to stopped", func(t *testing.T) {
		fx := newApp(t, "installing")
		if got := fx.listStatus("web"); got != "stopped" {
			t.Fatalf("status = %v, want stopped", got)
		}
	})

	t.Run("a running container wins over the transient status", func(t *testing.T) {
		fx := newApp(t, "installing")
		fx.m.tryLockCreating("web")
		defer fx.m.unlockCreating("web")
		fx.dock.mu.Lock()
		fx.dock.running["web"] = true
		fx.dock.mu.Unlock()
		if got := fx.listStatus("web"); got != "running" {
			t.Fatalf("status = %v, want running", got)
		}
	})
}
