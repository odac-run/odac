package sysinfo

import (
	"context"
	"os/exec"
)

// chrootCommand has no Windows equivalent — there is no chroot and no
// container-in-container host root to reach into. Reporting false keeps the
// caller on the PATH lookup alone.
func chrootCommand(context.Context, string, string, ...string) (*exec.Cmd, bool) {
	return nil, false
}
