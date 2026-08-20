package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"odac/internal/gpu"
	"odac/internal/jscanon"
)

// PCI vendor ids of the display-controller vendors worth reporting.
const (
	pciVendorNvidia = "0x10de"
	pciVendorAMD    = "0x1002"
	pciVendorIntel  = "0x8086"
)

// gpuCacheTTL bounds re-probing: Get() feeds the Hub's hourly system.info
// task AND every auth handshake, so a reconnect storm must not become a
// probe storm (sysfs walk + one optional nvidia-smi exec).
const gpuCacheTTL = 5 * time.Minute

// nvidiaSMITimeout caps the enrichment exec; a wedged driver must not hold
// the payload hostage (the gate never depends on this call succeeding).
const nvidiaSMITimeout = 2 * time.Second

var (
	// sysfsRoot is a test seam; sysfs is the host's own even inside ODAC's
	// container, which is why the probe prefers it over /dev and /proc.
	sysfsRoot = "/sys"

	// nvidiaProcRoots mirrors osReleaseCandidates: the driver's /proc tree
	// seen through container walls (explicit bind mount, pid-host, own).
	nvidiaProcRoots = []string{
		"/proc/driver/nvidia/gpus",
		"/host/proc/driver/nvidia/gpus",
		"/proc/1/root/proc/driver/nvidia/gpus",
	}

	// devRoot is a test seam over the device tree. ODAC ships privileged
	// (docker-compose.yml), so this is the HOST's /dev — the driver's own
	// character devices are visible here even though nvidia-smi is not
	// installed in ODAC's image.
	devRoot = "/dev"

	// nvidiaSMIQuery is a test seam (the binary is absent in CI).
	nvidiaSMIQuery = runNvidiaSMI
)

// gpuDevice is one accelerator. vramBytes is 0 when no source exposes the
// size (NVIDIA with no reachable nvidia-smi); addr is the PCI merge key and
// path the sysfs directory live metrics are read from (empty for a card only
// the driver reported). Neither is part of the payload.
type gpuDevice struct {
	vendor    string
	model     string
	vramBytes int64
	addr      string
	path      string
}

// gpuSnapshot is one probe result, cached for gpuCacheTTL. runtime is the
// loose answer ("there is a working card of this kind"), schedulable the
// strict one ("the engine can hand it to a container"); reason names the
// missing piece whenever schedulable is false.
type gpuSnapshot struct {
	runtime     string
	schedulable bool
	reason      string
	devices     []gpuDevice
}

// gpuField renders the `gpu` payload member. Three questions, three fields,
// because they have genuinely different answers:
//
//   - runtime: what kind of working card is here (null = none). Inventory.
//   - schedulable: whether the engine can actually hand it to a container.
//     A driver without nvidia-container-toolkit reports runtime "nvidia" and
//     schedulable false — the hardware is real, the passthrough is not.
//   - reason: which piece is missing, null when schedulable. This is the
//     field that turns "why is my 3090 not showing up" into one line of
//     operator-facing guidance.
//
// The vocabulary comes from internal/gpu: the Cloud answers with the same
// words in app.create.
func (i *Info) gpuField() jscanon.Obj {
	snap := i.gpuState()
	devices := make([]any, 0, len(snap.devices))
	for _, d := range snap.devices {
		devices = append(devices, jscanon.Obj{
			{K: "vendor", V: d.vendor},
			{K: "model", V: d.model},
			{K: "vram", V: d.vramBytes},
		})
	}
	var runtime, reason any
	if snap.runtime != "" {
		runtime = snap.runtime
	}
	if snap.reason != "" {
		reason = snap.reason
	}
	return jscanon.Obj{
		{K: "runtime", V: runtime},
		{K: "schedulable", V: snap.schedulable},
		{K: "reason", V: reason},
		{K: "devices", V: devices},
	}
}

