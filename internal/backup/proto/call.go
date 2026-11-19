package proto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrNoResult is an odac-backup that ended without a result or error
// event: it crashed, ran out of memory or was killed. Whatever a restore
// stream wrote to Handler.Output by then is incomplete.
var ErrNoResult = errors.New("proto: odac-backup ended without a result")

// ErrVersion is an odac-backup that cannot serve this build's Version,
// which happens when an update put the server and the image a protocol
// version apart.
var ErrVersion = errors.New("proto: odac-backup does not speak this protocol version")

// Handler receives what a run produces besides its result.
type Handler struct {
	// Stream is the dump of a backup whose first item is a stream. When
	// the run ends before Stream is drained and Stream is an io.Closer, it
	// is closed, so a dump blocked on its output gives up.
	Stream io.Reader
	// Output receives a stream restore's bytes.
	Output io.Writer
	// Snapshot sees every snapshot a backup stored or a list found, in
	// order. A failed backup may still have stored some.
	Snapshot func(Snapshot)
	// Progress sees progress reports.
	Progress func(Progress)
}

// Call runs one request against an odac-backup whose stdin and stdout are
// given, and owns both: they are closed when Call returns. Canceling ctx
// closes them at once, which ends the run on both sides; the caller still
// reaps the process or container.
func Call(ctx context.Context, stdin io.WriteCloser, stdout io.ReadCloser, req *Request, h Handler) (*Result, error) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			stdin.Close()
			stdout.Close()
		})
	}
	defer closeAll()
	stop := context.AfterFunc(ctx, closeAll)
	defer stop()

	r := *req
	r.Version = Version
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.HasStream() != (h.Stream != nil) {
		return nil, errors.New("proto: a stream item and Handler.Stream go together")
	}
	if r.streamsOut() != (h.Output != nil) {
		return nil, errors.New("proto: a stream restore and Handler.Output go together")
	}

	events := NewEventReader(stdout)
	if err := hello(events); err != nil {
		return nil, ctxErr(ctx, err)
	}
	if err := WriteRequest(stdin, &r); err != nil {
		return nil, ctxErr(ctx, fmt.Errorf("proto: send request: %w", err))
	}

	var (
		sent  chan error
		dump  *recordingReader
		wrote int64
	)
	if h.Stream != nil {
		sent = make(chan error, 1)
		dump = &recordingReader{r: h.Stream}
		go func() {
			sw := NewStreamWriter(stdin)
			_, err := io.Copy(sw, dump)
			if err == nil {
				err = sw.Close()
			}
			// Closing without the end frame is how a failed dump is
			// abandoned: odac-backup sees ErrTruncated and stores nothing.
			stdin.Close()
			sent <- err
		}()
	} else {
		stdin.Close()
	}

	res, runErr := readEvents(events, h, &wrote)
	closeAll()
	if sent != nil {
		select {
		case <-sent:
		default:
			dump.abandon()
			if c, ok := h.Stream.(io.Closer); ok {
				c.Close()
			}
			<-sent
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := dump.failure(); err != nil {
		return nil, fmt.Errorf("proto: read dump: %w", err)
	}
	if runErr != nil {
		return nil, runErr
	}
	if h.Output != nil && res.Bytes != wrote {
		return nil, fmt.Errorf("proto: restore reported %d bytes, %d arrived", res.Bytes, wrote)
	}
	return res, nil
}

func hello(events *EventReader) error {
	ev, err := events.Next()
	if errors.Is(err, io.EOF) {
		return ErrNoResult
	}
	if err != nil {
		return err
	}
	if ev.Type != EventHello {
		return fmt.Errorf("proto: odac-backup opened with %q, not hello", ev.Type)
	}
	if Version < ev.MinProtocol || Version > ev.Protocol {
		return fmt.Errorf("%w: odac-backup %s speaks %d to %d, the server speaks %d", ErrVersion, ev.Build, ev.MinProtocol, ev.Protocol, Version)
	}
	return nil
}

func readEvents(events *EventReader, h Handler, wrote *int64) (*Result, error) {
	for {
		ev, err := events.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoResult
		}
		if err != nil {
			return nil, err
		}
		switch ev.Type {
		case EventProgress:
			if h.Progress != nil && ev.Progress != nil {
				h.Progress(*ev.Progress)
			}
		case EventSnapshot:
			if h.Snapshot != nil && ev.Snapshot != nil {
				h.Snapshot(*ev.Snapshot)
			}
		case EventData:
			if h.Output == nil {
				return nil, errors.New("proto: data event outside a stream restore")
			}
			n, err := h.Output.Write(ev.Data)
			*wrote += int64(n)
			if err != nil {
				return nil, fmt.Errorf("proto: write restore output: %w", err)
			}
		case EventResult:
			if ev.Result == nil {
				return &Result{}, nil
			}
			return ev.Result, nil
		case EventError:
			if ev.Error == nil {
				return nil, &Error{Code: CodeFailed, Message: "error event without details"}
			}
			return nil, ev.Error
		case EventHello:
			return nil, errors.New("proto: second hello")
		}
		// Unknown types come from a newer build and are skipped.
	}
}

// recordingReader remembers a read failure, so a dump that died is
// reported as itself rather than as the truncated stream it caused. What
// fails after abandon is the close Call made, not the dump.
type recordingReader struct {
	r         io.Reader
	mu        sync.Mutex
	abandoned bool
	err       error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.mu.Lock()
		if !r.abandoned && r.err == nil {
			r.err = err
		}
		r.mu.Unlock()
	}
	return n, err
}

func (r *recordingReader) abandon() {
	r.mu.Lock()
	r.abandoned = true
	r.mu.Unlock()
}

func (r *recordingReader) failure() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}
