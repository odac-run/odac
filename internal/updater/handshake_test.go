package updater

// The socket-handshake half of the suite: jest's takeOver-invariant,
// rollback-safety and env-accumulation tests (real unix sockets, hand-
// scripted counterparty), plus Go-only full-protocol tests running BOTH
// real roles against one fake daemon — the cross-version cutover path has
// no jest equivalent, so these pin it.

import (
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptOldSide hand-scripts the OLD container's listener (the documented
// protocol) so the real NEW-side code under test is the only real thing,
// mirroring the jest invariant test. onMessage returns the reply to send
// ("" = none) and whether to stop reading.
func scriptOldSide(t *testing.T, socketPath string, onMessage func(msg string, conn net.Conn) bool) (accepted <-chan net.Conn, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	connCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		connCh <- conn
		buf := make([]byte, 4096)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				if done := onMessage(strings.TrimSpace(string(buf[:n])), conn); done {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	return connCh, func() { ln.Close() }
}

// --- zero-downtime takeOver invariant (jest port) ---

func TestTakeOverNeverLeavesTwoAutostartContainers(t *testing.T) {
	fx := newFixture(t)
	t.Setenv("ODAC_PREVIOUS_CONTAINER_NAME", "")
	fx.u.stabilityDelay = time.Hour // TAKEOVER_COMPLETE never fires here

	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}        // old, live
	fx.w.world["odac-update"] = &fakeContainer{policy: "no", running: true}             // new = the real Updater under test
	persistent := func(p string) bool { return p == "always" || p == "unless-stopped" } //

	type snapshot struct {
		label     string
		autostart []string
	}
	var snapMu sync.Mutex
	var snapshots []snapshot
	snap := func(label string) {
		var auto []string
		for name, c := range fx.w.world {
			if persistent(c.policy) && c.running {
				auto = append(auto, name)
			}
		}
		snapMu.Lock()
		snapshots = append(snapshots, snapshot{label, auto})
		snapMu.Unlock()
	}
	snap("before")
	fx.w.onEvent = snap // runs under the world mutex, after every mutation

	webReady := make(chan struct{})
	_, cleanup := scriptOldSide(t, fx.u.socketPath(), func(msg string, conn net.Conn) bool {
		switch msg {
		case "HANDSHAKE_READY":
			conn.Write([]byte("HANDSHAKE_ACK"))
		case "WEB_READY":
			close(webReady)
			return true
		}
		return false
	})
	defer cleanup()

	initDone := make(chan error, 1)
	go func() { initDone <- fx.u.Init() }() // NEW joining: real handshake + real takeOver

	select {
	case <-webReady:
	case <-time.After(3 * time.Second):
		t.Fatal("WEB_READY never arrived")
	}

	snapMu.Lock()
	defer snapMu.Unlock()
	for _, s := range snapshots {
		if len(s.autostart) > 1 {
			t.Fatalf("snapshot %q: %d containers can auto-start (%v) — a reboot here splits traffic", s.label, len(s.autostart), s.autostart)
		}
	}
	last := snapshots[len(snapshots)-1]
	if len(last.autostart) != 1 || last.autostart[0] != "odac" {
		t.Fatalf("final autostart = %v, want [odac]", last.autostart)
	}
	if c := fx.w.container("odac"); c == nil || c.policy != "unless-stopped" {
		t.Fatalf("odac = %+v, want unless-stopped", c)
	}
	if c := fx.w.container("odac-backup"); c == nil || c.policy != "no" {
		t.Fatalf("odac-backup = %+v, want policy no", c)
	}
	if fx.w.has("odac-update") {
		t.Fatal("odac-update must be gone after takeOver")
	}
	_ = initDone // Init still blocks awaiting HANDOVER_COMPLETE; the test ends here like jest's
}

// --- rollback safety (jest port) ---

// startExecute launches execute() with the fixture's System.Init wired to
// the real Updater.Init — the self-handshake regression's precondition —
// and waits for the update container and socket to appear.
func startExecute(t *testing.T, fx *fixture) <-chan error {
	t.Helper()
	fx.sys.initFn = fx.u.Init

	execDone := make(chan error, 1)
	go func() { execDone <- fx.u.execute() }()

	waitUntil(t, "odac-update creation", func() bool { return fx.w.hasEventPrefix("create:odac-update") })
	waitUntil(t, "update socket", func() bool {
		_, err := os.Stat(fx.u.socketPath())
		return err == nil
	})
	return execDone
}

func dialHandshake(t *testing.T, fx *fixture, onAck func(conn net.Conn)) {
	t.Helper()
	conn, err := net.Dial("unix", fx.u.socketPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("HANDSHAKE_READY")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 && strings.TrimSpace(string(buf[:n])) == "HANDSHAKE_ACK" {
			onAck(conn)
			return
		}
		if rerr != nil {
			t.Fatalf("no ACK: %v", rerr)
		}
	}
}

func TestRollbackAfterTakeOverCrashDoesNotSelfHandshake(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		env:   []string{"ODAC_CHANNEL=stable"},
		binds: []string{"/host/.odac:/app/.odac"},
	}

	execDone := startExecute(t, fx)

	dialHandshake(t, fx, func(conn net.Conn) {
		// Perform the real takeOver semantics as the new container would,
		// then crash before TAKEOVER_COMPLETE.
		if err := fx.w.Rename("odac", "odac-backup"); err != nil {
			t.Error(err)
		}
		if err := fx.w.UpdateRestartPolicy("odac-backup", "no"); err != nil {
			t.Error(err)
		}
		if err := fx.w.Rename("odac-update", "odac"); err != nil {
			t.Error(err)
		}
		if err := fx.w.UpdateRestartPolicy("odac", "unless-stopped"); err != nil {
			t.Error(err)
		}
		conn.Close()
	})

	if err := <-execDone; err == nil {
		t.Fatal("execute must fail after the premature disconnect")
	}

	// The critical assertion: rollback must not have handshaked with itself
	// and undone the takeover. If it had, only 'odac-backup' would remain.
	c := fx.w.container("odac")
	if c == nil {
		t.Fatal("no container named odac after rollback")
	}
	if c.policy != "unless-stopped" || !c.running {
		t.Fatalf("odac = %+v, want running with unless-stopped", c)
	}
	// The live container the rollback ran inside must never be force-removed.
	if fx.w.hasEvent("remove:odac") {
		t.Fatalf("rollback force-removed the live odac container; events = %v", fx.w.eventList())
	}
}

