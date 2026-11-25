//go:build !unix

package engine

import "errors"

func diskSpace(string) (avail, total int64, ok bool) { return 0, 0, false }

// SameFileSystem cannot tell on this platform.
func SameFileSystem(a, b string) (bool, error) {
	return false, errors.ErrUnsupported
}
