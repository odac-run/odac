package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Event types.
const (
	EventHello    = "hello"
	EventReady    = "ready"
	EventProgress = "progress"
	EventSnapshot = "snapshot"
	EventData     = "data"
	EventResult   = "result"
	EventError    = "error"
)

const (
	// MaxLine bounds one stdout event line.
	MaxLine = 1 << 20
	// dataChunk is the payload of one data event; base64 makes it about a
	// third larger on the line. Data events carry only stream restores,
	// which are rare enough that one line format beats a second framing.
	dataChunk = 192 << 10
	// minSecret is the shortest secret masked: masking a one-letter
	// password would blank that letter out of every message.
	minSecret = 4
)

// Event is one stdout line. Type says which member is set; hello carries
// the version fields.
type Event struct {
	Type        string    `json:"type"`
	Protocol    int       `json:"protocol,omitempty"`
	MinProtocol int       `json:"minProtocol,omitempty"`
	Build       string    `json:"build,omitempty"`
	Progress    *Progress `json:"progress,omitempty"`
	Snapshot    *Snapshot `json:"snapshot,omitempty"`
	Data        []byte    `json:"data,omitempty"`
	Result      *Result   `json:"result,omitempty"`
	Error       *Error    `json:"error,omitempty"`
}

// Emitter writes events for odac-backup. It is safe for concurrent use, so
// a progress ticker can share it with the run.
type Emitter struct {
	mu   sync.Mutex
	w    io.Writer
	mask *strings.Replacer
	err  error
}

// NewEmitter writes events to w.
func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w}
}

// Mask makes every later error message hide these values.
func (e *Emitter) Mask(secrets ...string) {
	var pairs []string
	for _, s := range secrets {
		if len(s) >= minSecret {
			pairs = append(pairs, s, "***")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(pairs) > 0 {
		e.mask = strings.NewReplacer(pairs...)
	}
}

func (e *Emitter) emit(ev *Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("proto: encode event: %w", err)
	}
	b = append(b, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if _, err := e.w.Write(b); err != nil {
		e.err = fmt.Errorf("proto: write event: %w", err)
	}
	return e.err
}

// Hello announces the versions this build serves. It is the first line.
func (e *Emitter) Hello(build string) error {
	return e.emit(&Event{Type: EventHello, Protocol: Version, MinProtocol: MinVersion, Build: build})
}

// Ready asks for the stream: the repository is open and the stream item
// is next. The server starts the dump only then, so a run that fails
// earlier (a wrong password, an unreachable target) never holds a
// transaction open in the database for nothing.
func (e *Emitter) Ready() error {
	return e.emit(&Event{Type: EventReady})
}

// Progress reports work done.
func (e *Emitter) Progress(p Progress) error {
	return e.emit(&Event{Type: EventProgress, Progress: &p})
}

// Snapshot reports one snapshot, stored by this run or listed.
func (e *Emitter) Snapshot(s Snapshot) error {
	return e.emit(&Event{Type: EventSnapshot, Snapshot: &s})
}

// Result closes a successful run.
func (e *Emitter) Result(r Result) error {
	return e.emit(&Event{Type: EventResult, Result: &r})
}

// Error closes a failed run, with secrets masked out of the message.
func (e *Emitter) Error(code, message string) error {
	e.mu.Lock()
	if e.mask != nil {
		message = e.mask.Replace(message)
	}
	e.mu.Unlock()
	return e.emit(&Event{Type: EventError, Error: &Error{Code: code, Message: message}})
}

// DataWriter returns a writer that sends what it is given as data events.
// Close flushes the last partial chunk.
func (e *Emitter) DataWriter() io.WriteCloser {
	return &dataWriter{e: e, buf: make([]byte, 0, dataChunk)}
}

type dataWriter struct {
	e   *Emitter
	buf []byte
}

func (d *dataWriter) Write(p []byte) (int, error) {
	done := 0
	for len(p) > 0 {
		n := min(len(p), dataChunk-len(d.buf))
		d.buf = append(d.buf, p[:n]...)
		p, done = p[n:], done+n
		if len(d.buf) == dataChunk {
			if err := d.flush(); err != nil {
				return done, err
			}
		}
	}
	return done, nil
}

func (d *dataWriter) flush() error {
	if len(d.buf) == 0 {
		return nil
	}
	err := d.e.emit(&Event{Type: EventData, Data: d.buf})
	d.buf = d.buf[:0]
	return err
}

func (d *dataWriter) Close() error { return d.flush() }

// EventReader reads odac-backup's stdout.
type EventReader struct {
	r *bufio.Reader
}

// NewEventReader reads events from r.
func NewEventReader(r io.Reader) *EventReader {
	return &EventReader{r: bufio.NewReaderSize(r, MaxLine)}
}

// Next returns the next event, io.EOF when the output ends between lines.
func (er *EventReader) Next() (*Event, error) {
	line, err := er.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("proto: event line longer than %d bytes", MaxLine)
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("proto: decode event: %w", err)
	}
	return &ev, nil
}
