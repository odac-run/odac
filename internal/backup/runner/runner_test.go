package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/docker"
)

// fakeProc is a helper process in memory: serve plays odac-backup on the
// request it read.
type fakeProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	exit    docker.ExitStatus

	mu       sync.Mutex
	stopped  bool
	removed  bool
	detached bool
}

func newFakeProc(serve func(req *proto.Request, in io.Reader, em *proto.Emitter)) *fakeProc {
	p := &fakeProc{done: make(chan struct{})}
	p.stdinR, p.stdinW = io.Pipe()
	p.stdoutR, p.stdoutW = io.Pipe()
	go func() {
		defer close(p.done)
		defer p.stdoutW.Close()
		em := proto.NewEmitter(p.stdoutW)
		em.Hello("test")
		br := bufio.NewReader(p.stdinR)
		req, err := proto.ReadRequest(br)
		if err != nil {
			em.Error(proto.CodeBadRequest, err.Error())
			return
		}
		serve(req, br, em)
	}()
	return p
}

func (p *fakeProc) Stdin() io.WriteCloser { return p.stdinW }
func (p *fakeProc) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeProc) Stderr() string        { return "" }
func (p *fakeProc) Wait(ctx context.Context) (docker.ExitStatus, error) {
	select {
	case <-p.done:
		return p.exit, nil
	case <-ctx.Done():
		return docker.ExitStatus{}, ctx.Err()
	}
}
func (p *fakeProc) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.stdinR.CloseWithError(errors.New("stopped"))
}
func (p *fakeProc) Remove() {
	p.mu.Lock()
	p.removed = true
	p.mu.Unlock()
	p.stdinR.Close()
	p.stdoutR.Close()
}
func (p *fakeProc) Detach() {
	p.mu.Lock()
	p.detached = true
	p.mu.Unlock()
}

type fakeDocker struct {
	mu         sync.Mutex
	proc       *fakeProc
	specs      []docker.HelperSpec
	dumps      []docker.DumpSpec
	dump       func() io.ReadCloser
	containers []docker.BackupContainer
	removed    []string
	logs       map[string]string
	swept      bool
}

func (d *fakeDocker) SelfImage() (string, error) { return "sha256:self", nil }
func (d *fakeDocker) StartHelper(_ context.Context, spec docker.HelperSpec) (Process, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	return d.proc, nil
}
func (d *fakeDocker) StartDump(_ context.Context, spec docker.DumpSpec) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dumps = append(d.dumps, spec)
	return d.dump(), nil
}
func (d *fakeDocker) BackupContainers() ([]docker.BackupContainer, error) { return d.containers, nil }
func (d *fakeDocker) FollowHelper(_ context.Context, id string) (io.ReadCloser, error) {
	out, ok := d.logs[id]
	if !ok {
		return nil, notFound{}
	}
	return io.NopCloser(strings.NewReader(out)), nil
}
func (d *fakeDocker) WaitBackupContainer(context.Context, string) (docker.ExitStatus, error) {
	return docker.ExitStatus{Code: 137, OOMKilled: true}, nil
}
func (d *fakeDocker) RemoveBackupContainer(b docker.BackupContainer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, b.ID)
}
func (d *fakeDocker) SweepBackupVolumes() { d.swept = true }

type notFound struct{}

func (notFound) Error() string { return "no such container" }
func (notFound) NotFound()     {}

