package imap

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A line with no newline used to be read whole into memory before its length
// was checked, so an anonymous peer could make the server buffer whatever it
// sent for the 30 minute idle deadline. The session must end once the reader's
// buffer is full, and the server must stop reading there.
func TestOverlongLineEndsSessionBeforeLogin(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := &Connection{conn: server, reader: bufio.NewReaderSize(server, maxLineSize)}
	served := make(chan struct{})
	go func() {
		c.Serve()
		server.Close()
		close(served)
	}()

	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(client)
		out <- string(data)
	}()

	// Far more than the buffer, with no newline. The write fails once the
	// server stops reading, which is the bound under test.
	sent, _ := client.Write(bytes.Repeat([]byte("A"), 16*maxLineSize))

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("session still open after an overlong line")
	}
	if sent > maxLineSize {
		t.Errorf("server read %d bytes of one line, limit %d", sent, maxLineSize)
	}
	if got := <-out; !strings.Contains(got, "* BAD Line too long") {
		t.Errorf("reply = %q, want BAD Line too long", got)
	}
}

// A line that fits is still returned whole, terminator included.
func TestReadLineReturnsLineWithinLimit(t *testing.T) {
	c := &Connection{reader: bufio.NewReaderSize(strings.NewReader("A1 NOOP\r\nA2"), 16)}
	line, err := c.readLine()
	if err != nil || line != "A1 NOOP\r\n" {
		t.Fatalf("readLine = %q, %v", line, err)
	}
}
