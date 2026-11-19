// Package serverid gives a server a permanent identity, independent of its
// hostname, IP and Hub pairing, so that data it writes to shared places
// (backup targets above all) stays attributable to it for its whole life.
//
// The id lives in its own file rather than in the "server" config module:
// that module is rewritten whole by both the watchdog and the server from
// their own in-memory copies, and one stale copy saved over a fresh one
// would silently hand the server a new identity on its next start. This
// file is created once, atomically, and never written again.
package serverid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the id file's name inside the ODAC base directory.
const FileName = "server-id"

const idBytes = 16

// Ensure returns the server id stored under baseDir, creating it on first
// use. Concurrent callers (the watchdog and the server racing on a fresh
// install) all end up with the same id: the file appears through a hard
// link, which fails rather than replaces when the name exists.
//
// An existing file that does not hold a valid id is an error, never a
// reason to mint a new one, because a changed id orphans every backup the
// server has written.
func Ensure(baseDir string) (string, error) {
	path := filepath.Join(baseDir, FileName)
	if id, err := read(path); !errors.Is(err, os.ErrNotExist) {
		return id, err
	}

	raw := make([]byte, idBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("serverid: generate: %w", err)
	}
	id := hex.EncodeToString(raw)

	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return "", fmt.Errorf("serverid: create base dir: %w", err)
	}
	tmp, err := os.CreateTemp(baseDir, FileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("serverid: create: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("serverid: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("serverid: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("serverid: close: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("serverid: publish: %w", err)
	}
	if d, err := os.Open(baseDir); err == nil {
		d.Sync()
		d.Close()
	}
	return read(path)
}

// Read returns the stored id without creating one. It reports
// os.ErrNotExist when the server has never started.
func Read(baseDir string) (string, error) {
	return read(filepath.Join(baseDir, FileName))
}

func read(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if !Valid(id) {
		return "", fmt.Errorf("serverid: %s does not hold a valid id; restore it from a backup of this server instead of deleting it", path)
	}
	return id, nil
}

// Valid reports whether s has the shape of a server id: 32 lowercase hex
// characters. Ids travel into directory names on shared targets, so
// anything else is refused rather than sanitized.
func Valid(s string) bool {
	if len(s) != idBytes*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
