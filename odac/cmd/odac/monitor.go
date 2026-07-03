// Monitor TUI: `odac monit` (app dashboard) and `odac debug` (module log
// viewer), ported from cli/src/Monitor.js. Frames are plain ANSI like Node's;
// golang.org/x/term is used only for raw mode and terminal size (contract 0.8
// left the TUI dependency open — a framework would replace the frame builder
// wholesale and lose byte-comparability with Monitor.js).
//
// Threading model: Node is single-threaded with async callbacks; here one
// event loop goroutine owns all monitor state, and async work (docker execs,
// the restart API call) posts apply-closures back into the loop.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"odac/internal/apiproto"
)

// Node Cli.color / Cli.#backgrounds name→SGR maps. Unknown names apply no
// code, exactly like Node ('cyan' is used by Monitor.js but not in the map).
var monColors = map[string]int{
	"red": 31, "green": 32, "yellow": 33, "blue": 34,
	"magenta": 35, "white": 37, "gray": 90,
}

var monBackgrounds = map[string]int{
	"red": 41, "green": 42, "yellow": 43, "blue": 44,
	"magenta": 45, "white": 47, "gray": 100,
}

// mcolor mirrors Cli.color(text, color, ...args): foreground wrap first, then
// per-arg background and bold wraps. Empty/unknown names are no-ops.
func mcolor(text, fg string, args ...string) string {
	out := text
	if code, ok := monColors[fg]; ok {
		out = fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, out)
	}
	for _, arg := range args {
		if code, ok := monBackgrounds[arg]; ok {
			out = fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, out)
		}
		if arg == "bold" {
			out = "\x1b[1m" + out + "\x1b[0m"
		}
	}
	return out
}

// micon mirrors the two-argument Cli.icon(status, selected): the selected row
// keeps its status color on a white background.
func micon(status string, selected bool) string {
	bg := ""
	if selected {
		bg = "white"
	}
	switch status {
	case "errored":
		return mcolor(" ! ", "red", bg)
	case "progress":
		return mcolor(" - ", "gray", bg)
	case "running":
		return mcolor(" ▶ ", "green", bg)
	case "stopped":
		return mcolor(" ⏸ ", "yellow", bg)
	case "success":
		return mcolor(" ✓ ", "green", bg)
	}
	return "   "
}

// mspacing mirrors Cli.spacing(text, len, direction) with ANSI-aware length.
// Negative padding is clamped to zero (Node throws on a too-narrow terminal).
func mspacing(text string, width int, direction string) string {
	visible := visibleLen(text)
	switch direction {
	case "right":
		return rep(" ", width-visible) + text
	case "center":
		left := (width - visible) / 2
		return rep(" ", left) + text + rep(" ", width-visible-left)
	}
	if visible > width {
		// Node: text.substr(0, text.length - visibleLen + width) — a raw cut
		// that may slice through escape codes; kept as-is for parity.
		runes := []rune(text)
		return string(runes[:len(runes)-visible+width])
	}
	return text + rep(" ", width-visible)
}

func rep(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(s, n)
}

// mformatDate mirrors Cli.formatDate: local "YYYY-MM-DD HH:MM:SS".
func mformatDate(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05")
}

// monAnsiRe is Monitor.js's #safeLog escape-sequence matcher (CSI codes plus
// OSC strings), used to truncate without cutting through escapes.
var monAnsiRe = regexp.MustCompile(
	`[\x{1b}\x{9b}][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]` +
		`|[\x{1b}\x{9b}]\].*?(?:\x{07}|[\x{1b}\x{9b}]\\)`)

