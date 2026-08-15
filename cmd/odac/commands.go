// Command table + dispatch + help, ported from core/Commands.js and
// cli/src/Cli.js (init/help/#detail/parseArg/question). Command and action
// names must stay identical to Node's — the server's action table (contract
// 0.1) and user muscle memory both depend on them.
package main

import (
	"bufio"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

type command struct {
	action      func(a *app, args []string) int
	args        []string // flag/positional names; presence stops sub-descent (Node: action.args)
	description string
	detail      bool // render list data as per-row detail blocks (Node: table: false)
	sub         []entry
	title       string
}

type entry struct {
	name string
	cmd  *command
}

func findEntry(entries []entry, name string) *command {
	for _, e := range entries {
		if e.name == name {
			return e.cmd
		}
	}
	return nil
}

// commands mirrors core/Commands.js in declaration order (help output order).
// Assigned in init(): the help action's closure references the table itself,
// which a plain var initializer would reject as an initialization cycle.
var commands []entry

func init() {
	commands = []entry{
		{"auth", &command{
			args:        []string{"key", "-k", "--key"},
			description: "Define your server to your ODAC account",
			action: func(a *app, args []string) int {
				key := parseArg(args, "-k", "--key")
				if key == "" && len(args) > 0 {
					key = args[0]
				}
				if key == "" {
					key = a.question(__("Enter your authentication key: "))
				}
				return a.call("auth", []any{key}, false)
			},
		}},
		{"debug", &command{
			description: "Debug ODAC Server",
			action: func(a *app, args []string) int {
				return a.monitor("debug")
			},
		}},
		{"help", &command{
			description: "List all available commands",
			action: func(a *app, args []string) int {
				return a.help("", false)
			},
		}},
		{"monit", &command{
			description: "Monitor Applications and Services",
			action: func(a *app, args []string) int {
				return a.monitor("monit")
			},
		}},
		{"restart", &command{
			description: "Restart ODAC Server",
			action: func(a *app, args []string) int {
				a.boot()
				return 0
			},
		}},
		{"update", &command{
			description: "Update ODAC Server",
			action: func(a *app, args []string) int {
				return a.call("update", nil, false)
			},
		}},
		{"app", &command{
			title: "APP",
			sub: []entry{
				{"create", &command{
					description: "Create a new application",
					args:        []string{"-t", "--type", "-n", "--name", "-u", "--url", "-b", "--branch", "--token", "-D", "--dev"},
					action:      appCreateAction,
				}},
				{"delete", &command{
					description: "Delete an App",
					args:        []string{"-i", "--id"},
					action: func(a *app, args []string) int {
						return a.call("app.delete", []any{a.appArg(args)}, false)
					},
				}},
				{"device", &command{
					sub: []entry{
						{"add", &command{
							description: "Connect a hardware device to an app",
							args:        []string{"-a", "--app", "-d", "--device"},
							action: func(a *app, args []string) int {
								app, device := a.appDeviceArgs(args, __("Enter the host device path (e.g. /dev/ttyACM0): "))
								return a.call("app.device.add", []any{app, device}, false)
							},
						}},
						{"delete", &command{
							description: "Disconnect a hardware device from an app",
							args:        []string{"-a", "--app", "-d", "--device"},
							action: func(a *app, args []string) int {
								app, device := a.appDeviceArgs(args, __("Enter the host device path to remove: "))
								return a.call("app.device.delete", []any{app, device}, false)
							},
						}},
					},
				}},
				{"isolate", &command{
					description: "Cut off an app's outbound network access. Use --off to restore it. Restart required.",
					args:        []string{"-i", "--id", "--off"},
					action:      appIsolateAction,
				}},
				{"list", &command{
					description: "List all apps",
					action: func(a *app, args []string) int {
						return a.call("app.list", nil, false)
					},
				}},
				{"network", &command{
					description: "Set an app's network mode: --host to share the host namespace, --bridge (default) for the shared network. Restart required.",
					args:        []string{"-i", "--id", "--host", "--bridge"},
					action:      appNetworkAction,
				}},
				{"privileged", &command{
					description: "Grant elevated access to an app: --root (default) or --full. Use --off to revoke. (At your own risk)",
					args:        []string{"-i", "--id", "--root", "--full", "--off"},
					action:      appPrivilegedAction,
				}},
				{"restart", &command{
					description: "Restart an App",
					args:        []string{"-i", "--id"},
					action: func(a *app, args []string) int {
						return a.call("app.restart", []any{a.appArg(args)}, false)
					},
				}},
				{"start", &command{
					description: "Start a stopped App",
					args:        []string{"-i", "--id"},
					action: func(a *app, args []string) int {
						return a.call("app.start", []any{a.appArg(args)}, false)
					},
				}},
				{"stop", &command{
					description: "Stop a running App",
					args:        []string{"-i", "--id"},
					action: func(a *app, args []string) int {
						return a.call("app.stop", []any{a.appArg(args)}, false)
					},
				}},
			},
		}},
		{"dns", &command{
			title: "DNS",
			sub: []entry{
				{"list", &command{
					description: "List DNS records for a domain",
					args:        []string{"-d", "--domain"},
					action: func(a *app, args []string) int {
						domain := parseArg(args, "-d", "--domain")
						if domain == "" && len(args) > 0 {
							domain = args[0]
						}
						if domain == "" {
							domain = a.question(__("Enter the domain name: "))
						}
						return a.call("dns.list", []any{domain}, true)
					},
				}},
			},
		}},
		{"domain", &command{
			title: "DOMAIN",
			sub: []entry{
				{"add", &command{
					description: "Add a domain to an application",
					args:        []string{"-d", "--domain", "-a", "--app"},
					action: func(a *app, args []string) int {
						domain := parseArg(args, "-d", "--domain")
						app := parseArg(args, "-a", "--app")
						if domain == "" && len(args) > 0 {
							domain = args[0]
						}
						if app == "" && len(args) > 1 {
							app = args[1]
						}
						if domain == "" {
							domain = a.question(__("Enter the domain name: "))
						}
						if app == "" {
							app = a.question(__("Enter the App ID or Name: "))
						}
						return a.call("domain.add", []any{domain, app}, false)
					},
				}},
				{"delete", &command{
					description: "Delete a domain",
					args:        []string{"-d", "--domain"},
					action: func(a *app, args []string) int {
						domain := parseArg(args, "-d", "--domain")
						if domain == "" && len(args) > 0 {
							domain = args[0]
						}
						if domain == "" {
							domain = a.question(__("Enter the domain name: "))
						}
						return a.call("domain.delete", []any{domain}, false)
					},
				}},
				{"list", &command{
					description: "List all domains",
					args:        []string{"-a", "--app"},
					action: func(a *app, args []string) int {
						app := parseArg(args, "-a", "--app")
						if app == "" && len(args) > 0 {
							app = args[0]
						}
						data := []any{}
						if app != "" {
							data = []any{app}
						}
						return a.call("domain.list", data, false)
					},
				}},
			},
		}},
		{"mail", &command{
			title: "MAIL",
			sub: []entry{
				{"create", &command{
					description: "Create a new mail account",
					args:        []string{"-e", "--email", "-p", "--password"},
					action: func(a *app, args []string) int {
						email, password, confirm := a.mailCredentials(args, __("Enter the password: "), __("Re-enter the password: "))
						return a.call("mail.create", []any{email, password, confirm}, false)
					},
				}},
				{"delete", &command{
					description: "Delete a mail account",
					args:        []string{"-e", "--email"},
					action: func(a *app, args []string) int {
						email := parseArg(args, "-e", "--email")
						if email == "" {
							email = a.question(__("Enter the e-mail address: "))
						}
						return a.call("mail.delete", []any{email}, false)
					},
				}},
				{"list", &command{
					description: "List all domain mail accounts",
					args:        []string{"-d", "--domain"},
					action: func(a *app, args []string) int {
						domain := parseArg(args, "-d", "--domain")
						if domain == "" {
							domain = a.question(__("Enter the domain name: "))
						}
						return a.call("mail.list", []any{domain}, false)
					},
				}},
				{"password", &command{
					description: "Change mail account password",
					args:        []string{"-e", "--email", "-p", "--password"},
					action: func(a *app, args []string) int {
						email, password, confirm := a.mailCredentials(args, __("Enter the new password: "), __("Re-enter the new password: "))
						return a.call("mail.password", []any{email, password, confirm}, false)
					},
				}},
			},
		}},
		{"ssl", &command{
			title: "SSL",
			sub: []entry{
				{"renew", &command{
					description: "Renew SSL certificate for a domain",
					args:        []string{"-d", "--domain"},
					action: func(a *app, args []string) int {
						domain := parseArg(args, "-d", "--domain")
						if domain == "" {
							domain = a.question(__("Enter the domain name: "))
						}
						return a.call("ssl.renew", []any{domain}, false)
					},
				}},
			},
		}},
	}
}

// dispatch ports Cli.init's argument walk: descend into sub-commands while
// the current node declares no args of its own; run the action or fall back
// to that command's help.
func (a *app) dispatch(args []string) int {
	full := strings.Join(args, " ")
	cmd := findEntry(commands, args[0])
	if cmd == nil {
		fmt.Fprintln(a.out, __("'%s' is not a valid command.", color("odac "+full, ansiYellow)))
		return 1
	}
	top := args[0]
	rest := args[1:]
	for len(rest) > 0 && len(cmd.args) == 0 {
		next := findEntry(cmd.sub, rest[0])
		if next == nil {
			return a.help(top, false)
		}
		cmd = next
		rest = rest[1:]
	}
	if cmd.action != nil {
		return cmd.action(a, rest)
	}
	return a.help(top, false)
}

// --- shared argument helpers ---

// parseArg ports Cli.parseArg: the value following the first matching prefix.
func parseArg(args []string, prefixes ...string) string {
	for i := 0; i+1 < len(args); i++ {
		if slices.Contains(prefixes, args[i]) {
			return args[i+1]
		}
	}
	return ""
}

// question ports Cli.question: prompt on stdout, read one trimmed line.
func (a *app) question(prompt string) string {
	fmt.Fprint(a.out, prompt)
	if a.reader == nil {
		a.reader = bufio.NewReader(a.in)
	}
	line, _ := a.reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func (a *app) appArg(args []string) string {
	app := parseArg(args, "-i", "--id")
	if app == "" && len(args) > 0 {
		app = args[0]
	}
	if app == "" {
		app = a.question(__("Enter the App ID or Name: "))
	}
	return app
}

func (a *app) appDeviceArgs(args []string, devicePrompt string) (string, string) {
	app := parseArg(args, "-a", "--app")
	device := parseArg(args, "-d", "--device")
	if app == "" && len(args) > 0 {
		app = args[0]
	}
	if device == "" && len(args) > 1 {
		device = args[1]
	}
	if app == "" {
		app = a.question(__("Enter the App ID or Name: "))
	}
	if device == "" {
		device = a.question(devicePrompt)
	}
	return app, device
}

// mailCredentials implements the shared create/password flow: when -p is
// given the confirmation is skipped; otherwise the password is asked twice.
func (a *app) mailCredentials(args []string, passwordPrompt, confirmPrompt string) (string, string, string) {
	email := parseArg(args, "-e", "--email")
	password := parseArg(args, "-p", "--password")
	fromFlag := password != ""
	if email == "" {
		email = a.question(__("Enter the e-mail address: "))
	}
	if password == "" {
		password = a.question(passwordPrompt)
	}
	confirm := password
	if !fromFlag {
		confirm = a.question(confirmPrompt)
	}
	return email, password, confirm
}

// --- complex actions ---

var (
	gitSchemeRe = regexp.MustCompile(`^(https?|git|ssh)://`)
	gitScpRe    = regexp.MustCompile(`^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:`)
	nameCharsRe = regexp.MustCompile(`[^a-zA-Z0-9-]`)
)

// deriveAppName ports `path.basename(url, '.git')` + sanitize.
func deriveAppName(url string) string {
	base := strings.TrimSuffix(path.Base(url), ".git")
	return nameCharsRe.ReplaceAllString(base, "-")
}

func appCreateAction(a *app, args []string) int {
	typ := parseArg(args, "-t", "--type")
	if typ == "" {
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				typ = arg
				break
			}
		}
	}
	isDev := slices.Contains(args, "-D") || slices.Contains(args, "--dev")

	if typ == "git" || typ == "github" {
		url := parseArg(args, "-u", "--url")
		if url == "" {
			url = a.question(__("Enter Git URL: "))
		}
		name := parseArg(args, "-n", "--name")
		if name == "" {
			name = deriveAppName(url)
		}
		// branch/token stay null when absent — Node sends them as JSON null.
		config := map[string]any{
			"type":   "git",
			"url":    url,
			"name":   name,
			"branch": nullable(parseArg(args, "-b", "--branch")),
			"token":  nullable(parseArg(args, "--token")),
			"dev":    isDev,
		}
		return a.call("app.create", []any{config}, false)
	}

	if typ == "" {
		typ = a.question(__("Enter the app type or repo: "))
	}

	if gitSchemeRe.MatchString(typ) || gitScpRe.MatchString(typ) {
		// Auto-detected Git URL: Node builds this object WITHOUT branch/token keys.
		config := map[string]any{
			"type": "git",
			"url":  typ,
			"name": deriveAppName(typ),
			"dev":  isDev,
		}
		return a.call("app.create", []any{config}, false)
	}
	if isDev {
		return a.call("app.create", []any{map[string]any{"type": "app", "app": typ, "dev": true}}, false)
	}
	return a.call("app.create", []any{typ}, false)
}

