//go:build !linux && !darwin

package docker

// statfsBytes cannot measure a filesystem on this platform; callers treat
// that as unknown.
func statfsBytes(string) (free, total uint64, ok bool) { return 0, 0, false }
