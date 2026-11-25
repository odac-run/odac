//go:build unix

package backup

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// freeBytes reports the bytes free to this process on the file system
// holding dir.
func freeBytes(dir string) (int64, bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}

// diskSpace reports the bytes free to this process and the size of the
// file system holding dir.
func diskSpace(dir string) (free, total int64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}

// ownerOf is the owner of a directory, which a file the restore writes
// into it takes, so that a database running as its own user can read it.
func ownerOf(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// sameDevice reports whether a and b are on one file system, so that a
// rename between them works.
func sameDevice(a, b string) bool {
	var sa, sb unix.Stat_t
	if unix.Stat(a, &sa) != nil || unix.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
