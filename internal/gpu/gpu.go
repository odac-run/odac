// Package gpu is the single source of truth for ODAC's GPU vocabulary: the
// runtime names the Cloud gates on (the `gpu` member of system.info) and
// requests with (the `gpu` member of app.create), plus the parser that turns
// an untrusted payload object into a validated Spec.
//
// It is deliberately dependency-free: sysinfo reports capability with it,
// appmgr persists a request with it, and docker translates that request into
// container config with it. One vocabulary, three consumers, no drift.
package gpu

import (
	"fmt"
	"strconv"
	"strings"
)

// Runtime names. These are wire values — the Cloud sends them in app.create
// and reads them back in system.info, so they may not be renamed casually.
const (
	RuntimeNvidia = "nvidia"
	RuntimeROCm   = "rocm"
	RuntimeIntel  = "intel"
)

// Vendor names, the inventory half of the vocabulary.
const (
	VendorNvidia = "nvidia"
	VendorAMD    = "amd"
	VendorIntel  = "intel"
)

// Reasons explain why a host cannot run GPU workloads — the `reason` member
// of system.info's gpu object. They are wire values the Cloud renders into
// operator-facing guidance, so the set is closed and additions are additive.
//
// Deliberately absent: a code for "the driver is there but ODAC's container
// cannot see it". From inside the container that state is indistinguishable
// from having no driver at all, so ReasonNoDriver covers both and its
// wording must stay honest about the ambiguity.
const (
	// ReasonDisabled: ODAC_GPU_RUNTIME=none. The operator turned it off.
	ReasonDisabled = "disabled"
	// ReasonNoDevice: no GPU on the PCI bus (and no driver reported one).
	ReasonNoDevice = "no_device"
	// ReasonNoDriver: a card is present but no driver answered — none
	// installed, or none visible from ODAC's container.
	ReasonNoDriver = "no_driver"
	// ReasonNoContainerRuntime: the NVIDIA driver works, but the daemon has
	// no `nvidia` runtime registered — nvidia-container-toolkit is missing
	// or was never wired into Docker. The card is real; containers cannot
	// have it.
	ReasonNoContainerRuntime = "no_container_runtime"
	// ReasonNoRenderNode: ROCm/Intel need DRM render nodes to pass through
	// and the host exposes none.
	ReasonNoRenderNode = "no_render_node"
	// ReasonUnsupportedDevice: a GPU is present that ODAC does not schedule
	// on (an Intel iGPU, say — slower than the CPU for inference).
	ReasonUnsupportedDevice = "unsupported_device"
)

// CountAll requests every device the host exposes ("all" on the wire, -1 in
// Docker's DeviceRequest vocabulary — the two happen to agree).
const CountAll = -1

// maxCount bounds an explicit device count. No host ODAC targets has more
// GPUs than this, and an unbounded count from the wire is a foot-gun.
const maxCount = 64

// runtimeVendors pins the legal runtime↔vendor pairs. A request naming both
// must agree; naming one lets the other be derived.
var runtimeVendors = map[string]string{
	RuntimeNvidia: VendorNvidia,
	RuntimeROCm:   VendorAMD,
	RuntimeIntel:  VendorIntel,
}

// vendorRuntimes is the reverse table.
var vendorRuntimes = map[string]string{
	VendorNvidia: RuntimeNvidia,
	VendorAMD:    RuntimeROCm,
	VendorIntel:  RuntimeIntel,
}

// Spec is a validated GPU request attached to an app. Count is either
// CountAll or a positive number of devices.
type Spec struct {
	Vendor  string
	Runtime string
	Count   int
}

// Parse validates the `gpu` member of an app.create payload (or of a
// persisted app). A missing, null or empty value is not an error: it means
// "CPU app", and (nil, nil) is returned. Anything present but malformed IS
// an error — silently dropping the request would start a CUDA-only image on
// the CPU, which crash-loops instead of failing loudly at create time.
func Parse(v any) (*Spec, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gpu must be an object")
	}
	if len(raw) == 0 {
		return nil, nil
	}

	vendor := lower(raw["vendor"])
	runtime := lower(raw["runtime"])
	switch {
	case runtime == "" && vendor == "":
		return nil, fmt.Errorf("gpu needs a runtime or a vendor")
	case runtime == "":
		runtime = vendorRuntimes[vendor]
		if runtime == "" {
			return nil, fmt.Errorf("unsupported GPU vendor: %s", vendor)
		}
	}

	wantVendor, known := runtimeVendors[runtime]
	if !known {
		return nil, fmt.Errorf("unsupported GPU runtime: %s", runtime)
	}
	if vendor == "" {
		vendor = wantVendor
	} else if vendor != wantVendor {
		return nil, fmt.Errorf("GPU vendor %s does not match runtime %s", vendor, runtime)
	}

	count, err := parseCount(raw["count"])
	if err != nil {
		return nil, err
	}
	return &Spec{Vendor: vendor, Runtime: runtime, Count: count}, nil
}

// parseCount accepts "all" (or nothing) plus decimal counts in either JSON
// shape — the Cloud sends "all" today, numbers are honoured for the
// multi-GPU hosts that want a slice of the box.
func parseCount(v any) (int, error) {
	switch value := v.(type) {
	case nil:
		return CountAll, nil
	case float64:
		return validCount(int(value), value == float64(int(value)))
	case int:
		return validCount(value, true)
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" || trimmed == "all" {
			return CountAll, nil
		}
		n, err := strconv.Atoi(trimmed)
		return validCount(n, err == nil)
	}
	return 0, fmt.Errorf("invalid GPU count")
}

func validCount(n int, wellFormed bool) (int, error) {
	if !wellFormed || n < 1 || n > maxCount {
		return 0, fmt.Errorf("invalid GPU count")
	}
	return n, nil
}

// Map renders the Spec the way it is persisted in the app config and echoed
// back to the Cloud in app.list. Count round-trips as "all" so the stored
// object is the same shape the Cloud sent.
func (s *Spec) Map() map[string]any {
	if s == nil {
		return nil
	}
	count := any("all")
	if s.Count != CountAll {
		count = float64(s.Count) // JSON number, like a decoded payload
	}
	return map[string]any{
		"vendor":  s.Vendor,
		"runtime": s.Runtime,
		"count":   count,
	}
}

// String is the log form: "nvidia×all", "rocm×2".
func (s *Spec) String() string {
	if s == nil {
		return "none"
	}
	if s.Count == CountAll {
		return s.Runtime + "×all"
	}
	return s.Runtime + "×" + strconv.Itoa(s.Count)
}

func lower(v any) string {
	s, _ := v.(string)
	return strings.ToLower(strings.TrimSpace(s))
}
