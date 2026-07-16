// Package applog is the Go port of server/src/Container/Logger.js: per-app
// build and runtime log capture under <baseDir>/logs/<app>/{builds,runtime}.
//
// Responsibilities, matching Node:
//   - Build streams: a line-splitting analyzer that counts error/warning
//     heuristics into per-phase stats, broadcasts lines to subscribers, and
//     appends the raw bytes to builds/<id>.log; Finalize writes the
//     builds/<id>.json summary and prunes to the 10 newest builds.
//   - Runtime streams: daily runtime/<YYYY-MM-DD>.log files with a 100 MB
//     in-day size rotation and a stats.json 24h error-hour histogram.
//     ⚠ The size rotation ordering is load-bearing (production crash in
//     Node when reversed): rename the live file FIRST, swap in the new
//     stream, and only then close the old one — writes keep flowing during
//     the swap and must never hit a closed stream.
//   - Subscriptions: per-stream ring buffers (build 500 / runtime 100)
//     replayed to new subscribers, then live fan-out of {t, d, ts} entries.
//
// Deviations from Node (deliberate): Finalize flushes the analyzer's
// leftover partial line and closes the build log file (Node never ends the
// analyzer pipe — a file-handle leak per build); the throttled stats.json
// writes and rotation checks run synchronously under the control's mutex
// (Node relies on the single-threaded event loop); the 7-day runtime age
// sweep runs synchronously in NewRuntimeStream.
package applog

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stream selects one of the two broadcast channels of a Logger.
type Stream string

const (
	// Build is the build-output channel (analyzer lines).
	Build Stream = "build"
	// Runtime is the app-runtime channel (raw docker/process chunks).
	Runtime Stream = "runtime"
)

// Entry is one broadcast payload: Node's {t, d, ts}.
type Entry struct {
	T  string `json:"t"`
	D  string `json:"d"`
	TS int64  `json:"ts"`
}

// Phase is one build phase; field order mirrors Node's key insertion order.
type Phase struct {
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	Status   string  `json:"status"`
	Errors   int     `json:"errors"`
	Warnings int     `json:"warnings"`
	End      int64   `json:"end,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

// buildSummary is the builds/<id>.json shape (Node's stats literal order).
type buildSummary struct {
	ID        string         `json:"id"`
	Timestamp int64          `json:"timestamp"`
	Duration  float64        `json:"duration"`
	Status    string         `json:"status"`
	Errors    int            `json:"errors"`
	Warnings  int            `json:"warnings"`
	Phases    []*Phase       `json:"phases"`
	Metadata  map[string]any `json:"metadata"`
}

// BuildInfo is getLastBuild()'s result shape.
type BuildInfo struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Time     int64          `json:"time"`
	Duration float64        `json:"duration"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
	Phases   []*Phase       `json:"phases"`
	Metadata map[string]any `json:"metadata"`
}

// DailyBuild is one entry of DailySummary.Builds (note: no warnings field,
// matching Node's getDailySummary literal).
type DailyBuild struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Time     int64          `json:"time"`
	Duration float64        `json:"duration"`
	Errors   int            `json:"errors"`
	Phases   []*Phase       `json:"phases"`
	Metadata map[string]any `json:"metadata"`
}

// DailySummary is getDailySummary()'s result shape.
type DailySummary struct {
	Total         int          `json:"total"`
	Success       int          `json:"success"`
	Failed        int          `json:"failed"`
	TotalDuration float64      `json:"totalDuration"`
	AvgDuration   float64      `json:"avgDuration"`
	Builds        []DailyBuild `json:"builds"`
}

// runtimeStats is the runtime/stats.json shape: per-hour error flags (0/1)
// for the current and previous day.
type runtimeStats struct {
	Date      string `json:"date"`
	Today     []int  `json:"today"`
	Yesterday []int  `json:"yesterday"`
}

const (
	buildBufferSize   = 500
	runtimeBufferSize = 100
	maxLineLength     = 65536 // 64KB per-line cap against memory exhaustion

	maxRuntimeBytes   = 100 * 1024 * 1024 // in-day size cap; one .1 backup ≈ 200 MB peak
	rotateCheckEvery  = 1024 * 1024       // stat the file every ~1 MB of writes
	runtimeKeeepDays  = 7
	buildsToKeep      = 10
	statsSaveThrottle = 2 * time.Second
)

var (
	errorRe      = regexp.MustCompile(`(?i)error`)
	nodeModRe    = regexp.MustCompile(`(?i)node_modules`)
	warningRe    = regexp.MustCompile(`(?i)warning`)
	npmWarnRe    = regexp.MustCompile(`(?i)npm warn`)
	spinnerRe    = regexp.MustCompile(`^[-|/\\]\s*$`)
	errLogPrefix = "Failed to read build log: "
)

