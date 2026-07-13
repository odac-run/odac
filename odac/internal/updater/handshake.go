package updater

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"odac/internal/logx"
)

// performHandshake ports #performHandshake: the NEW-instance side of the
// update protocol. Blocks until the handover completes (HANDOVER_COMPLETE),
// fails (HANDOVER_FAILED / socket error) or the 60s handshake timeout fires.
// Note the timeout spans the WHOLE handshake — Node clears it only in the
// HANDOVER_COMPLETE branch.
func (u *Updater) performHandshake(socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}

	result := make(chan error, 1)
	deliver := func(err error) {
		select {
		case result <- err:
		default:
		}
	}

	timer := time.AfterFunc(u.handshakeTimeout, func() {
		conn.Close()
		deliver(errors.New("Handshake timeout"))
	})
	defer timer.Stop()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				message := strings.TrimSpace(string(buf[:n]))
				switch {
				case message == "HANDSHAKE_ACK":
					// The ACK work (takeOver + readiness + stability timer)
					// must not block the read loop: HANDOVER_COMPLETE
					// arrives on this same connection.
					go u.onHandshakeAck(conn, deliver)
				case message == "HANDOVER_COMPLETE":
					timer.Stop()
					u.log.Log("Update completed successfully.")

					// We are the live instance now, not an updater. The
					// container keeps ODAC_UPDATE_MODE=true in its env for
					// the rest of its life (container env is immutable) and
					// Proxy/DNS/Mail read it on every Start to decide
					// whether to adopt an existing process. Clear it
					// in-process so a later restart adopts instead of
					// blindly respawning.
					os.Unsetenv("ODAC_UPDATE_MODE")

					// Signal the watchdog to switch to standard logs.
					fmt.Fprintln(logx.Stdout, "ODAC_CMD:SWITCH_LOGS")
					conn.Close()
					os.Remove(socketPath)
					deliver(nil)
					return
				case strings.HasPrefix(message, "HANDOVER_FAILED"):
					conn.Close()
					deliver(errors.New(message))
					return
				}
			}
			if rerr != nil {
				// If we crash or disconnect, the old instance rolls back.
				deliver(rerr)
				return
			}
		}
	}()

	if _, err := conn.Write([]byte("HANDSHAKE_READY")); err != nil {
		deliver(err)
	}

	return <-result
}

// onHandshakeAck is #performHandshake's HANDSHAKE_ACK branch: take over the
// container identity, start the services, wait for Proxy/DNS readiness,
// signal WEB_READY and arm the 12s stability timer.
func (u *Updater) onHandshakeAck(conn net.Conn, deliver func(error)) {
	u.log.Log("Ack received. Taking over & Starting stability timer (15s)...")

	// 1. Take over the container name.
	u.takeOver()

	// 2. Start services. The ready callback was registered by system.Init
	// before the handshake began (see system.Init's deviation note), so the
	// services genuinely start here — spawning fresh Proxy/DNS that overlap
	// the old instance's via SO_REUSEPORT.
	u.triggerReady()
	u.log.Log("Services started. Waiting for Proxy & DNS readiness...")

	// 3. Wait for Proxy and DNS to be fully ready (config synced).
	var proxyReady, dnsReady bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); proxyReady = u.waitReady(u.deps.Proxy) }()
	go func() { defer wg.Done(); dnsReady = u.waitReady(u.deps.DNS) }()
	wg.Wait()

	if !proxyReady || !dnsReady {
		u.log.Log("WARNING: Readiness check incomplete (Proxy: %s, DNS: %s). Proceeding anyway.", jsBool(proxyReady), jsBool(dnsReady))
	} else {
		u.log.Log("Proxy & DNS confirmed ready with config synced.")
	}

	u.log.Log("Signaling old container to stop Web...")
	if _, err := conn.Write([]byte("WEB_READY")); err != nil {
		u.log.Error("Startup failed: %s", err.Error())
		conn.Close()
		deliver(err)
		return
	}

	// 4. General system stability check, then signal completion.
	time.AfterFunc(u.stabilityDelay, func() {
		u.log.Log("General stability check passed (15s total). Signaling completion...")
		conn.Write([]byte("TAKEOVER_COMPLETE"))
	})
}

