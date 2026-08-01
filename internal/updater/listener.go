package updater

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// handover is the OLD-instance side of the update: the unix-socket listener
// created by #createUpdateListener and its completion promise.
type handover struct {
	u           *Updater
	ln          net.Listener
	completion  chan error // buffered; nil = handover completed
	globalTimer *time.Timer

	once sync.Once
	mu   sync.Mutex
	// handoverCompleted gates the rollback: a socket closing after
	// TAKEOVER_COMPLETE is the normal end of the protocol.
	handoverCompleted bool
	// servicesRestarted keeps a second rollback from re-running System.Init.
	servicesRestarted bool
}

func (l *handover) complete(err error) {
	l.once.Do(func() { l.completion <- err })
}

// createUpdateListener ports #createUpdateListener: listen on update.sock
// (chmod 666 so the incoming container can connect), with the 5-minute
// global timeout. The completion channel resolves when the handover
// finishes or fails.
func (u *Updater) createUpdateListener() (*handover, error) {
	socketPath := u.socketPath()

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	// Node's server.close() never unlinks the socket file; Go's does by
	// default. Match Node: the file is removed only where Updater.js
	// removes it (rollback here, HANDOVER_COMPLETE on the NEW side) — on
	// the global timeout it stays behind as the stale-socket recovery path.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	os.Chmod(socketPath, 0o666)

	l := &handover{u: u, ln: ln, completion: make(chan error, 1)}
	// Extended timeout for the stability check (5 minutes total). Node does
	// NOT unlink the socket here — a restarted instance treats it as stale.
	l.globalTimer = time.AfterFunc(u.globalTimeout, func() {
		ln.Close()
		l.complete(errors.New("Update process timed out globally"))
	})

	u.log.Log("Listening on update socket: %s", socketPath)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (timeout, completion or rollback)
			}
			go l.handleConn(conn)
		}
	}()

	return l, nil
}

func (l *handover) handleConn(conn net.Conn) {
	u := l.u
	u.log.Log("New container connected. Starting handover...")

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			l.handleMessage(conn, strings.TrimSpace(string(buf[:n])))
		}
		if err != nil {
			break
		}
	}

	// Node's socket 'close' handler: if the connection dies before the
	// handover completed, the new container crashed — roll back.
	l.mu.Lock()
	done := l.handoverCompleted
	l.mu.Unlock()
	if done {
		return
	}
	l.rollback()
}

func (l *handover) handleMessage(conn net.Conn, message string) {
	u := l.u
	u.log.Log("Received: %s", message)

	// --- ZERO-DOWNTIME HANDSHAKE PROTOCOL (lifecycle.md message table) ---
	switch message {
	case "HANDSHAKE_READY":
		u.log.Log("New container ready. Stopping services (except Web & DNS) and releasing ports...")
		// Stop everything except Web and DNS (Mail, Api, Hub) — Web and DNS
		// keep serving via SO_REUSEPORT while the new container overlaps.
		if sys := u.system(); sys != nil {
			sys.Stop(true)
		}
		u.log.Log("Non-overlap services stopped. Ports released. Web & DNS still running via SO_REUSEPORT.")

		conn.Write([]byte("HANDSHAKE_ACK"))
		u.log.Log("ACK sent. New container can now bind ports (Web & DNS will overlap via SO_REUSEPORT).")

	case "WEB_READY":
		u.log.Log("New container's Web & DNS are stable. Stopping old overlap services now...")
		u.stopWebServices()
		u.log.Log("Old Web & DNS stopped. Zero-downtime handover complete (overlap: ~3s).")

	case "TAKEOVER_COMPLETE":
		u.log.Log("Stability check passed. New instance is stable.")
		l.mu.Lock()
		l.handoverCompleted = true
		l.mu.Unlock()

		if _, err := conn.Write([]byte("HANDOVER_COMPLETE")); err != nil {
			conn.Write([]byte("HANDOVER_FAILED:" + err.Error()))
			l.complete(err)
		} else {
			l.complete(nil)
		}
		l.globalTimer.Stop()
		time.AfterFunc(u.destructDelay, func() {
			conn.Close()
			l.ln.Close()
			u.selfDestruct()
		})
	}
}

