//go:build !unix

package engine

func diskSpace(string) (avail, total int64, ok bool) { return 0, 0, false }
