package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/engine"
	"odac/internal/backup/proto"
)

const (
	testServer = "0123456789abcdef0123456789abcdef"
	testPass   = "correct horse battery staple"
)

type target struct {
	repo, cache string
}

func newTarget(t *testing.T) target {
	base := t.TempDir()
	return target{repo: filepath.Join(base, "repo"), cache: filepath.Join(base, "cache")}
}

func (tg target) request(op string) *proto.Request {
	return &proto.Request{
		Op: op,
		Repo: proto.Repo{
			Storage:  proto.Storage{Filesystem: &proto.FilesystemStorage{Path: tg.repo}},
			Password: testPass,
			ServerID: testServer,
			CacheDir: tg.cache,
		},
	}
}

// call runs serve behind proto.Call, the way the server will run the
// binary, and returns serve's exit code with Call's answer.
func call(t *testing.T, req *proto.Request, h proto.Handler) (*proto.Result, int, error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		code := serve(context.Background(), inR, outW)
		outW.Close()
		inR.Close()
		exit <- code
	}()
	res, err := proto.Call(context.Background(), inW, outR, req, h)
	return res, <-exit, err
}

func mustCall(t *testing.T, req *proto.Request, h proto.Handler) *proto.Result {
	t.Helper()
	res, code, err := call(t, req, h)
	if err != nil || code != 0 {
		t.Fatalf("%s: exit %d, %v", req.Op, code, err)
	}
	return res
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestServeEndToEnd(t *testing.T) {
	t.Parallel()
	tg := newTarget(t)
	if res := mustCall(t, tg.request(proto.OpInit), proto.Handler{}); !res.Created {
		t.Fatal("first init did not create the repository")
	}
	if res := mustCall(t, tg.request(proto.OpInit), proto.Handler{}); res.Created {
		t.Fatal("second init created the repository again")
	}

	dump := randomBytes(t, 3<<20)
	dir := t.TempDir()
	files := map[string][]byte{"a.txt": []byte("alpha"), "sub/b.bin": randomBytes(t, 1<<20)}
	for name, data := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	req := tg.request(proto.OpBackup)
	req.Backup = &proto.Backup{Run: "run-1", Trigger: "schedule", Items: []proto.Item{
		{Source: proto.Source{Job: "nightly", App: "db", Name: "postgres"}, Consistency: engine.Consistent, Stream: "dump.sql"},
		{Source: proto.Source{Job: "nightly", App: "web", Name: "files"}, Consistency: engine.Live, Dir: dir},
	}}
	var stored []proto.Snapshot
	res := mustCall(t, req, proto.Handler{
		Stream:   bytes.NewReader(dump),
		Snapshot: func(s proto.Snapshot) { stored = append(stored, s) },
	})
	if res.Count != 2 || len(stored) != 2 {
		t.Fatalf("backup: result %+v, %d snapshot events", res, len(stored))
	}
	stream, files0 := stored[0], stored[1]
	if stream.Kind != engine.KindStream || stream.Bytes != int64(len(dump)) || stream.Run != "run-1" || stream.Host != testServer {
		t.Fatalf("stream snapshot %+v", stream)
	}
	if files0.Kind != engine.KindDir || files0.Files != 2 || files0.Consistency != engine.Live {
		t.Fatalf("directory snapshot %+v", files0)
	}

	list := tg.request(proto.OpList)
	list.List = &proto.List{Run: "run-1"}
	var listed []proto.Snapshot
	if res := mustCall(t, list, proto.Handler{Snapshot: func(s proto.Snapshot) { listed = append(listed, s) }}); res.Count != 2 || len(listed) != 2 {
		t.Fatalf("list: %+v, %d events", res, len(listed))
	}

	rs := tg.request(proto.OpRestore)
	rs.Restore = &proto.Restore{ID: stream.ID}
	var out bytes.Buffer
	if res := mustCall(t, rs, proto.Handler{Output: &out}); res.Bytes != int64(len(dump)) || !bytes.Equal(out.Bytes(), dump) {
		t.Fatalf("stream restore returned %d bytes, want %d", out.Len(), len(dump))
	}

	restored := filepath.Join(t.TempDir(), "out")
	rs.Restore = &proto.Restore{ID: files0.ID, Dir: restored}
	if res := mustCall(t, rs, proto.Handler{}); res.Files != 2 {
		t.Fatalf("directory restore: %+v", res)
	}
	for name, want := range files {
		if got, _ := os.ReadFile(filepath.Join(restored, name)); !bytes.Equal(got, want) {
			t.Fatalf("%s differs after restore", name)
		}
	}

	if res := mustCall(t, tg.request(proto.OpUsage), proto.Handler{}); res.Bytes < int64(len(dump)) {
		t.Fatalf("usage %d is below the dump alone", res.Bytes)
	}
	m := tg.request(proto.OpMaintain)
	m.Maintain = &proto.Maintain{}
	if res := mustCall(t, m, proto.Handler{}); res.Bytes == 0 {
		t.Fatal("maintain reported no size")
	}
	v := tg.request(proto.OpVerify)
	v.Verify = &proto.Verify{Percent: 100}
	if res := mustCall(t, v, proto.Handler{}); res.Verify == nil || res.Verify.Snapshots != 2 || len(res.Verify.Errors) != 0 {
		t.Fatalf("verify: %+v", res.Verify)
	}

	del := tg.request(proto.OpDelete)
	del.Delete = &proto.Delete{IDs: []string{stream.ID}}
	mustCall(t, del, proto.Handler{})
	listed = nil
	list.List = &proto.List{}
	mustCall(t, list, proto.Handler{Snapshot: func(s proto.Snapshot) { listed = append(listed, s) }})
	if len(listed) != 1 || listed[0].ID != files0.ID {
		t.Fatalf("after delete: %+v", listed)
	}
}

func TestServeErrorCodes(t *testing.T) {
	t.Parallel()
	tg := newTarget(t)
	code := func(req *proto.Request) string {
		t.Helper()
		_, exit, err := call(t, req, proto.Handler{})
		var pe *proto.Error
		if !errors.As(err, &pe) || exit != 1 {
			t.Fatalf("%s: exit %d, %v", req.Op, exit, err)
		}
		if strings.Contains(pe.Message, req.Repo.Password) {
			t.Fatalf("%s: the password reached the message %q", req.Op, pe.Message)
		}
		return pe.Code
	}
	if got := code(tg.request(proto.OpUsage)); got != proto.CodeNotInitialized {
		t.Fatalf("usage before init: %s", got)
	}
	mustCall(t, tg.request(proto.OpInit), proto.Handler{})
	wrong := tg.request(proto.OpUsage)
	wrong.Repo.Password = "not the password, but long enough"
	if got := code(wrong); got != proto.CodeWrongPassword {
		t.Fatalf("wrong password: %s", got)
	}
	rs := tg.request(proto.OpRestore)
	rs.Restore = &proto.Restore{ID: "k0123456789abcdef0123456789abcdef", Dir: filepath.Join(t.TempDir(), "x")}
	if got := code(rs); got != proto.CodeNotFound {
		t.Fatalf("restore of a missing snapshot: %s", got)
	}
	bad := tg.request(proto.OpBackup)
	bad.Backup = &proto.Backup{Run: "r", Trigger: "manual", Items: []proto.Item{{Source: proto.Source{Job: "j", App: "../x", Name: "n"}, Consistency: engine.Live, Dir: t.TempDir()}}}
	if got := code(bad); got != proto.CodeFailed {
		t.Fatalf("invalid source name: %s", got)
	}
}

// serveRaw drives serve with hand-made stdin and returns its events, for
// the cases proto.Call is built to never produce.
func serveRaw(t *testing.T, ctx context.Context, stdin io.Reader) (int, []proto.Event) {
	t.Helper()
	var out bytes.Buffer
	code := serve(ctx, stdin, &out)
	er := proto.NewEventReader(&out)
	var evs []proto.Event
	for {
		ev, err := er.Next()
		if errors.Is(err, io.EOF) {
			return code, evs
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, *ev)
	}
}

func streamRequest(tg target) *proto.Request {
	req := tg.request(proto.OpBackup)
	req.Version = proto.Version
	req.Backup = &proto.Backup{Run: "r", Trigger: "schedule", Items: []proto.Item{
		{Source: proto.Source{Job: "j", App: "db", Name: "postgres"}, Consistency: engine.Consistent, Stream: "dump.sql"},
	}}
	return req
}

func lastError(t *testing.T, evs []proto.Event) string {
	t.Helper()
	if len(evs) == 0 || evs[0].Type != proto.EventHello {
		t.Fatalf("events do not open with hello: %+v", evs)
	}
	last := evs[len(evs)-1]
	if last.Type != proto.EventError {
		t.Fatalf("last event is %q, want error", last.Type)
	}
	return last.Error.Code
}

// A dump cut short (no end frame) stores nothing, even though every byte
// that did arrive was read without error.
func TestServeTruncatedStreamStoresNothing(t *testing.T) {
	t.Parallel()
	tg := newTarget(t)
	mustCall(t, tg.request(proto.OpInit), proto.Handler{})

	var stdin bytes.Buffer
	if err := proto.WriteRequest(&stdin, streamRequest(tg)); err != nil {
		t.Fatal(err)
	}
	proto.NewStreamWriter(&stdin).Write(randomBytes(t, 2<<20))
	code, evs := serveRaw(t, context.Background(), &stdin)
	if got := lastError(t, evs); code != 1 || got != proto.CodeStream {
		t.Fatalf("exit %d, code %s", code, got)
	}

	list := tg.request(proto.OpList)
	list.List = &proto.List{}
	if res := mustCall(t, list, proto.Handler{Snapshot: func(proto.Snapshot) { t.Fatal("a truncated dump was stored") }}); res.Count != 0 {
		t.Fatalf("list after truncated dump: %+v", res)
	}
}

// frames is an endless framed stream that cancels the run once it has
// been read from.
type frames struct {
	hdr    []byte
	cancel func()
	buf    []byte
}

func (f *frames) Read(p []byte) (int, error) {
	if len(f.hdr) > 0 {
		n := copy(p, f.hdr)
		f.hdr = f.hdr[n:]
		return n, nil
	}
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if len(f.buf) == 0 {
		f.buf = binary.BigEndian.AppendUint32(nil, 64<<10)
		f.buf = append(f.buf, make([]byte, 64<<10)...)
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

func TestServeCanceled(t *testing.T) {
	t.Parallel()
	tg := newTarget(t)
	mustCall(t, tg.request(proto.OpInit), proto.Handler{})
	var hdr bytes.Buffer
	proto.WriteRequest(&hdr, streamRequest(tg))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Without cancellation reaching the engine this run never ends.
	code, evs := serveRaw(t, ctx, &frames{hdr: hdr.Bytes(), cancel: cancel})
	if got := lastError(t, evs); code != 1 || got != proto.CodeCanceled {
		t.Fatalf("exit %d, code %s", code, got)
	}
}

// fill sets every field reachable from v to a non-zero value.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(7)
	}
}

// roundTrip fills a value of type A, carries it through B and back, and
// fails if anything was lost on the way: a field on one side only.
func roundTrip[A, B any](t *testing.T) {
	t.Helper()
	var a A
	fill(reflect.ValueOf(&a).Elem())
	back := convert[A](convert[B](a))
	if !reflect.DeepEqual(a, back) {
		t.Errorf("%T does not survive a trip through %T:\n%+v\n%+v", a, *new(B), a, back)
	}
}

func TestWireTypesMatchEngine(t *testing.T) {
	roundTrip[engine.Storage, proto.Storage](t)
	roundTrip[proto.Storage, engine.Storage](t)
	roundTrip[engine.BackupResult, proto.Snapshot](t)
	roundTrip[proto.Snapshot, engine.BackupResult](t)
	roundTrip[engine.VerifyResult, proto.VerifyResult](t)
	roundTrip[proto.VerifyResult, engine.VerifyResult](t)
}