// rollback ports the listener's premature-close handler (rewritten by
// 11a9b00). Ordering is load-bearing — see the inline comments.
func (l *handover) rollback() {
	u := l.u
	u.log.Log("CRITICAL: New container disconnected prematurely! Initiating ROLLBACK...")

	// Tear this listener down before anything else. The rollback restarts
	// our services through System.Init, which re-enters Updater.Init; if the
	// socket were still listening, Init would find it, handshake with THIS
	// process and drive takeOver inside the old container — renaming us to
	// 'odac-backup' and leaving no container named 'odac' at all.
	l.globalTimer.Stop()
	l.ln.Close()
	os.Remove(u.socketPath())

	if err := l.rollbackSteps(); err != nil {
		u.log.Error("Rollback failed: %s", err.Error())
	}

	// Unblock execute(), which is still waiting on the completion channel.
	// Without this it hangs until the 5-minute global timeout, and #updating
	// stays true — blocking every later update attempt.
	l.complete(errors.New("Rolled back: new container disconnected prematurely"))
}

func (l *handover) rollbackSteps() error {
	u := l.u

	u.fetchContainerLogs(updateContainerName)

	// Establish who we are before removing anything: a rollback must never
	// force-remove the container it is running inside. Before takeOver we
	// still own 'odac' and the failed container is 'odac-update'; after
	// takeOver we are 'odac-backup' and the failed one has claimed 'odac'.
	selfName := u.resolveSelfName()
	failedName := updateContainerName
	if selfName == backupContainerName {
		failedName = containerName
	}

	l.mu.Lock()
	restarted := l.servicesRestarted
	l.servicesRestarted = true
	l.mu.Unlock()
	if !restarted {
		u.log.Log("Restarting services for rollback...")
		sys := u.system()
		if sys == nil {
			return errors.New("System is not wired")
		}
		if err := sys.Init(); err != nil { // restart all services (awaited)
			return err
		}
		u.log.Log("Services restarted successfully")
	}

	// Clean up the failed container.
	if err := u.deps.Docker.Remove(failedName, true); err != nil {
		if !isNotFound(err) {
			u.log.Log("Warning: could not remove %s: %s", failedName, err.Error())
		}
	} else {
		u.log.Log("Failed new container removed: %s", failedName)
	}

	// Restore our name, if takeOver already renamed us away from it.
	if err := u.deps.Docker.Rename(backupContainerName, containerName); err != nil {
		if !isNotFound(err) {
			u.log.Error("Failed to restore name: %s", err.Error())
		}
	} else {
		u.log.Log("Restored self to \"%s\".", containerName)
	}

	// takeOver revoked our restart policy on the way out; take it back.
	u.ensureRestartPolicy()
	u.log.Log("Rollback successful. Continuing operations.")
	return nil
}

// selfDestruct ports #selfDestruct: the OLD instance's exit after a
// successful handover.
func (u *Updater) selfDestruct() {
	u.log.Log("Old container mission complete. Stopping overlap services and exiting.")

	// Stop Web and DNS now (kept running during the handover overlap).
	u.stopWebServices()
	u.log.Log("Web & DNS services stopped.")

	// Disable the restart policy so Docker cannot resurrect this container
	// (redundant with takeOver's revoke since 11a9b00 — kept).
	if err := u.deps.Docker.UpdateRestartPolicy(backupContainerName, "no"); err != nil {
		u.log.Error("Failed to disable restart policy: %s", err.Error())
	} else {
		u.log.Log("Restart policy disabled.")
	}

	u.exit(0)
}

func (u *Updater) stopWebServices() {
	if u.deps.Proxy != nil {
		u.deps.Proxy.Stop()
	}
	if u.deps.DNS != nil {
		u.deps.DNS.Stop()
	}
}

// fetchContainerLogs ports #fetchContainerLogs: surface the crashed new
// container's internal log (docker cp of its log file; docker logs as the
// fallback) into our own log before the rollback removes it.
func (u *Updater) fetchContainerLogs(name string) {
	logFileName := ".odac.log"
	if name == updateContainerName {
		logFileName = "." + name + ".log"
	}

	content, err := u.deps.Docker.ReadFile(name, filepath.Join(u.baseDir, "logs", logFileName))
	if err == nil {
		lines := strings.Split(string(content), "\n")
		if len(lines) > 100 {
			lines = lines[len(lines)-100:]
		}
		u.log.Log(fmt.Sprintf("--- INTERNAL LOGS FOR %s ---", name))
		u.log.Log(strings.Join(lines, "\n"))
		u.log.Log("-----------------------------------")
		return
	}

	u.log.Log("Could not fetch internal logs via cp: %s. Trying standard logs...", err.Error())

	rc, err := u.deps.Docker.Logs(name, false, "50")
	if err != nil {
		u.log.Log("Could not fetch docker logs: %s", err.Error())
		return
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	u.log.Log(fmt.Sprintf("--- DOCKER STD LOGS (FALLBACK) FOR %s ---", name))
	u.log.Log(string(b))
	u.log.Log("-----------------------------------")
}
