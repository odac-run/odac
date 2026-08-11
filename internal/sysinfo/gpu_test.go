package sysinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"odac/internal/gpu"
	"odac/internal/jscanon"
)

// fakeSysfs builds a sysfs tree and points the probe at it for one test.
type fakeSysfs struct{ root string }

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	fs := &fakeSysfs{root: t.TempDir()}
	previous := sysfsRoot
	sysfsRoot = fs.root
	t.Cleanup(func() { sysfsRoot = previous })
	// No NVIDIA /proc tree and no nvidia-smi unless a test adds them.
	previousRoots, previousQuery, previousDev := nvidiaProcRoots, nvidiaSMIQuery, devRoot
	nvidiaProcRoots = []string{filepath.Join(fs.root, "absent")}
	nvidiaSMIQuery = func() []nvidiaSMIEntry { return nil }
	devRoot = filepath.Join(fs.root, "dev")
	t.Cleanup(func() {
		nvidiaProcRoots, nvidiaSMIQuery, devRoot = previousRoots, previousQuery, previousDev
	})
	return fs
}

// pciDevice writes one PCI device node; attrs are extra sysfs attributes.
func (f *fakeSysfs) pciDevice(t *testing.T, addr, class, vendor, device string, attrs map[string]string) {
	t.Helper()
	dir := filepath.Join(f.root, "bus", "pci", "devices", addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"class": class, "vendor": vendor, "device": device}
	for name, value := range attrs {
		files[name] = value
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fakeSysfs) dir(t *testing.T, parts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(append([]string{f.root}, parts...)...), 0o755); err != nil {
		t.Fatal(err)
	}
}

// devNode writes a stand-in for a driver character device under devRoot.
func (f *fakeSysfs) devNode(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(f.root, "dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o660); err != nil {
		t.Fatal(err)
	}
}

// nvidiaProcTree writes a /proc/driver/nvidia/gpus/<addr>/information file.
func (f *fakeSysfs) nvidiaProcTree(t *testing.T, addr, model string) {
	t.Helper()
	root := filepath.Join(f.root, "procnvidia")
	dir := filepath.Join(root, addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Model: \t " + model + "\nIRQ: \t 89\nGPU UUID: \t GPU-fake\n"
	if err := os.WriteFile(filepath.Join(dir, "information"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	nvidiaProcRoots = []string{root}
}

func TestProbeGPUNvidiaWithSMI(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")
	nvidiaSMIQuery = func() []nvidiaSMIEntry {
		return parseNvidiaSMI("00000000:01:00.0, NVIDIA GeForce RTX 4090, 24564\n")
	}

	snap := probeGPU(nil)

	if snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, want nvidia", snap.runtime)
	}
	if len(snap.devices) != 1 {
		t.Fatalf("devices = %+v", snap.devices)
	}
	device := snap.devices[0]
	if device.vendor != "nvidia" || device.model != "GeForce RTX 4090" || device.vramBytes != 24564*1024*1024 {
		t.Errorf("device = %+v", device)
	}
}

// A card sysfs missed (blind /sys in a container) still lands in the payload
// when the driver reports it.
func TestProbeGPUSMIOnly(t *testing.T) {
	newFakeSysfs(t)
	nvidiaSMIQuery = func() []nvidiaSMIEntry {
		return parseNvidiaSMI("00000000:07:00.0, NVIDIA H100 PCIe, 81559\n")
	}

	snap := probeGPU(nil)

	if snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, want nvidia", snap.runtime)
	}
	if len(snap.devices) != 1 || snap.devices[0].model != "H100 PCIe" || snap.devices[0].vramBytes != 81559*1024*1024 {
		t.Fatalf("devices = %+v", snap.devices)
	}
}

// Without nvidia-smi the /proc tree still names the card; VRAM degrades to 0
// but the gate stays open.
func TestProbeGPUNvidiaProcFallback(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030200", pciVendorNvidia, "0x20b2", nil)
	fs.nvidiaProcTree(t, "0000:01:00.0", "NVIDIA A100-SXM4-80GB")

	snap := probeGPU(nil)

	if snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, want nvidia", snap.runtime)
	}
	if len(snap.devices) != 1 || snap.devices[0].model != "NVIDIA A100-SXM4-80GB" || snap.devices[0].vramBytes != 0 {
		t.Fatalf("devices = %+v", snap.devices)
	}
}

