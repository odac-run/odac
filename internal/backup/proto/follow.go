package proto

import (
	"context"
	"errors"
	"io"
	"sync"
)

// Follow reads a run another server started, from its hello to its result,
// without sending anything: the server that wrote the request has gone
// (an update replaced it) and the helper's output was replayed from its
// log. Only runs without output data can be followed, so h.Stream and
// h.Output must be nil. stdout is closed when Follow returns.
func Follow(ctx context.Context, stdout io.ReadCloser, h Handler) (*Result, error) {
	if h.Stream != nil || h.Output != nil {
		return nil, errors.New("proto: a followed run has no stream and no output")
	}
	var once sync.Once
	closeOut := func() { once.Do(func() { stdout.Close() }) }
	defer closeOut()
	stop := context.AfterFunc(ctx, closeOut)
	defer stop()

	events := NewEventReader(stdout)
	if err := hello(events); err != nil {
		return nil, ctxErr(ctx, err)
	}
	var wrote int64
	res, err := readEvents(events, h, &wrote, nil)
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	return res, err
}