func (u *Updater) waitReady(ws WebService) bool {
	if ws == nil {
		return false
	}
	return ws.WaitForReady(u.readyTimeout)
}

// takeOver ports #takeOver: claim the 'odac' name from the previous
// container. previousName comes from ODAC_PREVIOUS_CONTAINER_NAME (2189336)
// so a host that was running under a non-canonical name — including
// 'odac-backup' after a rollback — is handled without guessing.
func (u *Updater) takeOver() {
	u.log.Log("Taking over container identity...")
	targetName := containerName
	backupName := backupContainerName
	previousName := os.Getenv("ODAC_PREVIOUS_CONTAINER_NAME")
	if previousName == "" {
		previousName = targetName
	}

	// 1. Remove the existing backup if any. When the OLD instance was
	// already running AS odac-backup (updating a rolled-back host), removing
	// "the backup" would kill it — force-remove a leftover 'odac' instead.
	if previousName != backupName {
		if err := u.deps.Docker.Remove(backupName, true); err != nil {
			if !isNotFound(err) {
				u.log.Log("Warning cleaning backup: %s", err.Error())
			}
		} else {
			u.log.Log("Previous backup removed.")
		}
	} else {
		if err := u.deps.Docker.Remove(targetName, true); err != nil {
			if !isNotFound(err) {
				u.log.Log("Warning: Could not remove leftover target container: %s", err.Error())
			}
		} else {
			u.log.Log("Leftover target container removed.")
		}
	}

	// 2. Rename the old container to backup.
	if previousName != backupName {
		if err := u.deps.Docker.Rename(previousName, backupName); err != nil {
			if !isNotFound(err) {
				// If the rename fails, remove it to clear the name.
				u.log.Log("Warning: Could not rename old container to backup: %s. Attempting force remove.", err.Error())
				if rerr := u.deps.Docker.Remove(previousName, true); rerr != nil {
					u.log.Log("Critical: Failed to remove old container: %s", rerr.Error())
				}
			}
		} else {
			u.log.Log("Old container renamed to backup.")
		}
	} else {
		u.log.Log("Old container is already named backup.")
	}

	// 3. Revoke the backup's restart policy BEFORE granting our own.
	//
	// Invariant (11a9b00): at most one container may carry a persistent
	// restart policy at any instant. Proxy/DNS/Mail bind with SO_REUSEPORT,
	// so two ODAC instances resurrected by Docker after a host reboot would
	// not collide on ports — they would silently split traffic, run two Hub
	// connections and two ACME clients. selfDestruct also revokes this, but
	// only ~12s later; a hard reboot inside that window must not find two
	// auto-starting containers. The cost is a millisecond-wide gap where no
	// container auto-starts — recoverable with `docker start odac`; a split
	// brain is not.
	if err := u.deps.Docker.UpdateRestartPolicy(backupName, "no"); err != nil {
		if !isNotFound(err) {
			u.log.Error("Failed to revoke backup restart policy: %s", err.Error())
		}
	} else {
		u.log.Log("Backup restart policy revoked.")
	}

	// 4. Rename self.
	if err := u.deps.Docker.Rename(updateContainerName, targetName); err != nil {
		u.log.Error("Failed to rename self: %s", err.Error())
		return
	}
	u.log.Log("Renamed self to %s", targetName)

	// 5. Adopt the persistent restart policy — strictly AFTER the rename:
	// until we own the 'odac' name a failed handover must stay dead rather
	// than be restarted by Docker as a half-migrated instance.
	u.ensureRestartPolicy()
}

func jsBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
