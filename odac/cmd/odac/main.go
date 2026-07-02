// odac is the ODAC CLI (Go port of cli/ — task 2.1: skeleton + connector).
//
// This skeleton covers the connector (contract 0.1 client via
// internal/apiproto), liveness checking, the no-argument status view and a
// generic `odac api <action> [args...]` invoker. Command surface parity with
// core/Commands.js (including boot-on-demand of a stopped server) is task
// 2.2; the monitor TUI is 2.3; i18n is 2.4. Until 2.2 lands, the Node CLI
// remains the user-facing entry point.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"odac/internal/apiproto"
	"odac/internal/config"
)

type app struct {
	cfg    *config.Store
	client *apiproto.Client
	out    io.Writer
	errOut io.Writer
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
		out:    os.Stdout,
		errOut: os.Stderr,
	}
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
	fmt.Fprintln(a.out, "\n "+color("ODAC", ansiMagenta)+" \n")

	if len(args) == 0 {
		return a.status()
	}

	switch args[0] {
	case "api":
		if len(args) < 2 {
			fmt.Fprintln(a.errOut, "Usage: odac api <action> [args...]")
			return 1
		}
		return a.callAction(args[1], args[2:])
	}

	fmt.Fprintf(a.errOut, "'odac %s' is not ported to the Go CLI yet (task 2.2); use the Node CLI.\n",
		strings.Join(args, " "))
	return 1
}

// callAction performs one request/response cycle: root auth from api.json,
// progress lines rendered as they stream, final response rendered last.
func (a *app) callAction(action string, rawArgs []string) int {
	if !a.check() {
		// Node's CLI boots the server here; boot-on-demand is ported in 2.2
		// together with the rest of the init flow.
		fmt.Fprintln(a.errOut, "Odac server is not running.")
		return 1
	}

	auth, _ := a.cfg.Map("api")["auth"].(string)
	r := &renderer{out: a.out, errOut: a.errOut}
	resp, err := a.client.Call(
		apiproto.Request{Auth: auth, Action: action, Data: parseArgs(rawArgs)},
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

// parseArgs converts CLI arguments to the positional `data` array. Arguments
// that parse as JSON are passed typed (numbers, booleans, objects); everything
// else is a plain string, matching how Node command handlers receive values.
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
