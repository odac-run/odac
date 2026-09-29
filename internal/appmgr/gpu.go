package appmgr

import (
	"errors"

	"odac/internal/docker"
	"odac/internal/gpu"
)

// resolveGPU turns an app's parsed GPU request into the spec its container is
// actually created with, and reports nil for "run on the CPU".
//
// The host is consulted here, at every container create, rather than once at
// app create time, because the request is stored verbatim: a host that gains
// a card later needs only a restart, one that loses it does not strand the
// app, and app.list keeps serializing the request the Cloud sent instead of a
// resolution that drifts under it.
//
// Callers pass a Spec rather than the persisted object on purpose. Parsing is
// pure and belongs under the config lock with every other conversion; the
// host probe behind CanPassthrough is not, and must not be holding that lock.
//
// A required request is handed through untouched even when the host looks
// unable to serve it. That refusal belongs to the daemon, whose own words
// reach the app's build log; downgrading it silently would start a CUDA-only
// image on the CPU to crash-loop there, which is exactly what the create-time
// pre-flight exists to prevent.
func (m *Manager) resolveGPU(name string, spec *gpu.Spec) *gpu.Spec {
	if spec == nil || !spec.Optional {
		return spec
	}

	host := m.deps.GPUHost
	if spec.IsAuto() {
		// Nothing names a runtime, so without a host to ask there is no
		// request left to make.
		if host == nil {
			return nil
		}
		runtime := host.GPURuntime()
		if runtime == "" || !host.CanPassthrough(runtime) {
			m.log.Log("App %s asked for an optional GPU and this host has none to give. Starting on the CPU.", name)
			return nil
		}
		resolved := *spec
		resolved.Runtime = runtime
		resolved.Vendor = gpu.VendorFor(runtime)
		m.log.Log("App %s: optional GPU resolved to %s.", name, resolved.String())
		return &resolved
	}

	if host != nil && !host.CanPassthrough(spec.Runtime) {
		m.log.Log("App %s asked for an optional %s GPU this host cannot pass to a container. Starting on the CPU.", name, spec.Runtime)
		return nil
	}
	return spec
}

// startWithGPUFallback runs a container and, when the daemon refuses it over
// an optional GPU request, retries once without the request.
//
// This is the second door, and the one that matters most. resolveGPU asks the
// host what it can do, but the engine has the last word and can disagree (a
// device node that disappeared, a runtime deregistered since the last probe).
// Without this retry the app would fail its start, and Manager.Check would
// recreate it on the next 1-second pulse, forever: a respawn loop that still
// reports healthy to the watchdog because every probe hits a fresh container.
// An app that asked to accelerate "when possible" must never be able to reach
// that state.
func (m *Manager) startWithGPUFallback(name string, options docker.RunOptions, buildLog docker.BuildLog, isCancelled func() bool) (bool, error) {
	started, err := m.deps.Docker.RunApp(name, options, buildLog, isCancelled)
	if err == nil || options.GPU == nil || !options.GPU.Optional {
		return started, err
	}
	if !errors.Is(err, docker.ErrGPUUnavailable) {
		return started, err
	}
	m.log.Log("App %s: the container engine refused the optional GPU (%s): %s. Retrying on the CPU.", name, options.GPU.String(), err.Error())
	options.GPU = nil
	return m.deps.Docker.RunApp(name, options, buildLog, isCancelled)
}

// gpuRow renders one app.list row's `gpu` member: the reservation the app
// asked for, plus what its container actually holds right now.
//
// The attachment is nested under `attached` rather than merged in beside the
// request's own members, and that is the whole design. app.list's `gpu` is
// the shape the Cloud sent at create time and may be handed straight back to
// app.gpu; writing a resolved `vendor` next to the request's would turn an
// auto reservation into a pinned one on that round-trip, freezing today's
// hardware into the config. gpu.Parse ignores members it does not know, so
// nesting keeps the echo lossless.
//
// Absent when nothing is attached, the same rule Spec.Map follows: a member
// that only ever says "no" is churn in a payload the dashboard diffs.
func gpuRow(request map[string]any, running bool, attachment *docker.GPUAttachment) map[string]any {
	// The persisted map is shared with the config, so never write into it.
	row := copyMap(request)
	// A hand-edited config could carry a stale one; the engine is the only
	// thing allowed to answer this.
	delete(row, "attached")
	if !running || attachment == nil {
		return row
	}

	attached := map[string]any{"vendor": attachment.Vendor}
	if len(attachment.Nodes) > 0 {
		nodes := make([]any, len(attachment.Nodes))
		for i, node := range attachment.Nodes {
			nodes[i] = node
		}
		attached["nodes"] = nodes
	}
	switch {
	case attachment.Count == gpu.CountAll:
		attached["count"] = "all"
	case attachment.Count > 0:
		attached["count"] = float64(attachment.Count)
	}
	row["attached"] = attached
	return row
}