func newRunner(t *testing.T, d *fakeDocker) *Runner {
	t.Helper()
	r, err := New(Config{Docker: d, Binary: "/app/bin/odac-backup", DumpRate: 1 << 30, Limits: docker.Limits{Memory: 1, PidsLimit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func backupRequest() *proto.Request {
	return &proto.Request{
		Op: proto.OpBackup,
		Repo: proto.Repo{
			Storage:  proto.Storage{Filesystem: &proto.FilesystemStorage{Path: "/odac/srv1"}},
			Password: "correct horse battery staple",
			ServerID: "srv1",
			CacheDir: "/app/.odac/backup/cache",
		},
		Backup: &proto.Backup{Run: "r1", Trigger: "schedule", Items: []proto.Item{
			{Source: proto.Source{Job: "j", App: "db", Name: "dump"}, Consistency: "consistent", Stream: "dump.sql"},
			{Source: proto.Source{Job: "j", App: "web", Name: "data"}, Consistency: "live", Dir: "/app/.odac/apps/web/data"},
		}},
	}
}

func TestHelperSpecMapsPaths(t *testing.T) {
	req, spec, err := helperSpec(Invocation{Request: backupRequest(), Target: &Target{Root: "/mnt/disk"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Repo.CacheDir != helperCache || req.Repo.Storage.Filesystem.Path != "/backup/target/odac/srv1" {
		t.Errorf("cache %q, repository %q", req.Repo.CacheDir, req.Repo.Storage.Filesystem.Path)
	}
	if req.Backup.Items[1].Dir != "/backup/src/1" || req.Backup.Items[0].Dir != "" {
		t.Errorf("items %+v", req.Backup.Items)
	}
	want := []docker.HelperMount{
		{Source: "/app/.odac/backup/cache", Target: helperCache},
		{Source: "/mnt/disk", HostSource: true, Target: helperTarget},
		{Source: "/app/.odac/apps/web/data", Target: "/backup/src/1", ReadOnly: true},
	}
	if len(spec.Mounts) != len(want) {
		t.Fatalf("mounts %+v", spec.Mounts)
	}
	for i := range want {
		if spec.Mounts[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, spec.Mounts[i], want[i])
		}
	}
	if spec.Network || !spec.Follow || strings.Join(spec.Caps, ",") != "DAC_OVERRIDE,DAC_READ_SEARCH" {
		t.Errorf("network %v follow %v caps %v", spec.Network, spec.Follow, spec.Caps)
	}
	if backupRequest().Backup.Items[1].Dir != "/app/.odac/apps/web/data" {
		t.Error("the caller's request was rewritten")
	}
}

func TestHelperSpecVariants(t *testing.T) {
	share := backupRequest()
	_, spec, err := helperSpec(Invocation{Request: share, Target: &Target{Share: &docker.Share{Type: "nfs", Addr: "nas", Path: "/x"}}})
	if err != nil || spec.Share == nil || spec.Share.Target != helperTarget {
		t.Fatalf("share: %+v, %v", spec.Share, err)
	}

	s3 := backupRequest()
	s3.Repo.Storage = proto.Storage{S3: &proto.S3Storage{Endpoint: "s3.example", Bucket: "b"}}
	if _, spec, err := helperSpec(Invocation{Request: s3}); err != nil || !spec.Network {
		t.Fatalf("s3: network %v, %v", spec.Network, err)
	}

	restore := backupRequest()
	restore.Op, restore.Backup = proto.OpRestore, nil
	restore.Restore = &proto.Restore{ID: "k1"}
	_, spec, err = helperSpec(Invocation{Request: restore})
	if err != nil || spec.Follow {
		t.Fatalf("a stream restore is followable (its bytes would reach the log): %v", err)
	}
	restore.Restore.Dir = "/app/.odac/apps/web/restore"
	req, spec, err := helperSpec(Invocation{Request: restore})
	if err != nil || req.Restore.Dir != helperRestore || !strings.Contains(strings.Join(spec.Caps, ","), "CHOWN") {
		t.Fatalf("restore dir %q caps %v, %v", req.Restore.Dir, spec.Caps, err)
	}

	bad := backupRequest()
	bad.Backup.Items[1].Dir = "relative"
	if _, _, err := helperSpec(Invocation{Request: bad}); err == nil {
		t.Error("a relative source was mounted")
	}
	if _, _, err := helperSpec(Invocation{Request: backupRequest(), Target: &Target{Root: "/"}}); err == nil {
		t.Error("the host's root was mounted as a target")
	}
}

func TestLocalRequest(t *testing.T) {
	req, err := localRequest(Invocation{Request: backupRequest(), Target: &Target{Root: "/mnt/disk"}})
	if err != nil || req.Repo.Storage.Filesystem.Path != "/mnt/disk/odac/srv1" || req.Repo.CacheDir != "/app/.odac/backup/cache" {
		t.Fatalf("local request %+v, %v", req, err)
	}
	if _, err := localRequest(Invocation{Request: backupRequest(), Target: &Target{Share: &docker.Share{Type: "nfs"}}}); err == nil {
		t.Fatal("a share has no local form")
	}
}

// storeAll reads the stream and reports how many bytes it held.
func storeAll(req *proto.Request, in io.Reader, em *proto.Emitter) {
	em.Ready()
	sr := proto.NewStreamReader(in)
	n, err := io.Copy(io.Discard, sr)
	if err != nil {
		em.Error(proto.CodeStream, err.Error())
		return
	}
	em.Snapshot(proto.Snapshot{ID: "k1", Run: req.Backup.Run, Bytes: n})
	em.Result(proto.Result{Count: 1})
}

type closeTracker struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func (c *closeTracker) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func TestRunStreamsTheDump(t *testing.T) {
	data := bytes.Repeat([]byte("insert into t values (1);\n"), 50000)
	dump := &closeTracker{Reader: bytes.NewReader(data)}
	d := &fakeDocker{proc: newFakeProc(storeAll), dump: func() io.ReadCloser { return dump }}
	r := newRunner(t, d)
	var snaps []proto.Snapshot
	res, err := r.Run(context.Background(), Invocation{
		Request: backupRequest(),
		Dump:    &docker.DumpSpec{Image: "postgres:17", App: "db", Cmd: []string{"pg_dump"}},
		Handler: proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }},
		Labels:  map[string]string{"odac.backup.job": "j"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || len(snaps) != 1 || snaps[0].Bytes != int64(len(data)) {
		t.Fatalf("result %+v snapshots %+v", res, snaps)
	}
	if spec := d.specs[0]; spec.Image != "sha256:self" || spec.Entrypoint[0] != "/app/bin/odac-backup" || spec.Labels[LabelRun] != "r1" || spec.Labels[LabelOp] != proto.OpBackup || spec.Labels["odac.backup.job"] != "j" {
		t.Errorf("helper spec %+v", spec)
	}
	if len(d.dumps) != 1 || d.dumps[0].Labels[LabelRun] != "r1" || d.dumps[0].Limits.Memory == 0 && r.cfg.DumpLimits.Memory != 0 {
		t.Errorf("dumps %+v", d.dumps)
	}
	if !dump.closed || !d.proc.removed {
		t.Errorf("dump closed %v, helper removed %v", dump.closed, d.proc.removed)
	}
}

// A run that fails before its stream (a wrong password) never starts the
// dump, so no transaction is opened in the database for nothing.
func TestRunNoDumpWhenTheHelperRefuses(t *testing.T) {
	d := &fakeDocker{
		proc: newFakeProc(func(_ *proto.Request, _ io.Reader, em *proto.Emitter) {
			em.Error(proto.CodeWrongPassword, "wrong password")
		}),
		dump: func() io.ReadCloser { t.Error("the dump was started"); return io.NopCloser(strings.NewReader("")) },
	}
	_, err := newRunner(t, d).Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"pg_dump"}}})
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeWrongPassword {
		t.Fatalf("Run = %v", err)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n > 1<<20 {
		return 0, errors.New("dump exit 1: pg_dump: error: connection to server lost")
	}
	clear(p)
	f.n += len(p)
	return len(p), nil
}

func TestRunDumpFailure(t *testing.T) {
	var helperSaw error
	d := &fakeDocker{
		proc: newFakeProc(func(_ *proto.Request, in io.Reader, em *proto.Emitter) {
			em.Ready()
			sr := proto.NewStreamReader(in)
			io.Copy(io.Discard, sr)
			helperSaw = sr.Err()
			em.Error(proto.CodeStream, "truncated")
		}),
		dump: func() io.ReadCloser { return io.NopCloser(&failingReader{}) },
	}
	_, err := newRunner(t, d).Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"pg_dump"}}})
	if err == nil || !strings.Contains(err.Error(), "connection to server lost") {
		t.Fatalf("Run = %v, want the dump's own failure", err)
	}
	<-d.proc.done
	if !errors.Is(helperSaw, proto.ErrTruncated) {
		t.Fatalf("the helper saw %v, not a truncated stream", helperSaw)
	}
}

