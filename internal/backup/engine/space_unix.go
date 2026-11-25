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

// SameFileSystem reports whether a and b live on one file system, which a
// bind mount keeps visible inside a container: a target on the disk that
// holds the data it protects survives a mistaken delete and nothing else.
func SameFileSystem(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}
