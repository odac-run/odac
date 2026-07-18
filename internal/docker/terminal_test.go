package docker

import (
	"bufio"
	"context"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"odac/internal/logx"
)

// termFakeAPI overrides the exec surface of fakeAPI with an interactive TTY
// model (jest's createContainerMock): the Tty exec is the session, untagged
// execs are reap sweeps whose scripts are captured.
type termFakeAPI struct {
	*fakeAPI

	tmu           sync.Mutex
	execOptions   []container.ExecOptions
	attachOptions []container.ExecAttachOptions
	reapScripts   []string
	resizes       []container.ResizeOptions

	// runningSeq answers ContainerExecInspect for the SESSION exec: shift
	// while more than one remains, then repeat the last (jest semantics).
	runningSeq []bool
	exitCode   int

	sessionServer net.Conn // write = pty output, read = pty input

	execCreateErr error
	resizeErr     error
}

func newTermFakeAPI() *termFakeAPI {
	return &termFakeAPI{fakeAPI: newFakeAPI(), runningSeq: []bool{false}}
}

func (f *termFakeAPI) ContainerExecCreate(_ context.Context, _ string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.execCreateErr != nil {
		return container.ExecCreateResponse{}, f.execCreateErr
	}
	f.execOptions = append(f.execOptions, options)
	if options.Tty {
		return container.ExecCreateResponse{ID: "session"}, nil
	}
	f.reapScripts = append(f.reapScripts, options.Cmd[2])
	return container.ExecCreateResponse{ID: "reap"}, nil
}

func (f *termFakeAPI) ContainerExecAttach(_ context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	f.attachOptions = append(f.attachOptions, config)
	if execID == "session" {
		server, client := net.Pipe()
		f.sessionServer = server
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	// Reap exec: drain hits EOF immediately.
	server, client := net.Pipe()
	server.Close()
	return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
}

func (f *termFakeAPI) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	running := f.runningSeq[0]
	if len(f.runningSeq) > 1 {
		f.runningSeq = f.runningSeq[1:]
	}
	inspect := container.ExecInspect{Running: running}
	if !running {
		inspect.ExitCode = f.exitCode
	}
	return inspect, nil
}

func (f *termFakeAPI) ContainerExecResize(_ context.Context, _ string, options container.ResizeOptions) error {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	if f.resizeErr != nil {
		err := f.resizeErr
		f.resizeErr = nil
		return err
	}
	f.resizes = append(f.resizes, options)
	return nil
}

func (f *termFakeAPI) sessionOptions(t *testing.T) container.ExecOptions {
	t.Helper()
	f.tmu.Lock()
	defer f.tmu.Unlock()
	for _, o := range f.execOptions {
		if o.Tty {
			return o
		}
	}
	t.Fatal("no session exec created")
	return container.ExecOptions{}
}

func (f *termFakeAPI) reapOptions(t *testing.T) container.ExecOptions {
	t.Helper()
	f.tmu.Lock()
	defer f.tmu.Unlock()
	for _, o := range f.execOptions {
		if !o.Tty {
			return o
		}
	}
	t.Fatal("no reap exec created")
	return container.ExecOptions{}
}

func (f *termFakeAPI) scripts() []string {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	return append([]string(nil), f.reapScripts...)
}

// exitRecorder collects onExit invocations.
type exitRecorder struct {
	mu    sync.Mutex
	calls []ExitInfo
}

func (r *exitRecorder) record(info ExitInfo) {
	r.mu.Lock()
	r.calls = append(r.calls, info)
	r.mu.Unlock()
}

func (r *exitRecorder) list() []ExitInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ExitInfo(nil), r.calls...)
}

func durPtr(d time.Duration) *time.Duration { return &d }

// openTerminal opens a session with the timers off (a stranded 15-minute
// timer would outlive the test) and a fast reap grace.
func openTerminal(t *testing.T, f *termFakeAPI, options TerminalOptions) *Terminal {
	t.Helper()
	if options.IdleTimeout == nil {
		options.IdleTimeout = durPtr(0)
	}
	if options.MaxLifetime == nil {
		options.MaxLifetime = durPtr(0)
	}
	term := newTerminal(f, logx.New("Container", "Terminal"), "web", options)
	term.reapGrace = 5 * time.Millisecond
	if err := term.open(); err != nil {
		t.Fatal(err)
	}
	return term
}