// endless blocks until closed, like a dump stuck on its output.
type endless struct {
	once   sync.Once
	closed chan struct{}
}

func (e *endless) Read(p []byte) (int, error) {
	select {
	case <-e.closed:
		return 0, io.ErrClosedPipe
	default:
		clear(p)
		return len(p), nil
	}
}

func (e *endless) Close() error {
	e.once.Do(func() { close(e.closed) })
	return nil
}

func TestRunReleaseHandsOver(t *testing.T) {
	dump := &endless{closed: make(chan struct{})}
	d := &fakeDocker{proc: newFakeProc(storeAll), dump: func() io.ReadCloser { return dump }}
	r := newRunner(t, d)
	time.AfterFunc(50*time.Millisecond, r.Release)
	_, err := r.Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"pg_dump"}}})
	if !errors.Is(err, ErrReleased) {
		t.Fatalf("Run = %v, want ErrReleased", err)
	}
	if !d.proc.detached || d.proc.removed || d.proc.stopped {
		t.Errorf("detached %v removed %v stopped %v", d.proc.detached, d.proc.removed, d.proc.stopped)
	}
	select {
	case <-dump.closed:
	default:
		t.Error("the dump outlived the server reading it")
	}
	if _, err := r.Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"x"}}}); !errors.Is(err, ErrReleased) {
		t.Errorf("a run after Release = %v", err)
	}
}