// CanPassthrough answers a stricter question than the system.info gate: not
// "is there a GPU here" but "can the container engine actually hand one to
// an app container". The two differ on purpose — a host with drivers but no
// nvidia-container-toolkit still advertises its card to the Cloud (it is
// real hardware, and the operator wants to see it), yet cannot run a single
// GPU container. appmgr asks this before creating an app so the operator
// gets "install the toolkit" instead of a failed deploy.
//
// Evidence is engine-side only for NVIDIA: the daemon's registered runtimes
// are the one signal that survives ODAC's own container walls. ROCm and
// Intel need no engine support, only host driver nodes, which sysfs shows.
// ODAC_GPU_RUNTIME overrides everything, for hosts ODAC cannot see into.
func (i *Info) CanPassthrough(runtime string) bool {
	if runtime == "" {
		return true // a CPU app needs nothing
	}
	if forced, ok := forcedGPURuntime(); ok {
		return forced == runtime
	}
	return passthroughReason(runtime, i.engineRuntimes()) == ""
}

// passthroughReason reports why runtime cannot be handed to a container, ""
// when it can. Single source for both the create-time pre-flight and the
// system.info diagnosis, so the two can never disagree about what a host is
// missing.
func passthroughReason(runtime string, engineRuntimes []string) string {
	switch runtime {
	case gpu.RuntimeNvidia:
		// Engine-side evidence only: the daemon's runtime list is the one
		// signal that survives ODAC's own container walls.
		if hasRuntime(engineRuntimes, gpu.RuntimeNvidia) {
			return ""
		}
		return gpu.ReasonNoContainerRuntime
	case gpu.RuntimeROCm:
		if dirExists(filepath.Join(sysfsRoot, "class", "kfd")) && hasRenderNode() {
			return ""
		}
		return gpu.ReasonNoRenderNode
	case gpu.RuntimeIntel:
		if hasRenderNode() {
			return ""
		}
		return gpu.ReasonNoRenderNode
	}
	return gpu.ReasonUnsupportedDevice
}

// hasRenderNode reports whether the host exposes a DRM render node — the
// /dev/dri entries ROCm and Intel compute are passed through as. sysfs is
// read rather than /dev because it is the host's view under every install
// shape, privileged or not.
func hasRenderNode() bool {
	entries, err := os.ReadDir(filepath.Join(sysfsRoot, "class", "drm"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "renderD") {
			return true
		}
	}
	return false
}

// gpuState returns the cached snapshot, re-probing when stale, and announces
// a changed answer to whoever installed the hook.
func (i *Info) gpuState() gpuSnapshot {
	snap, changed := i.refreshGPU()
	if changed != nil {
		// Outside the lock on purpose: the hook re-enters Get().
		changed()
	}
	return snap
}

// refreshGPU re-probes when the cache is stale and hands back the change hook
// when this probe actually moved the payload, nil otherwise. The probe runs
// under the lock so concurrent callers coalesce into one hardware walk
// instead of stampeding the driver.
//
// The hook is returned rather than invoked here because it ends up calling
// Get() again — firing it under gpuMu would deadlock. The first probe never
// fires it: there is no previous answer to differ from, and the payload that
// prompted it carries the result anyway.
func (i *Info) refreshGPU() (gpuSnapshot, func()) {
	i.gpuMu.Lock()
	defer i.gpuMu.Unlock()
	now := i.now()
	if i.hasGPU && now.Sub(i.gpuAt) < gpuCacheTTL {
		return i.gpuSnap, nil
	}
	previous, had := i.gpuSnap, i.hasGPU
	i.gpuSnap = probeGPU(i.engineRuntimes())
	i.gpuAt, i.hasGPU = now, true
	if had && i.gpuChanged != nil && !i.gpuSnap.sameAs(previous) {
		return i.gpuSnap, i.gpuChanged
	}
	return i.gpuSnap, nil
}

