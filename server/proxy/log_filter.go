package main

import (
	"bytes"
	"io"
	"log"
	"os"
)

// LogFilter implements io.Writer to filter out specific log messages.
// It is used to suppress noisy logs from the standard library (e.g. net/http).
type LogFilter struct {
	w       io.Writer
	ignores [][]byte
}

// NewLogFilter creates a new LogFilter that writes to w but discards
// any write containing any of the ignore strings.
func NewLogFilter(w io.Writer, ignores []string) *LogFilter {
	ignoreBytes := make([][]byte, len(ignores))
	for i, s := range ignores {
		ignoreBytes[i] = []byte(s)
	}
	return &LogFilter{
		w:       w,
		ignores: ignoreBytes,
	}
}

// Write checks if p contains any ignore pattern. If so, it discards the write.
// Otherwise, it writes p to the underlying writer.
func (f *LogFilter) Write(p []byte) (n int, err error) {
	for _, ignore := range f.ignores {
		if bytes.Contains(p, ignore) {
			// Discard the log silently
			return len(p), nil
		}
	}
	return f.w.Write(p)
}

// createErrorLogger creates a standard library logger that filters out specific noise.
// This is critical for keeping logs clean in high-traffic environments where
// client-side connection drops (TLS handshake errors) are common and not actionable.
func createErrorLogger() *log.Logger {
	return log.New(NewLogFilter(os.Stderr, []string{
		"http: TLS handshake error",
		"TLS handshake error",
	}), "", log.LstdFlags|log.Lmicroseconds)
}
