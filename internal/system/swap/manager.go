package swap

import (
	"fmt"
	"path/filepath"
	"time"

	"odac/internal/config"
	"odac/internal/logx"
)

// checkGate is how often Check actually does work. The system orchestrator
// calls Check on its 1s tick; this self-gate throttles the real logic to ~30s
// so a single spike never drives a decision and /proc is not read every second.
const checkGate = 30 * time.Second

// Manager is the swap service the orchestrator drives. It implements Checker:
// on each (gated) tick it reads a live snapshot, asks decide() what to do, and
// enacts the one action through the controller. State is just the streak
// counters and the gate clock, so a restart re-discovers existing increments
// from /proc/swaps on the next tick (idempotent — no separate reconcile needed).
type Manager struct {
	cfg *config.Store
	ctl controller
	log *logx.Logger
	st  counters

	now     func() time.Time // seam for tests
	gate    time.Duration
	lastRun time.Time
}

// New builds the swap Manager. On non-Linux hosts the controller is a no-op and
// snapshots are never ok, so Check holds forever — safe to wire unconditionally.
func New(cfg *config.Store) *Manager {
	return &Manager{
		cfg:  cfg,
		ctl:  newController(),
		log:  logx.New("Swap"),
		now:  time.Now,
		gate: checkGate,
	}
}

// Check is the Checker entry point invoked by the orchestrator's tick. It
// self-gates to the check interval, then runs one decide→enact cycle.
func (m *Manager) Check() {
	now := m.now()
	if !m.lastRun.IsZero() && now.Sub(m.lastRun) < m.gate {
		return
	}
	m.lastRun = now

	cfg := m.loadConfig()
	dir := m.swapDir(cfg)
	s := readSnapshot(dir)
	m.enact(decide(s, cfg, &m.st), s, cfg, dir)
}

// loadConfig reads the live system.swap object under the store's read lock;
// config can change at runtime, so it is re-read every tick.
func (m *Manager) loadConfig() Config {
	var c Config
	m.cfg.View(func() { c = configFromMap(m.cfg.Map("swap")) })
	return c
}

// swapDir is where swapfiles are created: the configured override, or
// <baseDir>/swap. The base directory is always on host-backed storage (it holds
// config and logs), so swapon accepts files there even when the process runs in
// a container whose root filesystem is overlayfs.
func (m *Manager) swapDir(cfg Config) string {
	if cfg.Dir != "" {
		return cfg.Dir
	}
	return filepath.Join(m.cfg.BaseDir(), "swap")
}

// enact applies decide's output. Failures are logged and swallowed — a failed
// grow/shrink must never crash the orchestrator tick; the next tick retries.
func (m *Manager) enact(d decision, s snapshot, cfg Config, dir string) {
	switch d.action {
	case grow:
		path := nextPath(s.areas, swapPrefix(dir))
		if err := m.ctl.open(path, d.size); err != nil {
			m.log.Error(fmt.Sprintf("grow failed: %v", err))
			return
		}
		m.log.Log(fmt.Sprintf("grew swap: %s +%d MiB (total %d→%d MiB) — %s [%s]",
			path, d.size/mib, sumAreas(s.areas)/mib, (sumAreas(s.areas)+d.size)/mib,
			d.reason, context(s)))
		m.persist(cfg, append(pathsOf(s.areas), path))
	case shrink:
		if err := m.ctl.remove(d.target); err != nil {
			m.log.Error(fmt.Sprintf("shrink failed: %v", err))
			return
		}
		m.log.Log(fmt.Sprintf("shrank swap: removed %s (total %d→%d MiB) — %s [%s]",
			d.target, sumAreas(s.areas)/mib, sumAreasExcept(s.areas, d.target)/mib,
			d.reason, context(s)))
		m.persist(cfg, without(pathsOf(s.areas), d.target))
	}
}

// context renders the deciding signals for a log line.
func context(s snapshot) string {
	availPct := int64(0)
	if s.memTotal > 0 {
		availPct = s.memAvail * 100 / s.memTotal
	}
	return fmt.Sprintf("memAvail %d%%, psi %.1f, swapFill %d%%, poolFill %d%%",
		availPct, s.psiSomeAvg10, swapFillPct(s), poolFillPct(s))
}

// persist keeps /etc/fstab's managed block in sync with the live increment set
// when persistence is enabled.
func (m *Manager) persist(cfg Config, paths []string) {
	if !cfg.Persist {
		return
	}
	if err := m.ctl.syncFstab(paths); err != nil {
		m.log.Error(fmt.Sprintf("fstab sync failed: %v", err))
	}
}

// nextPath is the path for a new increment: one past the highest existing
// index (so a just-freed top index is reused after a shrink).
func nextPath(areas []area, prefix string) string {
	max := 0
	for _, a := range areas {
		if idx := indexOf(a.path, prefix); idx > max {
			max = idx
		}
	}
	return fmt.Sprintf("%s%d", prefix, max+1)
}

func sumAreasExcept(areas []area, drop string) int64 {
	var t int64
	for _, a := range areas {
		if a.path != drop {
			t += a.size
		}
	}
	return t
}

func pathsOf(areas []area) []string {
	paths := make([]string, len(areas))
	for i, a := range areas {
		paths[i] = a.path
	}
	return paths
}

func without(paths []string, drop string) []string {
	out := paths[:0:0]
	for _, p := range paths {
		if p != drop {
			out = append(out, p)
		}
	}
	return out
}