// msafeLog mirrors Monitor.js #safeLog: pad or truncate a possibly-colored
// log line to maxWidth visible characters, keeping escape sequences intact
// and appending a reset when truncated.
func msafeLog(log string, maxWidth int) string {
	if log == "" {
		return rep(" ", maxWidth)
	}
	content := strings.ReplaceAll(strings.ReplaceAll(log, "\r", ""), "\t", "  ")

	var b strings.Builder
	current := 0
	last := 0
	for _, loc := range monAnsiRe.FindAllStringIndex(content, -1) {
		before := content[last:loc[0]]
		if n := len([]rune(before)); current+n > maxWidth {
			b.WriteString(string([]rune(before)[:maxWidth-current]))
			return b.String() + "\x1b[0m"
		} else {
			b.WriteString(before)
			current += n
		}
		b.WriteString(content[loc[0]:loc[1]])
		last = loc[1]
	}
	tail := content[last:]
	if n := len([]rune(tail)); current+n > maxWidth {
		b.WriteString(string([]rune(tail)[:maxWidth-current]))
		return b.String() + "\x1b[0m"
	} else {
		b.WriteString(tail)
		current += n
	}
	return b.String() + rep(" ", maxWidth-current)
}

// monCol1 is the left-column width: floor(width/12*3), capped at 50.
func monCol1(width int) int {
	c1 := width * 3 / 12
	if c1 > 50 {
		c1 = 50
	}
	return c1
}

const monShortcuts = "Mouse | ↑/↓ Navigate | ↵ Select | R Restart | Ctrl+C Exit"

type monApp struct {
	id     string
	name   string
	status string
}

type monStat struct{ cpu, mem string }

type monitor struct {
	a    *app
	mode string // "debug" | "monit"
	out  io.Writer

	// injected for tests
	size   func() (cols, rows int)
	docker func(args ...string) (stdout, stderr string, err error)
	now    func() time.Time

	width, height int // Node: columns-3 / rows, refreshed each frame

	modules  []string
	selected int
	watch    []int // debug: watched module indices

	apps        []monApp // monit: public then internal, both name-sorted
	publicCount int
	stats       map[string]monStat
	maxCPULen   int
	maxMemLen   int
	statuses    map[string]string
	lineToApp   map[int]int // rendered line → apps index

	logsContent   []string
	logsMtime     time.Time
	logsWatched   []int
	logsSelected  int
	logsLastFetch time.Time
	fetchingLogs  bool
	restarting    map[string]string

	current string // last flushed frame (skip identical redraws)
}

func newMonitor(a *app, mode string, out io.Writer) *monitor {
	m := &monitor{
		a:    a,
		mode: mode,
		out:  out,
		modules: []string{"api", "app", "config", "container", "dns", "hub",
			"mail", "proxy", "server", "ssl", "updater"},
		stats:        map[string]monStat{},
		statuses:     map[string]string{},
		lineToApp:    map[int]int{},
		restarting:   map[string]string{},
		logsSelected: -1,
		docker:       runDocker,
		now:          time.Now,
		size:         func() (int, int) { return 80, 24 },
	}
	if f, ok := out.(*os.File); ok {
		fd := int(f.Fd())
		m.size = func() (int, int) {
			cols, rows, err := term.GetSize(fd)
			if err != nil {
				return 80, 24
			}
			return cols, rows
		}
	}
	return m
}

func runDocker(args ...string) (string, string, error) {
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// monitor is the `odac monit` / `odac debug` entry point: raw mode + mouse
// tracking on a real TTY, then the event loop until Ctrl+C.
func (a *app) monitor(mode string) int {
	fIn, inOK := a.in.(*os.File)
	fOut, outOK := a.out.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(fIn.Fd())) || !term.IsTerminal(int(fOut.Fd())) {
		fmt.Fprintln(a.errOut, "The monitor requires an interactive terminal.")
		return 1
	}

	// Terminal title, like the Monitor constructor.
	if runtime.GOOS == "windows" {
		fmt.Fprint(a.out, "title ODAC Debug\n")
	} else {
		fmt.Fprint(a.out, "\x1b]2;ODAC Debug\x1b\x5c")
	}

	oldState, err := term.MakeRaw(int(fIn.Fd()))
	if err != nil {
		fmt.Fprintln(a.errOut, "Failed to enter raw mode:", err)
		return 1
	}
	fmt.Fprint(a.out, "\x1b[?25l\x1b[?1000h") // hide cursor, mouse tracking on

	m := newMonitor(a, mode, fOut)
	code := m.run(fIn)

	term.Restore(int(fIn.Fd()), oldState)
	fmt.Fprint(a.out, "\x1b[?25h\x1b[?1000l\x1bc") // Node's Ctrl+C cleanup
	return code
}