// sameAs compares what the payload actually carries. addr and path are merge
// keys the Cloud never sees, so a card whose sysfs path moved is not news
// worth an unscheduled broadcast.
func (s gpuSnapshot) sameAs(other gpuSnapshot) bool {
	if s.runtime != other.runtime || s.schedulable != other.schedulable ||
		s.reason != other.reason || len(s.devices) != len(other.devices) {
		return false
	}
	for idx := range s.devices {
		a, b := s.devices[idx], other.devices[idx]
		if a.vendor != b.vendor || a.model != b.model || a.vramBytes != b.vramBytes {
			return false
		}
	}
	return true
}

// engineRuntimes is the container engine's registered OCI runtimes, or nil
// when no engine is wired (docker-less host, tests).
func (i *Info) engineRuntimes() []string {
	if i.containerRuntimes == nil {
		return nil
	}
	return i.containerRuntimes()
}

// probeGPU walks sysfs for the inventory, enriches NVIDIA cards from the
// driver's own sources, decides the runtime gate and diagnoses whatever is
// still missing.
func probeGPU(engineRuntimes []string) gpuSnapshot {
	devices, driverSeen := enrichNvidia(scanPCIGPUs(sysfsRoot))
	runtime := gpuRuntime(devices, engineRuntimes, driverSeen)
	schedulable, reason := gpuCapability(devices, runtime, engineRuntimes, driverSeen)
	return gpuSnapshot{
		runtime:     runtime,
		schedulable: schedulable,
		reason:      reason,
		devices:     devices,
	}
}

// gpuCapability answers "can this host actually run a GPU workload", and
// when it cannot, which piece is missing. It is the diagnosis behind the
// payload: the Cloud renders the reason instead of leaving an operator
// staring at a null runtime next to a card they know is installed.
func gpuCapability(devices []gpuDevice, runtime string, engineRuntimes []string, nvidiaDriverSeen bool) (bool, string) {
	if forced, ok := forcedGPURuntime(); ok {
		if forced == "" {
			return false, gpu.ReasonDisabled
		}
		// The operator asserted the host can do it; their word beats probes
		// that cannot see through the container wall in the first place.
		return true, ""
	}

	if runtime != "" {
		if reason := passthroughReason(runtime, engineRuntimes); reason != "" {
			return false, reason
		}
		return true, ""
	}

	// No runtime at all: name the evidence that was missing.
	switch {
	case len(devices) == 0:
		return false, gpu.ReasonNoDevice
	case hasVendor(devices, gpu.VendorNvidia) && !nvidiaDriverSeen:
		return false, gpu.ReasonNoDriver
	case hasVendor(devices, gpu.VendorAMD):
		// A card with no /sys/class/kfd means the ROCm stack is not loaded.
		return false, gpu.ReasonNoDriver
	}
	return false, gpu.ReasonUnsupportedDevice
}

// gpuRuntime decides the gate. Evidence, strongest first: an explicit
// operator override; the daemon advertising an nvidia runtime (proof on its
// own — sysfs is not always visible from inside ODAC's container); an NVIDIA
// card whose driver answered; an AMD card on a host with the ROCm compute
// interface (/sys/class/kfd) up.
func gpuRuntime(devices []gpuDevice, engineRuntimes []string, nvidiaDriverSeen bool) string {
	if forced, ok := forcedGPURuntime(); ok {
		return forced
	}
	if hasRuntime(engineRuntimes, gpu.RuntimeNvidia) {
		return gpu.RuntimeNvidia
	}
	if nvidiaDriverSeen && hasVendor(devices, gpu.VendorNvidia) {
		return gpu.RuntimeNvidia
	}
	if hasVendor(devices, gpu.VendorAMD) && dirExists(filepath.Join(sysfsRoot, "class", "kfd")) {
		return gpu.RuntimeROCm
	}
	return ""
}

// forcedGPURuntime honours ODAC_GPU_RUNTIME ("nvidia" | "rocm" | "none"): the
// escape hatch for hosts whose driver state ODAC cannot see through the
// container wall. Unset or unrecognised means auto-detect.
func forcedGPURuntime() (string, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ODAC_GPU_RUNTIME"))) {
	case gpu.RuntimeNvidia:
		return gpu.RuntimeNvidia, true
	case gpu.RuntimeROCm:
		return gpu.RuntimeROCm, true
	case "none", "off", "disabled":
		return "", true
	}
	return "", false
}

