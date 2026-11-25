package serverid

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureCreatesOnceAndKeeps(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read before Ensure = %v, want ErrNotExist", err)
	}
	first, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(first) {
		t.Fatalf("Ensure returned invalid id %q", first)
	}
	again, err := Ensure(dir)
	if err != nil || again != first {
		t.Fatalf("second Ensure = %q, %v; want %q", again, err, first)
	}
	read, err := Read(dir)
	if err != nil || read != first {
		t.Fatalf("Read = %q, %v; want %q", read, err, first)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("base dir holds %d entries, want only the id file", len(entries))
	}
}

func TestEnsureConcurrentCallersAgree(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = Ensure(dir)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d got %q, caller 0 got %q", i, ids[i], ids[0])
		}
	}
}

func TestEnsureRefusesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("not-an-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir); err == nil {
		t.Fatal("Ensure accepted a corrupt id file")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "not-an-id\n" {
		t.Fatalf("Ensure rewrote a corrupt id file to %q", raw)
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"", "0123", "0123456789abcdef0123456789ABCDEF", "0123456789abcdef0123456789abcdeg", "../../etc/passwd0123456789abcdef"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
	if !Valid("0123456789abcdef0123456789abcdef") {
		t.Error("Valid rejected a well-formed id")
	}
}
