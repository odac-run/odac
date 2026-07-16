// terminal.go ports server/src/Container/Terminal.js plus Container.js's
// createTerminalSession(): one interactive shell inside a running container,
// over a Docker exec with a real TTY. Full behavioral pinning lives in
// docs/migration/contracts/hub-protocol.md "Terminal sessions".
//
// Reaping: Docker exposes no "kill exec" API and exec PIDs live in the
// daemon's namespace, so each session tags its exec with a unique env var;
// children inherit environ, and one /proc sweep finds the whole tree. The
// sweep signals children first (highest pid first — the shell is the oldest
// and lowest and must die LAST so it can wait() its children; killing it
// first leaves zombies on a non-init pid 1), SIGHUP before SIGKILL (an
// interactive shell ignores SIGTERM but hangs up properly on HUP), and the
// reaper exec is untagged so it can never signal itself.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"odac/internal/logx"
)

// SessionEnv is the env var carrying the session tag (SESSION_ENV).
const SessionEnv = "ODAC_TTY_SESSION"

// DefaultShell picks bash when the image ships it, falling back to sh.
// `exec` so the shell replaces this probe process and stays the tagged root
// of the session tree.
var DefaultShell = []string{"/bin/sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}

const (
	defaultCols  = 80
	defaultRows  = 24
	maxDimension = 1000

	reapGraceDefault   = time.Second
	defaultIdleTimeout = 15 * time.Minute
	defaultMaxLifetime = 4 * time.Hour
)

// reapScript builds the tag sweep (see the package comment). Must stay a
// single line: a multi-line `for … done` followed by `| sort` parses as
// `done \n | sort`, a shell syntax error.
func reapScript(tag, signal string) string {
	return `pids=$(for p in /proc/[0-9]*; do pid=${p##*/}; [ -r "$p/environ" ] || continue; ` +
		`if tr '\0' '\n' < "$p/environ" 2>/dev/null | grep -qx "` + SessionEnv + `=` + tag + `"; then echo "$pid"; fi; ` +
		`done | sort -rn); ` +
		`[ -n "$pids" ] || exit 0; ` +
		`shell=$(echo "$pids" | tail -n 1); others=$(echo "$pids" | sed '$d'); ` +
		`if [ -n "$others" ]; then for p in $others; do kill -` + signal + ` "$p" 2>/dev/null; done; sleep 1; fi; ` +
		`kill -` + signal + ` "$shell" 2>/dev/null; true`
}

// clampDimension ports Node's clampDimension: non-positive falls back,
// oversized clamps to 1000.
func clampDimension(value, fallback int) int {
	if value < 1 {
		return fallback
	}
	if value > maxDimension {
		return maxDimension
	}
	return value
}

// ExitInfo is the onExit payload: reason ∈ exited | closed | idle |
// lifetime | error (plus the Hub-side reasons); ExitCode is the exec's exit
// code, or nil while running/unknown (Node's null).
type ExitInfo struct {
	Reason   string
	ExitCode any
}

// TerminalOptions ports the Terminal constructor options. IdleTimeout /
// MaxLifetime: nil → default (15m / 4h), pointer-to-0 → disabled (Node's
// `??` distinction between undefined and 0).
type TerminalOptions struct {
	Cols, Rows      int
	Command         []string
	User            string
	Env             []string
	Workdir         string
	IdleTimeout     *time.Duration
	MaxLifetime     *time.Duration
	AllowPrivileged bool
	OnData          func([]byte)
	OnExit          func(ExitInfo)
}

// SetAppNames injects the managed-app-name resolver CreateTerminalSession
// validates against. Node resolved App via the DI registry; the Go docker
// package cannot import appmgr (appmgr imports docker), so main wires this
// seam instead.
func (c *Client) SetAppNames(fn func() []string) {
	c.mu.Lock()
	c.appNames = fn
	c.mu.Unlock()
}

// CreateTerminalSession ports Container.createTerminalSession: takes an APP
// name, never a raw container name — resolving through the app list keeps a
// spoofed command from opening a shell in a container odac does not manage,
// including odac's own (which mounts the Docker socket and would hand over
// the host).
func (c *Client) CreateTerminalSession(appName string, options TerminalOptions) (*Terminal, error) {
	if !c.available {
		return nil, fmt.Errorf("Docker is not available")
	}

	c.mu.Lock()
	lister := c.appNames
	c.mu.Unlock()
	known := false
	if lister != nil {
		for _, name := range lister() {
			if name == appName {
				known = true
				break
			}
		}
	}
	if !known {
		return nil, fmt.Errorf("Unknown app: %s", appName)
	}

	// One inspect answers all three gates: running, privileged, and which
	// user the app itself runs as.
	info, err := c.api.ContainerInspect(context.Background(), appName)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("Container %s is not running", appName)
		}
		return nil, err
	}
	if info.State == nil || !info.State.Running {
		return nil, fmt.Errorf("Container %s is not running", appName)
	}

	// A shell in a --privileged container has the host's devices and kernel
	// capabilities: a host shell in container clothing. Refuse unless the
	// operator explicitly opted in.
	if info.HostConfig != nil && info.HostConfig.Privileged && !options.AllowPrivileged {
		return nil, fmt.Errorf("Refusing a terminal in privileged container %s", appName)
	}

	// Run as the app's own user rather than root: an image that declares
	// USER gets that user — we never hand out more than the app already
	// holds. An explicit option wins for the rare operator override.
	if options.User == "" && info.Config != nil {
		options.User = info.Config.User
	}

	t := newTerminal(c.api, c.log, appName, options)
	if err := t.open(); err != nil {
		return nil, err
	}
	return t, nil
}