// scanPCIGPUs lists display controllers from sysfs. This is the only source
// that works unprivileged and unmodified inside a container, so it carries
// the inventory; the driver-specific passes only enrich it.
func scanPCIGPUs(root string) []gpuDevice {
	dir := filepath.Join(root, "bus", "pci", "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var devices []gpuDevice
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		// PCI class 0x03xxxx is "display controller": VGA (0x030000) plus
		// the headless 3D controllers datacenter cards report (0x030200).
		if !strings.HasPrefix(sysfsValue(path, "class"), "0x03") {
			continue
		}
		vendor := gpuVendorName(sysfsValue(path, "vendor"))
		if vendor == "" {
			continue
		}
		devices = append(devices, gpuDevice{
			vendor:    vendor,
			model:     pciModel(vendor, path),
			vramBytes: pciVRAMBytes(path),
			addr:      normalizeAddr(entry.Name()),
			path:      path,
		})
	}
	return devices
}

// enrichNvidia fills model and VRAM for NVIDIA cards from the driver itself:
// /proc/driver/nvidia knows the marketing name, only nvidia-smi knows the
// VRAM size. Either answering also proves the driver is alive, which is the
// second return value. A card nvidia-smi reports but sysfs missed is
// appended — the driver's word beats a blind sysfs.
func enrichNvidia(devices []gpuDevice) ([]gpuDevice, bool) {
	// Two independent driver signals before any parsing: the loaded kernel
	// module (sysfs, always the host's) and the driver's control device
	// (/dev, the host's because ODAC runs privileged). Either one proves the
	// driver is up even when nvidia-smi is absent from ODAC's image.
	driverSeen := dirExists(filepath.Join(sysfsRoot, "module", "nvidia")) ||
		pathExists(filepath.Join(devRoot, "nvidiactl"))

	for addr, model := range nvidiaProcModels() {
		driverSeen = true
		if idx := indexByAddr(devices, addr); idx >= 0 {
			devices[idx].model = model
		}
	}

	for _, entry := range nvidiaSMIQuery() {
		driverSeen = true
		idx := indexByAddr(devices, entry.addr)
		if idx < 0 {
			devices = append(devices, gpuDevice{
				vendor:    gpu.VendorNvidia,
				model:     entry.model,
				vramBytes: entry.vramBytes,
				addr:      entry.addr,
			})
			continue
		}
		if entry.model != "" {
			devices[idx].model = entry.model
		}
		if entry.vramBytes > 0 {
			devices[idx].vramBytes = entry.vramBytes
		}
	}
	return devices, driverSeen
}

// nvidiaProcModels maps PCI address to marketing name from the first
// readable /proc/driver/nvidia tree.
func nvidiaProcModels() map[string]string {
	models := map[string]string{}
	for _, root := range nvidiaProcRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(root, entry.Name(), "information"))
			if err != nil {
				continue
			}
			if model := fieldAfterColon(string(raw), "Model:"); model != "" {
				models[normalizeAddr(entry.Name())] = model
			}
		}
		if len(models) > 0 {
			return models
		}
	}
	return models
}

// nvidiaSMIEntry is one parsed nvidia-smi row.
type nvidiaSMIEntry struct {
	addr      string
	model     string
	vramBytes int64
}

// runNvidiaSMI asks the driver for the authoritative name and VRAM of every
// card. An absent binary — the common case inside ODAC's own container —
// yields nothing and costs one PATH lookup.
func runNvidiaSMI() []nvidiaSMIEntry {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSMITimeout)
	defer cancel()
	cmd, ok := nvidiaSMICommand(ctx,
		"--query-gpu=pci.bus_id,name,memory.total",
		"--format=csv,noheader,nounits")
	if !ok {
		return nil
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}

