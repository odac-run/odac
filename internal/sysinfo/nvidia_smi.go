package sysinfo

import (
	"context"
	"os/exec"
	"path/filepath"
)

// hostRootCandidates are the host filesystem as seen from inside ODAC's own
// container: the pid-host path (docker-compose.yml sets `pid: host`) and an
// explicit bind mount, mirroring the layering osReleaseCandidates uses.
var hostRootCandidates = []string{"/proc/1/root", "/host"}

// lookPath is a test seam: a CI machine that happens to have a GPU must not
// change which branch the tests exercise.
var lookPath = exec.LookPath

// nvidiaSMIPaths are where the NVIDIA driver installs nvidia-smi on a host.
var nvidiaSMIPaths = []string{
	"/usr/bin/nvidia-smi",
	"/usr/local/bin/nvidia-smi",
	"/bin/nvidia-smi",
}

// nvidiaSMICommand builds one nvidia-smi invocation, or reports false when
// the binary is out of reach.
//
// ODAC's own image is Alpine and deliberately ships no NVIDIA userland, so
// the PATH lookup normally misses and the host's binary is used instead —
// executed inside a chroot to the host root. The chroot is what makes it
// work at all: nvidia-smi links against the driver's own libnvidia-ml, which
// only exists in the host's (usually glibc) userland, so running the binary
// with ODAC's musl loader would fail. Reading it this way needs the
// privileged + pid-host profile ODAC already ships with; on a leaner install
// nothing is found and the VRAM and utilization figures stay zero, exactly
// as they were before.
//
// This is read-only host inspection — ODAC never mutates the host to get it.
func nvidiaSMICommand(ctx context.Context, args ...string) (*exec.Cmd, bool) {
	if bin, err := lookPath("nvidia-smi"); err == nil {
		return exec.CommandContext(ctx, bin, args...), true
	}
	for _, root := range hostRootCandidates {
		for _, path := range nvidiaSMIPaths {
			if !pathExists(filepath.Join(root, path)) {
				continue
			}
			if cmd, ok := chrootCommand(ctx, root, path, args...); ok {
				return cmd, true
			}
		}
	}
	return nil, false
}
