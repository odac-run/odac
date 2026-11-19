package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Size limits. Every length read off the wire is checked against one, so a
// corrupt or hostile prefix costs an error, not an allocation.
const (
	// MaxHeader bounds the request header (a delete of many ids is the
	// largest).
	MaxHeader = 4 << 20
	// MaxChunk bounds one stdin data frame.
	MaxChunk = 1 << 20
)

// ErrTruncated is a stream that ended without its end frame: the dump died
// or the server went away. Such a stream must store nothing, which is why
// the end is marked rather than read off EOF.
var ErrTruncated = errors.New("proto: stream ended before its end frame")

// WriteRequest writes req as a length-prefixed header.
func WriteRequest(w io.Writer, req *Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("proto: encode request: %w", err)
	}
	if len(b) > MaxHeader {
		return fmt.Errorf("proto: request header is %d bytes, limit %d", len(b), MaxHeader)
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadRequest reads and validates the header. A failure is always an
// *Error, ready to be sent back as the run's error event.
func ReadRequest(r io.Reader) (*Request, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, badRequest("read header: %v", err)
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxHeader {
		return nil, badRequest("header length %d outside 1-%d", size, MaxHeader)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, badRequest("read header: %v", err)
	}
	var req Request
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, badRequest("decode header: %v", err)
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// StreamWriter frames a stream for stdin. Close writes the end frame; a
// stream given up on is simply never closed, so the reader sees
// ErrTruncated.
type StreamWriter struct {
	w   io.Writer
	buf []byte
}

// NewStreamWriter frames writes to w.
func NewStreamWriter(w io.Writer) *StreamWriter {
	return &StreamWriter{w: w}
}

func (s *StreamWriter) frame(n int) []byte {
	if cap(s.buf) < 4+n {
		s.buf = make([]byte, 4+n)
	}
	return s.buf[:4+n]
}

// Write sends p as one or more frames.
func (s *StreamWriter) Write(p []byte) (int, error) {
	done := 0
	for len(p) > 0 {
		n := min(len(p), MaxChunk)
		f := s.frame(n)
		binary.BigEndian.PutUint32(f, uint32(n))
		copy(f[4:], p[:n])
		if _, err := s.w.Write(f); err != nil {
			return done, err
		}
		p, done = p[n:], done+n
	}
	return done, nil
}

// ReadFrom reads straight into the frame buffer, so io.Copy costs one
// write per chunk and no extra copy.
func (s *StreamWriter) ReadFrom(r io.Reader) (int64, error) {
	const chunk = 256 << 10
	var total int64
	f := s.frame(chunk)
	for {
		n, err := r.Read(f[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(f, uint32(n))
			if _, werr := s.w.Write(f[:4+n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Close writes the end frame. It does not close the underlying writer.
func (s *StreamWriter) Close() error {
	_, err := s.w.Write([]byte{0, 0, 0, 0})
	return err
}

// StreamReader reads a framed stream and returns io.EOF only at the end
// frame.
type StreamReader struct {
	r    io.Reader
	left uint32
	err  error
}

// NewStreamReader reads frames from r.
func NewStreamReader(r io.Reader) *StreamReader {
	return &StreamReader{r: r}
}

func (s *StreamReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.left == 0 {
		var n [4]byte
		if _, err := io.ReadFull(s.r, n[:]); err != nil {
			return 0, s.fail(err)
		}
		size := binary.BigEndian.Uint32(n[:])
		if size == 0 {
			s.err = io.EOF
			return 0, io.EOF
		}
		if size > MaxChunk {
			s.err = fmt.Errorf("proto: stream frame of %d bytes, limit %d", size, MaxChunk)
			return 0, s.err
		}
		s.left = size
	}
	n, err := s.r.Read(p[:min(len(p), int(s.left))])
	s.left -= uint32(n)
	if err != nil && (s.left > 0 || !errors.Is(err, io.EOF)) {
		return n, s.fail(err)
	}
	return n, nil
}

func (s *StreamReader) fail(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		s.err = ErrTruncated
	} else {
		s.err = fmt.Errorf("proto: read stream: %w", err)
	}
	return s.err
}

// Err is the error that stopped the stream, nil while it reads and after
// its end frame. The engine reports a reader's failure only as text, so
// this is how the run tells a cut stream from a storage failure.
func (s *StreamReader) Err() error {
	if errors.Is(s.err, io.EOF) {
		return nil
	}
	return s.err
}

// Done reports whether the end frame was read.
func (s *StreamReader) Done() bool { return errors.Is(s.err, io.EOF) }
