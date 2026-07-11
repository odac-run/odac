package applog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// BuildControl is the control object returned by NewBuildStream — Node's
// createBuildStream return value: an analyzing writer plus phase/finalize
// bookkeeping. Write it from exactly one producer at a time or wrap calls in
// your own ordering; internal state is still mutex-guarded.
type BuildControl struct {
	logger      *Logger
	logPath     string
	summaryPath string
	startMs     int64

	// stats guarded by the logger-independent mu (file writes too).
	stats     buildSummary
	leftover  string
	file      *os.File
	finalized bool
}

// NewBuildStream opens builds/<buildID>.log for appending and returns the
// control object. The build broadcast buffer is cleared (a new build must
// not replay the previous one) and an init line is broadcast immediately so
// subscribers don't see an empty screen.
func (l *Logger) NewBuildStream(buildID string, metadata map[string]any) (*BuildControl, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}

	l.mu.Lock()
	l.buffers[Build] = nil
	l.mu.Unlock()

	l.notify(Build, "out", "[Builder] Build session "+buildID+" initialized.", l.now().UnixMilli())

	logPath := filepath.Join(l.buildsDir, buildID+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	start := l.now().UnixMilli()
	return &BuildControl{
		logger:      l,
		logPath:     logPath,
		summaryPath: filepath.Join(l.buildsDir, buildID+".json"),
		startMs:     start,
		file:        file,
		stats: buildSummary{
			ID:        buildID,
			Timestamp: start,
			Status:    "pending",
			Phases:    []*Phase{},
			Metadata:  metadata,
		},
	}, nil
}

// Path returns the on-disk build log path.
func (b *BuildControl) Path() string { return b.logPath }

// Write feeds build output through the line analyzer (error/warning
// heuristics, phase attribution, subscriber broadcast) and appends the raw
// bytes to the log file. Implements io.Writer.
func (b *BuildControl) Write(p []byte) (int, error) {
	l := b.logger
	l.mu.Lock()
	if b.finalized {
		l.mu.Unlock()
		return len(p), nil
	}
	data := b.leftover + string(p)

	// Safety catch: a single line above 64KB is force-split to prevent
	// memory exhaustion.
	if len(data) > maxLineLength {
		forceLine := data[:maxLineLength]
		preview := forceLine
		if len(preview) > 100 {
			preview = preview[:100]
		}
		warn := "[Warning] Long line truncated: " + preview + "..."
		data = data[maxLineLength:]
		l.mu.Unlock()
		l.notify(Build, "out", warn, l.now().UnixMilli())
		l.mu.Lock()
	}

	lines := strings.Split(data, "\n")
	b.leftover = lines[len(lines)-1]
	lines = lines[:len(lines)-1]

	type out struct {
		typ, line string
	}
	var emits []out
	for _, raw := range lines {
		line := strings.ReplaceAll(raw, "\r", "")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || spinnerRe.MatchString(trimmed) {
			continue
		}

		isError := errorRe.MatchString(line) && !nodeModRe.MatchString(line)
		isWarning := warningRe.MatchString(line) && !npmWarnRe.MatchString(line)

		if isError {
			b.stats.Errors++
			for _, ph := range b.stats.Phases {
				if ph.End == 0 {
					ph.Errors++
				}
			}
		}
		if isWarning {
			b.stats.Warnings++
			for _, ph := range b.stats.Phases {
				if ph.End == 0 {
					ph.Warnings++
				}
			}
		}

		typ := "out"
		if isError {
			typ = "err"
		}
		emits = append(emits, out{typ, line})
	}

	_, err := b.file.Write(p)
	l.mu.Unlock()

	for _, e := range emits {
		l.notify(Build, e.typ, e.line, l.now().UnixMilli())
	}
	return len(p), err
}

// StartPhase records the start of a named phase.
func (b *BuildControl) StartPhase(name string) {
	b.logger.mu.Lock()
	defer b.logger.mu.Unlock()
	b.stats.Phases = append(b.stats.Phases, &Phase{
		Name:   name,
		Start:  b.logger.now().UnixMilli(),
		Status: "running",
	})
}

// EndPhase closes the most recent still-running phase with this name.
func (b *BuildControl) EndPhase(name string, success bool) {
	b.logger.mu.Lock()
	defer b.logger.mu.Unlock()
	for i := len(b.stats.Phases) - 1; i >= 0; i-- {
		ph := b.stats.Phases[i]
		if ph.Name == name && ph.End == 0 {
			ph.End = b.logger.now().UnixMilli()
			ph.Duration = float64(ph.End-ph.Start) / 1000
			ph.Status = statusWord(success)
			break
		}
	}
}

func statusWord(success bool) string {
	if success {
		return "success"
	}
	return "failed"
}

// Finalize flushes the analyzer's trailing partial line, auto-closes any
// still-running phases, writes the .json summary, closes the log file and
// prunes old builds. Idempotent; only the first call wins.
func (b *BuildControl) Finalize(success bool) error {
	l := b.logger
	l.mu.Lock()
	if b.finalized {
		l.mu.Unlock()
		return nil
	}
	b.finalized = true

	var flushLine string
	if b.leftover != "" {
		line := strings.TrimSpace(strings.ReplaceAll(b.leftover, "\r", ""))
		if line != "" && !spinnerRe.MatchString(line) {
			flushLine = line
		}
		b.leftover = ""
	}

	now := l.now().UnixMilli()
	for _, ph := range b.stats.Phases {
		if ph.End == 0 {
			ph.End = now
			ph.Duration = float64(ph.End-ph.Start) / 1000
			ph.Status = statusWord(success)
		}
	}
	b.stats.Status = statusWord(success)
	b.stats.Duration = float64(now-b.startMs) / 1000

	raw, err := json.MarshalIndent(b.stats, "", "  ")
	b.file.Close()
	l.mu.Unlock()

	if flushLine != "" {
		l.notify(Build, "out", flushLine, now)
	}

	if err == nil {
		err = os.WriteFile(b.summaryPath, raw, 0o644)
	}

	l.pruneBuilds()
	return err
}

// Subscribe subscribes to the build broadcast channel.
func (b *BuildControl) Subscribe(cb func(Entry)) func() {
	return b.logger.Subscribe(cb, Build)
}
