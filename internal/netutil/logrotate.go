// Package netutil holds the small helpers the data-plane binaries
// (odac-proxy, odac-dns, odac-mail) used to inline per-binary while they
// were independent Go modules. Single module since 4.3 → shared for real.
package netutil

import (
	"os"
	"path/filepath"
	"sync"
)

// RotateWriter is an io.Writer that appends to path and renames it to
// path+".1" once it grows past maxBytes (size checked every ~4KiB written).
type RotateWriter struct {
	mu         sync.Mutex
	path       string
	f          *os.File
	maxBytes   int64
	sinceCheck int64
}

func NewRotateWriter(path string, maxBytes int64) (*RotateWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &RotateWriter{path: path, f: f, maxBytes: maxBytes}, nil
}

func (w *RotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		nf, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return 0, err
		}
		w.f = nf
	}

	n, err := w.f.Write(p)
	if err != nil {
		w.f = nil
		return n, err
	}

	w.sinceCheck += int64(n)
	if w.sinceCheck >= 4096 {
		w.sinceCheck = 0
		if fi, e := w.f.Stat(); e == nil && fi.Size() > w.maxBytes {
			_ = w.f.Close()
			_ = os.Rename(w.path, w.path+".1")
			if nf, openErr := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); openErr == nil {
				w.f = nf
			} else {
				w.f = nil
			}
		}
	}
	return n, err
}
