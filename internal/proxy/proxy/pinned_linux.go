//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

const pinnedDialSupported = true

var errNotSocket = errors.New("not a socket")

// dialPinned connects to root/dir/name without resolving the name twice. The
// app owns dir and could swap the socket for a symlink between a check and a
// plain connect(), and the ODAC container holds docker.sock. So each component
// is opened with O_NOFOLLOW, the final inode must be a socket, and the connect
// goes through /proc/self/fd, which names that inode and nothing else.
func dialPinned(ctx context.Context, root, dir, name string) (net.Conn, error) {
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)
	dfd, err := unix.Openat(rfd, dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, errNotSocket
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
}