func TestTerminalOpenCreatesTaggedTTYExec(t *testing.T) {
	f := newTermFakeAPI()
	term := openTerminal(t, f, TerminalOptions{Cols: 120, Rows: 40})
	defer term.Close("closed")

	options := f.sessionOptions(t)
	if !options.Tty || !options.AttachStdin || !options.AttachStdout || !options.AttachStderr {
		t.Errorf("attach flags: %+v", options)
	}
	// [h, w] — sizing at create avoids a first paint at 0x0.
	if options.ConsoleSize == nil || *options.ConsoleSize != [2]uint{40, 120} {
		t.Errorf("ConsoleSize = %v", options.ConsoleSize)
	}
	if len(options.Env) != 1 || !regexp.MustCompile(`^ODAC_TTY_SESSION=[a-f0-9]{32}$`).MatchString(options.Env[0]) {
		t.Errorf("Env = %v", options.Env)
	}
	if strings.Join(options.Cmd, " ") != strings.Join(DefaultShell, " ") {
		t.Errorf("Cmd = %v", options.Cmd)
	}

	// Tty must be repeated on attach or the daemon streams multiplexed
	// frames.
	f.tmu.Lock()
	attach := f.attachOptions[0]
	f.tmu.Unlock()
	if !attach.Tty {
		t.Error("Tty not repeated in exec attach")
	}
}

func TestTerminalOptionsPassThrough(t *testing.T) {
	f := newTermFakeAPI()
	term := openTerminal(t, f, TerminalOptions{
		Command: []string{"/bin/zsh"},
		User:    "app",
		Workdir: "/srv",
		Env:     []string{"FOO=bar"},
	})
	defer term.Close("closed")

	options := f.sessionOptions(t)
	if strings.Join(options.Cmd, ",") != "/bin/zsh" {
		t.Errorf("Cmd = %v", options.Cmd)
	}
	if options.User != "app" || options.WorkingDir != "/srv" {
		t.Errorf("User/WorkingDir = %s/%s", options.User, options.WorkingDir)
	}
	if len(options.Env) != 2 || !strings.HasPrefix(options.Env[0], "ODAC_TTY_SESSION=") || options.Env[1] != "FOO=bar" {
		t.Errorf("Env = %v", options.Env)
	}
}