func TestRollbackBeforeTakeOverCrashKeepsLiveOdac(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		env:   []string{"ODAC_CHANNEL=stable"},
		binds: []string{"/host/.odac:/app/.odac"},
	}

	execDone := startExecute(t, fx)

	dialHandshake(t, fx, func(conn net.Conn) {
		conn.Close() // crash immediately, before anything takeOver would do
	})

	if err := <-execDone; err == nil {
		t.Fatal("execute must fail after the premature disconnect")
	}

	// Pre-11a9b00 behavior: rollback force-removed whatever was named
	// 'odac' — at this point the live container itself.
	if fx.w.hasEvent("remove:odac") {
		t.Fatalf("rollback force-removed the live odac container; events = %v", fx.w.eventList())
	}
	c := fx.w.container("odac")
	if c == nil || !c.running || c.policy != "unless-stopped" {
		t.Fatalf("odac = %+v, want live with unless-stopped", c)
	}
	if fx.w.has("odac-update") {
		t.Fatal("the failed odac-update must be removed by the rollback")
	}
}

// --- update env accumulation (jest port) ---

func TestExecuteStripsAllUpdateCycleEnvVars(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		env: []string{
			"PATH=/usr/bin",
			"ODAC_CHANNEL=stable",
			// A host that went through the pre-11a9b00 update path at least
			// once: these should have been stripped and weren't.
			"ODAC_UPDATE_MODE=true",
			"ODAC_INSTANCE_ID=stale-instance",
			"ODAC_PREVIOUS_INSTANCE_ID=stale-previous",
			"ODAC_UPDATE_SOCKET_PATH=/stale/path.sock",
			"ODAC_LOG_NAME=.odac-update",
			// v1.11.0 addition to UPDATE_ENV_KEYS.
			"ODAC_PREVIOUS_CONTAINER_NAME=stale-name",
		},
		binds: []string{"/host/.odac:/app/.odac"},
	}

	execDone := startExecute(t, fx)

	newEnv := fx.w.container("odac-update").env
	countOf := func(key string) int {
		n := 0
		for _, e := range newEnv {
			if strings.HasPrefix(e, key+"=") {
				n++
			}
		}
		return n
	}
	for _, key := range updateEnvKeys {
		if got := countOf(key); got != 1 {
			t.Errorf("%s appears %d times in the new env, want exactly 1 (env: %v)", key, got, newEnv)
		}
	}
	for _, e := range newEnv {
		if e == "ODAC_INSTANCE_ID=stale-instance" {
			t.Error("the stale instance id leaked into the new env")
		}
		if e == "ODAC_PREVIOUS_CONTAINER_NAME=stale-name" {
			t.Error("the stale previous-container name leaked into the new env")
		}
	}
	has := func(v string) bool {
		for _, e := range newEnv {
			if e == v {
				return true
			}
		}
		return false
	}
	if !has("PATH=/usr/bin") || !has("ODAC_CHANNEL=stable") {
		t.Errorf("inherited env lost non-cycle vars: %v", newEnv)
	}
	// v1.11.0: the fresh handover vars point at THIS container.
	if !has("ODAC_PREVIOUS_CONTAINER_NAME=odac") {
		t.Errorf("ODAC_PREVIOUS_CONTAINER_NAME must carry the resolved self name; env: %v", newEnv)
	}
	if !has("ODAC_LOG_NAME=.odac-update") || !has("ODAC_UPDATE_MODE=true") {
		t.Errorf("missing handover env vars: %v", newEnv)
	}

	// Unblock execute() so the test leaves no dangling listener.
	dialHandshake(t, fx, func(conn net.Conn) { conn.Close() })
	if err := <-execDone; err == nil {
		t.Fatal("execute should report the rolled-back handover")
	}
}

