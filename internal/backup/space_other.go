//go:build !unix

package backup

import "os"

func freeBytes(string) (int64, bool) { return 0, false }

func diskSpace(string) (free, total int64, ok bool) { return 0, 0, false }

func ownerOf(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
