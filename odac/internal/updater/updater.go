// Package updater is the Go port of server/src/System/Updater.js (task 3.7):
// zero-downtime self-update per docs/migration/contracts/lifecycle.md. It
// implements BOTH handshake roles byte-exact — at cutover a Go NEW instance
// handshakes with a Node OLD one (and reversed under rollback) over
// ~/.odac/run/update.sock with the 60s/300s/12s load-bearing timings.
//
// Deviations from Node (deliberate, see STATE.md's 3.7 entry):
//   - docker CLI exec calls (pull, cp, build) are unified on the Docker SDK /
//     a socket-mounted docker:cli runner — sanctioned by lifecycle.md's
//     migration notes; the image-ID comparison semantics (.Image of the
//     container vs .Id of the image tag) are preserved.
//   - Start() clears the #updating latch when the update-availability check
//     fails (Node's throw left it latched forever, blocking every later
//     update attempt until a restart).
//   - New-container log streaming reads the TTY stream as-is; Node demuxes a
//     non-multiplexed stream, corrupting the first bytes of each chunk
//     (cosmetic — the lines are only relayed into the old instance's log).
package updater

import (
	"archive/tar"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"odac/internal/api"
	"odac/internal/logx"
)

const (
	containerName       = "odac"
	updateContainerName = "odac-update"
	backupContainerName = "odac-backup"
	runnerImage         = "docker:cli"
	defaultImage        = "odacrun/odac:latest"
)

// updateEnvKeys are the env vars the update cycle injects into the incoming
// container. They describe a single handover, so they are stripped from the
// inherited env before the next one is built — otherwise every update
// appends another copy.
var updateEnvKeys = []string{
	"ODAC_UPDATE_MODE",
	"ODAC_INSTANCE_ID",
	"ODAC_PREVIOUS_INSTANCE_ID",
	"ODAC_UPDATE_SOCKET_PATH",
	"ODAC_LOG_NAME",
	"ODAC_PREVIOUS_CONTAINER_NAME",
}

// SystemControl is the System.js surface the updater drives: Stop(exceptWeb)
// during the handover and re-Init on the rollback path.
type SystemControl interface {
	Init() error
	Stop(exceptWeb bool)
}

// WebService is the Proxy/DNS surface used during the handover overlap.
type WebService interface {
	Stop()
	WaitForReady(timeout time.Duration) bool
}

// Deps carries the updater's collaborators. System is set later via
// SetSystem — system.New needs the updater, closing the same construction
// cycle Node's DI resolved lazily.
type Deps struct {
	Docker Docker
	Proxy  WebService
	DNS    WebService
}

// Updater mirrors the Updater.js singleton. It implements system.Updater.
type Updater struct {
	baseDir string
	deps    Deps
	log     *logx.Logger
	image   string

	sysMu sync.Mutex
	sys   SystemControl

	// Test seams. Production values: runtime.GOOS, os.Hostname,
	// os.ReadFile, the /.dockerenv stat, os.Exit and Node's load-bearing
	// timings (60s handshake, 300s global, 12s stability, 1s destruct
	// delay, 10s readiness).
	platform         string
	hostname         func() string
	readFile         func(string) ([]byte, error)
	inContainer      func() bool
	exit             func(int)
	handshakeTimeout time.Duration
	globalTimeout    time.Duration
	stabilityDelay   time.Duration
	destructDelay    time.Duration
	readyTimeout     time.Duration

	mu           sync.Mutex
	updating     bool
	isUpdateMode bool
	ready        bool
	callbacks    []func()
}

