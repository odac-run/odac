// Terminal rendering: ANSI colors, status icons, the progress-line spinner
// and final-response output, ported from cli/src/Cli.js (icon/color) and
// cli/src/Connector.js (data handler + printTable).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"odac/internal/apiproto"
)

const (
	ansiRed     = 31
	ansiGreen   = 32
	ansiYellow  = 33
	ansiMagenta = 35
	ansiGray    = 90
)

func color(text string, code int) string {
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, text)
}

// icon mirrors Cli.icon(status) — the three-char colored status markers
// prefixed to progress lines.
func icon(status string) string {
	switch status {
	case "errored":
		return color(" ! ", ansiRed)
	case "progress":
		return color(" - ", ansiGray)
	case "running":
		return color(" ▶ ", ansiGreen)
	case "stopped":
		return color(" ⏸ ", ansiYellow)
	case "success":
		return color(" ✓ ", ansiGreen)
	}
	return "   "
}

// renderer reproduces Connector.js's data handler: progress lines for the
// same process overwrite each other in place; a new process starts a new
// line; the final response is printed after a closing newline.
type renderer struct {
	out         io.Writer
	errOut      io.Writer
	detail      bool // per-row detail blocks instead of a table (Node: table: false)
	lastProcess string
	sawProgress bool
}

func (r *renderer) progress(p apiproto.Progress) {
	if r.sawProgress && r.lastProcess == p.Process {
		// clear line + cursor to column 0, byte-matching Node's
		// clearLine(0) + cursorTo(0)
		fmt.Fprint(r.out, "\x1b[2K\x1b[1G")
	} else {
		r.lastProcess = p.Process
		fmt.Fprint(r.out, "\n")
	}
	r.sawProgress = true
	fmt.Fprint(r.out, icon(p.Status)+p.Message+"\r")
}

func (r *renderer) final(resp *apiproto.Response) {
	if r.sawProgress {
		fmt.Fprintln(r.out)
	}
	if !resp.Result {
		msg := resp.Message
		if msg == "" {
			msg = "Unknown error"
		}
		fmt.Fprintln(r.errOut, msg)
		if len(resp.Data) > 0 && !bytes.Equal(resp.Data, []byte("null")) {
			var pretty bytes.Buffer
			if json.Indent(&pretty, resp.Data, "", "  ") == nil {
				fmt.Fprintln(r.errOut, pretty.String())
			}
		}
		return
	}

	if resp.Message != "" {
		fmt.Fprintln(r.out, resp.Message)
	}
	renderData(r.out, resp.Data, r.detail)
}

// renderData prints the final response payload: non-empty arrays of rows as a
// table (or detail blocks), other objects as pretty JSON (Node: console.dir).
func renderData(out io.Writer, data json.RawMessage, detail bool) {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return
	}
	var rows []map[string]any
	if json.Unmarshal(data, &rows) == nil {
		if len(rows) > 0 {
			if detail {
				printDetail(out, data, rows)
			} else {
				printTable(out, data, rows)
			}
		}
		return
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") == nil {
			fmt.Fprintln(out, pretty.String())
		}
	}
}

// tableWidth mirrors Node's `process.stdout.columns || 80` (#printTable):
// tables stretch to the terminal width, and a piped stdout stretches to 80.
// Variable so tests can pin it.
var tableWidth = func() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// printTable ports Connector.js #printTable: uppercase headers, columns
// padded to max(content, header)+2 and stretched evenly to the terminal
// width, a dashed separator, date-like numeric values formatted as local
// "YYYY-MM-DD HH:MM". Column order follows the first row's JSON key order
// (Go maps don't preserve it, so it is re-read from the raw payload).
func printTable(out io.Writer, raw json.RawMessage, rows []map[string]any) {
	keys := firstRowKeys(raw)
	if len(keys) == 0 {
		return
	}

	cells := make([][]string, len(rows))
	for i, row := range rows {
		cells[i] = make([]string, len(keys))
		for j, k := range keys {
			cells[i][j] = cellValue(k, row[k])
		}
	}

	// Widths use Node's measure, not the rendered cell: `String(val || '')`
	// joins arrays with "," (no space) and maps falsy values to "", while the
	// cell renders with ", " and "-". Array columns therefore measure one
	// short per extra element, exactly like #printTable.
	widths := make([]int, len(keys))
	for j, k := range keys {
		widths[j] = len([]rune(strings.ToUpper(k)))
		for _, row := range rows {
			if n := len([]rune(measureValue(k, row[k]))); n > widths[j] {
				widths[j] = n
			}
		}
		widths[j] += 2
	}

	total := 0
	for _, w := range widths {
		total += w
	}
	if tw := tableWidth(); tw > total {
		extra := (tw - total) / len(keys)
		for j := range widths {
			widths[j] += extra
		}
	}

	var line strings.Builder
	for j, k := range keys {
		pad(&line, strings.ToUpper(k), widths[j])
	}
	fmt.Fprintln(out, line.String())

	line.Reset()
	for _, w := range widths {
		line.WriteString(strings.Repeat("-", w))
	}
	fmt.Fprintln(out, line.String())

	for i := range cells {
		line.Reset()
		for j := range keys {
			pad(&line, cells[i][j], widths[j])
		}
		fmt.Fprintln(out, line.String())
	}
	fmt.Fprintln(out)
}

