// Package supervise is the Go port of the data-plane process supervision
// blocks of server/src/{Proxy,DNS,Mail}.js — three byte-identical copies in
// Node, one generic Supervisor here (contracts/proxy-control.md "Process
// contract", shared by dns/mail per their contracts).
//
// Per supervised binary:
//
//   - Detached spawn (Setsid, all stdio to the null device) so the process
//     survives orchestrator restarts.
//   - PID file <runDir>/<name>-<instanceId>.pid written by the ORCHESTRATOR
//     with O_EXCL ("wx"); EEXIST means a concurrent spawn won the race and the
//     redundant child is killed. Update mode overwrites instead.
//   - Orphan adoption on startup (skipped when ODAC_UPDATE_MODE=true): PID
//     alive + socket file exists + /proc/<pid>/cmdline contains the binary
//     name (skipped where /proc is unavailable). Any failure cleans the stale
//     files and spawns fresh.
//   - Config sync scheduled 1s after spawn/adopt (OnSync callback).
//   - ODAC_PREVIOUS_INSTANCE_ID run files GC'd 60s after spawn.
//
// Deviations from Node (deliberate, documented in STATE.md):
//
//   - TCP fallback mode is not ported (task 3.2 decision): the socket env var
//     is always set, on every platform. In Node the binaries' TCP port line on
//     stdout was discarded, so TCP mode never worked anywhere.
//   - Node never notices an adopted orphan dying (its fake process handle has
//     no exit event, so check() sees the slot occupied forever). Ensure probes
//     adopted PIDs so the 1s check tick can respawn them.
//   - The exit watcher only unlinks the PID file when it still holds our PID,
//     so a stop→restart cannot lose the successor's PID file to a late
//     exit handler (latent race in Node, never hit there).
package supervise

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"odac/internal/logx"
)

// Options configures a Supervisor for one data-plane binary.
type Options struct {
	Name      string // run-file prefix: <Name>-<instanceId>.{pid,sock}
	Binary    string // binary base name, e.g. "odac-proxy" (.exe appended on Windows)
	SocketEnv string // env var carrying the control socket path (asymmetric per binary — keep)
	Display   string // human name in log lines: "Proxy", "DNS", "Mail"
	BinDir    string // directory holding the binary
	RunDir    string // <base>/run
	Log       *logx.Logger
	OnSync    func() // config push, scheduled SyncDelay after spawn/adopt
}

// Supervisor manages one detached data-plane process.
type Supervisor struct {
	o Options

	// Node's load-bearing delays; overridable in tests only.
	SyncDelay    time.Duration // spawn/adopt → config sync (1s)
	CleanupDelay time.Duration // previous-instance file GC (60s)

	mu           sync.Mutex
	proc         *proc
	syncTimer    *time.Timer
	cleanupTimer *time.Timer

	claimPid func(path string, pid int, update bool) error // test seam
}

// proc is a running child (cmd set) or an adopted orphan (cmd nil).
type proc struct {
	pid int
	cmd *exec.Cmd
}

// New wires a Supervisor. Nothing runs until Ensure.
func New(o Options) *Supervisor {
	return &Supervisor{
		o:            o,
		SyncDelay:    time.Second,
		CleanupDelay: 60 * time.Second,
		claimPid:     claimPidFile,
	}
}

// InstanceID returns ODAC_INSTANCE_ID or "default" (read live — the updater
// sets it on the new instance's environment).
func InstanceID() string {
	if id := os.Getenv("ODAC_INSTANCE_ID"); id != "" {
		return id
	}
	return "default"
}

// PidFile returns <runDir>/<name>-<instanceId>.pid.
func (s *Supervisor) PidFile() string {
	return filepath.Join(s.o.RunDir, s.o.Name+"-"+InstanceID()+".pid")
}

// SocketPath returns <runDir>/<name>-<instanceId>.sock.
func (s *Supervisor) SocketPath() string {
	return filepath.Join(s.o.RunDir, s.o.Name+"-"+InstanceID()+".sock")
}

