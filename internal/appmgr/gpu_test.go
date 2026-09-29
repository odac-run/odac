package appmgr

import (
	"fmt"
	"sync/atomic"
	"testing"

	"odac/internal/docker"
	"odac/internal/gpu"
)

// newOptionalGPUFixture builds a startable container app carrying request as
// its persisted `gpu` object.
func newOptionalGPUFixture(t *testing.T, request map[string]any) *fixture {
	t.Helper()
	app := map[string]any{
		"id": float64(1), "name": "cam", "type": "container", "image": "recorder:latest",
		"ports": []any{map[string]any{"container": float64(3000)}},
	}
	if request != nil {
		app["gpu"] = request
	}
	fx := newFixture(t, []any{app})
	fx.setHTTPPorts(3000)
	return fx
}

// startedGPU runs the app and reports the spec the container was created
// with, nil for a CPU start.
func startedGPU(t *testing.T, fx *fixture) *gpu.Spec {
	t.Helper()
	if err := fx.m.run("cam", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fx.dock.runCallCount() == 0 {
		t.Fatal("the container was never created")
	}
	return fx.dock.runCallAt(0).options.GPU
}

// The whole point of an optional request: the app installs and starts on a
// host with nothing to give it.
func TestOptionalGPUCreateSurvivesGPUlessHost(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "frigate", "image": "x/frigate:latest"})
	fx.gpuHost.allow() // this host can pass nothing through
	fx.gpuHost.runtime = ""

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "frigate", "name": "cam",
		"gpu": map[string]any{"optional": true},
	})
	if !r.Status {
		t.Fatalf("optional GPU request blocked the create: %v", r.Message)
	}
	fx.waitIdle(t)

	if spec := fx.dock.runCallAt(0).options.GPU; spec != nil {
		t.Fatalf("started with %s on a host that has no GPU", spec)
	}
}

// A required request keeps refusing at create time. Optional is an opt-in,
// never a softening of the existing contract.
func TestRequiredGPUStillFailsPreflight(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})
	fx.gpuHost.allow()

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "gen",
		"gpu": map[string]any{"runtime": "nvidia"},
	})
	if r.Status {
		t.Fatal("a required GPU request must not survive a host that cannot serve it")
	}
}

func TestResolveOptionalGPUAtStart(t *testing.T) {
	// Auto follows the host, which is why it is stored unresolved.
	t.Run("auto takes the host's runtime", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"optional": true, "count": "all"})
		fx.gpuHost.runtime = gpu.RuntimeIntel
		fx.gpuHost.allow(gpu.RuntimeIntel)

		spec := startedGPU(t, fx)
		if spec == nil || spec.Runtime != gpu.RuntimeIntel || spec.Vendor != gpu.VendorIntel {
			t.Fatalf("resolved to %+v, want an Intel request", spec)
		}
		if !spec.Optional || spec.Count != gpu.CountAll {
			t.Errorf("resolution changed the request: %+v", spec)
		}
	})

	t.Run("auto falls back to the CPU on a bare host", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"optional": true})
		fx.gpuHost.runtime = ""

		if spec := startedGPU(t, fx); spec != nil {
			t.Fatalf("started with %s on a host reporting no GPU", spec)
		}
	})

	// The host has a card, but the engine cannot hand it over, which is
	// no better than having none.
	t.Run("auto falls back when the host cannot pass its card through", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"optional": true})
		fx.gpuHost.runtime = gpu.RuntimeNvidia
		fx.gpuHost.allow() // driver present, toolkit missing

		if spec := startedGPU(t, fx); spec != nil {
			t.Fatalf("started with %s on a host that cannot pass it", spec)
		}
	})

	t.Run("a pinned optional runtime is dropped when unavailable", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "nvidia", "optional": true})
		fx.gpuHost.allow(gpu.RuntimeIntel) // an Intel host, asked for NVIDIA

		if spec := startedGPU(t, fx); spec != nil {
			t.Fatalf("started with %s, want the CPU", spec)
		}
	})

	t.Run("a pinned optional runtime survives when available", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "intel", "optional": true})
		fx.gpuHost.allow(gpu.RuntimeIntel)

		spec := startedGPU(t, fx)
		if spec == nil || spec.Runtime != gpu.RuntimeIntel {
			t.Fatalf("resolved to %+v, want the Intel request kept", spec)
		}
	})

	// A required request is never downgraded: the refusal belongs to the
	// daemon, whose own words reach the app's build log.
	t.Run("a required request reaches the engine untouched", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "nvidia"})
		fx.gpuHost.allow() // host says no, and is not asked

		spec := startedGPU(t, fx)
		if spec == nil || spec.Runtime != gpu.RuntimeNvidia {
			t.Fatalf("required request altered: %+v", spec)
		}
	})
}

