package main

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"odac/internal/apiproto"
)

// recordingServer answers every action with a bare success and records the
// last request for assertions.
func recordingServer(t *testing.T) (string, *apiproto.Request) {
	t.Helper()
	last := &apiproto.Request{}
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		*last = req
		conn.Write([]byte(`{"id":"r","result":true,"message":"ok"}`))
	})
	return addr, last
}

func TestDispatchActionsAndData(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		stdin      string
		wantAction string
		wantData   []any
	}{
		{"app list", []string{"app", "list"}, "", "app.list", []any{}},
		{"app delete positional", []string{"app", "delete", "42"}, "", "app.delete", []any{"42"}},
		{"app delete flag", []string{"app", "delete", "-i", "42"}, "", "app.delete", []any{"42"}},
		{"app restart", []string{"app", "restart", "blog"}, "", "app.restart", []any{"blog"}},
		{"app start", []string{"app", "start", "blog"}, "", "app.start", []any{"blog"}},
		{"app stop", []string{"app", "stop", "blog"}, "", "app.stop", []any{"blog"}},
		{"app device add", []string{"app", "device", "add", "blog", "/dev/ttyACM0"}, "",
			"app.device.add", []any{"blog", "/dev/ttyACM0"}},
		{"app device delete flags", []string{"app", "device", "delete", "-a", "blog", "-d", "/dev/x"}, "",
			"app.device.delete", []any{"blog", "/dev/x"}},
		{"domain add positional", []string{"domain", "add", "example.com", "blog"}, "",
			"domain.add", []any{"example.com", "blog"}},
		{"domain add flags", []string{"domain", "add", "-d", "example.com", "-a", "blog"}, "",
			"domain.add", []any{"example.com", "blog"}},
		{"domain delete", []string{"domain", "delete", "example.com"}, "", "domain.delete", []any{"example.com"}},
		{"domain list bare", []string{"domain", "list"}, "", "domain.list", []any{}},
		{"domain list filtered", []string{"domain", "list", "blog"}, "", "domain.list", []any{"blog"}},
		{"dns list", []string{"dns", "list", "example.com"}, "", "dns.list", []any{"example.com"}},
		{"mail create flags skip confirm", []string{"mail", "create", "-e", "a@x.com", "-p", "pw"}, "",
			"mail.create", []any{"a@x.com", "pw", "pw"}},
		{"mail create interactive", []string{"mail", "create"}, "a@x.com\npw1\npw2\n",
			"mail.create", []any{"a@x.com", "pw1", "pw2"}},
		{"mail delete", []string{"mail", "delete", "-e", "a@x.com"}, "", "mail.delete", []any{"a@x.com"}},
		{"mail list", []string{"mail", "list", "-d", "x.com"}, "", "mail.list", []any{"x.com"}},
		{"mail password", []string{"mail", "password", "-e", "a@x.com", "-p", "np"}, "",
			"mail.password", []any{"a@x.com", "np", "np"}},
		{"ssl renew", []string{"ssl", "renew", "-d", "example.com"}, "", "ssl.renew", []any{"example.com"}},
		{"auth positional", []string{"auth", "SECRETKEY"}, "", "auth", []any{"SECRETKEY"}},
		{"auth interactive", []string{"auth"}, "typedkey\n", "auth", []any{"typedkey"}},
		{"update", []string{"update"}, "", "update", []any{}},
		{"privileged default root", []string{"app", "privileged", "blog"}, "yes\n",
			"app.privileged", []any{"blog", "root"}},
		{"privileged full", []string{"app", "privileged", "blog", "--full"}, "YES\n",
			"app.privileged", []any{"blog", "full"}},
		{"privileged off no confirm", []string{"app", "privileged", "blog", "--off"}, "",
			"app.privileged", []any{"blog", "off"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, last := recordingServer(t)
			a, _, errOut := testApp(t, addr)
			a.in = strings.NewReader(tt.stdin)
			if code := a.run(tt.argv); code != 0 {
				t.Fatalf("exit = %d, stderr: %s", code, errOut)
			}
			if last.Action != tt.wantAction {
				t.Errorf("action = %q, want %q", last.Action, tt.wantAction)
			}
			if !reflect.DeepEqual(last.Data, tt.wantData) {
				t.Errorf("data = %#v, want %#v", last.Data, tt.wantData)
			}
			if last.Auth != "roottoken" {
				t.Errorf("auth = %q", last.Auth)
			}
		})
	}
}