// Running reports whether a process is currently managed (spawned or adopted).
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// Pid returns the managed process's PID, or 0.
func (s *Supervisor) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return 0
	}
	return s.proc.pid
}

// Ensure ports spawnProxy()/spawnDNS()/spawnMail(): adopt an orphan or spawn
// fresh; no-op while a managed process is alive. Called from Start and from
// the 1s check tick.
func (s *Supervisor) Ensure() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.proc != nil {
		// Spawned children are cleared by their exit watcher; adopted orphans
		// have no such hook, so probe them here (deviation, see package doc).
		if s.proc.cmd != nil || pidAlive(s.proc.pid) {
			return
		}
		s.o.Log.Error(fmt.Sprintf("Adopted Go %s (PID %d) is gone. Respawning...", s.o.Display, s.proc.pid))
		removePidFileIfOwn(s.PidFile(), s.proc.pid)
		s.proc = nil
	}

	if err := os.MkdirAll(s.o.RunDir, 0o755); err != nil {
		s.o.Log.Error(fmt.Sprintf("Failed to create run dir %s: %s", s.o.RunDir, err))
		return
	}

	update := os.Getenv("ODAC_UPDATE_MODE") == "true"
	if update {
		s.o.Log.Log(fmt.Sprintf("Update mode detected. Forcing new %s instance spawn...", s.o.Display))
	} else if s.adoptLocked() {
		return
	}
	s.spawnLocked(update)
}

// adoptLocked runs the orphan-adoption triple check. True = adopted.
func (s *Supervisor) adoptLocked() bool {
	pidFile := s.PidFile()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.o.Log.Log(fmt.Sprintf("Orphaned %s PID file issue. Cleaning up.", s.o.Display))
			os.Remove(pidFile)
		}
		return false
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || pid <= 0 || !pidAlive(pid) {
		s.o.Log.Log(fmt.Sprintf("Orphaned %s PID file issue. Cleaning up.", s.o.Display))
		os.Remove(pidFile)
		return false
	}

	// The socket file MUST exist — PID reuse or a crashed binary otherwise.
	// The process is NOT killed (it may be an unrelated process reusing the
	// PID); only the stale PID file goes.
	if _, err := os.Stat(s.SocketPath()); err != nil {
		s.o.Log.Log(fmt.Sprintf("PID %d exists but socket file is missing. PID reuse detected or %s crashed. Ignoring orphan...", pid, s.o.Display))
		os.Remove(pidFile)
		return false
	}

	// /proc cmdline double check (Linux only; elsewhere the socket check above
	// is the guard, same as Node).
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		if !strings.Contains(string(cmdline), s.o.Binary) {
			s.o.Log.Log(fmt.Sprintf("PID %d is active but command line does not match %s. PID reuse detected!", pid, s.o.Display))
			os.Remove(pidFile)
			return false
		}
	}

	s.o.Log.Log(fmt.Sprintf("Found orphaned Go %s (PID: %d). Reconnecting...", s.o.Display, pid))
	s.proc = &proc{pid: pid}
	s.scheduleSyncLocked()
	return true
}

func (s *Supervisor) binaryFile() string {
	if runtime.GOOS == "windows" {
		return s.o.Binary + ".exe"
	}
	return s.o.Binary
}