// Terminal is one open exec session.
type Terminal struct {
	api  API
	log  *logx.Logger
	name string
	tag  string // secret, ours — never caller input: it lands in a shell command

	onData func([]byte)
	onExit func(ExitInfo)

	command []string
	user    string
	env     []string
	workdir string

	idleTimeout time.Duration
	maxLifetime time.Duration
	reapGrace   time.Duration // test hook

	mu        sync.Mutex
	cols      int
	rows      int
	execID    string
	conn      io.WriteCloser
	closer    func()
	closing   bool
	closed    bool
	idleTimer *time.Timer
	lifeTimer *time.Timer
	openedAt  time.Time
}

func newTerminal(api API, log *logx.Logger, name string, options TerminalOptions) *Terminal {
	tagBytes := make([]byte, 16)
	rand.Read(tagBytes)

	t := &Terminal{
		api:         api,
		log:         log,
		name:        name,
		tag:         hex.EncodeToString(tagBytes),
		onData:      options.OnData,
		onExit:      options.OnExit,
		command:     options.Command,
		user:        options.User,
		env:         options.Env,
		workdir:     options.Workdir,
		cols:        clampDimension(options.Cols, defaultCols),
		rows:        clampDimension(options.Rows, defaultRows),
		idleTimeout: defaultIdleTimeout,
		maxLifetime: defaultMaxLifetime,
		reapGrace:   reapGraceDefault,
	}
	if len(t.command) == 0 {
		t.command = DefaultShell
	}
	if options.IdleTimeout != nil {
		t.idleTimeout = *options.IdleTimeout
	}
	if options.MaxLifetime != nil {
		t.maxLifetime = *options.MaxLifetime
	}
	if t.onData == nil {
		t.onData = func([]byte) {}
	}
	if t.onExit == nil {
		t.onExit = func(ExitInfo) {}
	}
	return t
}

// Closed reports whether the session has fully torn down.
func (t *Terminal) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Container returns the container (= app) name.
func (t *Terminal) Container() string { return t.name }