// appIDArg resolves the app from -i/--id, else the first non-flag argument,
// else a prompt.
func appIDArg(a *app, args []string) string {
	app := parseArg(args, "-i", "--id")
	if app == "" {
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				app = arg
				break
			}
		}
	}
	if app == "" {
		app = a.question(__("Enter the App ID or Name: "))
	}
	return app
}

func appNetworkAction(a *app, args []string) int {
	app := appIDArg(a, args)

	mode := "bridge"
	if slices.Contains(args, "--host") {
		mode = "host"
	}

	if mode == "host" {
		fmt.Fprintln(a.out, __("WARNING: Host networking removes this app's network isolation. It shares the host's network namespace, binds host ports directly (published port mappings stop applying), and can reach every service listening on loopback — including ODAC's own API. Apps with routed domains are refused, because host networking rules out zero-downtime deploys."))
		if !strings.EqualFold(a.question(__(`Type "yes" to continue: `)), "yes") {
			fmt.Fprintln(a.out, __("Aborted."))
			return 1
		}
	}
	return a.call("app.network", []any{app, mode}, false)
}

func appIsolateAction(a *app, args []string) int {
	app := appIDArg(a, args)
	isolated := !slices.Contains(args, "--off")

	if isolated {
		fmt.Fprintln(a.out, __("NOTE: An isolated app has no outbound network access at all — no package installs at runtime, no outbound API calls, no update checks. Domains and the proxy keep working, but published ports become reachable from this host only, and the app can no longer reach apps on the shared network."))
	}
	return a.call("app.isolate", []any{app, isolated}, false)
}