// run is the event loop: 250ms render tick, 2s docker stats tick (monit),
// stdin chunks, and apply-closures posted by async work.
func (m *monitor) run(stdin io.Reader) int {
	events := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				events <- chunk
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()

	apply := make(chan func(), 32)
	done := make(chan struct{})
	post := func(f func()) {
		select {
		case apply <- f:
		case <-done:
		}
	}

	renderTick := time.NewTicker(250 * time.Millisecond)
	defer renderTick.Stop()
	var statsC <-chan time.Time
	if m.mode == "monit" {
		statsTick := time.NewTicker(2 * time.Second)
		defer statsTick.Stop()
		statsC = statsTick.C
		m.fetchStats(post)
		m.fetchStatuses(post)
	}
	m.render(post)

	for {
		select {
		case <-renderTick.C:
			m.render(post)
		case <-statsC:
			m.fetchStats(post)
			m.fetchStatuses(post)
		case f := <-apply:
			f()
			m.render(post)
		case chunk, ok := <-events:
			if !ok || m.handleInput(chunk, post) {
				close(done)
				return 0
			}
			m.render(post)
		}
	}
}

// render rebuilds the frame and flushes it only when it changed (Node's
// #current check). Raw mode disables OPOST, so LF→CRLF happens here.
func (m *monitor) render(post func(func())) {
	cols, rows := m.size()
	m.width, m.height = cols-3, rows

	var frame string
	if m.mode == "debug" {
		m.loadModuleLogs()
		frame = m.debugFrame()
	} else {
		m.refreshApps()
		m.loadMonitLogs(post)
		frame = m.monitFrame()
	}
	if frame == m.current {
		return
	}
	m.current = frame
	fmt.Fprint(m.out, "\x1bc")
	fmt.Fprint(m.out, strings.ReplaceAll(frame, "\n", "\r\n"))
	fmt.Fprint(m.out, "\x1b[?25l\x1b[?1000h")
}

// handleInput consumes one stdin chunk, which may batch several events.
// Reports true on Ctrl+C. Mirrors Monitor.js's per-mode key maps.
func (m *monitor) handleInput(buf []byte, post func(func())) bool {
	total := len(m.modules)
	if m.mode == "monit" {
		total = len(m.apps)
	}
	for i := 0; i < len(buf); {
		switch {
		case buf[i] == 3: // Ctrl+C
			return true
		case len(buf)-i >= 6 && buf[i] == 0x1b && buf[i+1] == '[' && buf[i+2] == 'M':
			m.mouse(buf[i+3], int(buf[i+4])-32, int(buf[i+5])-32, total)
			i += 6
		case len(buf)-i >= 3 && buf[i] == 0x1b && buf[i+1] == '[':
			switch buf[i+2] {
			case 'A':
				if m.selected > 0 {
					m.selected--
				}
			case 'B':
				if m.selected+1 < total {
					m.selected++
				}
			}
			i += 3
		case buf[i] == 13 && m.mode == "debug": // Enter toggles watch
			m.toggleWatch(m.selected)
			i++
		case (buf[i] == 'r' || buf[i] == 'R') && m.mode == "monit":
			m.restartSelected(post)
			i++
		default:
			i++
		}
	}
	return false
}