func (s *Supervisor) spawnLocked(update bool) {
	binPath := filepath.Join(s.o.BinDir, s.binaryFile())
	if _, err := os.Stat(binPath); err != nil {
		s.o.Log.Error(fmt.Sprintf("Go %s binary not found at %s.", s.o.Display, binPath))
		return
	}

	socket := s.SocketPath()
	s.o.Log.Log(fmt.Sprintf("Starting Go %s (Socket: %s)...", s.o.Display, socket))

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), s.o.SocketEnv+"="+socket)
	cmd.SysProcAttr = detachAttr()
	// Stdin/Stdout/Stderr stay nil → the null device (Node: stdio ignored, so
	// an orchestrator restart can't SIGPIPE the detached process).
	if err := cmd.Start(); err != nil {
		s.o.Log.Error(fmt.Sprintf("Failed to spawn Go %s: %s", s.o.Display, err))
		return
	}
	pid := cmd.Process.Pid

	if err := s.claimPid(s.PidFile(), pid, update); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.o.Log.Error(fmt.Sprintf("Race condition detected: PID file %s already exists. Stopping redundant %s instance.", s.PidFile(), s.o.Display))
			terminate(cmd.Process.Pid)
			go cmd.Wait() // reap
			return
		}
		// Node would leave the child running unmanaged here; managing it is
		// strictly safer, so log and continue.
		s.o.Log.Error(fmt.Sprintf("Failed to write %s PID file: %s", s.o.Display, err))
	}

	s.o.Log.Log(fmt.Sprintf("Go %s started with PID %d", s.o.Display, pid))
	p := &proc{pid: pid, cmd: cmd}
	s.proc = p
	s.watchExit(p)
	s.scheduleSyncLocked()

	if prev := os.Getenv("ODAC_PREVIOUS_INSTANCE_ID"); prev != "" {
		s.scheduleCleanupLocked(prev)
	}
}

// watchExit ports the child's on('exit') handler: clear the slot and the PID
// file. The next check tick respawns via Ensure.
func (s *Supervisor) watchExit(p *proc) {
	go func() {
		p.cmd.Wait()
		code := -1
		if state := p.cmd.ProcessState; state != nil {
			code = state.ExitCode()
		}
		s.o.Log.Error(fmt.Sprintf("Go %s exited with code %d", s.o.Display, code))
		s.mu.Lock()
		if s.proc == p {
			s.proc = nil
		}
		s.mu.Unlock()
		removePidFileIfOwn(s.PidFile(), p.pid)
	}()
}

func (s *Supervisor) scheduleSyncLocked() {
	if s.syncTimer != nil {
		s.syncTimer.Stop()
	}
	if s.o.OnSync == nil {
		return
	}
	s.syncTimer = time.AfterFunc(s.SyncDelay, s.o.OnSync)
}

// scheduleCleanupLocked GCs the previous instance's run files well after the
// update handover completed.
func (s *Supervisor) scheduleCleanupLocked(prev string) {
	if s.cleanupTimer != nil {
		s.cleanupTimer.Stop()
	}
	s.cleanupTimer = time.AfterFunc(s.CleanupDelay, func() {
		s.o.Log.Log(fmt.Sprintf("Cleaning up files from previous instance: %s", prev))
		for _, ext := range []string{".pid", ".sock"} {
			file := filepath.Join(s.o.RunDir, s.o.Name+"-"+prev+ext)
			if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.o.Log.Log(fmt.Sprintf("Warning: Failed to cleanup previous instance files: %s", err))
			}
		}
		s.o.Log.Log(fmt.Sprintf("Cleanup successful for instance %s", prev))
	})
}

// Stop ports stop(): SIGTERM the managed process (spawned or adopted), unlink
// the socket file, cancel pending timers. The PID file is left to the exit
// watcher (spawned) or the next adoption attempt (adopted) — Node parity.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	p := s.proc
	s.proc = nil
	if s.syncTimer != nil {
		s.syncTimer.Stop()
		s.syncTimer = nil
	}
	if s.cleanupTimer != nil {
		s.cleanupTimer.Stop()
		s.cleanupTimer = nil
	}
	s.mu.Unlock()

	if p != nil {
		terminate(p.pid)
		os.Remove(s.SocketPath())
	}
}

// claimPidFile writes the PID file with O_EXCL — or plain truncate in update
// mode, where the fresh instance deliberately takes the name over.
func claimPidFile(path string, pid int, update bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !update {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(strconv.Itoa(pid))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// removePidFileIfOwn unlinks path only while it still records pid, so a late
// exit handler can't delete a successor's PID file.
func removePidFileIfOwn(path string, pid int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(raw)) == strconv.Itoa(pid) {
		os.Remove(path)
	}
}
