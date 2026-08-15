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