// The second door. Without the retry, Manager.Check recreates the failed
// container every second and the app never runs at all.
func TestOptionalGPURetriesOnCPUAfterEngineRefusal(t *testing.T) {
	fx := newOptionalGPUFixture(t, map[string]any{"runtime": "intel", "optional": true})
	fx.gpuHost.allow(gpu.RuntimeIntel) // the probe says yes...

	var calls atomic.Int32
	fx.dock.runErr = func(string) error { // ...the daemon disagrees, once
		if calls.Add(1) == 1 {
			return fmt.Errorf("%w (intel): error gathering device information", docker.ErrGPUUnavailable)
		}
		return nil
	}

	if err := fx.m.run("cam", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fx.dock.runCallCount(); got != 2 {
		t.Fatalf("RunApp called %d times, want 2 (GPU then CPU)", got)
	}
	if spec := fx.dock.runCallAt(0).options.GPU; spec == nil {
		t.Error("the first attempt should have carried the GPU request")
	}
	if spec := fx.dock.runCallAt(1).options.GPU; spec != nil {
		t.Errorf("the retry still carried %s", spec)
	}
}

// A required app must keep failing loudly, and no failure unrelated to the
// GPU may be retried away.
func TestGPUFallbackDoesNotSwallowOtherFailures(t *testing.T) {
	t.Run("required request is not retried", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "intel"})
		fx.dock.runErr = func(string) error {
			return fmt.Errorf("%w (intel): no such device", docker.ErrGPUUnavailable)
		}
		if err := fx.m.run("cam", nil); err == nil {
			t.Fatal("a required GPU refusal must surface")
		}
		if got := fx.dock.runCallCount(); got != 1 {
			t.Errorf("RunApp called %d times, want 1", got)
		}
	})

	t.Run("an unrelated error is not retried", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "intel", "optional": true})
		fx.gpuHost.allow(gpu.RuntimeIntel)
		fx.dock.runErr = func(string) error { return fmt.Errorf("port already allocated") }

		if err := fx.m.run("cam", nil); err == nil {
			t.Fatal("an unrelated start failure must surface")
		}
		if got := fx.dock.runCallCount(); got != 1 {
			t.Errorf("RunApp called %d times, want 1", got)
		}
	})
}

// SetGPU stores an optional request unresolved on purpose: the app follows
// the host across a card appearing or disappearing.
func TestSetGPUOptional(t *testing.T) {
	t.Run("auto is persisted without a runtime", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, nil)
		fx.gpuHost.runtime = gpu.RuntimeNvidia // present, and deliberately ignored

		if r := fx.m.SetGPU("cam", map[string]any{"optional": true}); !r.Status {
			t.Fatalf("failed: %v", r.Message)
		}
		persisted, _ := fx.app(0)["gpu"].(map[string]any)
		if persisted["optional"] != true {
			t.Fatalf("persisted gpu = %v", fx.app(0)["gpu"])
		}
		if _, pinned := persisted["runtime"]; pinned {
			t.Errorf("an optional request must not freeze today's runtime: %v", persisted)
		}
	})

	// Without --optional the same bare request still refuses, so an operator
	// who wanted a GPU is never quietly left on the CPU.
	t.Run("a GPU-less host still refuses a bare request", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, nil)
		fx.gpuHost.runtime = ""

		if r := fx.m.SetGPU("cam", map[string]any{}); r.Status {
			t.Fatal("bare reservation accepted on a GPU-less host")
		}
	})

	t.Run("optional passes the pre-flight a required one fails", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, nil)
		fx.gpuHost.allow() // nothing can be passed through

		if r := fx.m.SetGPU("cam", map[string]any{"runtime": "nvidia"}); r.Status {
			t.Fatal("required reservation accepted on an incapable host")
		}
		if r := fx.m.SetGPU("cam", map[string]any{"runtime": "nvidia", "optional": true}); !r.Status {
			t.Fatalf("optional reservation refused: %v", r.Message)
		}
	})
}

// listRow returns the single app's row from a detailed List.
func listRow(t *testing.T, fx *fixture) map[string]any {
	t.Helper()
	r := fx.m.List(true)
	if !r.Status {
		t.Fatalf("List failed: %v", r.Message)
	}
	rows, _ := r.Data.([]any)
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row == nil {
		t.Fatalf("row = %v", rows[0])
	}
	return row
}

