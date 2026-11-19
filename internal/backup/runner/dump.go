package runner

import (
	"context"
	"io"
	"sync"

	"golang.org/x/time/rate"
)

// dumpBurst is the most a dump reads ahead of its rate.
const dumpBurst = 256 << 10

// dumpStream is a backup's stream: a dump container started on the first
// Read, so a run that fails before it reaches its stream (a wrong
// password) never opens a transaction in the database, and read no faster
// than its rate.
type dumpStream struct {
	start func() (io.ReadCloser, error)
	lim   *rate.Limiter
	ctx   context.Context
	stop  context.CancelFunc

	mu     sync.Mutex
	rc     io.ReadCloser
	closed bool
}

func newDumpStream(bytesPerSec int64, start func() (io.ReadCloser, error)) *dumpStream {
	ctx, stop := context.WithCancel(context.Background())
	return &dumpStream{
		start: start,
		lim:   rate.NewLimiter(rate.Limit(bytesPerSec), dumpBurst),
		ctx:   ctx,
		stop:  stop,
	}
}

func (d *dumpStream) source() (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, io.ErrClosedPipe
	}
	if d.rc == nil {
		rc, err := d.start()
		if err != nil {
			return nil, err
		}
		d.rc = rc
	}
	return d.rc, nil
}

func (d *dumpStream) Read(p []byte) (int, error) {
	rc, err := d.source()
	if err != nil {
		return 0, err
	}
	if len(p) > dumpBurst {
		p = p[:dumpBurst]
	}
	n, err := rc.Read(p)
	if n > 0 {
		if werr := d.lim.WaitN(d.ctx, n); werr != nil && err == nil {
			err = io.ErrClosedPipe
		}
	}
	return n, err
}

// Close kills the dump if it still runs. It is safe to call more than once
// and concurrently with Read, which it unblocks.
func (d *dumpStream) Close() error {
	d.mu.Lock()
	rc := d.rc
	already := d.closed
	d.closed = true
	d.mu.Unlock()
	if already {
		return nil
	}
	d.stop()
	if rc != nil {
		return rc.Close()
	}
	return nil
}
