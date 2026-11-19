//go:build unix

package engine

import "golang.org/x/sys/unix"

// diskSpace reports the bytes free to this process and the size of the
// file system holding dir.
func diskSpace(dir string) (avail, total int64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}