func TestRunCancelStopsTheHelper(t *testing.T) {
	dump := &endless{closed: make(chan struct{})}
	d := &fakeDocker{proc: newFakeProc(storeAll), dump: func() io.ReadCloser { return dump }}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := newRunner(t, d).Run(ctx, Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"pg_dump"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if !d.proc.stopped || !d.proc.removed || d.proc.detached {
		t.Errorf("stopped %v removed %v detached %v", d.proc.stopped, d.proc.removed, d.proc.detached)
	}
}

func TestRunExplainsAKilledHelper(t *testing.T) {
	p := newFakeProc(func(*proto.Request, io.Reader, *proto.Emitter) {})
	p.exit = docker.ExitStatus{Code: 137, OOMKilled: true}
	req := backupRequest()
	req.Backup.Items = req.Backup.Items[1:]
	_, err := newRunner(t, &fakeDocker{proc: p}).Run(context.Background(), Invocation{Request: req})
	if !errors.Is(err, proto.ErrNoResult) || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("Run = %v", err)
	}
}

func events(lines ...func(*proto.Emitter)) string {
	var b bytes.Buffer
	em := proto.NewEmitter(&b)
	em.Hello("old")
	for _, l := range lines {
		l(em)
	}
	return b.String()
}

func TestAdopt(t *testing.T) {
	follow := map[string]string{docker.LabelBackupRole: docker.RoleHelper, docker.LabelBackupFollow: "true", LabelRun: "r1"}
	d := &fakeDocker{
		containers: []docker.BackupContainer{
			{ID: "h1", Role: docker.RoleHelper, Labels: follow},
			{ID: "h2", Role: docker.RoleHelper, Labels: follow},
			{ID: "h3", Role: docker.RoleHelper, Labels: follow},
			{ID: "d1", Role: docker.RoleDump, Labels: map[string]string{docker.LabelBackupRole: docker.RoleDump}},
			{ID: "h4", Role: docker.RoleHelper, Labels: map[string]string{docker.LabelBackupRole: docker.RoleHelper}},
		},
		logs: map[string]string{
			"h1": events(func(em *proto.Emitter) { em.Snapshot(proto.Snapshot{ID: "k1"}) }, func(em *proto.Emitter) { em.Result(proto.Result{Count: 1}) }),
			"h2": events(),
		},
	}
	r := newRunner(t, d)
	orphans, err := r.Adopt()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 3 || strings.Join(d.removed, ",") != "d1,h4" || !d.swept {
		t.Fatalf("orphans %d, removed %v, swept %v", len(orphans), d.removed, d.swept)
	}
	var snaps []proto.Snapshot
	res, err := orphans[0].Follow(context.Background(), proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }})
	if err != nil || res.Count != 1 || len(snaps) != 1 || orphans[0].Labels()[LabelRun] != "r1" {
		t.Fatalf("Follow = %+v, %v, %v", res, err, snaps)
	}
	if _, err := orphans[1].Follow(context.Background(), proto.Handler{}); !errors.Is(err, proto.ErrNoResult) || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("a helper that died = %v", err)
	}
	if _, err := orphans[2].Follow(context.Background(), proto.Handler{}); !errors.Is(err, ErrGone) {
		t.Fatalf("a helper another server removed = %v", err)
	}
	if strings.Join(d.removed, ",") != "d1,h4,h1,h2" {
		t.Fatalf("removed %v", d.removed)
	}
}