// mouse handles X10-encoded events: 96/97 wheel, 32 left/middle click.
func (m *monitor) mouse(b byte, x, y, total int) {
	switch b {
	case 96: // wheel up
		if m.selected > 0 {
			m.selected--
		}
	case 97: // wheel down
		if m.selected+1 < total {
			m.selected++
		}
	case 32: // click
		c1 := monCol1(m.width)
		if x > 1 && x < c1 && y < m.height-4 {
			if m.mode == "debug" {
				if idx := y - 2; idx >= 0 && idx < len(m.modules) {
					m.selected = idx
					m.toggleWatch(idx)
				}
			} else if idx, ok := m.lineToApp[y-2]; ok {
				m.selected = idx
			}
		}
	}
}

func (m *monitor) toggleWatch(idx int) {
	if pos := slices.Index(m.watch, idx); pos > -1 {
		m.watch = slices.Delete(m.watch, pos, pos+1)
	} else {
		m.watch = append(m.watch, idx)
	}
}

// --- debug mode: module list + merged ~/.odac/logs files ---

// selectedModules returns the watched module names, or all when none watched.
func (m *monitor) selectedModules() []string {
	if len(m.watch) == 0 {
		return m.modules
	}
	names := make([]string, 0, len(m.watch))
	for _, idx := range m.watch {
		if idx >= 0 && idx < len(m.modules) {
			names = append(names, m.modules[idx])
		}
	}
	return names
}

// loadModuleLogs merges .odac.log with the converted proxy log and filters by
// watched modules. Node's mtime cache compares Date objects by reference and
// never hits; the intended cache is implemented here (mtime + watch set).
func (m *monitor) loadModuleLogs() {
	logsDir := filepath.Join(m.a.cfg.BaseDir(), "logs")

	var log string
	var mtime time.Time
	if st, err := os.Stat(filepath.Join(logsDir, ".odac.log")); err == nil {
		mtime = st.ModTime()
		if raw, err := os.ReadFile(filepath.Join(logsDir, ".odac.log")); err == nil {
			log = string(raw)
		}
	}

	proxyIdx := slices.Index(m.modules, "proxy")
	if len(m.watch) == 0 || slices.Contains(m.watch, proxyIdx) {
		if st, err := os.Stat(filepath.Join(logsDir, "proxy.log")); err == nil {
			if st.ModTime().After(mtime) {
				mtime = st.ModTime()
			}
			if raw, err := os.ReadFile(filepath.Join(logsDir, "proxy.log")); err == nil {
				log += "\n" + convertProxyLog(string(raw))
			}
		}
	}

	if slices.Equal(m.watch, m.logsWatched) && mtime.Equal(m.logsMtime) && m.logsContent != nil {
		return
	}
	m.logsContent = filterModuleLines(log, m.selectedModules(), m.height-4)
	m.logsMtime = mtime
	m.logsWatched = slices.Clone(m.watch)
}

var proxyLineRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})(?:\.\d+)?\s+(.*)$`)

// convertProxyLog rewrites the Go-style proxy log
// ("2006/01/02 15:04:05.000000 [INFO] msg") into watchdog log format so it
// merges with .odac.log.
func convertProxyLog(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]string, len(lines))
	for i, line := range lines {
		match := proxyLineRe.FindStringSubmatch(line)
		if match == nil {
			out[i] = "[LOG][proxy] " + line
			continue
		}
		dateStr := strings.Replace(strings.ReplaceAll(match[1], "/", "-"), " ", "T", 1)
		message := match[2]
		tag := "LOG"
		if strings.Contains(message, "[ERROR]") || strings.Contains(message, "[WARN]") {
			tag = "ERR"
		}
		out[i] = "[" + tag + "][" + dateStr + "][proxy] " + message
	}
	return strings.Join(out, "\n")
}

// filterModuleLines keeps lines mentioning a selected module tag and
// recolors their [LOG]/[ERR] prefixes, keeping the last `keep` lines.
func filterModuleLines(log string, selected []string, keep int) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(strings.TrimSpace(log), "\r\n", "\n"), "\n") {
		lower := strings.ToLower(line)
		module := ""
		for _, name := range selected {
			if strings.Contains(lower, "["+strings.ToLower(name)+"]") {
				module = name
				break
			}
		}
		if module == "" {
			continue
		}
		out = append(out, formatModuleLine(line, module))
	}
	if keep >= 0 && len(out) > keep {
		out = out[len(out)-keep:]
	}
	return out
}

// formatModuleLine recolors "[LOG][<iso date>]...[module] msg" lines. The
// date and message are located by brackets rather than Node's fixed offsets
// (which mis-slice the 19-char dates of merged proxy lines).
func formatModuleLine(line, module string) string {
	isErr := strings.HasPrefix(line, "[ERR]")
	if !isErr && !strings.HasPrefix(line, "[LOG]") {
		return line
	}
	rest := line[5:]
	if !strings.HasPrefix(rest, "[") {
		return line
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return line
	}
	dateStr := rest[1:end]
	if t, ok := parseLogDate(dateStr); ok {
		dateStr = mformatDate(t)
	}

	message := ""
	tag := "[" + strings.ToLower(module) + "]"
	if pos := strings.Index(strings.ToLower(line), tag); pos > -1 {
		message = strings.TrimSpace(line[pos+len(tag):])
	}

	dateColor := "green"
	if isErr {
		dateColor = "red"
	}
	return mcolor("["+dateStr+"]", dateColor, "bold") +
		mcolor("["+module+"]", "white", "bold") + " " + message
}

func parseLogDate(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	// Converted proxy dates carry no zone; Node's `new Date` reads them local.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// debugFrame builds the module-list + logs box (Monitor.js #debug).
func (m *monitor) debugFrame() string {
	c1 := monCol1(m.width)
	var b strings.Builder

	b.WriteString(mcolor("┌", "gray"))
	b.WriteString(mcolor(rep("─", 5), "gray"))
	title := "Modules"
	b.WriteString(" " + title + " ")
	b.WriteString(mcolor(rep("─", c1-len(title)-7), "gray"))
	b.WriteString(mcolor("┬", "gray"))
	b.WriteString(mcolor(rep("─", 5), "gray"))
	title = "Logs"
	b.WriteString(" " + title + " ")
	b.WriteString(mcolor(rep("─", m.width-c1-len(title)-7), "gray"))
	b.WriteString(mcolor("┐\n", "gray"))

	for i := 0; i < m.height-3; i++ {
		if i < len(m.modules) {
			fg, bg, style := "white", "", ""
			if i == m.selected {
				fg, bg, style = "blue", "white", "bold"
			}
			mark := "[ ] "
			if slices.Contains(m.watch, i) {
				mark = "[X] "
			}
			b.WriteString(mcolor("│", "gray"))
			b.WriteString(mcolor(mark, fg, bg, style))
			b.WriteString(mcolor(mspacing(m.modules[i], c1-4, ""), fg, bg, style))
			b.WriteString(mcolor("│", "gray"))
		} else {
			b.WriteString(mcolor("│", "gray"))
			b.WriteString(rep(" ", c1))
			b.WriteString(mcolor("│", "gray"))
		}
		line := " "
		if i < len(m.logsContent) && m.logsContent[i] != "" {
			line = m.logsContent[i]
		}
		b.WriteString(mspacing(line, m.width-c1, ""))
		b.WriteString(mcolor("│\n", "gray"))
	}

	b.WriteString(m.footer(c1))
	return b.String()
}

// --- monit mode: app dashboard + docker stats/logs ---

// refreshApps partitions config apps into public (having a domain) and
// internal, both name-sorted, like Monitor.js #monitor.
func (m *monitor) refreshApps() {
	rawApps, _ := m.a.cfg.Get("apps").([]any)

	hasDomain := map[string]bool{}
	for _, conf := range m.a.cfg.Map("domains") {
		c, ok := conf.(map[string]any)
		if !ok {
			continue
		}
		if id := conf2str(c["appId"]); id != "" {
			hasDomain[id] = true
		}
	}

	var public, internal []monApp
	for _, ra := range rawApps {
		am, ok := ra.(map[string]any)
		if !ok {
			continue
		}
		app := monApp{
			id:     conf2str(am["id"]),
			name:   conf2str(am["name"]),
			status: conf2str(am["status"]),
		}
		if hasDomain[app.id] || hasDomain[app.name] {
			public = append(public, app)
		} else {
			internal = append(internal, app)
		}
	}
	sort.SliceStable(public, func(i, j int) bool { return public[i].name < public[j].name })
	sort.SliceStable(internal, func(i, j int) bool { return internal[i].name < internal[j].name })

	m.apps = append(public, internal...)
	m.publicCount = len(public)
	if m.selected >= len(m.apps) && len(m.apps) > 0 {
		m.selected = len(m.apps) - 1
	}
}

func conf2str(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// loadMonitLogs fetches `docker logs` for the selected app, throttled to one
// fetch per second per selection (Monitor.js #load/#fetchDockerLogs).
func (m *monitor) loadMonitLogs(post func(func())) {
	if m.selected >= len(m.apps) {
		return
	}
	name := m.apps[m.selected].name
	if name == "" {
		return
	}
	if msg, ok := m.restarting[name]; ok {
		m.logsContent = []string{msg}
		return
	}
	if m.fetchingLogs || (m.logsSelected == m.selected && m.now().Sub(m.logsLastFetch) < time.Second) {
		return
	}
	m.fetchingLogs = true
	selected, keep := m.selected, m.height-4
	tail := strconv.Itoa(m.height)
	go func() {
		stdout, stderr, err := m.docker("logs", "-t", "--tail", tail, name)
		lines := parseDockerLogs(stdout, stderr, err, keep)
		post(func() {
			m.fetchingLogs = false
			m.logsContent = lines
			m.logsSelected = selected
			m.logsLastFetch = m.now()
		})
	}()
}

// parseDockerLogs sorts `docker logs -t` output by timestamp and colors each
// line's date prefix (red on error-looking content).
func parseDockerLogs(stdout, stderr string, err error, keep int) []string {
	raw := strings.ReplaceAll(stdout+"\n"+stderr, "\r\n", "\n")
	var lines []string
	for _, l := range strings.Split(raw, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if err != nil && len(lines) == 0 {
		return []string{mcolor("Error fetching logs: "+err.Error(), "red")}
	}

	type item struct {
		line string
		ts   int64
		t    time.Time
		ok   bool
	}
	items := make([]item, len(lines))
	for i, line := range lines {
		it := item{line: line}
		if sp := strings.IndexByte(line, ' '); sp > -1 {
			if t, perr := time.Parse(time.RFC3339Nano, line[:sp]); perr == nil {
				it.ts, it.t, it.ok = t.UnixMilli(), t, true
			}
		}
		items[i] = it
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ts < items[j].ts })

	out := make([]string, len(items))
	for i, it := range items {
		sp := strings.IndexByte(it.line, ' ')
		if sp == -1 || !it.ok {
			out[i] = it.line
			continue
		}
		content := it.line[sp+1:]
		colorName := "green"
		if strings.Contains(content, "[ERR]") || strings.Contains(strings.ToLower(content), "error") {
			colorName = "red"
		}
		out[i] = mcolor("["+mformatDate(it.t)+"]", colorName, "bold") + " " + content
	}
	if keep >= 0 && len(out) > keep {
		out = out[len(out)-keep:]
	}
	return out
}

func (m *monitor) fetchStats(post func(func())) {
	go func() {
		stdout, _, err := m.docker("stats", "--no-stream", "--format", "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}")
		if err != nil {
			return
		}
		stats, maxCPU, maxMem := parseStats(stdout)
		post(func() {
			for name, s := range stats {
				m.stats[name] = s
			}
			m.maxCPULen, m.maxMemLen = maxCPU, maxMem
		})
	}()
}

var (
	statMemRe = regexp.MustCompile(`(\d+)\.\d+([a-zA-Z]+)`)
	statCPURe = regexp.MustCompile(`(\d+)\.\d+%`)
)

// parseStats condenses `docker stats` output: "15.25MiB / 2GiB" → "15MB",
// "0.35%" → "0%", tracking column widths for aligned rendering.
func parseStats(stdout string) (map[string]monStat, int, int) {
	stats := map[string]monStat{}
	maxCPU, maxMem := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			continue
		}
		name := parts[0]
		mem := strings.TrimSpace(strings.Split(parts[2], "/")[0])
		cpu := strings.TrimSpace(parts[1])

		mem = strings.Replace(statMemRe.ReplaceAllString(mem, "$1$2"), "iB", "B", 1)
		cpu = statCPURe.ReplaceAllString(cpu, "$1%")

		if len(cpu) > maxCPU {
			maxCPU = len(cpu)
		}
		if len(mem) > maxMem {
			maxMem = len(mem)
		}
		stats[name] = monStat{cpu: cpu, mem: mem}
	}
	return stats, maxCPU, maxMem
}

func (m *monitor) fetchStatuses(post func(func())) {
	go func() {
		stdout, _, err := m.docker("ps", "-a", "--format", "{{.Names}}|{{.State}}")
		if err != nil {
			return
		}
		statuses := parseStatuses(stdout)
		post(func() {
			for name, s := range statuses {
				m.statuses[name] = s
			}
		})
	}()
}

func parseStatuses(stdout string) map[string]string {
	statuses := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		var status string
		switch strings.TrimSpace(parts[1]) {
		case "running":
			status = "running"
		case "restarting":
			status = "progress"
		case "dead":
			status = "errored"
		default: // exited, paused, created, ...
			status = "stopped"
		}
		statuses[name] = status
	}
	return statuses
}

// restartSelected sends app.restart for the selected app over the API socket
// (Monitor.js #restartContainer) and shows the outcome for 2 seconds.
func (m *monitor) restartSelected(post func(func())) {
	if m.selected >= len(m.apps) {
		return
	}
	name := m.apps[m.selected].name
	if name == "" {
		return
	}
	m.restarting[name] = mcolor("Restarting "+name+"...", "yellow")

	auth, _ := m.a.cfg.Map("api")["auth"].(string)
	client := m.a.client
	go func() {
		resp, err := client.Call(
			apiproto.Request{Auth: auth, Action: "app.restart", Data: []any{name}}, nil)
		var msg string
		switch {
		case err != nil:
			msg = mcolor("Connection failed: "+err.Error(), "red")
		case resp.Result:
			msg = mcolor("Successfully restarted "+name, "green")
		default:
			errMsg := resp.Message
			if errMsg == "" {
				errMsg = "Unknown error"
			}
			msg = mcolor("Error restarting "+name+": "+errMsg, "red")
		}
		post(func() { m.restarting[name] = msg })
		time.AfterFunc(2*time.Second, func() {
			post(func() { delete(m.restarting, name) })
		})
	}()
}

// monitLogLine returns the bottom-aligned log line for a rendered row
// (Monitor.js #getLogLine).
func (m *monitor) monitLogLine(index int) string {
	offset := m.height - 4 - len(m.logsContent)
	if index < offset || index-offset >= len(m.logsContent) {
		return " "
	}
	if line := m.logsContent[index-offset]; line != "" {
		return line
	}
	return " "
}

// monitFrame builds the app dashboard (Monitor.js #monitor + #render*).
func (m *monitor) monitFrame() string {
	c1 := monCol1(m.width)
	m.lineToApp = map[int]int{}

	mainTitle := "Apps"
	if m.publicCount > 0 && m.publicCount < len(m.apps) {
		mainTitle = "Public"
	}

	var b strings.Builder
	// header
	b.WriteString(mcolor("┌", "gray"))
	if len(m.apps) > 0 {
		b.WriteString(mcolor("─", "gray"))
		b.WriteString(" " + mainTitle + " ")
		b.WriteString(mcolor(rep("─", c1-len(mainTitle)-3), "gray"))
	} else {
		b.WriteString(mcolor(rep("─", c1), "gray"))
	}
	b.WriteString(mcolor("┬", "gray"))
	b.WriteString(mcolor(rep("─", m.width-c1), "gray"))
	b.WriteString(mcolor("┐\n", "gray"))

	// app rows
	rendered := 0
	shownInternalHeader := false
	for i := 0; i < len(m.apps) && rendered < m.height-4; i++ {
		if i >= m.publicCount && m.publicCount > 0 && !shownInternalHeader {
			b.WriteString(m.groupHeader(c1, "Internal", rendered))
			rendered++
			shownInternalHeader = true
			if rendered >= m.height-4 {
				break
			}
		}

		m.lineToApp[rendered] = i
		app := m.apps[i]
		selected := i == m.selected

		stats := ""
		if s, ok := m.stats[app.name]; ok {
			stats = "[" + padEnd(s.mem, m.maxMemLen) + "| " + padStart(s.cpu, m.maxCPULen) + "]"
		}

		b.WriteString(mcolor("│", "gray"))
		status := app.status
		if s, ok := m.statuses[app.name]; ok {
			status = s
		}
		b.WriteString(micon(status, selected))

		maxLen := m.appNameWidth(c1, stats)
		display := app.name
		if runes := []rune(display); len(runes) > maxLen {
			display = string(runes[:maxLen])
		}
		display = padEnd(display, maxLen)

		fg, bg, style := "white", "", ""
		if selected {
			fg, bg, style = "blue", "white", "bold"
		}
		b.WriteString(mcolor(display, fg, bg, style))

		if stats != "" {
			statsColor := "cyan" // not in the color map: renders uncolored, like Node
			if selected {
				statsColor = "blue"
			}
			b.WriteString(mcolor(stats, statsColor, bg))
		}
		b.WriteString(mcolor(" ", "white", bg))
		b.WriteString(mcolor(" │", "gray"))
		b.WriteString(msafeLog(m.monitLogLine(rendered), m.width-c1))
		b.WriteString(mcolor("│\n", "gray"))
		rendered++
	}

	// filler rows
	for ; rendered < m.height-4; rendered++ {
		b.WriteString(mcolor("│", "gray"))
		b.WriteString(rep(" ", c1))
		b.WriteString(mcolor("│", "gray"))
		b.WriteString(msafeLog(m.monitLogLine(rendered), m.width-c1))
		b.WriteString(mcolor("│\n", "gray"))
	}

	b.WriteString(m.footer(c1))
	return b.String()
}

func (m *monitor) appNameWidth(c1 int, stats string) int {
	if n := c1 - 5 - len(stats); n > 0 {
		return n
	}
	return 0
}

func (m *monitor) groupHeader(c1 int, title string, line int) string {
	var b strings.Builder
	b.WriteString(mcolor("│", "gray"))
	b.WriteString(mcolor("─", "gray"))
	b.WriteString(" " + title + " ")
	b.WriteString(mcolor(rep("─", c1-len(title)-3), "gray"))
	b.WriteString(mcolor("│", "gray"))
	b.WriteString(msafeLog(m.monitLogLine(line), m.width-c1))
	b.WriteString(mcolor("│\n", "gray"))
	return b.String()
}

func (m *monitor) footer(c1 int) string {
	var b strings.Builder
	b.WriteString(mcolor("└", "gray"))
	b.WriteString(mcolor(rep("─", c1), "gray"))
	b.WriteString(mcolor("┴", "gray"))
	b.WriteString(mcolor(rep("─", m.width-c1), "gray"))
	b.WriteString(mcolor("┘\n", "gray"))
	b.WriteString(mcolor(" ODAC", "magenta", "bold"))
	b.WriteString(mcolor(mspacing(monShortcuts, m.width+1-len("ODAC"), "right"), "gray"))
	return b.String()
}

func padEnd(s string, width int) string {
	return s + rep(" ", width-len([]rune(s)))
}

func padStart(s string, width int) string {
	return rep(" ", width-len([]rune(s))) + s
}
