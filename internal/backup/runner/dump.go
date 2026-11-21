package runner

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ErrStalled is a stream that delivered nothing for Config.DumpIdle while
// it was being read: a dump waiting on a lock, or a source that hung.
var ErrStalled = errors.New("runner: the stream delivered nothing for too long")

// dumpBurst is the most a dump reads ahead of its rate.
const dumpBurst = 256 << 10

// dumpStream is a backup's stream: a dump container started on the first
// Read, so a run that fails before it reaches its stream (a wrong
// password) never opens a transaction in the database, and read no faster
// than its rate. With an idle bound, a Read that waits longer than it for
// a byte kills the dump; only time spent inside the source's Read counts,
// so a helper that reads slowly (a slow upload) never trips it.
type dumpStream struct {
	start func() (io.ReadCloser, error)
	lim   *rate.Limiter
	ctx   context.Context
	stop  context.CancelFunc
	idle  time.Duration

	mu      sync.Mutex
	rc      io.ReadCloser
	closed  bool
	watch   *time.Timer
	stalled atomic.Bool
}

func newDumpStream(bytesPerSec int64, idle time.Duration, start func() (io.ReadCloser, error)) *dumpStream {
	ctx, stop := context.WithCancel(context.Background())
	return &dumpStream{
		start: start,
		lim:   rate.NewLimiter(rate.Limit(bytesPerSec), dumpBurst),
		ctx:   ctx,
		stop:  stop,
		idle:  idle,
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
		if d.idle > 0 {
			d.watch = time.AfterFunc(time.Hour, func() {
				d.stalled.Store(true)
				rc.Close()
			})
			d.watch.Stop()
		}
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
	if d.watch != nil {
		d.watch.Reset(d.idle)
	}
	n, err := rc.Read(p)
	if d.watch != nil {
		d.watch.Stop()
	}
	if d.stalled.Load() {
		return 0, ErrStalled
	}
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
	rc, watch := d.rc, d.watch
	already := d.closed
	d.closed = true
	d.mu.Unlock()
	if already {
		return nil
	}
	d.stop()
	if watch != nil {
		watch.Stop()
	}
	if rc != nil {
		return rc.Close()
	}
	return nil
}