// A card with no driver evidence is inventory only — the Cloud must not
// schedule GPU work there.
func TestProbeGPUDeviceWithoutDriverIsNotGated(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)

	snap := probeGPU(nil)

	if snap.runtime != "" {
		t.Errorf("runtime = %q, want empty", snap.runtime)
	}
	if len(snap.devices) != 1 || snap.devices[0].model != "NVIDIA GPU (2684)" {
		t.Fatalf("devices = %+v", snap.devices)
	}
}

// The daemon knowing the nvidia runtime is evidence on its own: ODAC's own
// container often cannot see the host's driver state.
func TestProbeGPUEngineRuntimeGates(t *testing.T) {
	newFakeSysfs(t)

	if snap := probeGPU([]string{"io.containerd.runc.v2", "nvidia", "runc"}); snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, want nvidia", snap.runtime)
	}
	if snap := probeGPU([]string{"runc"}); snap.runtime != "" {
		t.Errorf("runtime = %q, want empty", snap.runtime)
	}
}

func TestProbeGPUROCm(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:03:00.0", "0x030000", pciVendorAMD, "0x740f", map[string]string{
		"mem_info_vram_total": "68702699520",
		"product_name":        "AMD Instinct MI210",
	})

	if snap := probeGPU(nil); snap.runtime != "" {
		t.Errorf("runtime = %q without /sys/class/kfd, want empty", snap.runtime)
	}

	fs.dir(t, "class", "kfd")
	snap := probeGPU(nil)

	if snap.runtime != gpu.RuntimeROCm {
		t.Errorf("runtime = %q, want rocm", snap.runtime)
	}
	if len(snap.devices) != 1 {
		t.Fatalf("devices = %+v", snap.devices)
	}
	if device := snap.devices[0]; device.vendor != "amd" || device.model != "AMD Instinct MI210" || device.vramBytes != 68702699520 {
		t.Errorf("device = %+v", device)
	}
}

// Non-GPU PCI functions (and unknown vendors) must not reach the payload.
func TestScanPCIGPUsFiltersNonGPUs(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:00:1f.3", "0x040300", pciVendorIntel, "0xa348", nil) // audio
	fs.pciDevice(t, "0000:02:00.0", "0x030000", "0x1234", "0x0001", nil)       // unknown vendor
	fs.pciDevice(t, "0000:00:02.0", "0x030000", pciVendorIntel, "0x9bc4", nil) // iGPU

	devices := scanPCIGPUs(sysfsRoot)

	if len(devices) != 1 || devices[0].vendor != "intel" {
		t.Fatalf("devices = %+v", devices)
	}
	if snap := probeGPU(nil); snap.runtime != "" {
		t.Errorf("an iGPU must not gate GPU workloads: %q", snap.runtime)
	}
}

func TestForcedGPURuntime(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")

	t.Setenv("ODAC_GPU_RUNTIME", "none")
	if snap := probeGPU(nil); snap.runtime != "" {
		t.Errorf("runtime = %q, want the override to close the gate", snap.runtime)
	}
	if snap := probeGPU(nil); len(snap.devices) != 1 {
		t.Errorf("the override must not hide inventory: %+v", snap.devices)
	}

	t.Setenv("ODAC_GPU_RUNTIME", "ROCm")
	if snap := probeGPU(nil); snap.runtime != gpu.RuntimeROCm {
		t.Errorf("runtime = %q, want rocm", snap.runtime)
	}

	t.Setenv("ODAC_GPU_RUNTIME", "junk")
	if snap := probeGPU(nil); snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, an unknown override must fall back to detection", snap.runtime)
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	entries := parseNvidiaSMI("00000000:01:00.0, NVIDIA GeForce RTX 4090, 24564\n" +
		"00000000:41:00.0, Quadro RTX 8000, [N/A]\n" +
		"garbage line\n\n")

	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0] != (nvidiaSMIEntry{addr: "01:00.0", model: "GeForce RTX 4090", vramBytes: 24564 * 1024 * 1024}) {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1] != (nvidiaSMIEntry{addr: "41:00.0", model: "Quadro RTX 8000", vramBytes: 0}) {
		t.Errorf("entry 1 = %+v", entries[1])
	}
}

