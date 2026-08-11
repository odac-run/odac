//go:build !windows

package sysinfo

import (
	"context"
	"os/exec"
	"syscall"
)

// chrootCommand runs path with root as its filesystem root. The kernel
// applies the chroot in the forked child before exec, so the binary and
// every library it needs are resolved inside the host's userland. Needs
// CAP_SYS_CHROOT, which the privileged install has; without it the exec
// fails like any other and the caller degrades to zero figures.
func chrootCommand(ctx context.Context, root, path string, args ...string) (*exec.Cmd, bool) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = "/" // the pre-chroot cwd does not exist in the new root
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	return cmd, true
}