// --- full protocol, both real roles (Go-only spec: the cutover path) ---

func TestFullHandshakeBothRolesRealAgainstOneDaemon(t *testing.T) {
	// OLD side: real execute() + listener. NEW side: a second real Updater
	// sharing the same fake daemon and socket — exactly the cutover
	// topology, minus the process boundary.
	fxOld := newFixture(t)
	fxOld.w.world["odac"] = &fakeContainer{
		policy: "unless-stopped", running: true,
		env:   []string{"PATH=/usr/bin", "ODAC_CHANNEL=stable"},
		binds: []string{"/host/.odac:/app/.odac"},
	}
	out := captureStdout(t)

	execDone := startExecute(t, fxOld)

	// Build the NEW instance over the same world and the same baseDir (the
	// shared ~/.odac volume), with the env execute() computed for it.
	newEnv := fxOld.w.container("odac-update").env
	for _, e := range newEnv {
		if k, v, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "ODAC_") {
			t.Setenv(k, v)
		}
	}

	fxNew := &fixture{
		w:     fxOld.w,
		sys:   &fakeSystem{},
		proxy: &fakeWeb{name: "proxy", ready: true},
		dns:   &fakeWeb{name: "dns", ready: true},
	}
	uNew := New(fxOld.u.baseDir, Deps{Docker: fxNew.w, Proxy: fxNew.proxy, DNS: fxNew.dns})
	uNew.SetSystem(fxNew.sys)
	uNew.platform = "linux"
	uNew.hostname = func() string { return "test-host" }
	uNew.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	uNew.inContainer = func() bool { return true }
	uNew.exit = func(int) {}
	uNew.handshakeTimeout = 3 * time.Second
	uNew.stabilityDelay = 30 * time.Millisecond
	uNew.readyTimeout = 50 * time.Millisecond
	fxNew.u = uNew

	// system.Init registers the ready callback BEFORE the updater gate runs
	// (the documented 3.7 deviation) — mirror that here.
	servicesStarted := make(chan struct{})
	uNew.OnReady(func() { close(servicesStarted) })

	if err := uNew.Init(); err != nil {
		t.Fatalf("NEW Init failed: %v", err)
	}

	if err := <-execDone; err != nil {
		t.Fatalf("OLD execute failed: %v", err)
	}

	select {
	case <-servicesStarted:
	default:
		t.Fatal("the NEW instance's services never started")
	}
	if !uNew.IsUpdateMode() {
		t.Fatal("NEW must report update mode after a successful handshake")
	}

	// OLD exits 0 after the destruct delay.
	select {
	case code := <-fxOld.exited:
		if code != 0 {
			t.Fatalf("OLD exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OLD never self-destructed")
	}

	// End state: the new container owns 'odac' with the persistent policy,
	// the old one is the powerless backup, odac-update is gone.
	if c := fxOld.w.container("odac"); c == nil || c.policy != "unless-stopped" {
		t.Fatalf("odac = %+v", c)
	}
	if c := fxOld.w.container("odac-backup"); c == nil || c.policy != "no" {
		t.Fatalf("odac-backup = %+v", c)
	}
	if fxOld.w.has("odac-update") {
		t.Fatal("odac-update must be renamed away")
	}

	// OLD's service teardown: Stop(exceptWeb=true) at HANDSHAKE_READY, then
	// Proxy/DNS at WEB_READY and again in selfDestruct.
	calls := fxOld.sys.callList()
	if len(calls) == 0 || calls[0] != "stop:exceptWeb=true" {
		t.Fatalf("OLD system calls = %v, want stop:exceptWeb=true first", calls)
	}
	if fxOld.proxy.stopCount() < 2 || fxOld.dns.stopCount() < 2 {
		t.Fatalf("OLD proxy/dns stops = %d/%d, want 2 each (WEB_READY + selfDestruct)", fxOld.proxy.stopCount(), fxOld.dns.stopCount())
	}

	// NEW cleared the in-process update marker and told the watchdog to
	// switch logs; the socket is gone.
	if os.Getenv("ODAC_UPDATE_MODE") != "" {
		t.Fatal("ODAC_UPDATE_MODE must be cleared in-process after HANDOVER_COMPLETE")
	}
	if !strings.Contains(out(), "ODAC_CMD:SWITCH_LOGS") {
		t.Fatal("SWITCH_LOGS marker missing")
	}
	if _, err := os.Stat(fxOld.u.socketPath()); err == nil {
		t.Fatal("update socket must be unlinked after the handover")
	}
}