func appPrivilegedAction(a *app, args []string) int {
	app := appIDArg(a, args)

	mode := "root"
	if slices.Contains(args, "--off") {
		mode = "off"
	} else if slices.Contains(args, "--full") {
		mode = "full"
	}

	if mode != "off" {
		warning := __("WARNING: This will run the app as ROOT inside its container. Grant only to apps you trust.")
		if mode == "full" {
			warning = __("WARNING: FULL privileged mode gives this app COMPLETE access to host devices and the kernel. This is dangerous and entirely at your own risk.")
		}
		fmt.Fprintln(a.out, warning)
		if !strings.EqualFold(a.question(__(`Type "yes" to continue: `)), "yes") {
			fmt.Fprintln(a.out, __("Aborted."))
			return 1
		}
	}
	return a.call("app.privileged", []any{app, mode}, false)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- help ---

type helpRow struct {
	desc   string
	prefix string // colored "odac app create <args>"; empty for title rows
	title  string
}

// help ports Cli.help/#detail: filter narrows to one top-level command
// (dispatch fallback), actionOnly lists only top-level runnable commands
// (status view). The `auth` command is hidden once Hub-authenticated.
func (a *app) help(filter string, actionOnly bool) int {
	token, _ := a.cfg.Map("hub")["token"].(string)
	var rows []helpRow
	for _, e := range commands {
		if filter != "" && e.name != filter {
			continue
		}
		if token != "" && e.name == "auth" {
			continue
		}
		if actionOnly && e.cmd.action == nil {
			continue
		}
		collectHelp(&rows, e.name, e.cmd)
	}

	width := 0
	for _, row := range rows {
		if n := visibleLen(row.prefix); n > width {
			width = n
		}
	}
	for _, row := range rows {
		if row.title != "" {
			fmt.Fprintln(a.out, "\n"+color(row.title, ansiGray))
			continue
		}
		pad := strings.Repeat(" ", width-visibleLen(row.prefix))
		fmt.Fprintln(a.out, row.prefix+pad+" : "+row.desc)
	}
	fmt.Fprintln(a.out)
	return 0
}

func collectHelp(rows *[]helpRow, path string, c *command) {
	if c.title != "" {
		*rows = append(*rows, helpRow{title: c.title})
	}
	if c.description != "" {
		positional := ""
		for _, arg := range c.args {
			if !strings.HasPrefix(arg, "-") {
				positional += " <" + arg + ">"
			}
		}
		*rows = append(*rows, helpRow{
			prefix: color("odac "+path, 91) + color(positional, ansiGray),
			desc:   __(c.description),
		})
	}
	for _, sub := range c.sub {
		collectHelp(rows, path+" "+sub.name, sub.cmd)
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLen(s string) int {
	return len([]rune(ansiRe.ReplaceAllString(s, "")))
}