// New wires an Updater for a base dir (usually ~/.odac). A nil deps.Docker
// degrades to an always-failing client, like dockerode on a socket-less host.
//
// ODAC_IMAGE overrides the update image tag (no Node equivalent — Node
// hardcodes it). It exists for the 3.8 staging cutover, where the whole
// pull → compare → create pipeline must run for real against a local
// registry instead of Docker Hub. Unset in production.
func New(baseDir string, deps Deps) *Updater {
	if deps.Docker == nil {
		deps.Docker = unavailableDocker{}
	}
	image := os.Getenv("ODAC_IMAGE")
	if image == "" {
		image = defaultImage
	}
	return &Updater{
		baseDir:          baseDir,
		deps:             deps,
		log:              logx.New("Updater"),
		image:            image,
		platform:         runtime.GOOS,
		hostname:         func() string { h, _ := os.Hostname(); return h },
		readFile:         os.ReadFile,
		inContainer:      func() bool { _, err := os.Stat("/.dockerenv"); return err == nil },
		exit:             os.Exit,
		handshakeTimeout: 60 * time.Second,
		globalTimeout:    300 * time.Second,
		stabilityDelay:   12 * time.Second,
		destructDelay:    time.Second,
		readyTimeout:     10 * time.Second,
	}
}

// SetSystem closes the System↔Updater construction cycle.
func (u *Updater) SetSystem(sys SystemControl) {
	u.sysMu.Lock()
	u.sys = sys
	u.sysMu.Unlock()
}

func (u *Updater) system() SystemControl {
	u.sysMu.Lock()
	defer u.sysMu.Unlock()
	return u.sys
}

func (u *Updater) channel() string {
	if c := os.Getenv("ODAC_CHANNEL"); c != "" {
		return c
	}
	return "stable"
}

func (u *Updater) isBuildMode() bool {
	c := u.channel()
	return c != "stable" && c != "latest"
}

func (u *Updater) targetBranch() string {
	if u.channel() == "beta" {
		return "dev"
	}
	return u.channel()
}

func (u *Updater) socketPath() string {
	return filepath.Join(u.baseDir, "run", "update.sock")
}

func (u *Updater) downloadPath() string {
	return filepath.Join(u.baseDir, "tmp", "odac_source")
}

// Init ports Updater.init(): an existing update socket means we are the NEW
// instance of an update — attempt the handshake (blocking until the handover
// completes); a failed handshake unlinks the stale socket and continues as a
// normal startup with the 11a9b00 self-heal steps.
func (u *Updater) Init() error {
	socketPath := u.socketPath()

	if _, err := os.Stat(socketPath); err == nil {
		u.log.Log("Update socket found. Attempting handshake with previous process...")
		if err := u.performHandshake(socketPath); err == nil {
			u.mu.Lock()
			u.isUpdateMode = true
			u.mu.Unlock()
			// Handshake successful — it already triggered ready.
			return nil
		} else {
			u.log.Log("Handshake failed or stale socket: %s. Continuing as normal startup.", err.Error())
			os.Remove(socketPath)
		}
	}

	// Normal startup (not updating or failed update check). After a crashed
	// update attempt was restarted, switch the watchdog back to standard logs.
	if strings.Contains(os.Getenv("ODAC_LOG_NAME"), "odac-update") {
		// Bare marker, no module prefix — the watchdog scans raw stdout.
		fmt.Fprintln(logx.Stdout, "ODAC_CMD:SWITCH_LOGS")
	}

	// Self-heal a host left inconsistent by a crashed or rolled-back update.
	// Identity first: ensureRestartPolicy looks the container up by name, so
	// the name has to be ours again before the policy can be repaired. Both
	// are idempotent and cheap — a no-op on healthy hosts.
	u.ensureIdentity()
	u.ensureRestartPolicy()

	u.triggerReady()
	return nil
}

// OnReady ports Updater.onReady: immediate call when already ready.
func (u *Updater) OnReady(cb func()) {
	u.mu.Lock()
	if u.ready {
		u.mu.Unlock()
		cb()
		return
	}
	u.callbacks = append(u.callbacks, cb)
	u.mu.Unlock()
}

func (u *Updater) triggerReady() {
	u.mu.Lock()
	if u.ready {
		u.mu.Unlock()
		return
	}
	u.ready = true
	cbs := u.callbacks
	u.callbacks = nil
	u.mu.Unlock()
	// Node wraps each callback in try/catch. Go policy (PLAN.md 3.1 trap
	// note): no recover — a panicking startup callback crashes the process.
	for _, cb := range cbs {
		cb()
	}
}

