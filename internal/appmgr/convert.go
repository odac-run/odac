package appmgr

import (
	"odac/internal/docker"
	"odac/internal/gpu"
	"odac/internal/kernel"
	"odac/internal/netmode"
	"odac/internal/resources"
)

// copyMap shallow-copies a decoded-JSON map.
func copyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// toCmd converts a persisted `cmd` value ([]any of strings, or null) to the
// Docker command slice.
func toCmd(v any) []string {
	list, _ := v.([]any)
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, jsString(item))
	}
	return out
}

// toMounts converts persisted `volumes` entries to docker.Mount.
func toMounts(v any) []docker.Mount {
	list, _ := v.([]any)
	out := make([]docker.Mount, 0, len(list))
	for _, item := range list {
		if entry, _ := item.(map[string]any); entry != nil {
			host, _ := entry["host"].(string)
			container, _ := entry["container"].(string)
			out = append(out, docker.Mount{Host: host, Container: container})
		}
	}
	return out
}

// toGPU converts a persisted `gpu` object to a request. The value was
// validated at creation time, so a malformed one here means a hand-edited
// config: it degrades to no GPU rather than failing the start.
func toGPU(v any) *gpu.Spec {
	spec, err := gpu.Parse(v)
	if err != nil {
		return nil
	}
	return spec
}

// toKernel converts a persisted app record's `caps` / `sysctls` members into
// a request. Like toGPU the values were validated at creation time, so a
// malformed one here means a hand-edited config: it degrades to no request
// rather than failing the start, because the alternative is an app the
// engine never brings back.
func toKernel(app map[string]any) *kernel.Spec {
	spec, err := kernel.Parse(app)
	if err != nil {
		return nil
	}
	return spec
}

// toResources converts a persisted app record's `shmSize` member into a
// request. Like toGPU the value was validated at creation time, so a
// malformed one here means a hand-edited config: it degrades to the engine
// default rather than failing the start, because the alternative is an app
// the engine never brings back.
func toResources(app map[string]any) *resources.Spec {
	spec, err := resources.Parse(app)
	if err != nil {
		return nil
	}
	return spec
}

// toNetworkMode converts a persisted `networkMode` value to a canonical mode.
// Like toGPU it degrades to the safe default (isolated bridge) rather than
// failing the start, since only a hand-edited config can be malformed here.
func toNetworkMode(v any) string {
	mode, err := netmode.Parse(v)
	if err != nil {
		return netmode.Bridge
	}
	return mode
}

// toDevices converts persisted `devices` entries to docker.Device.
func toDevices(v any) []docker.Device {
	list, _ := v.([]any)
	out := make([]docker.Device, 0, len(list))
	for _, item := range list {
		if entry, _ := item.(map[string]any); entry != nil {
			host, _ := entry["host"].(string)
			container, _ := entry["container"].(string)
			out = append(out, docker.Device{Host: host, Container: container})
		}
	}
	return out
}
