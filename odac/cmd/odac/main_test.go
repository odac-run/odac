package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"odac/internal/apiproto"
	"odac/internal/config"
)

// fakeServer implements the server side of contract 0.1 for one request per
// connection: read a single JSON document, reply with a scripted stream.
func fakeServer(t *testing.T, handler func(req apiproto.Request, conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64*1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				var req apiproto.Request
				if json.Unmarshal(buf[:n], &req) != nil {
					c.Write([]byte(`{"result":false,"message":"invalid_json"}`))
					return
				}
				handler(req, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func testApp(t *testing.T, addr string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("api", map[string]any{"auth": "roottoken"})
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return &app{
		cfg:    cfg,
		client: &apiproto.Client{Addr: addr},
		out:    out,
		errOut: errOut,
	}, out, errOut
}

func TestRunAPIActionEndToEnd(t *testing.T) {
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		if req.Auth != "roottoken" {
			conn.Write([]byte(`{"id":"e","result":false,"message":"unauthorized"}`))
			return
		}
		if req.Action != "app.list" || len(req.Data) != 0 {
			t.Errorf("server got action=%q data=%v", req.Action, req.Data)
		}
		conn.Write([]byte(`{"process":"list","status":"progress","message":"listing"}` + "\r\n"))
		time.Sleep(10 * time.Millisecond)
		conn.Write([]byte(`{"id":"a1","result":true,"message":null,` +
			`"data":[{"name":"blog","status":"running","created":1767225600000}]}`))
	})

	a, out, errOut := testApp(t, addr)
	if code := a.run([]string{"api", "app.list"}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}

	got := out.String()
	for _, want := range []string{"listing", "NAME", "STATUS", "CREATED", "blog", "running", "2026-01-01"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunAPIActionError(t *testing.T) {
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		conn.Write([]byte(`{"id":"e1","result":false,"message":"unknown_action"}`))
	})

	a, _, errOut := testApp(t, addr)
	if code := a.run([]string{"api", "nope"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "unknown_action") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunStatusOffline(t *testing.T) {
	// Closed port + no watchdog PID in config → offline path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	a, out, _ := testApp(t, dead)
	if code := a.run(nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "Offline") || !strings.Contains(got, "Not logged in") {
		t.Errorf("status output:\n%s", got)
	}
	if strings.Contains(got, "Uptime") {
		t.Errorf("offline status must not print uptime:\n%s", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	a, _, errOut := testApp(t, "127.0.0.1:0")
	if code := a.run([]string{"frobnicate"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "frobnicate") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []any
	}{
		{"plain strings", []string{"example.com", "www"}, []any{"example.com", "www"}},
		{"typed values", []string{"5", "true", `{"a":1}`}, []any{float64(5), true, map[string]any{"a": float64(1)}}},
		{"quoted string stays string", []string{`"5"`}, []any{"5"}},
		{"empty", nil, []any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseArgs(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseArgs(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestUptimeString(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{3*time.Minute + 5*time.Second, "3m 5s"},
		{2*time.Hour + 10*time.Minute, "2h 10m"},
		{50 * time.Hour, "2d 2h"},
		{49 * time.Hour, "2d 1h"},
		{24 * time.Hour, "1d"},
	}
	for _, tt := range tests {
		if got := uptimeString(tt.d); got != tt.want {
			t.Errorf("uptimeString(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestIcon(t *testing.T) {
	tests := []struct {
		status, symbol string
	}{
		{"errored", "!"}, {"progress", "-"}, {"running", "▶"},
		{"stopped", "⏸"}, {"success", "✓"}, {"other", "   "},
	}
	for _, tt := range tests {
		if got := icon(tt.status); !strings.Contains(got, tt.symbol) {
			t.Errorf("icon(%q) = %q, want to contain %q", tt.status, got, tt.symbol)
		}
	}
}

func TestCellValue(t *testing.T) {
	tests := []struct {
		name string
		key  string
		v    any
		want string
	}{
		{"nil is dash", "x", nil, "-"},
		{"empty string is dash", "x", "", "-"},
		{"false is dash (Node falsy)", "x", false, "-"},
		{"array joins", "ports", []any{float64(80), float64(443)}, "80, 443"},
		{"plain number", "count", float64(12), "12"},
		{"ms timestamp under date key", "created", float64(1767225600000), "2026-01-01"},
		{"seconds timestamp under date key", "updated", float64(1767225600), "2026-01-01"},
		{"numeric string under date key", "started", "1767225600000", "2026-01-01"},
		{"number under non-date key untouched", "size", float64(1767225600000), "1767225600000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cellValue(tt.key, tt.v); !strings.HasPrefix(got, tt.want) {
				t.Errorf("cellValue(%q, %v) = %q, want prefix %q", tt.key, tt.v, got, tt.want)
			}
		})
	}
}

func TestFirstRowKeysPreservesJSONOrder(t *testing.T) {
	raw := json.RawMessage(`[{"zeta":1,"alpha":{"nested":true},"mid":[1,2]},{"other":0}]`)
	want := []string{"zeta", "alpha", "mid"}
	if got := firstRowKeys(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("firstRowKeys = %v, want %v", got, want)
	}
}

func TestProcessName(t *testing.T) {
	if name := processName(os.Getpid()); name == "" {
		t.Error("processName(own pid) = \"\", want non-empty")
	}
	if name := processName(1 << 30); name != "" {
		t.Errorf("processName(bogus pid) = %q, want \"\"", name)
	}
}
