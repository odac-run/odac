// Package watchdog supervises the ODAC server process: stale-PID cleanup at
// startup, restart-on-crash with a rate budget, and buffered log capture with
// size-based trimming. Behavioral contract: docs/migration/contracts/
// lifecycle.md, "Watchdog contract" (port of watchdog/src/Watchdog.js).
package watchdog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"odac/internal/config"
)

const (
	maxRestartsInWindow = 100
	restartWindow       = 5 * time.Minute
	flushInterval       = time.Second

	// Logs are appended incrementally; once a buffer passes maxLines its file
	// is rewritten down to the last trimLines, then appending resumes.
	maxLines  = 2000
	trimLines = 1000
)

// stream is one buffered log target. buf holds the full in-memory content,
// flushed counts bytes already on disk (-1 forces a full rewrite, e.g. first
// flush or after a log-name switch), dirty marks unflushed data.
type stream struct {
	buf     string
	flushed int
	dirty   bool
}

// flush appends new buffer content to file, rewriting down to trimLines once
// the buffer grows past maxLines. Caller must hold the owning Watchdog's mu.
func (st *stream) flush(file string) error {
	if !st.dirty {
		return nil
	}

	prevFlushed := st.flushed
	lines := strings.Split(st.buf, "\n")
	rewrite := st.flushed < 0 || len(lines) > maxLines

	var payload string
	if rewrite {
		if len(lines) > trimLines {
			st.buf = strings.Join(lines[len(lines)-trimLines:], "\n")
		}
		payload = st.buf
	} else {
		payload = st.buf[st.flushed:]
	}

	st.flushed = len(st.buf)
	st.dirty = false

	var err error
	if rewrite {
		err = os.WriteFile(file, []byte(payload), 0o644)
	} else if payload != "" {
		err = appendFile(file, payload)
	}
	if err != nil {
		// Retry next cycle: re-append the same bytes, or force a fresh rewrite.
		st.dirty = true
		if rewrite {
			st.flushed = -1
		} else {
			st.flushed = prevFlushed
		}
		return err
	}
	return nil
}

// Watchdog supervises a single server child process.
type Watchdog struct {
	cfg       *config.Store
	logDir    string
	serverCmd []string

	mu      sync.Mutex // guards logS, errS, logName
	logS    stream
	errS    stream
	logName string

	restartCount int
	lastRestart  time.Time

	childMu sync.Mutex
	child   *os.Process

	// Test seam; production value is stopSupervised.
	reap func(pid int)
}

// New creates a watchdog that runs serverCmd (argv form) as its supervised
// child. Log files live under <cfg.BaseDir()>/logs, named by ODAC_LOG_NAME
// (default ".odac").
func New(cfg *config.Store, serverCmd []string) *Watchdog {
	logName := os.Getenv("ODAC_LOG_NAME")
	if logName == "" {
		logName = ".odac"
	}
	return &Watchdog{
		cfg:       cfg,
		logDir:    filepath.Join(cfg.BaseDir(), "logs"),
		serverCmd: serverCmd,
		logS:      stream{flushed: -1},
		errS:      stream{flushed: -1},
		logName:   logName,
		reap:      stopSupervised,
	}
}

// Run supervises the server until it exits cleanly (code 0) or exhausts the
// restart budget. Never returns: it terminates the process via os.Exit.
func (w *Watchdog) Run() {
	go func() {
		for range time.Tick(flushInterval) {
			w.flushAll()
		}
	}()

	// bin/odac semantics: the child must not outlive the watchdog.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		w.killChild()
		w.shutdown(0)
	}()

	for {
		code, err := w.runServerOnce()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Failed to run server:", err)
		}
		if code == 0 && err == nil {
			fmt.Println("Server process exited normally (code 0). Watchdog shutting down.")
			w.shutdown(0)
		}

		// The server may have persisted config changes since our last load.
		w.cfg.Reload()
		w.appendErrLine(fmt.Sprintf("Process closed with code %d", code))

		if !w.registerCrash(time.Now()) {
			fmt.Fprintln(os.Stderr, "Server has crashed too many times. Not restarting.")
			w.shutdown(1)
		}
		fmt.Println("Server process closed. Restarting...")
	}
}

// registerCrash counts a crash against the restart budget and reports whether
// a restart is still allowed. The window resets after restartWindow of calm.
func (w *Watchdog) registerCrash(now time.Time) bool {
	if now.Sub(w.lastRestart) > restartWindow {
		w.restartCount = 0
	}
	w.restartCount++
	w.lastRestart = now
	return w.restartCount < maxRestartsInWindow
}