// open creates the exec, attaches, and starts the timers.
func (t *Terminal) open() error {
	ctx := context.Background()

	options := container.ExecOptions{
		Cmd:          t.command,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		// Sizing the pty up front avoids a first paint at 0x0. [h, w].
		ConsoleSize: &[2]uint{uint(t.rows), uint(t.cols)},
		Env:         append([]string{SessionEnv + "=" + t.tag}, t.env...),
	}
	if t.user != "" {
		options.User = t.user
	}
	if t.workdir != "" {
		options.WorkingDir = t.workdir
	}

	exec, err := t.api.ContainerExecCreate(ctx, t.name, options)
	if err != nil {
		return err
	}

	// Tty must be REPEATED here: the daemon uses the start-body flag to
	// choose raw-vs-multiplexed framing — omit it and the stream arrives
	// with 8-byte stream headers even though the exec really owns a pty.
	hijack, err := t.api.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return err
	}

	t.mu.Lock()
	t.execID = exec.ID
	t.conn = hijack.Conn
	t.closer = hijack.Close
	t.openedAt = time.Now()
	t.armIdleTimerLocked()
	if t.maxLifetime > 0 {
		t.lifeTimer = time.AfterFunc(t.maxLifetime, func() { t.Close("lifetime") })
	}
	t.mu.Unlock()

	go t.readLoop(hijack)

	// Audit trail: who got a shell where, and as which user. Correlate with
	// the Hub-side session record for the human behind it.
	user := t.user
	if user == "" {
		user = "container-default"
	}
	t.log.Log("[AUDIT] terminal opened: container=%s user=%s", t.name, user)
	return nil
}

// readLoop pumps raw pty output to onData; EOF means the shell exited.
func (t *Terminal) readLoop(hijack types.HijackedResponse) {
	buf := make([]byte, 32*1024)
	for {
		n, err := hijack.Reader.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			t.onData(chunk)
		}
		if err != nil {
			t.mu.Lock()
			quiet := t.closing || t.closed
			t.mu.Unlock()
			if err == io.EOF {
				t.Close("exited")
			} else if !quiet {
				// destroy() during Close surfaces here; only real failures
				// are worth reporting.
				t.log.Error("Terminal stream error on "+t.name+":", err.Error())
				t.Close("error")
			}
			return
		}
	}
}

// Write forwards user input to the pty. Resets the idle timer — output
// alone must not, or a chatty process would keep an abandoned session alive
// forever.
func (t *Terminal) Write(data []byte) bool {
	t.mu.Lock()
	if t.closed || t.conn == nil {
		t.mu.Unlock()
		return false
	}
	conn := t.conn
	t.armIdleTimerLocked()
	t.mu.Unlock()
	conn.Write(data)
	return true
}

// Resize maps cols/rows onto Docker's w/h, clamping nonsense values.
func (t *Terminal) Resize(cols, rows int) bool {
	t.mu.Lock()
	if t.closed || t.execID == "" {
		t.mu.Unlock()
		return false
	}
	t.cols = clampDimension(cols, t.cols)
	t.rows = clampDimension(rows, t.rows)
	execID := t.execID
	h, w := uint(t.rows), uint(t.cols)
	t.mu.Unlock()

	err := t.api.ContainerExecResize(context.Background(), execID, container.ResizeOptions{Height: h, Width: w})
	if err != nil {
		// The shell can exit between our check and the call; not an error
		// worth raising.
		t.log.Log("Terminal resize failed on %s: %s", t.name, err.Error())
		return false
	}
	return true
}

