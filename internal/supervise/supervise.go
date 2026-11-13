// Package supervise is the generic process supervisor shared by the
// data-plane binaries (proxy, dns, mail).
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
// Further guarantees:
//
//   - There is no TCP fallback: the socket env var is always set, on every
//     platform.
//   - Ensure probes adopted PIDs, so the 1s check tick can respawn an adopted
//     orphan that died (an adopted process has no exit event).
//   - The exit watcher only unlinks the PID file when it still holds our PID,
//     so a stop then restart cannot lose the successor's PID file to a late
//     exit handler.
//   - LegacyRunDir is where run files lived before they left the directory
//     mounted into apps (see internal/rundir). An orphan found there is still
//     adopted and talked to on its old socket, files there that adopt nothing
//     are removed, and the previous instance's files are GC'd from both.
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
	"odac/internal/rundir"
)

// Options configures a Supervisor for one data-plane binary.
type Options struct {
	Name         string // run-file prefix: <Name>-<instanceId>.{pid,sock}
	Binary       string // binary base name, e.g. "odac-proxy" (.exe appended on Windows)
	SocketEnv    string // env var carrying the control socket path (asymmetric per binary — keep)
	Display      string // human name in log lines: "Proxy", "DNS", "Mail"
	BinDir       string // directory holding the binary
	RunDir       string // rundir.Control, created 0700
	LegacyRunDir string // rundir.App, read for files of an older release; may be empty
	Log          *logx.Logger
	OnSync       func()   // config push, scheduled SyncDelay after spawn/adopt
	Env          []string // extra KEY=VALUE pairs for the binary
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

// proc is a running child (cmd set) or an adopted orphan (cmd nil), with the
// run files it was found at or spawned with.
type proc struct {
	pid     int
	cmd     *exec.Cmd
	pidFile string
	sock    string
	legacy  bool
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

func (s *Supervisor) runFile(dir, ext string) string {
	return filepath.Join(dir, s.o.Name+"-"+InstanceID()+ext)
}

// PidFile returns <runDir>/<name>-<instanceId>.pid, where a spawn records its PID.
func (s *Supervisor) PidFile() string {
	return s.runFile(s.o.RunDir, ".pid")
}

// SocketPath returns the managed process's control socket:
// <runDir>/<name>-<instanceId>.sock, or the old one of an orphan adopted from
// LegacyRunDir.
func (s *Supervisor) SocketPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil && s.proc.sock != "" {
		return s.proc.sock
	}
	return s.runFile(s.o.RunDir, ".sock")
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
		removePidFileIfOwn(s.proc.pidFile, s.proc.pid)
		if s.proc.legacy {
			os.Remove(s.proc.sock)
		}
		s.proc = nil
	}

	if err := rundir.Prepare(s.o.RunDir); err != nil {
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

// adoptLocked runs the orphan-adoption triple check, in RunDir first and then
// in LegacyRunDir. True = adopted.
func (s *Supervisor) adoptLocked() bool {
	if p := s.findOrphan(s.o.RunDir); p != nil {
		s.o.Log.Log(fmt.Sprintf("Found orphaned Go %s (PID: %d). Reconnecting...", s.o.Display, p.pid))
		s.proc = p
		s.scheduleSyncLocked()
		return true
	}
	if s.o.LegacyRunDir == "" {
		return false
	}
	p := s.findOrphan(s.o.LegacyRunDir)
	if p == nil {
		// Nothing lives there any more: an older release's leftovers must not
		// stay visible to apps.
		os.Remove(s.runFile(s.o.LegacyRunDir, ".pid"))
		os.Remove(s.runFile(s.o.LegacyRunDir, ".sock"))
		return false
	}
	p.legacy = true
	s.o.Log.Log(fmt.Sprintf("Found orphaned Go %s (PID: %d) in the legacy run dir. Reconnecting...", s.o.Display, p.pid))
	s.proc = p
	s.scheduleSyncLocked()
	return true
}

// findOrphan returns the live process recorded by dir's run files, or nil
// after removing a stale PID file.
func (s *Supervisor) findOrphan(dir string) *proc {
	pidFile := s.runFile(dir, ".pid")
	sock := s.runFile(dir, ".sock")
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.o.Log.Log(fmt.Sprintf("Orphaned %s PID file issue. Cleaning up.", s.o.Display))
			os.Remove(pidFile)
		}
		return nil
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || pid <= 0 || !pidAlive(pid) {
		s.o.Log.Log(fmt.Sprintf("Orphaned %s PID file issue. Cleaning up.", s.o.Display))
		os.Remove(pidFile)
		return nil
	}

	// The socket file MUST exist — PID reuse or a crashed binary otherwise.
	// The process is NOT killed (it may be an unrelated process reusing the
	// PID); only the stale PID file goes.
	if _, err := os.Stat(sock); err != nil {
		s.o.Log.Log(fmt.Sprintf("PID %d exists but socket file is missing. PID reuse detected or %s crashed. Ignoring orphan...", pid, s.o.Display))
		os.Remove(pidFile)
		return nil
	}

	// /proc cmdline double check (Linux only; elsewhere the socket check above
	// is the guard, same as Node).
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		if !strings.Contains(string(cmdline), s.o.Binary) {
			s.o.Log.Log(fmt.Sprintf("PID %d is active but command line does not match %s. PID reuse detected!", pid, s.o.Display))
			os.Remove(pidFile)
			return nil
		}
	}

	return &proc{pid: pid, pidFile: pidFile, sock: sock}
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

	socket := s.runFile(s.o.RunDir, ".sock")
	s.o.Log.Log(fmt.Sprintf("Starting Go %s (Socket: %s)...", s.o.Display, socket))

	cmd := exec.Command(binPath)
	cmd.Env = append(append(os.Environ(), s.o.Env...), s.o.SocketEnv+"="+socket)
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
	p := &proc{pid: pid, cmd: cmd, pidFile: s.PidFile(), sock: socket}
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
		removePidFileIfOwn(p.pidFile, p.pid)
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
		for _, dir := range []string{s.o.RunDir, s.o.LegacyRunDir} {
			if dir == "" {
				continue
			}
			for _, ext := range []string{".pid", ".sock"} {
				file := filepath.Join(dir, s.o.Name+"-"+prev+ext)
				if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
					s.o.Log.Log(fmt.Sprintf("Warning: Failed to cleanup previous instance files: %s", err))
				}
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
		os.Remove(p.sock)
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
