// Server liveness check and the no-argument status view, ported from
// cli/src/Connector.js check() and cli/src/Cli.js #status.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"odac/internal/apiproto"
)

// check reports whether the server is reachable: TCP probe first (works in
// Docker), then the bare-metal fallback of verifying the recorded watchdog
// PID is alive and named `node` or `odac-watchdog` (PID reuse guard).
func (a *app) check() bool {
	if apiproto.Ping(a.client.Addr, apiproto.DefaultDialTimeout) {
		return true
	}
	server := a.cfg.Map("server")
	pid, ok := server["watchdog"].(float64)
	if !ok || pid <= 0 {
		return false
	}
	name := processName(int(pid))
	return name == "node" || name == "odac-watchdog"
}

// processName returns the executable name of a running process, or "" when
// the process does not exist. /proc works on Linux; `ps` covers Darwin
// (where -o comm= may print the full executable path, hence the Base).
func processName(pid int) string {
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

// status renders the summary shown by a bare `odac` invocation. The command
// list that Node appends comes with the command table in task 2.2.
func (a *app) status() int {
	online := a.check()

	hub := a.cfg.Map("hub")
	token, _ := hub["token"].(string)
	authenticated := token != ""

	rows := []struct{ label, value string }{
		{"Status", statusValue(online)},
	}
	if online {
		server := a.cfg.Map("server")
		if started, ok := server["started"].(float64); ok && started > 0 {
			rows = append(rows, struct{ label, value string }{
				"Uptime", color(" "+uptimeString(time.Since(time.UnixMilli(int64(started)))), ansiGreen),
			})
		}
		apps, _ := a.cfg.Get("apps").([]any)
		rows = append(rows, struct{ label, value string }{
			"Apps", color(" "+strconv.Itoa(len(apps)), ansiGreen),
		})
		rows = append(rows, struct{ label, value string }{
			"Domains", color(" "+strconv.Itoa(len(a.cfg.Map("domains"))), ansiGreen),
		})
	}
	rows = append(rows, struct{ label, value string }{"Auth", authValue(authenticated)})

	width := 0
	for _, row := range rows {
		if len(row.label) > width {
			width = len(row.label)
		}
	}
	for _, row := range rows {
		fmt.Fprintf(a.out, "%s%s : %s\n", row.label, strings.Repeat(" ", width-len(row.label)), row.value)
	}
	if !authenticated {
		fmt.Fprintf(a.out, "Login on %s to manage all your server operations.\n", color("https://odac.run", 95))
	}
	fmt.Fprintln(a.out)
	return 0
}

func statusValue(online bool) string {
	if online {
		return color(" Online", ansiGreen)
	}
	return color(" Offline", ansiYellow)
}

func authValue(authenticated bool) string {
	if authenticated {
		return color(" Logged in", ansiGreen)
	}
	return color(" Not logged in", ansiYellow)
}

// uptimeString mirrors Cli.#status formatting: days+hours, or
// hours+minutes, minutes+seconds, plain seconds as the span shrinks.
func uptimeString(d time.Duration) string {
	seconds := int(d.Seconds())
	minutes := seconds / 60
	hours := minutes / 60
	days := hours / 24
	seconds %= 60
	minutes %= 60
	hours %= 24

	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd ", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh ", hours)
	}
	if minutes > 0 && days == 0 {
		fmt.Fprintf(&b, "%dm ", minutes)
	}
	if seconds > 0 && hours == 0 {
		fmt.Fprintf(&b, "%ds", seconds)
	}
	return strings.TrimSpace(b.String())
}