// IsUpdateMode ports Updater.check(): are we the NEW instance of an update?
func (u *Updater) IsUpdateMode() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.isUpdateMode
}

// Start ports Updater.start(). The three result strings are CLI-visible
// parity (lifecycle.md). A returned error renders as result(false, message)
// at the api/hub boundary, matching Node's thrown-exception path.
func (u *Updater) Start() (api.Result, error) {
	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		u.log.Log("Update request blocked: Update already in progress")
		return api.Res(false, "Update already in progress"), nil
	}
	u.updating = true
	u.mu.Unlock()

	available, err := u.checkForUpdates()
	if err != nil {
		// Deviation from Node: clear the latch — Node's throw here left
		// #updating true forever, blocking every later update attempt.
		u.setUpdating(false)
		return api.Result{}, err
	}
	if !available {
		u.log.Log("System is up to date.")
		u.setUpdating(false)
		return api.Res(true, "System is up to date"), nil
	}
	go func() { // Node: setTimeout(…, 1) — the update runs detached
		err := u.download()
		if err == nil {
			err = u.execute()
		}
		if err != nil {
			u.setUpdating(false)
			// Node parity: Log.js error() never substitutes %s (console.error
			// formats its module-prefix argument), so the marker stays literal.
			u.log.Error("Update process failed: %s", err.Error())
		}
	}()
	return api.Res(true, "Update process started"), nil
}

func (u *Updater) setUpdating(v bool) {
	u.mu.Lock()
	u.updating = v
	u.mu.Unlock()
}

func (u *Updater) checkForUpdates() (bool, error) {
	if u.isBuildMode() {
		u.log.Log("Custom channel detected (%s). Forcing update check to true.", u.channel())
		return true, nil
	}

	u.log.Log("Checking for updates...")
	localID := u.getLocalImageID()

	u.log.Log(fmt.Sprintf("Pulling %s...", u.image))
	// Pull the image to ensure we have the latest metadata and layers.
	if err := u.deps.Docker.Pull(u.image); err != nil {
		return false, err
	}

	remoteID := u.getRemoteImageID()

	if localID == "" || remoteID == "" {
		u.log.Log("Failed to determine image IDs. Local: %s, Remote: %s", jsStr(localID), jsStr(remoteID))
		return false, nil
	}

	if localID == remoteID {
		u.log.Log("Image is up to date (%s)", prefix12(localID))
		return false, nil
	}

	u.log.Log("Update available! Local: %s, Remote: %s", prefix12(localID), prefix12(remoteID))
	return true, nil
}

// getLocalImageID ports #getLocalImageId: the image ID of the container we
// run inside (resolved self name, falling back to the literal 'odac') via an
// SDK inspect — the .Image field, not the tag's ID.
func (u *Updater) getLocalImageID() string {
	selfName := u.resolveSelfName()
	if selfName == "" {
		selfName = containerName
	}
	info, err := u.deps.Docker.Inspect(selfName)
	if err != nil {
		u.log.Log("Could not get local image ID: %s", err.Error())
		return ""
	}
	return info.Image
}

// getRemoteImageID ports #getRemoteImageId: the local image store's ID for
// the update tag (after the pull refreshed it).
func (u *Updater) getRemoteImageID() string {
	id, err := u.deps.Docker.ImageID(u.image)
	if err != nil {
		u.log.Log("Could not get remote image ID: %s", err.Error())
		return ""
	}
	return id
}

var (
	shortIDPattern   = regexp.MustCompile(`^[0-9a-f]{12}$`)
	mountinfoPattern = regexp.MustCompile(`/docker/containers/([0-9a-f]{64})`)
	cgroupPattern    = regexp.MustCompile(`[-/]docker[-/]([0-9a-f]{64})`)
)

