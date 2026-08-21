// Package blob implements a content-addressed store for raw MIME messages.
// Messages are immutable once written, so the SHA-256 of the content doubles
// as its address: the same message delivered to many local recipients is
// stored once and referenced by every row that needs it.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// refLen is the length of a hex-encoded SHA-256 digest.
const refLen = sha256.Size * 2

// ErrInvalidRef is returned for a reference that is not a hex SHA-256 digest.
// Refs reach this package from database rows, so they are validated before
// being turned into a filesystem path.
var ErrInvalidRef = errors.New("blob: invalid reference")

// Store keeps blobs under a root directory, sharded two levels deep by the
// leading digest bytes so no single directory accumulates every object.
type Store struct {
	root string
}

// NewStore opens (and creates) a blob store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blob: empty root directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0750); err != nil {
		return nil, fmt.Errorf("blob: cannot create store: %w", err)
	}
	return &Store{root: dir}, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// validRef reports whether ref is a well-formed lowercase hex digest.
// Anything else could escape the store root once joined into a path.
func validRef(ref string) bool {
	if len(ref) != refLen {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// path maps a validated ref to its on-disk location.
func (s *Store) path(ref string) (string, error) {
	if !validRef(ref) {
		return "", ErrInvalidRef
	}
	return filepath.Join(s.root, ref[0:2], ref[2:4], ref), nil
}

// Put stores data and returns its content address. Writing the same bytes
// twice is a no-op that returns the existing ref.
func (s *Store) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	ref := hex.EncodeToString(sum[:])

	dst, err := s.path(ref)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dst); err == nil {
		return ref, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0750); err != nil {
		return "", fmt.Errorf("blob: cannot create shard: %w", err)
	}

	// Write to a temp file and rename, so a crash mid-write can never leave a
	// truncated object visible under its content address.
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return "", fmt.Errorf("blob: cannot create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("blob: close failed: %w", err)
	}
	if err := os.Chmod(tmpName, 0640); err != nil {
		return "", fmt.Errorf("blob: chmod failed: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("blob: rename failed: %w", err)
	}
	return ref, nil
}

// Get returns the full contents of a blob.
func (s *Store) Get(ref string) ([]byte, error) {
	p, err := s.path(ref)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Range returns length bytes starting at offset. A range running past the end
// of the blob is clamped rather than treated as an error, matching the way
// IMAP partial fetches are allowed to ask for more than exists.
func (s *Store) Range(ref string, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, errors.New("blob: negative range")
	}
	p, err := s.path(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if offset >= info.Size() {
		return nil, nil
	}
	if offset+length > info.Size() {
		length = info.Size() - offset
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(io.NewSectionReader(f, offset, length), buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Has reports whether a blob exists.
func (s *Store) Has(ref string) bool {
	p, err := s.path(ref)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Size returns the byte length of a blob without reading it.
func (s *Store) Size(ref string) (int64, error) {
	p, err := s.path(ref)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Delete removes a blob. Missing objects are not an error, so a sweep that
// races another deleter stays idempotent.
func (s *Store) Delete(ref string) error {
	p, err := s.path(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Walk calls fn for every stored ref. Entries whose name is not a valid ref
// are skipped, which keeps the temp directory out of the results.
func (s *Store) Walk(fn func(ref string, size int64, modTime int64) error) error {
	return filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != s.root && strings.HasPrefix(d.Name(), "tmp") && filepath.Dir(path) == s.root {
				return filepath.SkipDir
			}
			return nil
		}
		if !validRef(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(d.Name(), info.Size(), info.ModTime().Unix())
	})
}
