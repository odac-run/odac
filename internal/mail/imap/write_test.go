package imap

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// recordingConn records every write and deadline renewal instead of sending.
type recordingConn struct {
	net.Conn
	writes    []int
	deadlines int
	failAfter int // Write fails once this many writes succeeded; -1 never fails
	closed    bool
	buf       bytes.Buffer
}

func (r *recordingConn) Write(p []byte) (int, error) {
	if r.failAfter >= 0 && len(r.writes) >= r.failAfter {
		return 0, errors.New("i/o timeout")
	}
	r.writes = append(r.writes, len(p))
	return r.buf.Write(p)
}

func (r *recordingConn) SetWriteDeadline(time.Time) error { r.deadlines++; return nil }
func (r *recordingConn) Close() error                     { r.closed = true; return nil }
func (r *recordingConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

// TestLargeLiteralRenewsDeadlinePerChunk pins the fix for attachments that
// never finished downloading: one deadline over a 16MB literal cut it off on
// any link slower than about 6 Mbit/s.
func TestLargeLiteralRenewsDeadlinePerChunk(t *testing.T) {
	rc := &recordingConn{failAfter: -1}
	c := &Connection{conn: rc}
	body := bytes.Repeat([]byte("A"), 5*writeChunkSize+123)

	c.writeSectionResult("2", body, false, 0, 0)

	for _, n := range rc.writes {
		if n > writeChunkSize {
			t.Fatalf("write of %d bytes exceeds chunk size %d", n, writeChunkSize)
		}
	}
	if rc.deadlines != len(rc.writes) {
		t.Errorf("deadline renewed %d times for %d writes", rc.deadlines, len(rc.writes))
	}
	out := rc.buf.String()
	if !strings.HasPrefix(out, "BODY[2] {327803}\r\n") {
		t.Fatalf("unexpected literal header: %q", out[:min(len(out), 40)])
	}
	if got := payload(t, out); got != string(body) {
		t.Errorf("payload length %d, want %d", len(got), len(body))
	}
}

// TestFailedWriteClosesConnection checks that a write error ends the session
// instead of being ignored while the rest of the response goes nowhere.
func TestFailedWriteClosesConnection(t *testing.T) {
	rc := &recordingConn{failAfter: 2}
	c := &Connection{conn: rc}

	c.writeBytes(bytes.Repeat([]byte("A"), 4*writeChunkSize))
	if c.writeErr == nil || !rc.closed {
		t.Fatalf("writeErr=%v closed=%v, want error and closed connection", c.writeErr, rc.closed)
	}

	c.write("* OK after failure\r\n")
	if len(rc.writes) != 2 {
		t.Errorf("writes after failure were attempted: %v", rc.writes)
	}
}
