package main

import (
	"os"
	"path/filepath"
	"sync"
)

// Keep this file in sync with server/mail/logrotate.go and
// server/proxy/logrotate.go. Inlined per-binary on purpose: the three
// services have independent go.mod's so a shared module is more pain than
// duplicating ~50 lines.

type rotateWriter struct {
	mu         sync.Mutex
	path       string
	f          *os.File
	maxBytes   int64
	sinceCheck int64
}

func newRotateWriter(path string, maxBytes int64) (*rotateWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &rotateWriter{path: path, f: f, maxBytes: maxBytes}, nil
}

func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.f.Write(p)
	w.sinceCheck += int64(n)
	if w.sinceCheck >= 4096 {
		w.sinceCheck = 0
		if fi, e := w.f.Stat(); e == nil && fi.Size() > w.maxBytes {
			_ = w.f.Close()
			_ = os.Rename(w.path, w.path+".1")
			if nf, openErr := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); openErr == nil {
				w.f = nf
			}
		}
	}
	return n, err
}
