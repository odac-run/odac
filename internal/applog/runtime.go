package applog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeControl is the control object returned by NewRuntimeStream —
// Node's createRuntimeStream return value: a daily-rotated append writer
// with a size cap and an error-hour histogram.
type RuntimeControl struct {
	logger  *Logger
	logPath string

	// All fields below are guarded by the logger's mu; file writes are
	// serialized under it too (Node relied on the single-threaded loop).
	file            *os.File
	lastWrite       time.Time
	ended           bool
	stats           runtimeStats
	bytesSinceCheck int64
}

// NewRuntimeStream opens (appending) today's runtime/<YYYY-MM-DD>.log,
// loads/rolls stats.json for the new day, and sweeps runtime logs older
// than 7 days.
func (l *Logger) NewRuntimeStream() (*RuntimeControl, error) {
	today := l.now().UTC().Format("2006-01-02")
	logPath := filepath.Join(l.runtimeDir, today+".log")
	statsPath := filepath.Join(l.runtimeDir, "stats.json")

	stats := runtimeStats{Date: today, Today: make([]int, 24), Yesterday: make([]int, 24)}
	if raw, err := os.ReadFile(statsPath); err == nil {
		var parsed runtimeStats
		if err := json.Unmarshal(raw, &parsed); err == nil {
			if parsed.Date == today {
				stats = parsed
			} else if parsed.Date != "" {
				// Day changed: rotate explicitly and persist the roll.
				stats.Yesterday = parsed.Today
				if stats.Yesterday == nil {
					stats.Yesterday = make([]int, 24)
				}
				if rolled, err := json.Marshal(stats); err == nil {
					os.WriteFile(statsPath, rolled, 0o644)
				}
			}
		}
	}

	last := l.lastRuntimeWrite()
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	l.pruneRuntimeLogs()

	return &RuntimeControl{logger: l, logPath: logPath, file: file, lastWrite: last, stats: stats}, nil
}

// lastRuntimeWrite is when a runtime log of this app was last written, or
// the zero time when none holds anything.
func (l *Logger) lastRuntimeWrite() time.Time {
	var last time.Time
	entries, err := os.ReadDir(l.runtimeDir)
	if err != nil {
		return last
	}
	for _, de := range entries {
		n := de.Name()
		if !strings.HasSuffix(n, ".log") && !strings.HasSuffix(n, ".log.1") {
			continue
		}
		if info, err := de.Info(); err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.ModTime().After(last) {
			last = info.ModTime()
		}
	}
	return last
}

// Path returns the on-disk runtime log path for the day the stream opened.
func (r *RuntimeControl) Path() string { return r.logPath }

// LastWrite is when the app's runtime log was last written before this
// stream opened (zero for none). A stream attached to a container that was
// already running reads the container's output from there on: from its
// start, every restart of ODAC appended the container's whole log again.
func (r *RuntimeControl) LastWrite() time.Time { return r.lastWrite }

// Write appends a stdout chunk: broadcast, file write, size-rotation check.
func (r *RuntimeControl) Write(p []byte) {
	l := r.logger
	l.mu.Lock()
	if r.ended {
		l.mu.Unlock()
		return
	}
	r.file.Write(p)
	r.maybeRotateLocked(int64(len(p)))
	l.mu.Unlock()

	l.notify(Runtime, "out", string(p), l.now().UnixMilli())
}

// Error appends a stderr chunk: broadcast, error-hour histogram update
// (with day roll + throttled stats.json save), file write, rotation check.
func (r *RuntimeControl) Error(p []byte) {
	l := r.logger
	now := l.now()

	l.mu.Lock()
	if r.ended {
		l.mu.Unlock()
		return
	}

	today := now.UTC().Format("2006-01-02")
	if r.stats.Date != today {
		r.stats.Yesterday = r.stats.Today
		r.stats.Today = make([]int, 24)
		r.stats.Date = today
	}
	hour := now.Hour()
	if hour >= 0 && hour < len(r.stats.Today) && r.stats.Today[hour] == 0 {
		r.stats.Today[hour] = 1
		if now.Sub(l.lastStatsWrite) > statsSaveThrottle {
			l.lastStatsWrite = now
			if raw, err := json.Marshal(r.stats); err == nil {
				os.WriteFile(filepath.Join(l.runtimeDir, "stats.json"), raw, 0o644)
			}
		}
	}

	r.file.Write(p)
	r.maybeRotateLocked(int64(len(p)))
	l.mu.Unlock()

	l.notify(Runtime, "err", string(p), now.UnixMilli())
}

// maybeRotateLocked enforces the in-day 100 MB size cap. Caller holds mu.
//
// ⚠ Ordering is load-bearing (Node crashed in production with the reverse
// order): rename the live file FIRST — writes during the swap land in the
// .1 backup, which is safe on POSIX — then swap in a fresh file, and close
// the old handle LAST so no write ever hits a closed stream.
func (r *RuntimeControl) maybeRotateLocked(chunkLen int64) {
	l := r.logger
	r.bytesSinceCheck += chunkLen
	if r.bytesSinceCheck < l.rotateCheckEvery {
		return
	}
	r.bytesSinceCheck = 0

	info, err := os.Stat(r.logPath)
	if err != nil || info.Size() <= l.maxRuntimeBytes {
		// Stat failure (file missing, race with cleanup): drop one check
		// window; the stream is unchanged so writes still flow.
		return
	}

	if err := os.Rename(r.logPath, r.logPath+".1"); err != nil {
		return
	}
	fresh, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// Could not open the replacement: keep writing through the old
		// handle (now pointing at the .1 backup) rather than lose logs.
		return
	}
	old := r.file
	r.file = fresh
	old.Close()
}

// End closes the file stream. Idempotent. Subscribers are NOT cleared:
// subscriptions persist across restarts (new streams); the Hub unsubscribes
// when its client disconnects.
func (r *RuntimeControl) End() {
	l := r.logger
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	r.file.Close()
}

// Subscribe subscribes to the runtime broadcast channel.
func (r *RuntimeControl) Subscribe(cb func(Entry)) func() {
	return r.logger.Subscribe(cb, Runtime)
}
