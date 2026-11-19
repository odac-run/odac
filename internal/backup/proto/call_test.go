package proto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper is the odac-backup side of a test: serve runs on the request it
// read, with stdin positioned after the header.
type helper struct {
	hello func(*Emitter)
	serve func(req *Request, stdin io.Reader, em *Emitter)
}

type pipes struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
}

// start runs h against fresh pipes and returns the server's ends. The
// helper goroutine is waited for when the test ends.
func start(t *testing.T, h helper) (io.WriteCloser, io.ReadCloser) {
	t.Helper()
	p := pipes{}
	p.stdinR, p.stdinW = io.Pipe()
	p.stdoutR, p.stdoutW = io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer p.stdoutW.Close()
		em := NewEmitter(p.stdoutW)
		if h.hello != nil {
			h.hello(em)
		} else {
			em.Hello("test")
		}
		br := bufio.NewReader(p.stdinR)
		req, err := ReadRequest(br)
		if err != nil {
			var e *Error
			errors.As(err, &e)
			em.Error(e.Code, e.Message)
			return
		}
		em.Mask(req.Secrets()...)
		h.serve(req, br, em)
	}()
	t.Cleanup(func() {
		p.stdinR.Close()
		p.stdoutW.Close()
		wg.Wait()
	})
	return p.stdinW, p.stdoutR
}

// storeStream is a helper that reads the whole stream and reports it as a
// snapshot whose Bytes is the stream's length.
func storeStream(req *Request, stdin io.Reader, em *Emitter) {
	em.Ready()
	sr := NewStreamReader(stdin)
	n, err := io.Copy(io.Discard, sr)
	if err != nil {
		code := CodeFailed
		if errors.Is(sr.Err(), ErrTruncated) {
			code = CodeStream
		}
		em.Error(code, err.Error())
		return
	}
	em.Progress(Progress{Item: 0, Bytes: n})
	em.Snapshot(Snapshot{ID: "k1", Source: req.Backup.Items[0].Source, Run: req.Backup.Run, Bytes: n})
	em.Result(Result{Count: 1})
}