func TestAppCreateVariants(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		stdin    string
		wantData []any
	}{
		{"explicit git with branch", []string{"app", "create", "git", "-u", "https://github.com/x/My_Repo.git", "-b", "dev"}, "",
			[]any{map[string]any{"type": "git", "url": "https://github.com/x/My_Repo.git",
				"name": "My-Repo", "branch": "dev", "token": nil, "dev": false}}},
		{"git url auto-detected omits branch/token", []string{"app", "create", "https://github.com/x/repo.git"}, "",
			[]any{map[string]any{"type": "git", "url": "https://github.com/x/repo.git", "name": "repo", "dev": false}}},
		{"scp-style git url", []string{"app", "create", "git@github.com:x/repo.git", "-D"}, "",
			[]any{map[string]any{"type": "git", "url": "git@github.com:x/repo.git", "name": "repo", "dev": true}}},
		{"dev app type", []string{"app", "create", "node", "-D"}, "",
			[]any{map[string]any{"type": "app", "app": "node", "dev": true}}},
		{"plain type string", []string{"app", "create", "node"}, "", []any{"node"}},
		{"interactive git url", []string{"app", "create", "git"}, "https://github.com/x/asked.git\n",
			[]any{map[string]any{"type": "git", "url": "https://github.com/x/asked.git",
				"name": "asked", "branch": nil, "token": nil, "dev": false}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, last := recordingServer(t)
			a, _, errOut := testApp(t, addr)
			a.in = strings.NewReader(tt.stdin)
			if code := a.run(tt.argv); code != 0 {
				t.Fatalf("exit = %d, stderr: %s", code, errOut)
			}
			if last.Action != "app.create" {
				t.Errorf("action = %q", last.Action)
			}
			if !reflect.DeepEqual(last.Data, tt.wantData) {
				t.Errorf("data = %#v\nwant %#v", last.Data, tt.wantData)
			}
		})
	}
}

func TestPrivilegedAbort(t *testing.T) {
	addr, last := recordingServer(t)
	a, out, _ := testApp(t, addr)
	a.in = strings.NewReader("no\n")
	if code := a.run([]string{"app", "privileged", "blog"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "Aborted.") {
		t.Errorf("output:\n%s", out)
	}
	if last.Action != "" {
		t.Errorf("server was called with %q after abort", last.Action)
	}
}

func TestDNSListDetailView(t *testing.T) {
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		conn.Write([]byte(`{"id":"d","result":true,"message":null,` +
			`"data":[{"type":"A","name":"@","value":"1.2.3.4"},{"type":"MX","name":"@","value":"mail.x.com"}]}`))
	})
	a, out, _ := testApp(t, addr)
	if code := a.run([]string{"dns", "list", "x.com"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	if strings.Count(got, "---") != 3 { // one per row + closing
		t.Errorf("want 3 '---' separators:\n%s", got)
	}
	if !strings.Contains(got, "type  : A") || !strings.Contains(got, "value : mail.x.com") {
		t.Errorf("detail lines missing:\n%s", got)
	}
	if strings.Contains(got, "TYPE") {
		t.Errorf("detail view must not print table headers:\n%s", got)
	}
}

func TestHelpOutput(t *testing.T) {
	addr, _ := recordingServer(t)
	a, out, _ := testApp(t, addr)
	if code := a.run([]string{"help"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	for _, want := range []string{"APP", "DOMAIN", "MAIL", "odac app create", "odac auth", "<key>",
		"Renew SSL certificate for a domain"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q:\n%s", want, got)
		}
	}
}

func TestHelpHidesAuthWhenAuthenticated(t *testing.T) {
	addr, _ := recordingServer(t)
	a, out, _ := testApp(t, addr)
	a.cfg.Set("hub", map[string]any{"token": "tok", "secret": "sec"})
	a.run([]string{"help"})
	if strings.Contains(out.String(), "odac auth") {
		t.Errorf("authenticated help must hide auth:\n%s", out)
	}
}

func TestGroupWithoutActionShowsHelp(t *testing.T) {
	addr, last := recordingServer(t)
	a, out, _ := testApp(t, addr)
	if code := a.run([]string{"app"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "odac app create") {
		t.Errorf("expected app help:\n%s", out)
	}
	if last.Action != "" {
		t.Errorf("server was called with %q", last.Action)
	}
}

func TestBootOnDemandWhenOffline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	a, out, _ := testApp(t, dead)
	booted := false
	a.boot = func() { booted = true }
	a.run([]string{"help"})
	if !booted {
		t.Error("run() must attempt boot when the server is unreachable")
	}
	_ = out
}

func TestStatusListsCommands(t *testing.T) {
	addr, _ := recordingServer(t)
	a, out, _ := testApp(t, addr)
	if code := a.run(nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "Commands:") || !strings.Contains(got, "odac update") {
		t.Errorf("status must list runnable commands:\n%s", got)
	}
	if strings.Contains(got, "odac app create") {
		t.Errorf("status command list must only contain top-level actions:\n%s", got)
	}
}

func TestDeriveAppName(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://github.com/x/repo.git", "repo"},
		{"https://github.com/x/My_App.git", "My-App"},
		{"git@github.com:x/thing", "thing"},
		{"https://example.com/a/b/c.d.git", "c-d"},
	}
	for _, tt := range tests {
		if got := deriveAppName(tt.url); got != tt.want {
			t.Errorf("deriveAppName(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestWatchdogCommandEnvOverride(t *testing.T) {
	t.Setenv("ODAC_WATCHDOG_BIN", "/opt/custom/watchdog")
	cmd := watchdogCommand()
	if cmd == nil || cmd.Path != "/opt/custom/watchdog" {
		t.Errorf("cmd = %v", cmd)
	}
}
