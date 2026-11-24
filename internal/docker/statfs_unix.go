//go:build linux || darwin

package docker

import "golang.org/x/sys/unix"

// statfsBytes reports the available (to a non-root writer, like `df`) and
// total bytes of the filesystem holding path; ok is false when path cannot
// be stat'ed.
func statfsBytes(path string) (free, total uint64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, true
}
