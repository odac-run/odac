// odac is the ODAC CLI (Go port of cli/ — tasks 2.1 connector, 2.2 command
// surface, 2.3 monitor TUI, 2.4 i18n, 2.5 parity sign-off). Byte-parity with
// the Node CLI is verified; the bin/odac entry point switches over in the
// Phase 4 restructure.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"odac/internal/apiproto"
	"odac/internal/config"
	"odac/internal/lang"
)

// __ mirrors Node's global `__` (core/Odac.js → Lang.get): the same literals
// are wrapped at the same sites, so the translated surfaces stay greppable
// against cli/ and core/Commands.js for the 2.5 parity pass.
var __ = lang.T

type app struct {
	boot   func() // boot-on-demand hook; defaultBoot in production, stubbed in tests
	booted bool
	cfg    *config.Store
	client *apiproto.Client
	errOut io.Writer
	in     io.Reader
	out    io.Writer
	reader *bufio.Reader // lazy, persistent stdin reader for question()
}

func main() {
	cfg, err := config.Open(config.DefaultBaseDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to load config:", err)
		os.Exit(1)
	}

	a := &app{
		cfg:    cfg,
		client: &apiproto.Client{Addr: apiAddr()},
		errOut: os.Stderr,
		in:     os.Stdin,
		out:    os.Stdout,
	}
	a.boot = a.defaultBoot
	os.Exit(a.run(os.Args[1:]))
}

// apiAddr allows tests and smoke scripts to point the CLI at a fake server;
// real deployments always use the contract-fixed 127.0.0.1:1453.
func apiAddr() string {
	if addr := os.Getenv("ODAC_API_ADDR"); addr != "" {
		return addr
	}
	return apiproto.DefaultAddr
}

func (a *app) run(args []string) int {
	if len(args) == 1 && args[0] == "healthcheck" {
		// Docker HEALTHCHECK probe (not part of the Node surface): exit 0
		// when the server accepts connections. No banner, no boot attempt —
		// a health probe must never restart what it is checking.
		if apiproto.Ping(a.client.Addr, apiproto.DefaultDialTimeout) {
			return 0
		}
		fmt.Fprintln(a.errOut, "odac server unreachable at "+a.client.Addr)
		return 1
	}

	fmt.Fprintln(a.out, "\n "+color("ODAC", ansiMagenta)+" \n")

	// Cli.init boots the server before dispatching any command.
	if !a.check() {
		a.boot()
	}

	if len(args) == 0 {
		return a.status()
	}
	if args[0] == "api" {
		// Generic action invoker (not part of the Node surface): arguments
		// parse as JSON when possible, plain strings otherwise.
		if len(args) < 2 {
			fmt.Fprintln(a.errOut, "Usage: odac api <action> [args...]")
			return 1
		}
		return a.call(args[1], parseArgs(args[2:]), false)
	}
	return a.dispatch(args)
}

// call performs one request/response cycle: root auth from api.json,
// progress lines rendered as they stream, final response rendered last.
func (a *app) call(action string, data []any, detail bool) int {
	if !a.check() {
		fmt.Fprintln(a.errOut, "Odac server is not running.")
		return 1
	}

	auth, _ := a.cfg.Map("api")["auth"].(string)
	r := &renderer{out: a.out, errOut: a.errOut, detail: detail}
	resp, err := a.client.Call(
		apiproto.Request{Auth: auth, Action: action, Data: data},
		r.progress,
	)
	if err != nil {
		fmt.Fprintln(a.errOut, "Socket error:", err)
		return 1
	}
	r.final(resp)
	if resp.Result {
		return 0
	}
	return 1
}

// parseArgs converts `odac api` arguments to the positional `data` array.
func parseArgs(rawArgs []string) []any {
	data := make([]any, 0, len(rawArgs))
	for _, arg := range rawArgs {
		var v any
		if err := json.Unmarshal([]byte(arg), &v); err == nil {
			data = append(data, v)
		} else {
			data = append(data, arg)
		}
	}
	return data
}