func TestTerminalIO(t *testing.T) {
	f := newTermFakeAPI()
	var chunks []string
	var chunksMu sync.Mutex
	term := openTerminal(t, f, TerminalOptions{OnData: func(b []byte) {
		chunksMu.Lock()
		chunks = append(chunks, string(b))
		chunksMu.Unlock()
	}})

	// Output → onData.
	f.tmu.Lock()
	server := f.sessionServer
	f.tmu.Unlock()
	go server.Write([]byte("hello"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		chunksMu.Lock()
		got := strings.Join(chunks, "")
		chunksMu.Unlock()
		if got == "hello" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	chunksMu.Lock()
	if strings.Join(chunks, "") != "hello" {
		t.Fatalf("chunks = %v", chunks)
	}
	chunksMu.Unlock()

	// Input → pty.
	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := server.Read(buf)
		read <- string(buf[:n])
	}()
	if !term.Write([]byte("ls\n")) {
		t.Fatal("write returned false")
	}
	select {
	case got := <-read:
		if got != "ls\n" {
			t.Fatalf("pty received %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pty never received input")
	}

	// Resize maps cols/rows onto Docker w/h.
	if !term.Resize(100, 30) {
		t.Fatal("resize returned false")
	}
	f.tmu.Lock()
	last := f.resizes[len(f.resizes)-1]
	f.tmu.Unlock()
	if last.Height != 30 || last.Width != 100 {
		t.Fatalf("resize = %+v", last)
	}

	// Nonsense dimensions clamp instead of reaching Docker raw.
	term.Resize(99999, 0)
	f.tmu.Lock()
	last = f.resizes[len(f.resizes)-1]
	f.tmu.Unlock()
	if last.Width != 1000 || last.Height != 30 {
		t.Fatalf("clamped resize = %+v", last)
	}

	// A shell that exited mid-resize is not an error worth raising.
	f.tmu.Lock()
	f.resizeErr = notFoundErr{"no such exec"}
	f.tmu.Unlock()
	if term.Resize(90, 20) {
		t.Fatal("resize survived a dead exec")
	}

	// No-ops once closed.
	term.Close("closed")
	if term.Write([]byte("x")) {
		t.Fatal("write after close")
	}
	if term.Resize(90, 20) {
		t.Fatal("resize after close")
	}
}

func TestTerminalCloseReaps(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	term := openTerminal(t, f, TerminalOptions{User: "app", OnExit: rec.record})

	term.Close("closed")

	scripts := f.scripts()
	if len(scripts) != 1 {
		t.Fatalf("reap scripts = %d", len(scripts))
	}
	script := scripts[0]
	if !strings.Contains(script, "kill -HUP") {
		t.Error("no SIGHUP in reap script")
	}
	// Single line: `done \n | sort` is a shell syntax error.
	if strings.Contains(script, "\n") {
		t.Error("reap script is multi-line")
	}
	// Targets only this session's tag.
	tag := strings.TrimPrefix(f.sessionOptions(t).Env[0], "ODAC_TTY_SESSION=")
	if !strings.Contains(script, "ODAC_TTY_SESSION="+tag) {
		t.Error("reap script does not carry the session tag")
	}
	// Children first (highest pid first), the shell last, with the grace
	// pause between.
	if !strings.Contains(script, "sort -rn") {
		t.Error("pids not sorted -rn")
	}
	if strings.Index(script, `kill -HUP "$p"`) > strings.Index(script, `kill -HUP "$shell"`) {
		t.Error("shell signalled before its children")
	}
	if !regexp.MustCompile(`kill -HUP "\$p".*sleep 1.*kill -HUP "\$shell"`).MatchString(script) {
		t.Error("no pause between children and shell")
	}

	// The reaper exec is untagged (never matches itself) and runs as the
	// session user (environ of another uid needs CAP_SYS_PTRACE).
	reap := f.reapOptions(t)
	if len(reap.Env) != 0 {
		t.Errorf("reap exec is tagged: %v", reap.Env)
	}
	if reap.User != "app" {
		t.Errorf("reap user = %q", reap.User)
	}

	// onExit fires exactly once with the exit code, even on repeated close.
	term.Close("closed")
	calls := rec.list()
	if len(calls) != 1 || calls[0].Reason != "closed" || calls[0].ExitCode != 0 {
		t.Fatalf("onExit calls = %+v", calls)
	}
	if !term.Closed() {
		t.Fatal("terminal not closed")
	}
}

func TestTerminalReapEscalatesToSIGKILL(t *testing.T) {
	f := newTermFakeAPI()
	// Running after HUP, still running after the grace wait, dead once
	// KILLed.
	f.runningSeq = []bool{true, true, false, false}
	term := openTerminal(t, f, TerminalOptions{})

	term.Close("closed")

	scripts := f.scripts()
	if len(scripts) != 2 {
		t.Fatalf("reap scripts = %d", len(scripts))
	}
	if !strings.Contains(scripts[0], "kill -HUP") || !strings.Contains(scripts[1], "kill -KILL") {
		t.Fatalf("escalation = %v", scripts)
	}
}

func TestTerminalShellExitClosesAsExited(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	openTerminal(t, f, TerminalOptions{OnExit: rec.record})

	f.tmu.Lock()
	f.sessionServer.Close() // the shell hangs up
	f.tmu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.list()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := rec.list()
	if len(calls) != 1 || calls[0].Reason != "exited" || calls[0].ExitCode != 0 {
		t.Fatalf("onExit = %+v", calls)
	}
}

func TestTerminalSurvivesVanishedContainer(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	term := openTerminal(t, f, TerminalOptions{OnExit: rec.record})

	f.tmu.Lock()
	f.execCreateErr = notFoundErr{"No such container"}
	f.tmu.Unlock()

	term.Close("closed")
	if calls := rec.list(); len(calls) != 1 {
		t.Fatalf("onExit = %+v", calls)
	}
	if !term.Closed() {
		t.Fatal("terminal not closed")
	}
}

// drainPTY keeps reading the pty input side: net.Pipe writes are fully
// synchronous, so an undrained server would block term.Write (a real
// hijacked docker conn has kernel buffers).
func drainPTY(f *termFakeAPI) {
	f.tmu.Lock()
	server := f.sessionServer
	f.tmu.Unlock()
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()
}

func TestTerminalIdleTimeout(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	term := openTerminal(t, f, TerminalOptions{
		OnExit:      rec.record,
		IdleTimeout: durPtr(120 * time.Millisecond),
	})
	drainPTY(f)

	// Input pushes the deadline back.
	time.Sleep(80 * time.Millisecond)
	term.Write([]byte("x"))
	time.Sleep(80 * time.Millisecond)
	if len(rec.list()) != 0 {
		t.Fatal("closed despite fresh input")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.list()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := rec.list()
	if len(calls) != 1 || calls[0].Reason != "idle" {
		t.Fatalf("onExit = %+v", calls)
	}
}

func TestTerminalOutputDoesNotResetIdle(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	openTerminal(t, f, TerminalOptions{
		OnExit:      rec.record,
		IdleTimeout: durPtr(100 * time.Millisecond),
	})

	// A chatty process must not keep an abandoned session alive.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				f.tmu.Lock()
				server := f.sessionServer
				f.tmu.Unlock()
				server.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
				server.Write([]byte("output"))
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.list()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := rec.list()
	if len(calls) != 1 || calls[0].Reason != "idle" {
		t.Fatalf("onExit = %+v", calls)
	}
}

func TestTerminalLifetimeCap(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	term := openTerminal(t, f, TerminalOptions{
		OnExit:      rec.record,
		IdleTimeout: durPtr(100 * time.Millisecond),
		MaxLifetime: durPtr(250 * time.Millisecond),
	})
	drainPTY(f)

	// Steady input keeps resetting the idle timer; only the cap can end it.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				term.Write([]byte("x"))
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(rec.list()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := rec.list()
	if len(calls) != 1 || calls[0].Reason != "lifetime" {
		t.Fatalf("onExit = %+v", calls)
	}
}

func TestTerminalZeroTimeoutsDisable(t *testing.T) {
	f := newTermFakeAPI()
	rec := &exitRecorder{}
	term := openTerminal(t, f, TerminalOptions{OnExit: rec.record}) // helper zeroes both

	time.Sleep(150 * time.Millisecond)
	if len(rec.list()) != 0 {
		t.Fatal("timers fired despite 0 timeouts")
	}
	term.Close("closed")
}

// ---- CreateTerminalSession gates (Container.js side) ----

func runningInspect(privileged bool, user string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State:      &container.State{Running: true},
			HostConfig: &container.HostConfig{Privileged: privileged},
		},
		Config: &container.Config{User: user},
	}
}

func newTermClient(f *termFakeAPI, apps ...string) *Client {
	c := New(f, Options{})
	c.SetAppNames(func() []string { return apps })
	return c
}

func TestCreateTerminalSessionGates(t *testing.T) {
	f := newTermFakeAPI()
	f.inspects["web"] = runningInspect(false, "")
	c := newTermClient(f, "web")

	// Unknown app: never inspected, never exec'd.
	if _, err := c.CreateTerminalSession("odac", TerminalOptions{}); err == nil ||
		err.Error() != "Unknown app: odac" {
		t.Fatalf("unknown app err = %v", err)
	}

	// Not running (no inspect entry → 404).
	c2 := newTermClient(f, "ghost")
	if _, err := c2.CreateTerminalSession("ghost", TerminalOptions{}); err == nil ||
		err.Error() != "Container ghost is not running" {
		t.Fatalf("not-running err = %v", err)
	}

	// Privileged refusal without the opt-in.
	f.inspects["priv"] = runningInspect(true, "")
	c3 := newTermClient(f, "priv")
	if _, err := c3.CreateTerminalSession("priv", TerminalOptions{}); err == nil ||
		err.Error() != "Refusing a terminal in privileged container priv" {
		t.Fatalf("privileged err = %v", err)
	}
	term, err := c3.CreateTerminalSession("priv", TerminalOptions{AllowPrivileged: true, IdleTimeout: durPtr(0), MaxLifetime: durPtr(0)})
	if err != nil {
		t.Fatalf("privileged opt-in failed: %v", err)
	}
	term.Close("closed")
}

func TestCreateTerminalSessionUsesImageUser(t *testing.T) {
	f := newTermFakeAPI()
	f.inspects["web"] = runningInspect(false, "appuser")
	c := newTermClient(f, "web")

	term, err := c.CreateTerminalSession("web", TerminalOptions{IdleTimeout: durPtr(0), MaxLifetime: durPtr(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close("closed")
	if f.sessionOptions(t).User != "appuser" {
		t.Errorf("exec user = %q", f.sessionOptions(t).User)
	}
}

func TestCreateTerminalSessionDockerDown(t *testing.T) {
	f := newTermFakeAPI()
	f.pingErr = notFoundErr{"down"}
	c := New(f, Options{})
	if _, err := c.CreateTerminalSession("web", TerminalOptions{}); err == nil ||
		err.Error() != "Docker is not available" {
		t.Fatalf("docker-down err = %v", err)
	}
}