// parseNvidiaSMI reads the csv rows of runNvidiaSMI's query. memory.total is
// MiB under --format=nounits and is converted to the bytes the payload
// reports; unavailable columns ("[N/A]") parse to 0 rather than dropping the
// card.
func parseNvidiaSMI(out string) []nvidiaSMIEntry {
	var entries []nvidiaSMIEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		addr := normalizeAddr(fields[0])
		if addr == "" {
			continue
		}
		vram, _ := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		entries = append(entries, nvidiaSMIEntry{
			addr:      addr,
			model:     trimVendorPrefix(strings.TrimSpace(fields[1])),
			vramBytes: vram * 1024 * 1024, // nounits reports MiB
		})
	}
	return entries
}

// pciModel prefers whatever label the driver publishes; the PCI device id is
// the honest fallback (shipping a PCI id database is not worth the bytes,
// and NVIDIA cards get their real name from enrichNvidia anyway).
func pciModel(vendor, path string) string {
	for _, name := range []string{"product_name", "label"} {
		if value := sysfsValue(path, name); value != "" {
			return value
		}
	}
	id := strings.TrimPrefix(sysfsValue(path, "device"), "0x")
	if id == "" {
		return vendorLabel(vendor) + " GPU"
	}
	return vendorLabel(vendor) + " GPU (" + id + ")"
}

// pciVRAMBytes reads amdgpu's mem_info_vram_total, which is already in
// bytes. Other drivers do not publish the size in sysfs, so they report 0.
func pciVRAMBytes(path string) int64 {
	raw := sysfsValue(path, "mem_info_vram_total")
	if raw == "" {
		return 0
	}
	total, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || total <= 0 {
		return 0
	}
	return total
}

// gpuVendorName maps a PCI vendor id to the payload's vendor vocabulary;
// unknown vendors are not GPUs worth reporting.
func gpuVendorName(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case pciVendorNvidia:
		return gpu.VendorNvidia
	case pciVendorAMD:
		return gpu.VendorAMD
	case pciVendorIntel:
		return gpu.VendorIntel
	}
	return ""
}

// vendorLabel is the display spelling used inside model fallbacks.
func vendorLabel(vendor string) string {
	switch vendor {
	case gpu.VendorNvidia:
		return "NVIDIA"
	case gpu.VendorAMD:
		return "AMD"
	case gpu.VendorIntel:
		return "Intel"
	}
	return strings.ToUpper(vendor)
}

// trimVendorPrefix drops the vendor name nvidia-smi repeats in every model
// ("NVIDIA GeForce RTX 4090"); the vendor field already carries it.
func trimVendorPrefix(model string) string {
	if trimmed := strings.TrimPrefix(model, "NVIDIA "); trimmed != "" {
		return trimmed
	}
	return model
}

// normalizeAddr reduces a PCI address to "bus:device.function": the sources
// disagree on the domain field (sysfs "0000:", nvidia-smi "00000000:").
func normalizeAddr(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if j := strings.LastIndex(addr[:i], ":"); j >= 0 {
			return addr[j+1:]
		}
	}
	return addr
}

// indexByAddr locates a device by PCI address, -1 when absent.
func indexByAddr(devices []gpuDevice, addr string) int {
	for i := range devices {
		if devices[i].addr == addr {
			return i
		}
	}
	return -1
}

func hasVendor(devices []gpuDevice, vendor string) bool {
	for i := range devices {
		if devices[i].vendor == vendor {
			return true
		}
	}
	return false
}

func hasRuntime(runtimes []string, name string) bool {
	for _, runtime := range runtimes {
		if strings.Contains(strings.ToLower(runtime), name) {
			return true
		}
	}
	return false
}

// sysfsValue reads one sysfs attribute, empty on any error.
func sysfsValue(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// fieldAfterColon returns the value of the first "<prefix> <value>" line.
func fieldAfterColon(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// pathExists reports whether path exists at all — device nodes are character
// devices, not directories.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
