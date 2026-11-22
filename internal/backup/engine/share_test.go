package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageFilesAreTheSharedTable(t *testing.T) {
	root := t.TempDir()
	const (
		self   = "0123456789abcdef0123456789abcdef"
		peer   = "11111111111111111111111111111111"
		stale  = "22222222222222222222222222222222"
		liar   = "33333333333333333333333333333333"
		linked = "44444444444444444444444444444444"
	)
	at := func(id string) Storage {
		return Storage{Filesystem: &FilesystemStorage{Path: filepath.Join(root, id, RepositoryDir)}}
	}
	ctx := context.Background()
	when := time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
	if err := WriteUsage(at(self), UsageRecord{ServerID: self, Name: "me", Bytes: 7, Updated: when}); err != nil {
		t.Fatal(err)
	}
	if err := WriteUsage(at(peer), UsageRecord{ServerID: peer, Name: "peer", Bytes: 100, Garbage: 10, Updated: when}); err != nil {
		t.Fatal(err)
	}
	// Only an ownership file: a server that never measured itself.
	if _, err := ClaimOwner(ctx, at(stale), OwnerClaim{ServerID: stale, Name: "old", Seq: 1, Now: when}); err != nil {
		t.Fatal(err)
	}
	// A usage file copied from another directory.
	os.MkdirAll(filepath.Join(root, liar), 0o700)
	raw, _ := os.ReadFile(filepath.Join(root, peer, UsageFile))
	os.WriteFile(filepath.Join(root, liar, UsageFile), raw, 0o600)
	// A symlink is never followed.
	os.MkdirAll(filepath.Join(root, linked), 0o700)
	os.Symlink(filepath.Join(root, peer, UsageFile), filepath.Join(root, linked, UsageFile))
	// Not a server directory at all.
	os.MkdirAll(filepath.Join(root, "55555555555555555555555555555555"), 0o700)
	os.MkdirAll(filepath.Join(root, "lost+found"), 0o700)

	peers, err := ReadPeers(ctx, at(self), self)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PeerUsage{}
	for _, p := range peers {
		got[p.ID] = p
	}
	if len(peers) != 4 {
		t.Fatalf("peers: %+v", peers)
	}
	if p := got[peer]; p.Bytes != 100 || p.Garbage != 10 || !p.Updated.Equal(when) || p.Error != "" {
		t.Errorf("peer: %+v", p)
	}
	if p := got[stale]; p.Name != "old" || !p.Seen.Equal(when) || !p.Updated.IsZero() || p.Bytes != 0 {
		t.Errorf("stale: %+v", p)
	}
	if p := got[liar]; p.Bytes != 0 || !strings.Contains(p.Error, peer) {
		t.Errorf("a copied usage file was believed: %+v", p)
	}
	if p := got[linked]; p.Bytes != 0 || p.Error == "" {
		t.Errorf("a symlinked usage file was followed: %+v", p)
	}
	if err := WriteUsage(Storage{Filesystem: &FilesystemStorage{Path: filepath.Join(root, "elsewhere")}}, UsageRecord{ServerID: self}); err == nil {
		t.Error("a usage file outside the server directory layout")
	}
}