// runServerOnce performs startup checks, spawns the server, streams its
// output into the log buffers and waits for it to exit.
func (w *Watchdog) runServerOnce() (int, error) {
	w.startupChecks()

	if err := os.MkdirAll(w.logDir, 0o755); err != nil {
		return -1, err
	}

	cmd := exec.Command(w.serverCmd[0], w.serverCmd[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	w.childMu.Lock()
	w.child = cmd.Process
	w.childMu.Unlock()

	if srv := w.cfg.Map("server"); srv != nil {
		srv["pid"] = cmd.Process.Pid
		w.cfg.Touch("server")
		w.cfg.SaveDirty()
	}

	fmt.Printf("Watchdog process started with PID: %d\n", os.Getpid())
	fmt.Printf("Server process started with PID: %d\n", cmd.Process.Pid)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.consume(stdout, false) }()
	go func() { defer wg.Done(); w.consume(stderr, true) }()
	wg.Wait()

	waitErr := cmd.Wait()
	w.childMu.Lock()
	w.child = nil
	w.childMu.Unlock()

	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, waitErr
}

// consume prefixes each chunk of child output with [LOG]/[ERR] + ISO time and
// appends it to the buffers (stderr goes to both files, like Node). A chunk
// containing ODAC_CMD:SWITCH_LOGS switches log files back to ".odac" — the
// server prints it once a zero-downtime update completes.
func (w *Watchdog) consume(r io.Reader, isErr bool) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if !isErr && strings.Contains(chunk, "ODAC_CMD:SWITCH_LOGS") {
				fmt.Println("Watchdog: Switching to standard logs (.odac.log)...")
				w.mu.Lock()
				w.logName = ".odac"
				w.logS.flushed = -1
				w.errS.flushed = -1
				w.errS.dirty = true
				w.mu.Unlock()
			}

			prefix := "[LOG]"
			if isErr {
				prefix = "[ERR]"
			}
			entry := fmt.Sprintf("%s[%s] %s", prefix, isoNow(), chunk)

			w.mu.Lock()
			w.logS.buf += entry
			w.logS.dirty = true
			if isErr {
				w.errS.buf += entry
				w.errS.dirty = true
			}
			w.mu.Unlock()
		}
		if readErr != nil {
			return
		}
	}
}

// startupChecks kills stale supervised processes recorded in config, records
// this watchdog's identity and force-persists, then waits 1s (which doubles
// as the restart backoff, matching Node).
//
// Node also walked config.websites here, but no module file ever persists
// that key, so a fresh process can never observe it — dropped as dead code.
//
// Deviation from Node (found in the 3.8 staging rehearsal): in update mode
// the reaping is skipped entirely. The NEW instance shares the config volume
// AND the pid namespace with the still-live OLD instance, so server.watchdog
// and server.pid are the handshake peer, not stale leftovers — reaping them
// kills the OLD's pid-1, docker restarts it, and its reincarnation reads
// THIS watchdog's freshly recorded pid and kills the NEW right back (mutual
// murder, observed live). Node has the same flaw, only partially masked by
// Process.stop's name=='node' guard; documented in lifecycle.md.
func (w *Watchdog) startupChecks() {
	srv := w.cfg.Map("server")
	if srv == nil {
		srv = map[string]any{}
		w.cfg.Set("server", srv)
	}

	if os.Getenv("ODAC_UPDATE_MODE") != "true" {
		if pid := intVal(srv["watchdog"]); pid > 0 && pid != os.Getpid() {
			w.reap(pid)
		}
		if pid := intVal(srv["pid"]); pid > 0 {
			w.reap(pid)
		}
		if apps, ok := w.cfg.Get("apps").([]any); ok {
			for _, a := range apps {
				if app, ok := a.(map[string]any); ok {
					if pid := intVal(app["pid"]); pid > 0 {
						w.reap(pid)
					}
				}
			}
		}
	}

	srv["watchdog"] = os.Getpid()
	srv["started"] = time.Now().UnixMilli()
	w.cfg.Touch("server")
	if err := w.cfg.ForceSave(); err != nil {
		fmt.Fprintln(os.Stderr, "Error during startup checks:", err)
	}
	time.Sleep(time.Second)
}

func (w *Watchdog) flushAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.logS.dirty && !w.errS.dirty {
		return
	}
	if err := os.MkdirAll(w.logDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to save logs:", err)
		return
	}
	// Flush independently: a failure on the standard log must not skip the
	// error log, which carries the crash reason.
	if err := w.logS.flush(filepath.Join(w.logDir, w.logName+".log")); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to save standard log:", err)
	}
	if err := w.errS.flush(filepath.Join(w.logDir, w.logName+"_err.log")); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to save error log:", err)
	}
}

func (w *Watchdog) appendErrLine(msg string) {
	entry := fmt.Sprintf("[ERR][%s] %s\n", isoNow(), msg)
	w.mu.Lock()
	w.errS.buf += entry
	w.errS.dirty = true
	w.mu.Unlock()
}

func (w *Watchdog) killChild() {
	w.childMu.Lock()
	defer w.childMu.Unlock()
	if w.child != nil {
		w.child.Kill()
	}
}

// shutdown flushes pending logs best-effort and exits.
func (w *Watchdog) shutdown(code int) {
	w.flushAll()
	os.Exit(code)
}

// stopSupervised SIGTERMs pid only when the process looks like one of ours —
// the Node server/watchdog ("node") or an ODAC Go binary ("odac-*"). This is
// the PID-reuse guard from core/Process.js, widened for the Go binaries.
func stopSupervised(pid int) {
	name := processName(pid)
	if name != "node" && !strings.HasPrefix(name, "odac-") {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		p.Signal(syscall.SIGTERM)
	}
}

// processName returns the executable name of pid, or "" when unknown.
func processName(pid int) string {
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		return strings.TrimSpace(string(raw))
	}
	// macOS and other non-/proc platforms.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

func appendFile(file, payload string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(payload)
	return err
}

// isoNow matches JavaScript's Date.toISOString() (millisecond UTC).
func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func intVal(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
