package blob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	data := []byte("From: a@b.com\r\n\r\nhello")

	ref, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(ref) != refLen {
		t.Fatalf("ref length = %d, want %d", len(ref), refLen)
	}

	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Get = %q, want %q", got, data)
	}
	if size, _ := s.Size(ref); size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", size, len(data))
	}
}

// The same message fanned out to many local recipients must occupy one object.
func TestPutIsDeduplicated(t *testing.T) {
	s := newTestStore(t)
	data := []byte("duplicate message")

	first, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := s.Put(data)
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if first != second {
		t.Fatalf("refs differ: %s vs %s", first, second)
	}

	count := 0
	if err := s.Walk(func(string, int64, int64) error { count++; return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if count != 1 {
		t.Errorf("stored objects = %d, want 1", count)
	}
}

func TestRangeClampsPastEnd(t *testing.T) {
	s := newTestStore(t)
	ref, _ := s.Put([]byte("0123456789"))

	got, err := s.Range(ref, 8, 100)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if string(got) != "89" {
		t.Errorf("Range = %q, want %q", got, "89")
	}

	got, err = s.Range(ref, 50, 5)
	if err != nil {
		t.Fatalf("Range past end: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Range past end = %q, want empty", got)
	}
}

// Refs arrive from database rows, so a doctored value must not escape the root.
func TestInvalidRefIsRejected(t *testing.T) {
	s := newTestStore(t)
	outside := filepath.Join(filepath.Dir(s.Root()), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, ref := range []string{
		"../../secret",
		"..",
		"",
		strings.Repeat("z", refLen),
		strings.Repeat("A", refLen),
		strings.Repeat("a", refLen-1),
	} {
		if _, err := s.Get(ref); err != ErrInvalidRef {
			t.Errorf("Get(%q) error = %v, want ErrInvalidRef", ref, err)
		}
		if s.Has(ref) {
			t.Errorf("Has(%q) = true", ref)
		}
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ref, _ := s.Put([]byte("gone soon"))

	if err := s.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has(ref) {
		t.Error("blob still present after Delete")
	}
	if err := s.Delete(ref); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Walk feeds the orphan sweeper; the temp staging area must stay out of it.
func TestWalkSkipsTempDir(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put([]byte("real object")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "tmp", "put-123"), []byte("junk"), 0600); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	var refs []string
	if err := s.Walk(func(ref string, _ int64, _ int64) error {
		refs = append(refs, ref)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("Walk returned %d refs, want 1: %v", len(refs), refs)
	}
}