func TestCallBackupStream(t *testing.T) {
	stdin, stdout := start(t, helper{serve: storeStream})
	data := randomData(rand.New(rand.NewPCG(3, 4)), 3*MaxChunk+5)
	var snaps []Snapshot
	var progress []Progress
	req := validRequest()
	req.Backup.Items = req.Backup.Items[:1]
	res, err := Call(context.Background(), stdin, stdout, req, Handler{
		Stream:   bytes.NewReader(data),
		Snapshot: func(s Snapshot) { snaps = append(snaps, s) },
		Progress: func(p Progress) { progress = append(progress, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || len(snaps) != 1 || snaps[0].Bytes != int64(len(data)) || snaps[0].Run != "r1" {
		t.Fatalf("result %+v, snapshots %+v", res, snaps)
	}
	if len(progress) != 1 || progress[0].Bytes != int64(len(data)) {
		t.Fatalf("progress %+v", progress)
	}
}

type failingDump struct {
	sent int
}

func (d *failingDump) Read(p []byte) (int, error) {
	if d.sent > 2*MaxChunk {
		return 0, errors.New("pg_dump exited 1")
	}
	n := min(len(p), 4096)
	clear(p[:n])
	d.sent += n
	return n, nil
}

// A dump that dies is abandoned without its end frame; the helper stores
// nothing and the caller learns about the dump, not the stream.
func TestCallDumpFailure(t *testing.T) {
	var helperCode string
	var mu sync.Mutex
	stdin, stdout := start(t, helper{serve: func(req *Request, in io.Reader, em *Emitter) {
		em.Ready()
		sr := NewStreamReader(in)
		io.Copy(io.Discard, sr)
		mu.Lock()
		if errors.Is(sr.Err(), ErrTruncated) {
			helperCode = CodeStream
		}
		mu.Unlock()
		em.Error(CodeStream, sr.Err().Error())
	}})
	req := validRequest()
	req.Backup.Items = req.Backup.Items[:1]
	_, err := Call(context.Background(), stdin, stdout, req, Handler{Stream: &failingDump{}})
	if err == nil || !strings.Contains(err.Error(), "pg_dump exited 1") {
		t.Fatalf("Call = %v, want the dump's own error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if helperCode != CodeStream {
		t.Fatal("the helper did not see a truncated stream")
	}
}

// endlessDump never ends on its own; Close is what stops it.
type endlessDump struct {
	closed chan struct{}
	once   sync.Once
}

func (d *endlessDump) Read(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	n := min(len(p), 4096)
	clear(p[:n])
	return n, nil
}

func (d *endlessDump) Close() error {
	d.once.Do(func() { close(d.closed) })
	return nil
}

// The helper refuses before reading the stream (a wrong password): Call
// returns its error rather than waiting for a dump that never ends.
func TestCallHelperFailsBeforeTheStream(t *testing.T) {
	stdin, stdout := start(t, helper{serve: func(req *Request, in io.Reader, em *Emitter) {
		em.Error(CodeWrongPassword, "wrong repository password "+req.Repo.Password)
	}})
	req := validRequest()
	req.Backup.Items = req.Backup.Items[:1]
	_, err := Call(context.Background(), stdin, stdout, req, Handler{Stream: &endlessDump{closed: make(chan struct{})}})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeWrongPassword {
		t.Fatalf("Call = %v", err)
	}
	if strings.Contains(e.Message, req.Repo.Password) {
		t.Fatalf("the password reached the error message: %q", e.Message)
	}
}

func TestCallVersionMismatch(t *testing.T) {
	sawHeader := make(chan bool, 1)
	stdin, stdout := start(t, helper{
		hello: func(em *Emitter) {
			em.emit(&Event{Type: EventHello, Protocol: Version + 3, MinProtocol: Version + 2, Build: "9.9.9"})
		},
		serve: func(*Request, io.Reader, *Emitter) { sawHeader <- true },
	})
	_, err := Call(context.Background(), stdin, stdout, validInit(), Handler{})
	if !errors.Is(err, ErrVersion) || !strings.Contains(err.Error(), "9.9.9") {
		t.Fatalf("Call = %v, want ErrVersion naming the build", err)
	}
	select {
	case <-sawHeader:
		t.Fatal("the request was sent to a helper that cannot read it")
	default:
	}
}

func validInit() *Request {
	r := validRequest()
	r.Op, r.Backup = OpInit, nil
	return r
}

func TestCallHelperDies(t *testing.T) {
	for name, serve := range map[string]func(*Request, io.Reader, *Emitter){
		"no result":    func(_ *Request, _ io.Reader, em *Emitter) { em.Progress(Progress{}) },
		"partial line": func(_ *Request, _ io.Reader, em *Emitter) { em.w.Write([]byte(`{"type":"res`)) },
	} {
		stdin, stdout := start(t, helper{serve: serve})
		if _, err := Call(context.Background(), stdin, stdout, validInit(), Handler{}); err == nil {
			t.Errorf("%s: Call succeeded", name)
		} else if name == "no result" && !errors.Is(err, ErrNoResult) {
			t.Errorf("%s: %v, want ErrNoResult", name, err)
		}
	}
}

func TestCallCancel(t *testing.T) {
	block := make(chan struct{})
	stdin, stdout := start(t, helper{serve: func(*Request, io.Reader, *Emitter) { <-block }})
	// Registered after start, so it runs before start waits for the helper.
	t.Cleanup(func() { close(block) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := Call(ctx, stdin, stdout, validInit(), Handler{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return after cancel")
	}
}

func restoreRequest() *Request {
	r := validRequest()
	r.Op, r.Backup, r.Restore = OpRestore, nil, &Restore{ID: "k1"}
	return r
}

func TestCallRestoreStream(t *testing.T) {
	data := randomData(rand.New(rand.NewPCG(5, 6)), 5*dataChunk+11)
	for _, claim := range []int64{int64(len(data)), int64(len(data)) + 1} {
		stdin, stdout := start(t, helper{serve: func(_ *Request, _ io.Reader, em *Emitter) {
			w := em.DataWriter()
			io.Copy(w, bytes.NewReader(data))
			w.Close()
			em.emit(&Event{Type: "future-kind"})
			em.Result(Result{Bytes: claim})
		}})
		var out bytes.Buffer
		res, err := Call(context.Background(), stdin, stdout, restoreRequest(), Handler{Output: &out})
		if claim != int64(len(data)) {
			if err == nil {
				t.Fatal("a restore that lost bytes succeeded")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if res.Bytes != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("restored %d bytes, want %d", out.Len(), len(data))
		}
	}
}

func TestCallRequiresMatchingHandler(t *testing.T) {
	req := validRequest()
	for name, c := range map[string]struct {
		req *Request
		h   Handler
	}{
		"stream item without Stream": {req, Handler{}},
		"Stream without stream item": {validInit(), Handler{Stream: strings.NewReader("x")}},
		"restore without Output":     {restoreRequest(), Handler{}},
	} {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		if _, err := Call(context.Background(), inW, outR, c.req, c.h); err == nil {
			t.Errorf("%s: accepted", name)
		}
		inR.Close()
		outW.Close()
	}
}

func TestEventLineLimit(t *testing.T) {
	long := `{"type":"result","result":{"count":1}}` + strings.Repeat(" ", MaxLine) + "\n"
	er := NewEventReader(strings.NewReader(long))
	if _, err := er.Next(); err == nil {
		t.Fatal("an over-long line was accepted")
	}
	er = NewEventReader(strings.NewReader(`{"type":"result"}` + "\n"))
	if ev, err := er.Next(); err != nil || ev.Type != EventResult {
		t.Fatalf("Next = %+v, %v", ev, err)
	}
	if _, err := er.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of output = %v", err)
	}
}

func TestEmitterMasksSecrets(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(&out)
	em.Mask("supersecretvalue", "abc", "")
	em.Error(CodeFailed, fmt.Sprintf("dial s3: signature for %s rejected, abc", "supersecretvalue"))
	if strings.Contains(out.String(), "supersecretvalue") || !strings.Contains(out.String(), "abc") {
		t.Fatalf("masked output: %s", out.String())
	}
}

// A run replayed from a helper's log: the follower never writes, and sees
// the snapshots stored before the result.
func TestFollow(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(&out)
	em.Hello("test")
	em.Snapshot(Snapshot{ID: "k1", Run: "r1"})
	em.Result(Result{Count: 1})
	var snaps []Snapshot
	res, err := Follow(context.Background(), io.NopCloser(&out), Handler{Snapshot: func(s Snapshot) { snaps = append(snaps, s) }})
	if err != nil || res.Count != 1 || len(snaps) != 1 {
		t.Fatalf("Follow = %+v, %v, snapshots %v", res, err, snaps)
	}

	out.Reset()
	em.Hello("test")
	em.Progress(Progress{})
	if _, err := Follow(context.Background(), io.NopCloser(&out), Handler{}); !errors.Is(err, ErrNoResult) {
		t.Fatalf("a log that ends early = %v, want ErrNoResult", err)
	}
	if _, err := Follow(context.Background(), io.NopCloser(&out), Handler{Output: io.Discard}); err == nil {
		t.Fatal("a restore was followed")
	}
}

type watchedDump struct {
	endlessDump
	mu   sync.Mutex
	read bool
}

func (d *watchedDump) Read(p []byte) (int, error) {
	d.mu.Lock()
	d.read = true
	d.mu.Unlock()
	return d.endlessDump.Read(p)
}

// The stream is not touched before the helper asks for it: a dump that
// starts on its first read never starts for a run that fails first.
func TestCallStreamWaitsForReady(t *testing.T) {
	stdin, stdout := start(t, helper{serve: func(_ *Request, _ io.Reader, em *Emitter) {
		time.Sleep(50 * time.Millisecond)
		em.Error(CodeWrongPassword, "wrong password")
	}})
	req := validRequest()
	req.Backup.Items = req.Backup.Items[:1]
	dump := &watchedDump{endlessDump: endlessDump{closed: make(chan struct{})}}
	if _, err := Call(context.Background(), stdin, stdout, req, Handler{Stream: dump}); err == nil {
		t.Fatal("Call succeeded")
	}
	dump.mu.Lock()
	defer dump.mu.Unlock()
	if dump.read {
		t.Fatal("the stream was read before the helper was ready")
	}
	select {
	case <-dump.closed:
	default:
		t.Fatal("the unused stream was not closed")
	}

	stdin, stdout = start(t, helper{serve: func(_ *Request, _ io.Reader, em *Emitter) { em.Ready() }})
	if _, err := Call(context.Background(), stdin, stdout, validInit(), Handler{}); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("a ready without a stream item = %v", err)
	}
}
