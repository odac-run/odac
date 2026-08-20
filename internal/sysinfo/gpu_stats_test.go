package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"odac/internal/jscanon"
)

// noNvidiaStats silences the live-metrics exec for tests that do not script it.
func noNvidiaStats(t *testing.T) {
	t.Helper()
	previous := nvidiaSMIStatsQuery
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry { return nil }
	t.Cleanup(func() { nvidiaSMIStatsQuery = previous })
}

// amdCounters writes amdgpu's live sysfs counters onto an existing device.
func (f *fakeSysfs) amdCounters(t *testing.T, addr, busy, vramUsed, milliCelsius string) {
	t.Helper()
	dir := filepath.Join(f.root, "bus", "pci", "devices", addr)
	for name, value := range map[string]string{
		"gpu_busy_percent":   busy,
		"mem_info_vram_used": vramUsed,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hwmon := filepath.Join(dir, "hwmon", "hwmon3")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hwmon, "temp1_input"), []byte(milliCelsius+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGPUStatsNvidia(t *testing.T) {
	fs := newFakeSysfs(t)
	noNvidiaStats(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		return parseNvidiaSMIStats("00000000:01:00.0, 73, 8192, 24564, 61\n")
	}

	samples := New(nil, nil).gpuStats()

	if len(samples) != 1 {
		t.Fatalf("samples = %+v", samples)
	}
	want := gpuDeviceStats{
		usagePercent: 73,
		memUsedBytes: 8192 * 1024 * 1024,
		memTotBytes:  24564 * 1024 * 1024,
		tempCelsius:  61,
	}
	if samples[0] != want {
		t.Errorf("sample = %+v, want %+v", samples[0], want)
	}
}

// amdgpu publishes everything in sysfs — no exec on an AMD host.
func TestGPUStatsAMD(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:03:00.0", "0x030000", pciVendorAMD, "0x740f", map[string]string{
		"mem_info_vram_total": "68702699520",
	})
	fs.amdCounters(t, "0000:03:00.0", "42", "1073741824", "58000")
	fs.dir(t, "class", "kfd")

	execs := 0
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		execs++
		return nil
	}
	t.Cleanup(func() { nvidiaSMIStatsQuery = runNvidiaSMIStats })

	samples := New(nil, nil).gpuStats()

	if len(samples) != 1 {
		t.Fatalf("samples = %+v", samples)
	}
	want := gpuDeviceStats{
		usagePercent: 42,
		memUsedBytes: 1073741824,
		memTotBytes:  68702699520,
		tempCelsius:  58,
	}
	if samples[0] != want {
		t.Errorf("sample = %+v, want %+v", samples[0], want)
	}
	if execs != 0 {
		t.Errorf("an AMD-only host must not spawn nvidia-smi (%d execs)", execs)
	}
}

// A card nothing can measure still holds its slot, so the array stays
// positionally joinable with system.info's devices.
func TestGPUStatsUnmeasurableDeviceKeepsSlot(t *testing.T) {
	fs := newFakeSysfs(t)
	noNvidiaStats(t)
	fs.pciDevice(t, "0000:00:02.0", "0x030000", pciVendorIntel, "0x9bc4", nil)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		return parseNvidiaSMIStats("00000000:01:00.0, 10, 512, 24564, 40\n")
	}

	info := New(nil, nil)
	devices := info.gpuState().devices
	samples := info.gpuStats()

	if len(samples) != len(devices) {
		t.Fatalf("%d samples for %d devices", len(samples), len(devices))
	}
	// sysfs order is PCI order: the Intel iGPU (00:02.0) sorts first.
	if devices[0].vendor != "intel" || samples[0] != (gpuDeviceStats{}) {
		t.Errorf("unmeasurable device = %+v / %+v", devices[0], samples[0])
	}
	if samples[1].usagePercent != 10 {
		t.Errorf("nvidia sample = %+v", samples[1])
	}
}

func TestGPUStatsNoDevices(t *testing.T) {
	newFakeSysfs(t)
	execs := 0
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		execs++
		return nil
	}
	t.Cleanup(func() { nvidiaSMIStatsQuery = runNvidiaSMIStats })

	if samples := New(nil, nil).gpuStats(); len(samples) != 0 {
		t.Errorf("samples = %+v, want none", samples)
	}
	if execs != 0 {
		t.Errorf("a GPU-less host must never exec (%d execs)", execs)
	}
}

// The Cloud can dial system.stats down to a second; the sample floor keeps
// nvidia-smi from being spawned that often.
func TestGPUStatsSampleFloor(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")

	execs := 0
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		execs++
		return parseNvidiaSMIStats("00000000:01:00.0, 50, 1024, 24564, 55\n")
	}
	t.Cleanup(func() { nvidiaSMIStatsQuery = runNvidiaSMIStats })

	now := time.Now()
	info := New(nil, nil)
	info.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		info.gpuStats()
	}
	if execs != 1 {
		t.Fatalf("%d execs inside the floor, want 1", execs)
	}

	now = now.Add(gpuSampleMinInterval + time.Second)
	info.gpuStats()
	if execs != 2 {
		t.Errorf("%d execs after the floor, want 2", execs)
	}
}

func TestParseNvidiaSMIStats(t *testing.T) {
	entries := parseNvidiaSMIStats(
		"00000000:01:00.0, 73, 8192, 24564, 61\n" +
			"00000000:41:00.0, [N/A], [N/A], 49140, 44\n" +
			"short, row\n\n")

	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].addr != "01:00.0" || entries[0].stats.usagePercent != 73 ||
		entries[0].stats.memUsedBytes != 8192*1024*1024 || entries[0].stats.tempCelsius != 61 {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	// Unavailable columns degrade to zero without dropping the card.
	if entries[1].addr != "41:00.0" || entries[1].stats.usagePercent != 0 ||
		entries[1].stats.memUsedBytes != 0 || entries[1].stats.memTotBytes != 49140*1024*1024 {
		t.Errorf("entry 1 = %+v", entries[1])
	}

	// A driver reporting nonsense is clamped like cpuUsage clamps its own.
	if got := parseNvidiaSMIStats("00000000:01:00.0, 250, 0, 0, 0\n"); got[0].stats.usagePercent != 100 {
		t.Errorf("clamp = %+v", got[0].stats)
	}
}

// The payload shape the Cloud charts.
func TestGPUStatsFieldPayload(t *testing.T) {
	fs := newFakeSysfs(t)
	fs.pciDevice(t, "0000:01:00.0", "0x030000", pciVendorNvidia, "0x2684", nil)
	fs.dir(t, "module", "nvidia")
	nvidiaSMIStatsQuery = func() []nvidiaSMIStatsEntry {
		return parseNvidiaSMIStats("00000000:01:00.0, 73, 8192, 24564, 61\n")
	}
	t.Cleanup(func() { nvidiaSMIStatsQuery = runNvidiaSMIStats })

	raw, err := jscanon.Marshal(New(nil, nil).gpuStatsField())
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"memory":{"used":8589934592,"total":25757220864},"temperature":61,"usage":73}]`
	if string(raw) != want {
		t.Errorf("payload = %s\nwant       %s", raw, want)
	}

	newFakeSysfs(t)
	noNvidiaStats(t)
	if raw, err = jscanon.Marshal(New(nil, nil).gpuStatsField()); err != nil || string(raw) != "[]" {
		t.Errorf("GPU-less payload = %s (%v), want []", raw, err)
	}
}