// Logger captures build and runtime logs for one app.
type Logger struct {
	appName    string
	logsDir    string
	buildsDir  string
	runtimeDir string

	mu             sync.Mutex
	nextSubID      int
	subscribers    map[Stream]map[int]func(Entry)
	buffers        map[Stream][]Entry
	lastStatsWrite time.Time

	// now is the clock; overridable in tests.
	now func() time.Time

	// size-rotation tunables; overridable in tests.
	maxRuntimeBytes  int64
	rotateCheckEvery int64
}

// New builds a Logger rooted at <logsRoot>/<appName> (Node roots at
// ~/.odac/logs; pass filepath.Join(baseDir, "logs")). No I/O happens here.
func New(logsRoot, appName string) *Logger {
	logsDir := filepath.Join(logsRoot, appName)
	return &Logger{
		appName:          appName,
		logsDir:          logsDir,
		buildsDir:        filepath.Join(logsDir, "builds"),
		runtimeDir:       filepath.Join(logsDir, "runtime"),
		subscribers:      map[Stream]map[int]func(Entry){Build: {}, Runtime: {}},
		buffers:          map[Stream][]Entry{Build: {}, Runtime: {}},
		now:              time.Now,
		maxRuntimeBytes:  maxRuntimeBytes,
		rotateCheckEvery: rotateCheckEvery,
	}
}

// Init creates the builds/ and runtime/ directories. Safe to call repeatedly.
func (l *Logger) Init() error {
	if err := os.MkdirAll(l.buildsDir, 0o755); err != nil {
		return fmt.Errorf("applog: init %s: %w", l.appName, err)
	}
	if err := os.MkdirAll(l.runtimeDir, 0o755); err != nil {
		return fmt.Errorf("applog: init %s: %w", l.appName, err)
	}
	return nil
}

// Destroy removes this logger's on-disk directory tree. Best-effort: callers
// should have closed active streams first.
func (l *Logger) Destroy() error {
	return os.RemoveAll(l.logsDir)
}

// Subscribe replays the stream's buffered history to cb, registers it for
// live entries, and returns an unsubscribe function.
func (l *Logger) Subscribe(cb func(Entry), stream Stream) func() {
	l.mu.Lock()
	subs, ok := l.subscribers[stream]
	if !ok {
		l.mu.Unlock()
		return func() {}
	}
	history := append([]Entry(nil), l.buffers[stream]...)
	id := l.nextSubID
	l.nextSubID++
	l.mu.Unlock()

	// History first, then live registration — mirrors Node's ordering. A line
	// broadcast between the copy and the registration is dropped, same as
	// Node's single-tick equivalent window.
	for _, e := range history {
		cb(e)
	}

	l.mu.Lock()
	subs[id] = cb
	l.mu.Unlock()

	return func() {
		l.mu.Lock()
		delete(subs, id)
		l.mu.Unlock()
	}
}

// notify buffers the entry (ring, per-stream cap) and fans it out.
func (l *Logger) notify(stream Stream, typ, data string, ts int64) {
	e := Entry{T: typ, D: data, TS: ts}

	l.mu.Lock()
	buf := append(l.buffers[stream], e)
	max := runtimeBufferSize
	if stream == Build {
		max = buildBufferSize
	}
	if len(buf) > max {
		buf = buf[1:]
	}
	l.buffers[stream] = buf
	cbs := make([]func(Entry), 0, len(l.subscribers[stream]))
	for _, cb := range l.subscribers[stream] {
		cbs = append(cbs, cb)
	}
	l.mu.Unlock()

	for _, cb := range cbs {
		cb(e)
	}
}

// GetHealth returns the sliding 24h error-hour window ending at the current
// hour: [yesterday hour+1 .. 23] + [today 0 .. hour], front-padded with
// zeros to 24 entries. Missing/stale stats yield an all-zero window.
func (l *Logger) GetHealth() []int {
	zero := make([]int, 24)

	raw, err := os.ReadFile(filepath.Join(l.runtimeDir, "stats.json"))
	if err != nil {
		return zero
	}
	var stats runtimeStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return zero
	}

	today := l.now().UTC().Format("2006-01-02")
	if stats.Date != today {
		if stats.Date == "" {
			return zero
		}
		// Reader may run before the writer rotates: rotate virtually.
		stats.Yesterday = stats.Today
		stats.Today = make([]int, 24)
	}

	hour := l.now().Hour()
	combined := append(sliceFrom(stats.Yesterday, hour+1), sliceTo(stats.Today, hour+1)...)
	for len(combined) < 24 {
		combined = append([]int{0}, combined...)
	}
	return combined
}

func sliceFrom(s []int, i int) []int {
	if i >= len(s) {
		return nil
	}
	return append([]int(nil), s[i:]...)
}

func sliceTo(s []int, i int) []int {
	if i > len(s) {
		i = len(s)
	}
	return append([]int(nil), s[:i]...)
}