// Close tears the session down and reaps the process tree. Idempotent;
// onExit fires exactly once.
func (t *Terminal) Close(reason string) {
	t.mu.Lock()
	if t.closed || t.closing {
		t.mu.Unlock()
		return
	}
	t.closing = true
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
	if t.lifeTimer != nil {
		t.lifeTimer.Stop()
		t.lifeTimer = nil
	}
	closer := t.closer
	t.conn = nil
	t.closer = nil
	t.mu.Unlock()

	if closer != nil {
		closer()
	}

	// Closing the stream does NOT kill the exec: the shell and its children
	// keep running and exec inspect stays Running. Reap explicitly, always.
	t.reap()

	exitCode := t.exitCode()

	t.mu.Lock()
	t.closed = true
	t.closing = false
	openedAt := t.openedAt
	t.mu.Unlock()

	durationSec := 0
	if !openedAt.IsZero() {
		durationSec = int(time.Since(openedAt).Round(time.Second).Seconds())
	}
	exitDisplay := "null" // Node: %s of null
	if exitCode != nil {
		exitDisplay = fmt.Sprint(exitCode)
	}
	t.log.Log("[AUDIT] terminal closed: container=%s reason=%s exitCode=%s duration=%ss",
		t.name, reason, exitDisplay, durationSec)
	t.onExit(ExitInfo{Reason: reason, ExitCode: exitCode})
}

// reap: SIGHUP, then SIGKILL for whatever ignored it. SIGTERM is the wrong
// signal here — an interactive shell ignores it, so the children die and
// the shell is left orphaned; SIGHUP is the real terminal-hangup signal.
func (t *Terminal) reap() {
	err := t.runInContainer(reapScript(t.tag, "HUP"))
	if err != nil {
		// A stopped or removed container took the session down with it.
		if client.IsErrNotFound(err) || cerrdefs.IsConflict(err) {
			t.log.Log("Terminal reap skipped on %s: container gone", t.name)
			return
		}
		t.log.Error("Terminal reap failed on "+t.name+":", err.Error())
		return
	}

	if !t.isExecRunning() {
		return
	}
	time.Sleep(t.reapGrace)
	if !t.isExecRunning() {
		return
	}

	if err := t.runInContainer(reapScript(t.tag, "KILL")); err != nil {
		if client.IsErrNotFound(err) || cerrdefs.IsConflict(err) {
			t.log.Log("Terminal reap skipped on %s: container gone", t.name)
			return
		}
		t.log.Error("Terminal reap failed on "+t.name+":", err.Error())
		return
	}
	if t.isExecRunning() {
		t.log.Error("Terminal reap incomplete on " + t.name + ": session survived SIGKILL")
	}
}

func (t *Terminal) isExecRunning() bool {
	t.mu.Lock()
	execID := t.execID
	t.mu.Unlock()
	inspect, err := t.api.ContainerExecInspect(context.Background(), execID)
	return err == nil && inspect.Running
}

// exitCode returns the exec's exit code, or nil while running/unknown.
func (t *Terminal) exitCode() any {
	t.mu.Lock()
	execID := t.execID
	t.mu.Unlock()
	inspect, err := t.api.ContainerExecInspect(context.Background(), execID)
	if err != nil || inspect.Running {
		return nil
	}
	return inspect.ExitCode
}

// runInContainer is the fire-and-forget exec used for reaping. Untagged, so
// the sweep never matches itself; run as the SESSION user because reading
// another uid's /proc/<pid>/environ needs CAP_SYS_PTRACE, which a stock
// container's exec doesn't hold — matching the user gives both the read and
// the kill.
func (t *Terminal) runInContainer(script string) error {
	ctx := context.Background()
	options := container.ExecOptions{
		Cmd:          []string{"/bin/sh", "-c", script},
		AttachStdout: true,
		AttachStderr: true,
	}
	if t.user != "" {
		options.User = t.user
	}

	exec, err := t.api.ContainerExecCreate(ctx, t.name, options)
	if err != nil {
		return err
	}
	hijack, err := t.api.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer hijack.Close()
	// Drain, or the exec's end never surfaces.
	io.Copy(io.Discard, hijack.Reader)
	return nil
}

func (t *Terminal) armIdleTimerLocked() {
	if t.idleTimeout <= 0 {
		return
	}
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	t.idleTimer = time.AfterFunc(t.idleTimeout, func() { t.Close("idle") })
}