// --- handshake failure paths ---

func TestInitStaleSocketFallsBackToNormalStartup(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}
	// A socket file nobody listens on (a crashed OLD left it behind).
	stale, err := net.Listen("unix", fx.u.socketPath())
	if err != nil {
		t.Fatal(err)
	}
	stale.Close() // closing unlinks on some platforms — recreate as a plain file if gone
	if _, err := os.Stat(fx.u.socketPath()); err != nil {
		if err := os.WriteFile(fx.u.socketPath(), nil, 0o666); err != nil {
			t.Fatal(err)
		}
	}

	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if fx.u.IsUpdateMode() {
		t.Fatal("a stale socket must not put the instance in update mode")
	}
	if _, err := os.Stat(fx.u.socketPath()); err == nil {
		t.Fatal("the stale socket must be unlinked")
	}
	ready := false
	fx.u.OnReady(func() { ready = true })
	if !ready {
		t.Fatal("normal startup must trigger ready")
	}
}

func TestInitHandoverFailedFallsBackToNormalStartup(t *testing.T) {
	fx := newFixture(t)
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}

	_, cleanup := scriptOldSide(t, fx.u.socketPath(), func(msg string, conn net.Conn) bool {
		if msg == "HANDSHAKE_READY" {
			conn.Write([]byte("HANDOVER_FAILED:boom"))
			return true
		}
		return false
	})
	defer cleanup()

	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if fx.u.IsUpdateMode() {
		t.Fatal("a failed handover must not leave update mode set")
	}
}

func TestInitHandshakeTimeoutFallsBackToNormalStartup(t *testing.T) {
	fx := newFixture(t)
	fx.u.handshakeTimeout = 50 * time.Millisecond
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}

	// An OLD that accepts but never answers.
	_, cleanup := scriptOldSide(t, fx.u.socketPath(), func(string, net.Conn) bool { return false })
	defer cleanup()

	start := time.Now()
	if err := fx.u.Init(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Init took %v, the 50ms handshake timeout did not fire", elapsed)
	}
	if fx.u.IsUpdateMode() {
		t.Fatal("a timed-out handshake must not leave update mode set")
	}
}

func TestExecuteGlobalTimeoutRollsBackAndKeepsSocket(t *testing.T) {
	fx := newFixture(t)
	fx.u.globalTimeout = 60 * time.Millisecond
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true}

	err := fx.u.execute() // nobody ever connects
	if err == nil || !strings.Contains(err.Error(), "Update process timed out globally") {
		t.Fatalf("err = %v, want the global timeout", err)
	}
	if fx.w.has("odac-update") {
		t.Fatal("the never-started handover must clean up odac-update")
	}
	// Node does NOT unlink the socket on the global timeout — the restarted
	// instance treats it as stale. Pin that.
	if _, serr := os.Stat(fx.u.socketPath()); serr != nil {
		t.Fatal("the socket must be left in place on the global timeout (stale-socket recovery path)")
	}
}

func TestStartRunsDetachedAndReleasesLatchOnFailure(t *testing.T) {
	// 'Update process started' answers immediately; the detached run fails
	// (build mode without a host bind) and must release the latch.
	fx := newFixture(t)
	t.Setenv("ODAC_CHANNEL", "beta")
	fx.w.world["odac"] = &fakeContainer{policy: "unless-stopped", running: true} // no binds

	r, err := fx.u.Start()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != true || r.Message != "Update process started" {
		t.Fatalf("got %+v", r)
	}
	waitUntil(t, "latch release", func() bool {
		fx.u.mu.Lock()
		defer fx.u.mu.Unlock()
		return !fx.u.updating
	})
}