func TestDumpStreamRate(t *testing.T) {
	const size, rate = 2 << 20, 4 << 20
	ds := newDumpStream(rate, 0, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(make([]byte, size))), nil
	})
	defer ds.Close()
	start := time.Now()
	n, err := io.Copy(io.Discard, ds)
	took := time.Since(start)
	if err != nil || n != size {
		t.Fatalf("copied %d, %v", n, err)
	}
	// The first burst is free; the rest is paid at the rate.
	if min := time.Duration(float64(size-dumpBurst) / rate * float64(time.Second)); took < min*9/10 {
		t.Fatalf("%d bytes at %d/s took %v, want at least %v", size, rate, took, min)
	}
}

func TestDefaultLimits(t *testing.T) {
	h, d := DefaultLimits(64<<30, 16)
	if h.Memory != 2<<30 || h.NanoCPUs != 8e9 || d.Memory != 1<<30 {
		t.Errorf("large host: helper %+v dump %+v", h, d)
	}
	h, d = DefaultLimits(1<<30, 1)
	if h.Memory != 512<<20 || h.NanoCPUs != 1e9 || d.Memory != 256<<20 {
		t.Errorf("small host: helper %+v dump %+v", h, d)
	}
}

// hangingReader gives its first bytes, then blocks until it is closed.
type hangingReader struct {
	first []byte
	once  sync.Once
	done  chan struct{}
}

func (h *hangingReader) Read(p []byte) (int, error) {
	if len(h.first) > 0 {
		n := copy(p, h.first)
		h.first = h.first[n:]
		return n, nil
	}
	<-h.done
	return 0, io.ErrClosedPipe
}

func (h *hangingReader) Close() error {
	h.once.Do(func() { close(h.done) })
	return nil
}

func TestRunStalledDumpFails(t *testing.T) {
	hang := &hangingReader{first: []byte("partial"), done: make(chan struct{})}
	d := &fakeDocker{proc: newFakeProc(storeAll), dump: func() io.ReadCloser { return hang }}
	r := newRunner(t, d)
	r.cfg.DumpIdle = 50 * time.Millisecond
	start := time.Now()
	_, err := r.Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{Image: "p", App: "db", Cmd: []string{"mariadb-dump"}}})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("stalled dump: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a stalled dump took %v to end", took)
	}
}

// A slow reader is not a stalled dump: only waiting inside the dump's Read
// counts.
func TestDumpIdleIgnoresASlowReader(t *testing.T) {
	ds := newDumpStream(1<<30, 30*time.Millisecond, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(make([]byte, 3*dumpBurst))), nil
	})
	defer ds.Close()
	buf := make([]byte, dumpBurst)
	for {
		_, err := ds.Read(buf)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
}

func TestRunReadsASource(t *testing.T) {
	data := []byte("stored dump")
	d := &fakeDocker{proc: newFakeProc(storeAll), dump: func() io.ReadCloser { t.Error("a dump was started"); return nil }}
	src := &closeTracker{Reader: bytes.NewReader(data)}
	var snaps []proto.Snapshot
	_, err := newRunner(t, d).Run(context.Background(), Invocation{
		Request: backupRequest(),
		Source:  func(context.Context) (io.ReadCloser, error) { return src, nil },
		Handler: proto.Handler{Snapshot: func(s proto.Snapshot) { snaps = append(snaps, s) }},
	})
	if err != nil || len(snaps) != 1 || snaps[0].Bytes != int64(len(data)) || !src.closed {
		t.Fatalf("source run: %v %+v closed %v", err, snaps, src.closed)
	}
	_, err = newRunner(t, d).Run(context.Background(), Invocation{Request: backupRequest(), Dump: &docker.DumpSpec{}, Source: func(context.Context) (io.ReadCloser, error) { return src, nil }})
	if err == nil {
		t.Fatal("a dump and a source together were accepted")
	}
}
