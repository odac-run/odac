package netutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateWriterAppendsAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "svc.log")

	w, err := NewRotateWriter(path, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("log content = %q, err %v", got, err)
	}

	// Push well past maxBytes; the size check runs every ~4KiB, so after
	// ~16KiB the file must have been renamed to .1 and restarted.
	chunk := bytes.Repeat([]byte("x"), 1024)
	for i := 0; i < 16; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() > 9*1024 {
		t.Fatalf("live log not restarted: size %d, err %v", fi.Size(), err)
	}
}
