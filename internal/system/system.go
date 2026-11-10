// Package system is the orchestrator's top-level lifecycle: service start
// ordering, the 1-second check tick, and the stop semantics used by the
// zero-downtime update handshake.
//
// Nil service slots are skipped, so the skeleton runs standalone. All
// ordering and timing below is load-bearing:
//
//	Init: server.pid/started set → Updater.Init() → when ready:
//	      Proxy.Start, DNS.Start, Hub.Start; +1s: Mail.Start, Api.Start.
//	      +1s after Init (regardless of readiness): 1s tick of
//	      App.Check, SSL.Check, Proxy.Check, Mail.Check, Hub.Check.
//	Stop(exceptWeb): tick stops; Mail, Api, Hub stop always; DNS and Proxy
//	      only when exceptWeb is false (true = update-overlap mode, they keep
//	      serving via SO_REUSEPORT until the new instance takes over).
//
// Init after Stop must work: the updater's rollback path re-runs Init on a
// half-stopped instance, so services must be idempotently restartable.
package system

import (
	"os"
	"runtime"
	"sync"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// Narrow service interfaces — each service implements only what System.js
// actually calls on it.
type (
	Starter interface{ Start() }
	Stopper interface{ Stop() }
	Checker interface{ Check() }

	StartStopper interface {
		Starter
		Stopper
	}
	StartStopChecker interface {
		Starter
		Stopper
		Checker
	}
	// RestoreChecker is restored once at Init and checked on the tick: Restore
	// must run before anything starts allocating.
	RestoreChecker interface {
		Restore()
		Checker
	}
)

// Services holds the orchestrator's service slots. Nil slots are skipped
// (they fill in as tasks 3.2–3.6 land).
type Services struct {
	App   Checker          // 3.4 — check only (App has no system-level start)
	SSL   Checker          // 3.5 — check only
	Proxy StartStopChecker // 3.2
	DNS   StartStopper     // 3.2 — no check in the tick (Node parity)
	Mail  StartStopChecker // 3.2
	Api   StartStopper     // 3.3
	Hub   StartStopChecker // 3.6
	Swap  RestoreChecker   // elastic host-swap manager (Linux-only at runtime)
}

// Updater gates service startup: Init may block for the update handshake;
// OnReady fires its callback immediately when already ready. The production
// implementation is internal/updater (task 3.7).
type Updater interface {
	Init() error
	OnReady(func())
}

// System mirrors the System.js singleton.
type System struct {
	cfg     *config.Store
	svc     Services
	updater Updater
	log     *logx.Logger

	// Node's load-bearing timings (1s each); overridable in tests only.
	startupDelay time.Duration // ready → Mail/Api start
	tickDelay    time.Duration // Init → first tick setup
	tickEvery    time.Duration // check tick period

	mu       sync.Mutex
	tickStop chan struct{}
	tickDone chan struct{} // closed when the tick goroutine has returned
	timers   []*time.Timer
}

// New wires a System. Any Services slot may be nil.
func New(cfg *config.Store, svc Services, updater Updater) *System {
	return &System{
		cfg:          cfg,
		svc:          svc,
		updater:      updater,
		log:          logx.New("System"),
		startupDelay: time.Second,
		tickDelay:    time.Second,
		tickEvery:    time.Second,
	}
}

// Init records pid/started (and refreshes server.os/arch), runs the updater
// gate, and schedules service startup and the check tick.
//
// The ready callback registers BEFORE the updater gate runs. In update mode
// the updater's Init blocks until the handover completes, but ready fires at
// HANDSHAKE_ACK, and the services must start then: fresh Proxy/DNS overlap
// the old instance via SO_REUSEPORT. Registering after the gate would start
// them only after HANDOVER_COMPLETE, costing ~12s of the downtime the
// overlap exists to remove. On the normal-startup path ready triggers inside
// Init and the callback runs synchronously either way.
func (s *System) Init() error {
	s.recordServerInfo()

	s.updater.OnReady(func() {
		// Swap first, ahead of every service: after a reboot the swapfiles are
		// on disk but the kernel has no swap, and service startup is the
		// heaviest allocation burst. Inside the ready callback rather than
		// before the updater gate, because in update mode that gate is the
		// handshake — and the outgoing instance still runs its own swap manager
		// until it stops its tick at HANDSHAKE_READY. Ready fires after that,
		// so only one manager ever touches the host.
		if s.svc.Swap != nil {
			s.svc.Swap.Restore()
		}
		start(s.svc.Proxy)
		start(s.svc.DNS)
		start(s.svc.Hub)
		s.afterFunc(s.startupDelay, func() {
			start(s.svc.Mail)
			start(s.svc.Api)
		})
	})

	if err := s.updater.Init(); err != nil {
		return err
	}

	s.afterFunc(s.tickDelay, s.startTick)
	return nil
}

// Stop stops the tick first so checks cannot restart services, then Mail, Api
// and Hub; DNS and Proxy only stop when exceptWeb is false. Pending
// startup/tick timers are cancelled too, or they would restart Mail/Api/tick
// when Stop runs within the first 2s of Init.
func (s *System) Stop(exceptWeb bool) {
	s.mu.Lock()
	tickDone := s.tickDone
	if s.tickStop != nil {
		close(s.tickStop)
		s.tickStop = nil
		s.tickDone = nil
	}
	for _, t := range s.timers {
		t.Stop()
	}
	s.timers = nil
	s.mu.Unlock()

	// A tick already past its select would otherwise keep running checks
	// while the services below stop, and could respawn one of them.
	if tickDone != nil {
		<-tickDone
	}

	stop(s.svc.Mail)
	stop(s.svc.Api)
	stop(s.svc.Hub)
	if !exceptWeb {
		stop(s.svc.DNS)
		stop(s.svc.Proxy)
	}
}

func (s *System) startTick() {
	s.mu.Lock()
	if s.tickStop != nil {
		// Defensive: a second Init without Stop must not stack tickers
		// (Node would leak an interval here).
		close(s.tickStop)
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	s.tickStop = done
	s.tickDone = exited
	s.mu.Unlock()

	go func() {
		defer close(exited)
		t := time.NewTicker(s.tickEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				check(s.svc.App)
				check(s.svc.SSL)
				check(s.svc.Proxy)
				check(s.svc.Mail)
				check(s.svc.Hub)
				check(s.svc.Swap)
			}
		}
	}()
}

func (s *System) afterFunc(d time.Duration, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timers = append(s.timers, time.AfterFunc(d, fn))
}

// recordServerInfo sets server.pid and server.started (ms epoch) like
// System.init, and refreshes server.os/arch like Config.init. Values use
// Node's vocabulary (win32/x64) so the file never flip-flops between the
// two implementations while both exist.
func (s *System) recordServerInfo() {
	srv := s.cfg.Map("server")
	if srv == nil {
		srv = map[string]any{}
		s.cfg.Set("server", srv)
	}
	srv["pid"] = os.Getpid()
	srv["started"] = time.Now().UnixMilli()
	srv["os"] = nodeOS(runtime.GOOS)
	srv["arch"] = nodeArch(runtime.GOARCH)
	s.cfg.Touch("server")
}

// nodeOS maps runtime.GOOS to Node's os.platform() vocabulary.
func nodeOS(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos // linux, darwin, freebsd… already match
}

// nodeArch maps runtime.GOARCH to Node's os.arch() vocabulary.
func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return goarch // arm64, arm, riscv64… already match
	}
}

func start(v Starter) {
	if v != nil {
		v.Start()
	}
}

func stop(v Stopper) {
	if v != nil {
		v.Stop()
	}
}

func check(v Checker) {
	if v != nil {
		v.Check()
	}
}
