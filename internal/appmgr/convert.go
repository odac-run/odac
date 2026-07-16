package appmgr

import "odac/internal/docker"

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
