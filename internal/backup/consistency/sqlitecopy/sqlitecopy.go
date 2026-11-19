// Package sqlitecopy makes consistent copies of SQLite databases that are
// in use, so a backup never stores one torn between two transactions. It
// runs inside odac-backup, against app directories mounted read-only, and
// needs no sqlite3 binary in the app's image.
//
// The copy is taken with SQLite's online backup API, stepped once over the
// whole database, rather than with VACUUM INTO: the backup API copies pages
// verbatim, so an unchanged page stays byte-identical between runs and
// deduplicates, and it reads page contents without interpreting them,
// which matters for a file any app can plant. Both run inside one read
// transaction. In WAL mode that never blocks the app's writers; in
// rollback-journal mode it holds a shared lock for the whole copy, so
// writers wait until it ends (Info.BlocksWriters).
package sqlitecopy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// HeaderSize is how much of a file Detect reads.
const HeaderSize = 100

var magic = []byte("SQLite format 3\x00")

// Header is what a database file's first page says about it.
type Header struct {
	PageSize int64
	// WAL is a database in write-ahead-log mode.
	WAL bool
}

// Parse reads a database header. It is strict, because a file that only
// starts with the magic string is not worth opening: the page size must be
// a power of two the format allows and the payload fractions must be the
// fixed values every SQLite 3 database carries.
func Parse(h []byte, size int64) (Header, bool) {
	if len(h) < HeaderSize || !bytes.Equal(h[:16], magic) {
		return Header{}, false
	}
	ps := int64(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return Header{}, false
	}
	if h[21] != 64 || h[22] != 32 || h[23] != 32 {
		return Header{}, false
	}
	if size < ps || size%ps != 0 {
		return Header{}, false
	}
	return Header{PageSize: ps, WAL: h[18] == 2 && h[19] == 2}, true
}

// Candidate reports whether a regular file of this size could be a
// database, so a directory walk opens only those.
func Candidate(size int64) bool { return size >= 512 && size%512 == 0 }

// Detect reads the header of the file at path.
func Detect(path string) (Header, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Header{}, false, err
	}
	if !fi.Mode().IsRegular() || !Candidate(fi.Size()) {
		return Header{}, false, nil
	}
	h := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, h); err != nil {
		return Header{}, false, nil
	}
	hd, ok := Parse(h, fi.Size())
	return hd, ok, nil
}

// Siblings are the files SQLite keeps next to a database. A consistent
// copy replaces all of them: stored next to it, a WAL or hot journal from
// another moment would be replayed onto the copy when it is opened.
func Siblings(name string) []string {
	return []string{name + "-wal", name + "-shm", name + "-journal"}
}

// Info describes a copy.
type Info struct {
	Bytes int64
	WAL   bool
	// BlocksWriters is a rollback-journal database, whose writers waited
	// for the copy to end.
	BlocksWriters bool
}

// ErrBusy is a database whose lock could not be taken within the wait.
var ErrBusy = errors.New("sqlitecopy: database stayed locked")

// backuper is what the driver's connection offers for the backup API.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// Copy writes a consistent copy of the database at src to dst, which must
// not exist. The source is opened read-only and is never written; a lock
// held by a writer is waited for at most wait. The copy is one backup
// step, which SQLite cannot interrupt, so ctx is checked only between
// attempts.
func Copy(ctx context.Context, src, dst string, wait time.Duration) (Info, error) {
	var info Info
	if _, err := os.Lstat(dst); err == nil {
		return info, fmt.Errorf("sqlitecopy: %s exists", dst)
	}
	hd, ok, err := Detect(src)
	if err != nil {
		return info, err
	}
	if !ok {
		return info, fmt.Errorf("sqlitecopy: %s is not a SQLite database", src)
	}
	info.WAL = hd.WAL
	info.BlocksWriters = !hd.WAL

	db, err := sql.Open("sqlite", sourceURI(src))
	if err != nil {
		return info, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return info, err
	}
	defer conn.Close()

	deadline := time.Now().Add(wait)
	delay := 10 * time.Millisecond
	for {
		err = conn.Raw(func(dc any) error {
			b, ok := dc.(backuper)
			if !ok {
				return errors.New("sqlitecopy: the driver has no backup API")
			}
			bk, err := b.NewBackup(destURI(dst))
			if err != nil {
				return err
			}
			_, stepErr := bk.Step(-1)
			finErr := bk.Finish()
			if stepErr != nil {
				return stepErr
			}
			return finErr
		})
		if err == nil || !busy(err) {
			break
		}
		os.Remove(dst)
		os.Remove(dst + "-journal")
		if time.Now().Add(delay).After(deadline) {
			return info, fmt.Errorf("%w: %v", ErrBusy, err)
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Second)
	}
	if err != nil {
		os.Remove(dst)
		os.Remove(dst + "-journal")
		return info, fmt.Errorf("sqlitecopy: %s: %w", src, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return info, err
	}
	info.Bytes = fi.Size()
	return info, nil
}

func busy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code() & 0xff
		return code == 5 || code == 6 // SQLITE_BUSY, SQLITE_LOCKED
	}
	return strings.Contains(err.Error(), "database is locked")
}

// sourceURI opens the database read-only, so nothing can be written to it
// even where its directory is writable. A read-only mount leaves the WAL
// index read-only too, which SQLite handles since 3.22.
func sourceURI(p string) string {
	return (&url.URL{Scheme: "file", Path: p}).String() + "?mode=ro"
}

func destURI(p string) string {
	return (&url.URL{Scheme: "file", Path: p}).String()
}