// resolveSelfName ports #resolveSelfName: best-effort resolution of the name
// of the container we are running inside. Probe order (2189336): hostname
// first — a bridge-network container's default hostname is its short id
// (useless for the update container, which runs with NetworkMode host) —
// then the id Docker injects into our mount table and cgroup, then
// "whichever update-cycle container is running". Returns "" when every
// probe fails (Node: null).
func (u *Updater) resolveSelfName() string {
	if hn := u.hostname(); shortIDPattern.MatchString(hn) {
		if info, err := u.deps.Docker.Inspect(hn); err == nil {
			return strings.TrimPrefix(info.Name, "/")
		}
	}

	probes := []struct {
		file    string
		pattern *regexp.Regexp
	}{
		{"/proc/self/mountinfo", mountinfoPattern},
		{"/proc/self/cgroup", cgroupPattern},
	}
	for _, p := range probes {
		content, err := u.readFile(p.file)
		if err != nil {
			continue // probe unavailable on this kernel/storage driver
		}
		m := p.pattern.FindSubmatch(content)
		if m == nil {
			continue
		}
		info, err := u.deps.Docker.Inspect(string(m[1]))
		if err != nil {
			continue
		}
		return strings.TrimPrefix(info.Name, "/")
	}

	// Fallback: the live ODAC process necessarily inhabits one of the
	// update-cycle containers, and only one of them is running — us.
	for _, name := range []string{containerName, backupContainerName, updateContainerName} {
		if info, err := u.deps.Docker.Inspect(name); err == nil && info.Running {
			return name
		}
	}

	return ""
}

// ensureIdentity ports #ensureIdentity: reclaims the 'odac' container name
// when a crashed or rolled-back update left it unowned. execute() resolves
// the current container strictly by name, so a host stranded under
// 'odac-backup' could never self-update again without this.
func (u *Updater) ensureIdentity() {
	if !u.inContainer() {
		return
	}

	if _, err := u.deps.Docker.Inspect(containerName); err == nil {
		return // The name is owned; nothing to reclaim.
	} else if !isNotFound(err) {
		u.log.Error("Failed to inspect %s: %s", containerName, err.Error())
		return
	}

	selfName := u.resolveSelfName()
	if selfName == "" || selfName == containerName {
		u.log.Error("No container owns \"%s\" and self-identification failed. Rename manually.", containerName)
		return
	}

	if err := u.deps.Docker.Rename(selfName, containerName); err != nil {
		u.log.Error("Failed to reclaim identity from \"%s\": %s", selfName, err.Error())
		return
	}
	u.log.Log("Identity reclaimed: \"%s\" renamed to \"%s\".", selfName, containerName)
}

// ensureRestartPolicy ports #ensureRestartPolicy: the live 'odac' container
// must carry a persistent restart policy. The zero-downtime path creates the
// incoming container with 'no' so Docker cannot resurrect it if the handover
// fails; once it renames itself to 'odac' it must adopt the persistent
// policy (docker rename moves the name, not the HostConfig). Only a
// missing/'no' policy is repaired — an operator's deliberate 'always' stays.
func (u *Updater) ensureRestartPolicy() {
	if !u.inContainer() {
		return
	}

	info, err := u.deps.Docker.Inspect(containerName)
	if err != nil {
		if isNotFound(err) {
			return
		}
		u.log.Error("Failed to ensure restart policy: %s", err.Error())
		return
	}
	current := info.RestartPolicy
	if current == "" {
		current = "no"
	}
	if current != "no" {
		return
	}

	if err := u.deps.Docker.UpdateRestartPolicy(containerName, "unless-stopped"); err != nil {
		if isNotFound(err) {
			return
		}
		u.log.Error("Failed to ensure restart policy: %s", err.Error())
		return
	}
	u.log.Log(`Restart policy repaired: "no" -> "unless-stopped"`)
}

// prefix12 renders Node's id.substring(0, 12) log shorthand.
func prefix12(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// jsStr renders the empty string the way Node's %s printed null.
func jsStr(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

// randomUUID is crypto.randomUUID: a version-4 UUID.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// firstTarFile extracts the first regular file from a docker-cp tar stream.
func firstTarFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}