func pad(b *strings.Builder, s string, width int) {
	b.WriteString(s)
	if n := width - len([]rune(s)); n > 0 {
		b.WriteString(strings.Repeat(" ", n))
	}
}

// printDetail ports Connector.js's `table: false` branch: each row becomes a
// "---"-delimited block of "key : value" lines, keys padded per row. Unlike
// the table path there is no date formatting (Node doesn't apply it here).
func printDetail(out io.Writer, raw json.RawMessage, rows []map[string]any) {
	rawList := rawRows(raw)
	for i, row := range rows {
		var keys []string
		if i < len(rawList) {
			keys = objectKeys(rawList[i])
		}
		maxLen := 0
		for _, k := range keys {
			if len(k) > maxLen {
				maxLen = len(k)
			}
		}
		fmt.Fprintln(out, "---")
		for _, k := range keys {
			fmt.Fprintf(out, "%-*s : %s\n", maxLen, k, detailValue(row[k]))
		}
	}
	fmt.Fprintln(out, "---")
}

func rawRows(raw json.RawMessage) []json.RawMessage {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	return rows
}

// firstRowKeys extracts the first row's keys in JSON document order.
func firstRowKeys(raw json.RawMessage) []string {
	rows := rawRows(raw)
	if len(rows) == 0 {
		return nil
	}
	return objectKeys(rows[0])
}

// objectKeys extracts a JSON object's top-level keys in document order
// (Go maps don't preserve it, so it is re-read from the raw payload).
func objectKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var keys []string
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '{' || t == '[' {
				depth++
			} else {
				depth--
			}
		case string:
			// At depth 1 the decoder alternates key/value; skip values.
			if depth == 1 {
				keys = append(keys, t)
				skipValue(dec)
			}
		default:
			// non-string scalar can't be a key at depth 1 (skipValue
			// consumes values), ignore
		}
	}
	return keys
}

func skipValue(dec *json.Decoder) {
	tok, err := dec.Token()
	if err != nil {
		return
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		depth := 1
		for depth > 0 {
			tok, err := dec.Token()
			if err != nil {
				return
			}
			if d, ok := tok.(json.Delim); ok {
				if d == '{' || d == '[' {
					depth++
				} else {
					depth--
				}
			}
		}
	}
}

// measureValue is the string Node sizes a column by: `String(val || ”)` on
// the date-formatted row — falsy values measure as "" (they render as "-"),
// arrays measure with a bare "," join (they render with ", ").
func measureValue(key string, v any) string {
	if arr, ok := v.([]any); ok {
		if len(arr) == 0 {
			return ""
		}
		parts := make([]string, len(arr))
		for i, item := range arr {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	}
	switch n := v.(type) {
	case nil:
		return ""
	case bool:
		if !n {
			return ""
		}
		return "true"
	case string:
		if n == "" {
			return ""
		}
	case float64:
		if n == 0 {
			return ""
		}
	}
	return cellValue(key, v)
}

// cellValue formats one table cell like detailValue, plus the table-only
// date rule: numeric values under date-like keys become timestamps (ms vs s
// heuristic: values above 1e11 are already milliseconds).
func cellValue(key string, v any) string {
	if n, isNum := numericValue(v); isNum && n > 0 && isDateKey(key) {
		ms := n
		if n <= 1e11 {
			ms = n * 1000
		}
		return time.UnixMilli(int64(ms)).Format("2006-01-02 15:04")
	}
	return detailValue(v)
}

// detailValue formats one value: arrays join with ", "; Node's `val || '-'`
// renders every falsy value as "-".
func detailValue(v any) string {
	if v == nil {
		return "-"
	}
	if arr, ok := v.([]any); ok {
		parts := make([]string, len(arr))
		for i, item := range arr {
			parts[i] = fmt.Sprint(item)
		}
		if s := strings.Join(parts, ", "); s != "" {
			return s
		}
		return "-"
	}

	s := fmt.Sprint(v)
	if f, ok := v.(float64); ok {
		s = strconv.FormatFloat(f, 'f', -1, 64)
	}
	if s == "" || s == "false" || s == "0" {
		return "-"
	}
	return s
}

func isDateKey(key string) bool {
	lower := strings.ToLower(key)
	for _, marker := range []string{"created", "date", "started", "updated", "time"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}
