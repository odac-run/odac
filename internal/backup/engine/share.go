package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"odac/internal/serverid"
)

const usageFormat = 1

// UsageRecord is a server's usage file: what it last measured its
// repository at. Servers sharing a filesystem target read each other's to
// split its free space fairly, with no protocol between them, so the
// target itself is the shared table.
type UsageRecord struct {
	Format   int       `json:"format"`
	ServerID string    `json:"serverId"`
	Name     string    `json:"name,omitempty"`
	Bytes    int64     `json:"bytes"`
	Garbage  int64     `json:"garbage,omitempty"`
	Updated  time.Time `json:"updated"`
}

// PeerUsage is another server's directory on a shared filesystem target.
// Updated is when its usage file was written (zero without one) and Seen
// when its ownership file was. Error is a usage file that could not be
// read, which counts as none.
type PeerUsage struct {
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Garbage int64     `json:"garbage,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
	Seen    time.Time `json:"seen,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// serverDirOf is the server directory around a filesystem repository,
// which must live in <id>/repository.
func serverDirOf(st Storage, id string) (string, error) {
	if st.Filesystem == nil {
		return "", errors.New("engine: only a filesystem target is shared by usage files")
	}
	if !serverid.Valid(id) {
		return "", errors.New("engine: invalid server id")
	}
	p := filepath.Clean(st.Filesystem.Path)
	if filepath.Base(p) != RepositoryDir || filepath.Base(filepath.Dir(p)) != id {
		return "", fmt.Errorf("engine: an owned repository lives in %s/%s of its target", id, RepositoryDir)
	}
	return filepath.Dir(p), nil
}

// WriteUsage replaces this server's usage file. Only a run that claimed
// the directory may call it, so a clone stopped by the claim never
// overwrites it.
func WriteUsage(st Storage, rec UsageRecord) error {
	dir, err := serverDirOf(st, rec.ServerID)
	if err != nil {
		return err
	}
	rec.Format = usageFormat
	rec.Updated = rec.Updated.UTC()
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := replaceFile(dir, UsageFile, append(out, '\n')); err != nil {
		return fmt.Errorf("engine: write usage file: %w", err)
	}
	return nil
}

// ReadPeers lists the other server directories beside self's on a
// filesystem target, with what their usage and ownership files say. A
// directory with neither is not a server's and is left out. The files are
// written by other machines, so each is read only as a regular file of
// bounded size, never through a symlink.
func ReadPeers(ctx context.Context, st Storage, self string) ([]PeerUsage, error) {
	dir, err := serverDirOf(st, self)
	if err != nil {
		return nil, err
	}
	ids, err := serverIDsAt(ctx, st, self)
	if err != nil {
		return nil, fmt.Errorf("engine: list server directories: %w", err)
	}
	root := filepath.Dir(dir)
	sort.Strings(ids)
	out := []PeerUsage{}
	for _, id := range ids {
		if id == self {
			continue
		}
		p := PeerUsage{ID: id}
		var owner OwnerRecord
		ownerErr := readSmall(filepath.Join(root, id, OwnerFile), &owner)
		if ownerErr == nil && owner.ServerID == id {
			p.Name, p.Seen = owner.Name, owner.Updated
		}
		var u UsageRecord
		usageErr := readSmall(filepath.Join(root, id, UsageFile), &u)
		switch {
		case usageErr == nil && u.ServerID != id:
			p.Error = fmt.Sprintf("the usage file names server %s", u.ServerID)
		case usageErr == nil:
			p.Bytes, p.Garbage, p.Updated = max(u.Bytes, 0), max(u.Garbage, 0), u.Updated
			if p.Name == "" {
				p.Name = u.Name
			}
		case !errors.Is(usageErr, os.ErrNotExist):
			p.Error = usageErr.Error()
		case errors.Is(ownerErr, os.ErrNotExist):
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// readSmall decodes a regular file of at most ownerMaxSize bytes.
func readSmall(path string, v any) error {
	link, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !link.Mode().IsRegular() || link.Size() > ownerMaxSize {
		return fmt.Errorf("%s is not a small regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Replaced by something else between the two looks.
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, link) {
		return fmt.Errorf("%s changed while it was read", filepath.Base(path))
	}
	raw, err := io.ReadAll(io.LimitReader(f, ownerMaxSize))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}