// GetLastBuild returns the most recent build summary (newest .json by
// filename, descending — build ids embed a ms timestamp), with phases
// sorted to linear progression: completed phases by end time, running ones
// after, by start time. Nil when there are no builds.
func (l *Logger) GetLastBuild() *BuildInfo {
	names, err := os.ReadDir(l.buildsDir)
	if err != nil {
		return nil
	}
	var jsonFiles []string
	for _, de := range names {
		if strings.HasSuffix(de.Name(), ".json") {
			jsonFiles = append(jsonFiles, de.Name())
		}
	}
	if len(jsonFiles) == 0 {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(jsonFiles)))

	raw, err := os.ReadFile(filepath.Join(l.buildsDir, jsonFiles[0]))
	if err != nil {
		return nil
	}
	var data buildSummary
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}

	phases := data.Phases
	if phases == nil {
		phases = []*Phase{}
	}
	sort.SliceStable(phases, func(i, j int) bool {
		a, b := phases[i], phases[j]
		if a.End != 0 && b.End != 0 {
			return a.End < b.End
		}
		if a.End != 0 {
			return true
		}
		if b.End != 0 {
			return false
		}
		return a.Start < b.Start
	})

	meta := data.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return &BuildInfo{
		ID:       data.ID,
		Status:   data.Status,
		Time:     data.Timestamp,
		Duration: data.Duration,
		Errors:   data.Errors,
		Warnings: data.Warnings,
		Phases:   phases,
		Metadata: meta,
	}
}

// GetDailySummary aggregates the build summaries of the last 24 hours,
// newest first.
func (l *Logger) GetDailySummary() DailySummary {
	summary := DailySummary{Builds: []DailyBuild{}}
	oneDayAgo := l.now().UnixMilli() - 24*60*60*1000

	names, err := os.ReadDir(l.buildsDir)
	if err != nil {
		return summary
	}
	for _, de := range names {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(l.buildsDir, de.Name()))
		if err != nil {
			continue
		}
		var data buildSummary
		if err := json.Unmarshal(raw, &data); err != nil {
			continue // corrupted files are skipped like Node
		}
		if data.Timestamp <= oneDayAgo {
			continue
		}
		summary.Total++
		if data.Status == "success" {
			summary.Success++
		} else {
			summary.Failed++
		}
		summary.TotalDuration += data.Duration
		phases := data.Phases
		if phases == nil {
			phases = []*Phase{}
		}
		meta := data.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		summary.Builds = append(summary.Builds, DailyBuild{
			ID:       data.ID,
			Status:   data.Status,
			Time:     data.Timestamp,
			Duration: data.Duration,
			Errors:   data.Errors,
			Phases:   phases,
			Metadata: meta,
		})
	}

	if summary.Total > 0 {
		summary.AvgDuration = math.Round(summary.TotalDuration/float64(summary.Total)*100) / 100
	}
	sort.SliceStable(summary.Builds, func(i, j int) bool {
		return summary.Builds[i].Time > summary.Builds[j].Time
	})
	return summary
}

// ReadLastBuildLog returns the content of the most recent build .log file
// (newest by mtime). Like Node it reports failure as a string, never an
// error: "" when there are no logs, "Failed to read build log: …" when a
// read breaks mid-way.
func (l *Logger) ReadLastBuildLog() string {
	names, err := os.ReadDir(l.buildsDir)
	if err != nil {
		return ""
	}
	newest := ""
	var newestTime time.Time
	for _, de := range names {
		if !strings.HasSuffix(de.Name(), ".log") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			return errLogPrefix + err.Error()
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = de.Name()
		}
	}
	if newest == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(l.buildsDir, newest))
	if err != nil {
		return errLogPrefix + err.Error()
	}
	return string(raw)
}

// pruneBuilds keeps the 10 newest build summaries by mtime and deletes the
// .json/.log pairs of the rest. Best-effort.
func (l *Logger) pruneBuilds() {
	names, err := os.ReadDir(l.buildsDir)
	if err != nil {
		return
	}
	type fileTime struct {
		name string
		time time.Time
	}
	var jsonFiles []fileTime
	for _, de := range names {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		jsonFiles = append(jsonFiles, fileTime{de.Name(), info.ModTime()})
	}
	if len(jsonFiles) <= buildsToKeep {
		return
	}
	sort.Slice(jsonFiles, func(i, j int) bool { return jsonFiles[i].time.After(jsonFiles[j].time) })
	for _, ft := range jsonFiles[buildsToKeep:] {
		id := strings.TrimSuffix(ft.name, ".json")
		os.Remove(filepath.Join(l.buildsDir, ft.name))
		os.Remove(filepath.Join(l.buildsDir, id+".log"))
	}
}

// pruneRuntimeLogs deletes runtime .log files older than 7 days (mtime).
func (l *Logger) pruneRuntimeLogs() {
	names, err := os.ReadDir(l.runtimeDir)
	if err != nil {
		return
	}
	cutoff := l.now().Add(-runtimeKeeepDays * 24 * time.Hour)
	for _, de := range names {
		if !strings.HasSuffix(de.Name(), ".log") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(l.runtimeDir, de.Name()))
		}
	}
}
