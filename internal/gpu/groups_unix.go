//go:build !windows

package gpu

import (
	"os"
	"syscall"
)

// deviceGID reports the numeric group owning a device node.
func deviceGID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Gid), true
}