func TestNormalizeAddr(t *testing.T) {
	for input, want := range map[string]string{
		"0000:01:00.0":     "01:00.0",
		"00000000:01:00.0": "01:00.0",
		" 0000:0A:00.0 ":   "0a:00.0",
		"01:00.0":          "01:00.0",
	} {
		if got := normalizeAddr(input); got != want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

// The probe is expensive enough that every auth handshake must not repeat it.
func TestGPUStateCaches(t *testing.T) {
	newFakeSysfs(t)
	calls := 0
	nvidiaSMIQuery = func() []nvidiaSMIEntry {
		calls++
		return nil
	}

	now := time.Now()
	info := New(nil, nil)
	info.now = func() time.Time { return now }

	info.gpuState()
	info.gpuState()
	if calls != 1 {
		t.Fatalf("probed %d times within the TTL, want 1", calls)
	}

	now = now.Add(gpuCacheTTL + time.Second)
	info.gpuState()
	if calls != 2 {
		t.Errorf("probed %d times after the TTL, want 2", calls)
	}
}

// The payload shape the Cloud parses, end to end.
func TestGPUFieldPayload(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")
	nvidiaSMIQuery = func() []nvidiaSMIEntry {
		return parseNvidiaSMI("00000000:01:00.0, NVIDIA GeForce RTX 4090, 24564\n")
	}

	raw, err := jscanon.Marshal(New(nil, nil).gpuField())
	if err != nil {
		t.Fatal(err)
	}
	// Driver works, toolkit is not registered: the card is reported, the
	// scheduling gate is shut, and the reason names the missing piece.
	want := `{"runtime":"nvidia","schedulable":false,"reason":"no_container_runtime",` +
		`"devices":[{"vendor":"nvidia","model":"GeForce RTX 4090","vram":25757220864}]}`
	if string(raw) != want {
		t.Errorf("payload = %s\nwant       %s", raw, want)
	}

	// Same host once the toolkit is wired into Docker.
	raw, err = jscanon.Marshal(New(nil, func() []string { return []string{"runc", "nvidia"} }).gpuField())
	if err != nil {
		t.Fatal(err)
	}
	want = `{"runtime":"nvidia","schedulable":true,"reason":null,` +
		`"devices":[{"vendor":"nvidia","model":"GeForce RTX 4090","vram":25757220864}]}`
	if string(raw) != want {
		t.Errorf("payload = %s\nwant       %s", raw, want)
	}

	newFakeSysfs(t)
	raw, err = jscanon.Marshal(New(nil, nil).gpuField())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"runtime":null,"schedulable":false,"reason":"no_device","devices":[]}`; string(raw) != want {
		t.Errorf("GPU-less payload = %s, want %s", raw, want)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["runtime"] != nil {
		t.Errorf("runtime must decode to null, got %v", parsed["runtime"])
	}
}

// CanPassthrough is the create-time pre-flight and is deliberately stricter
// than the system.info gate: a driver-only host still advertises its card,
// but cannot run a GPU container.
func TestCanPassthrough(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")

	driverOnly := New(nil, nil)
	if snap := driverOnly.gpuState(); snap.runtime != gpu.RuntimeNvidia {
		t.Fatalf("the reporting gate should still advertise the card: %q", snap.runtime)
	}
	if driverOnly.CanPassthrough(gpu.RuntimeNvidia) {
		t.Error("a driver without the container toolkit must not pass the pre-flight")
	}
	if !driverOnly.CanPassthrough("") {
		t.Error("a CPU app must always pass")
	}

	withToolkit := New(nil, func() []string { return []string{"runc", "nvidia"} })
	if !withToolkit.CanPassthrough(gpu.RuntimeNvidia) {
		t.Error("a registered nvidia runtime must pass")
	}
	if withToolkit.CanPassthrough(gpu.RuntimeROCm) {
		t.Error("an nvidia runtime says nothing about ROCm")
	}
}

func TestCanPassthroughDeviceNodes(t *testing.T) {
	fs := newFakeSysfs(t)
	info := New(nil, nil)

	if info.CanPassthrough(gpu.RuntimeROCm) || info.CanPassthrough(gpu.RuntimeIntel) {
		t.Error("no render node means no passthrough")
	}

	fs.dir(t, "class", "drm", "renderD128")
	if !info.CanPassthrough(gpu.RuntimeIntel) {
		t.Error("a render node is all Intel needs")
	}
	if info.CanPassthrough(gpu.RuntimeROCm) {
		t.Error("ROCm also needs the kfd compute interface")
	}

	fs.dir(t, "class", "kfd")
	if !info.CanPassthrough(gpu.RuntimeROCm) {
		t.Error("kfd + render node must pass")
	}
}

// The operator override is the escape hatch for hosts ODAC cannot see into,
// so it has to win the pre-flight too — otherwise it could not unblock a
// deploy.
func TestCanPassthroughHonoursOverride(t *testing.T) {
	newFakeSysfs(t)
	info := New(nil, nil)

	t.Setenv("ODAC_GPU_RUNTIME", "nvidia")
	if !info.CanPassthrough(gpu.RuntimeNvidia) {
		t.Error("forced nvidia must pass the pre-flight")
	}
	if info.CanPassthrough(gpu.RuntimeROCm) {
		t.Error("forcing nvidia must not unlock rocm")
	}

	t.Setenv("ODAC_GPU_RUNTIME", "none")
	if info.CanPassthrough(gpu.RuntimeNvidia) {
		t.Error("a disabled host must refuse every runtime")
	}
	if !info.CanPassthrough("") {
		t.Error("CPU apps stay unaffected")
	}
}

// The diagnosis matrix. Each row is a real host state an operator can be in,
// and the reason is what the Cloud shows them instead of an unexplained null.
func TestGPUCapabilityReasons(t *testing.T) {
	nvidiaCard := func(t *testing.T, fs *fakeSysfs) {
		fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2204", nil)
	}

	cases := []struct {
		name            string
		setup           func(t *testing.T, fs *fakeSysfs)
		engineRuntimes  []string
		wantRuntime     string
		wantSchedulable bool
		wantReason      string
	}{{
		name:       "bare host, no card",
		setup:      func(*testing.T, *fakeSysfs) {},
		wantReason: gpu.ReasonNoDevice,
	}, {
		// The 3090-with-nothing-installed case.
		name:       "card present, no driver anywhere",
		setup:      nvidiaCard,
		wantReason: gpu.ReasonNoDriver,
	}, {
		name: "driver installed, toolkit never wired into Docker",
		setup: func(t *testing.T, fs *fakeSysfs) {
			nvidiaCard(t, fs)
			fs.dir(t, "module", "nvidia")
		},
		engineRuntimes: []string{"runc"},
		wantRuntime:    gpu.RuntimeNvidia,
		wantReason:     gpu.ReasonNoContainerRuntime,
	}, {
		name: "fully configured",
		setup: func(t *testing.T, fs *fakeSysfs) {
			nvidiaCard(t, fs)
			fs.dir(t, "module", "nvidia")
		},
		engineRuntimes:  []string{"runc", "nvidia"},
		wantRuntime:     gpu.RuntimeNvidia,
		wantSchedulable: true,
	}, {
		name: "AMD card, ROCm stack not loaded",
		setup: func(t *testing.T, fs *fakeSysfs) {
			fs.pciDevice(t, "0000:03:00.0", "0x030000", pciVendorAMD, "0x740f", nil)
		},
		wantReason: gpu.ReasonNoDriver,
	}, {
		name: "AMD card with kfd but no render node",
		setup: func(t *testing.T, fs *fakeSysfs) {
			fs.pciDevice(t, "0000:03:00.0", "0x030000", pciVendorAMD, "0x740f", nil)
			fs.dir(t, "class", "kfd")
		},
		wantRuntime: gpu.RuntimeROCm,
		wantReason:  gpu.ReasonNoRenderNode,
	}, {
		name: "AMD card, ROCm ready",
		setup: func(t *testing.T, fs *fakeSysfs) {
			fs.pciDevice(t, "0000:03:00.0", "0x030000", pciVendorAMD, "0x740f", nil)
			fs.dir(t, "class", "kfd")
			fs.dir(t, "class", "drm", "renderD128")
		},
		wantRuntime:     gpu.RuntimeROCm,
		wantSchedulable: true,
	}, {
		// Found a GPU, but not one ODAC schedules on.
		name: "Intel iGPU only",
		setup: func(t *testing.T, fs *fakeSysfs) {
			fs.pciDevice(t, "0000:00:02.0", "0x030000", pciVendorIntel, "0x9bc4", nil)
		},
		wantReason: gpu.ReasonUnsupportedDevice,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeSysfs(t)
			tc.setup(t, fs)

			snap := probeGPU(tc.engineRuntimes)

			if snap.runtime != tc.wantRuntime {
				t.Errorf("runtime = %q, want %q", snap.runtime, tc.wantRuntime)
			}
			if snap.schedulable != tc.wantSchedulable {
				t.Errorf("schedulable = %v, want %v", snap.schedulable, tc.wantSchedulable)
			}
			if snap.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", snap.reason, tc.wantReason)
			}
			// reason and schedulable are two sides of one verdict.
			if snap.schedulable != (snap.reason == "") {
				t.Errorf("schedulable=%v with reason=%q", snap.schedulable, snap.reason)
			}
		})
	}
}

// The operator override closes the gate with its own reason, and opens it
// past every probe — that is the whole point of the escape hatch.
func TestGPUCapabilityHonoursOverride(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2204", nil)
	fs.dir(t, "module", "nvidia")

	t.Setenv("ODAC_GPU_RUNTIME", "none")
	if snap := probeGPU([]string{"nvidia"}); snap.schedulable || snap.reason != gpu.ReasonDisabled {
		t.Errorf("disabled: schedulable=%v reason=%q", snap.schedulable, snap.reason)
	}

	t.Setenv("ODAC_GPU_RUNTIME", "nvidia")
	if snap := probeGPU(nil); !snap.schedulable || snap.reason != "" {
		t.Errorf("forced: schedulable=%v reason=%q", snap.schedulable, snap.reason)
	}
}

// The create-time pre-flight and the payload diagnosis must never disagree:
// both read passthroughReason.
func TestCanPassthroughMatchesReason(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2204", nil)
	fs.dir(t, "module", "nvidia")

	withoutToolkit := New(nil, func() []string { return []string{"runc"} })
	if snap := withoutToolkit.gpuState(); snap.schedulable != withoutToolkit.CanPassthrough(snap.runtime) {
		t.Errorf("payload says schedulable=%v, pre-flight says %v",
			snap.schedulable, withoutToolkit.CanPassthrough(snap.runtime))
	}

	withToolkit := New(nil, func() []string { return []string{"runc", "nvidia"} })
	if snap := withToolkit.gpuState(); snap.schedulable != withToolkit.CanPassthrough(snap.runtime) {
		t.Errorf("payload says schedulable=%v, pre-flight says %v",
			snap.schedulable, withToolkit.CanPassthrough(snap.runtime))
	}
}

// ODAC ships privileged (docker-compose.yml), so /dev inside its container is
// the host's: the driver's control device proves the driver is up even when
// the kernel module directory is unreadable and nvidia-smi is absent.
func TestProbeGPUDriverSeenViaDevNode(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2204", nil)

	// No /sys/module/nvidia, no proc tree, no nvidia-smi: gate stays shut.
	if snap := probeGPU(nil); snap.runtime != "" || snap.reason != gpu.ReasonNoDriver {
		t.Fatalf("runtime = %q, reason = %q", snap.runtime, snap.reason)
	}

	fs.devNode(t, "nvidiactl")

	snap := probeGPU(nil)
	if snap.runtime != gpu.RuntimeNvidia {
		t.Errorf("runtime = %q, want nvidia", snap.runtime)
	}
	if snap.schedulable || snap.reason != gpu.ReasonNoContainerRuntime {
		t.Errorf("schedulable = %v, reason = %q", snap.schedulable, snap.reason)
	}
}