func TestListReportsGPUAttachment(t *testing.T) {
	attach := func(fx *fixture, attachment *docker.GPUAttachment) {
		fx.dock.mu.Lock()
		defer fx.dock.mu.Unlock()
		fx.dock.status["cam"] = docker.Status{Running: true, GPU: attachment}
	}

	t.Run("reports what the container holds", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"optional": true, "count": "all"})
		attach(fx, &docker.GPUAttachment{Vendor: gpu.VendorIntel, Nodes: []string{"/dev/dri"}})

		attached, _ := listRow(t, fx)["gpu"].(map[string]any)["attached"].(map[string]any)
		if attached == nil {
			t.Fatalf("no attachment reported: %v", listRow(t, fx)["gpu"])
		}
		if attached["vendor"] != gpu.VendorIntel {
			t.Errorf("vendor = %v", attached["vendor"])
		}
		nodes, _ := attached["nodes"].([]any)
		if len(nodes) != 1 || nodes[0] != "/dev/dri" {
			t.Errorf("nodes = %v", attached["nodes"])
		}
	})

	t.Run("an NVIDIA attachment carries its device count", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "nvidia"})
		attach(fx, &docker.GPUAttachment{Vendor: gpu.VendorNvidia, Count: gpu.CountAll})

		attached, _ := listRow(t, fx)["gpu"].(map[string]any)["attached"].(map[string]any)
		if attached["count"] != "all" || attached["vendor"] != gpu.VendorNvidia {
			t.Errorf("attached = %v", attached)
		}
		if _, hasNodes := attached["nodes"]; hasNodes {
			t.Errorf("NVIDIA passthrough has no device nodes to report: %v", attached)
		}
	})

	// The case the whole feature exists for: the app asked, the host could
	// not, and the operator has to be able to see that.
	t.Run("an optional app that fell back to the CPU says so", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"optional": true})
		attach(fx, nil)

		row, _ := listRow(t, fx)["gpu"].(map[string]any)
		if row == nil {
			t.Fatal("the reservation vanished from the row")
		}
		if _, attached := row["attached"]; attached {
			t.Errorf("an unattached app must not claim a GPU: %v", row)
		}
		if row["optional"] != true {
			t.Errorf("the request itself must survive: %v", row)
		}
	})

	t.Run("a stopped container holds nothing", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, map[string]any{"runtime": "intel", "optional": true})
		fx.dock.mu.Lock()
		fx.dock.status["cam"] = docker.Status{Running: false, GPU: &docker.GPUAttachment{Vendor: gpu.VendorIntel}}
		fx.dock.mu.Unlock()

		row, _ := listRow(t, fx)["gpu"].(map[string]any)
		if _, attached := row["attached"]; attached {
			t.Errorf("a stopped app must not report an attachment: %v", row)
		}
	})

	// A CPU app must not grow a gpu member: every row would read as changed.
	t.Run("apps without a reservation are untouched", func(t *testing.T) {
		fx := newOptionalGPUFixture(t, nil)
		attach(fx, &docker.GPUAttachment{Vendor: gpu.VendorIntel, Nodes: []string{"/dev/dri"}})

		if value, present := listRow(t, fx)["gpu"]; present {
			t.Errorf("a CPU app grew a gpu member: %v", value)
		}
	})
}

// The row is a copy: reporting an attachment must never leak into the config
// the next save writes back to disk.
func TestListGPUAttachmentDoesNotTouchConfig(t *testing.T) {
	fx := newOptionalGPUFixture(t, map[string]any{"optional": true})
	fx.dock.mu.Lock()
	fx.dock.status["cam"] = docker.Status{Running: true, GPU: &docker.GPUAttachment{Vendor: gpu.VendorIntel}}
	fx.dock.mu.Unlock()

	listRow(t, fx)

	persisted, _ := fx.app(0)["gpu"].(map[string]any)
	if _, leaked := persisted["attached"]; leaked {
		t.Fatalf("runtime state leaked into the persisted request: %v", persisted)
	}
}

// The Cloud may hand an app.list row's gpu object straight back to app.gpu.
// If that echo pinned a runtime the request never named, an auto reservation
// would silently freeze onto whatever card the host had that day.
func TestGPURowEchoesBackLosslessly(t *testing.T) {
	fx := newOptionalGPUFixture(t, map[string]any{"optional": true, "count": "all"})
	fx.gpuHost.runtime = gpu.RuntimeIntel
	fx.dock.mu.Lock()
	fx.dock.status["cam"] = docker.Status{Running: true, GPU: &docker.GPUAttachment{Vendor: gpu.VendorIntel, Nodes: []string{"/dev/dri"}}}
	fx.dock.mu.Unlock()

	echoed, _ := listRow(t, fx)["gpu"].(map[string]any)
	if _, attached := echoed["attached"]; !attached {
		t.Fatal("test needs a row that actually carries an attachment")
	}

	if r := fx.m.SetGPU("cam", echoed); !r.Status {
		t.Fatalf("the Cloud's own echo was rejected: %v", r.Message)
	}
	persisted, _ := fx.app(0)["gpu"].(map[string]any)
	if _, pinned := persisted["runtime"]; pinned {
		t.Errorf("the echo pinned an auto reservation: %v", persisted)
	}
	if _, leaked := persisted["attached"]; leaked {
		t.Errorf("runtime state was persisted as config: %v", persisted)
	}
	if persisted["optional"] != true {
		t.Errorf("the request lost its optional flag: %v", persisted)
	}
}
