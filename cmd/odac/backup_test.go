package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/apiproto"
)

// backupServer answers each action with canned data and records every
// request in order.
type backupServer struct {
	mu      sync.Mutex
	replies map[string]string
	got     []apiproto.Request
}

func newBackupServer(t *testing.T, replies map[string]string) (*backupServer, string) {
	t.Helper()
	s := &backupServer{replies: replies}
	addr := fakeServer(t, func(req apiproto.Request, conn net.Conn) {
		s.mu.Lock()
		s.got = append(s.got, req)
		reply, ok := s.replies[req.Action]
		s.mu.Unlock()
		if !ok {
			reply = `{"id":"r","result":true,"message":"ok"}`
		}
		conn.Write([]byte(reply))
	})
	return s, addr
}

func (s *backupServer) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.got {
		out = append(out, r.Action)
	}
	return out
}

// payload is the object the last request of an action carried.
func (s *backupServer) payload(t *testing.T, action string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.got) - 1; i >= 0; i-- {
		if s.got[i].Action == action {
			if len(s.got[i].Data) == 0 {
				return nil
			}
			raw, _ := json.Marshal(s.got[i].Data[0])
			var m map[string]any
			json.Unmarshal(raw, &m)
			return m
		}
	}
	t.Fatalf("%s was not called; calls: %v", action, s.got)
	return nil
}

func reply(data string) string {
	return `{"id":"r","result":true,"message":"done","data":` + data + `}`
}

const targetList = `[{"id":"aaaaaaaaaaaaaaaa","name":"Offsite","kind":"s3","s3":{"endpoint":"s3.example.com","bucket":"b","accessKeyId":"AK"},"keySource":"generated","location":"remote","created":1}]`

func TestTargetAddShowsTheKeyAndSavesOnlyOnceConfirmed(t *testing.T) {
	recovery := `{"format":1,"serverId":"s1","target":{"id":"aaaaaaaaaaaaaaaa","name":"Disk","kind":"disk","disk":{"path":"/mnt/b"}},"directory":"s1/repository","key":"the-key"}`
	srv, addr := newBackupServer(t, map[string]string{
		"backup.target.prepare": reply(`{"token":"tok","expires":1,"recovery":` + recovery + `}`),
	})
	a, out, _ := testApp(t, addr)
	file := filepath.Join(t.TempDir(), "recovery.json")
	a.in = strings.NewReader("yes\n")
	if code := a.dispatch([]string{"backup", "target", "add", "--name", "Disk", "--disk", "/mnt/b", "--recovery-file", file}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "the-key") {
		t.Fatalf("the key was not shown:\n%s", out.String())
	}
	if got := srv.payload(t, "backup.target.prepare"); !reflect.DeepEqual(got, map[string]any{"name": "Disk", "disk": map[string]any{"path": "/mnt/b"}}) {
		t.Fatalf("spec %v", got)
	}
	if got := srv.payload(t, "backup.target.confirm"); got["token"] != "tok" {
		t.Fatalf("confirm %v", got)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery file %v %v", info, err)
	}
	if raw, _ := os.ReadFile(file); !strings.Contains(string(raw), "the-key") {
		t.Fatalf("recovery file holds no key: %s", raw)
	}

	// Not confirming drops the prepared target.
	srv2, addr2 := newBackupServer(t, map[string]string{"backup.target.prepare": reply(`{"token":"tok2","recovery":` + recovery + `}`)})
	a2, _, _ := testApp(t, addr2)
	a2.in = strings.NewReader("no\n")
	if code := a2.dispatch([]string{"backup", "target", "add", "--name", "Disk", "--disk", "/mnt/b"}); code == 0 {
		t.Fatal("an unconfirmed target exited 0")
	}
	if got := srv2.actions(); !reflect.DeepEqual(got, []string{"backup.target.prepare", "backup.target.cancel"}) {
		t.Fatalf("calls %v", got)
	}
}

func TestTargetAddAsksForTheS3Secret(t *testing.T) {
	srv, addr := newBackupServer(t, map[string]string{"backup.target.prepare": reply(`{"token":"t","recovery":{"key":"k","target":{}}}`)})
	a, _, _ := testApp(t, addr)
	a.in = strings.NewReader("s3cr3t\nyes\n")
	a.dispatch([]string{"backup", "target", "add", "--name", "Offsite", "--s3", "bucket", "--endpoint", "s3.example.com", "--access-key", "AK"})
	s3, _ := srv.payload(t, "backup.target.prepare")["s3"].(map[string]any)
	if s3["secretAccessKey"] != "s3cr3t" || s3["bucket"] != "bucket" {
		t.Fatalf("s3 %v", s3)
	}
}

func TestTargetUpdateSendsOnlyWhatChanged(t *testing.T) {
	srv, addr := newBackupServer(t, map[string]string{"backup.target.list": reply(targetList)})
	a, _, _ := testApp(t, addr)
	if code := a.dispatch([]string{"backup", "target", "update", "-t", "offsite", "--secret-key", "NEW"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := map[string]any{"id": "aaaaaaaaaaaaaaaa", "s3": map[string]any{"secretAccessKey": "NEW"}}
	if got := srv.payload(t, "backup.target.update"); !reflect.DeepEqual(got, want) {
		t.Fatalf("patch %v", got)
	}
}

func TestJobCreateBuildsTheSpec(t *testing.T) {
	srv, addr := newBackupServer(t, map[string]string{"backup.target.list": reply(targetList)})
	a, _, _ := testApp(t, addr)
	if code := a.dispatch([]string{"backup", "job", "create", "--name", "nightly", "--target", "Offsite", "--apps", "web, db", "--server", "--every", "6h", "--density", "high", "--max-age", "90", "--budget", "50g"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := map[string]any{
		"name": "nightly", "target": "aaaaaaaaaaaaaaaa", "apps": []any{"web", "db"}, "server": true,
		"every": float64(360), "density": "high", "maxAgeDays": float64(90), "budget": float64(50 << 30),
	}
	if got := srv.payload(t, "backup.job.create"); !reflect.DeepEqual(got, want) {
		t.Fatalf("spec %v\nwant %v", got, want)
	}
}

func TestParseEvery(t *testing.T) {
	for text, want := range map[string]int{"60": 60, "30m": 30, "6h": 360, "1d": 1440, "1h30m": 90} {
		if got, err := parseEvery(text); err != nil || got != want {
			t.Errorf("parseEvery(%q) = %d, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "soon", "90s"} {
		if _, err := parseEvery(text); err == nil {
			t.Errorf("parseEvery(%q) accepted", text)
		}
	}
}

const plan = `{"id":"","mode":"in-place","target":"aaaaaaaaaaaaaaaa","run":"run1","status":"planned","point":1700000000000,"lost":7200000,"apps":[{"app":"web","as":"web","method":"files","point":1700000000000}],"restarted":["front"],"start":0}`

func TestRestoreShowsWhatItGivesUpThenFollows(t *testing.T) {
	restorePollInterval = time.Millisecond
	started := strings.Replace(plan, `"id":""`, `"id":"r1"`, 1)
	srv, addr := newBackupServer(t, map[string]string{
		"backup.target.list":     reply(targetList),
		"backup.restore.preview": reply(plan),
		"backup.restore":         reply(started),
		"backup.restore.list":    reply(`[` + strings.Replace(started, `"status":"planned"`, `"status":"ok"`, 1) + `]`),
	})
	a, out, _ := testApp(t, addr)
	a.in = strings.NewReader("yes\n")
	if code := a.dispatch([]string{"backup", "restore", "apps", "--run", "run1", "--in-place", "--apps", "web"}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	for _, want := range []string{"is lost (2 h 0 min)", "front", "backed up first", "Restore r1 finished"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	want := map[string]any{"target": "aaaaaaaaaaaaaaaa", "run": "run1", "apps": []any{"web"}, "mode": "in-place"}
	for _, action := range []string{"backup.restore.preview", "backup.restore"} {
		if got := srv.payload(t, action); !reflect.DeepEqual(got, want) {
			t.Errorf("%s %v", action, got)
		}
	}
}

func TestRestoreAbortsWithoutYes(t *testing.T) {
	srv, addr := newBackupServer(t, map[string]string{"backup.target.list": reply(targetList), "backup.restore.preview": reply(plan)})
	a, _, _ := testApp(t, addr)
	a.in = strings.NewReader("no\n")
	if code := a.dispatch([]string{"backup", "restore", "apps", "--run", "run1"}); code == 0 {
		t.Fatal("exit 0 without yes")
	}
	for _, action := range srv.actions() {
		if action == "backup.restore" {
			t.Fatal("restored without yes")
		}
	}
	// Without --in-place the server's default (a new app) applies.
	if got := srv.payload(t, "backup.restore.preview"); got["mode"] != nil {
		t.Fatalf("mode sent: %v", got)
	}
}

func TestRestoreStatePicksTheOtherServerAndItsNewestState(t *testing.T) {
	other := strings.Repeat("b", 32)
	srv, addr := newBackupServer(t, map[string]string{
		"backup.target.list":    reply(targetList),
		"backup.target.servers": reply(`[{"id":"` + strings.Repeat("a", 32) + `","self":true,"repository":true},{"id":"` + other + `","name":"old","repository":true}]`),
		"backup.point.list":     reply(`[{"run":"r3","job":"j","time":3,"apps":[]},{"run":"r2","job":"j","time":2,"apps":[],"server":true}]`),
		"backup.server.restore": reply(`{"id":"s","run":"r2","dirs":["config"]}`),
	})
	booted := false
	a, out, _ := testApp(t, addr)
	a.boot = func() { booted = true }
	a.in = strings.NewReader("yes\nyes\n")
	if code := a.dispatch([]string{"backup", "restore", "state", "--key", "old-key"}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	want := map[string]any{"target": "aaaaaaaaaaaaaaaa", "server": other, "key": "old-key", "run": "r2"}
	if got := srv.payload(t, "backup.server.restore"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stage %v", got)
	}
	if !booted {
		t.Fatal("ODAC was not restarted")
	}
	if !strings.Contains(out.String(), "--server "+other+" --run r2 --in-place --key <key>") || strings.Contains(out.String(), "old-key") {
		t.Fatalf("next step:\n%s", out.String())
	}
}

func TestBackupKeyShowsTheKey(t *testing.T) {
	_, addr := newBackupServer(t, map[string]string{
		"backup.target.list": reply(targetList),
		"backup.key":         reply(`{"format":1,"serverId":"s1","target":{"name":"Offsite","s3":{"bucket":"b"}},"directory":"s1/repository","key":"kkk"}`),
	})
	a, out, _ := testApp(t, addr)
	if code := a.dispatch([]string{"backup", "key"}); code != 0 || !strings.Contains(out.String(), "kkk") {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
}

func TestBackupDispatch(t *testing.T) {
	jobList := reply(`[{"id":"j1","name":"nightly","target":"aaaaaaaaaaaaaaaa","allApps":true,"every":1440,"density":3.5,"created":1}]`)
	tests := []struct {
		argv   []string
		stdin  string
		action string
		want   map[string]any
	}{
		{[]string{"backup", "target", "delete", "Offsite"}, "", "backup.target.delete", map[string]any{"id": "aaaaaaaaaaaaaaaa"}},
		{[]string{"backup", "job", "run", "nightly"}, "", "backup.job.run", map[string]any{"id": "j1"}},
		{[]string{"backup", "job", "update", "-j", "j1", "--every", "30"}, "", "backup.job.update", map[string]any{"id": "j1", "every": float64(30)}},
		{[]string{"backup", "job", "delete", "-j", "nightly", "--purge"}, "yes\n", "backup.job.delete", map[string]any{"id": "j1", "purge": true}},
		{[]string{"backup", "points", "-t", "Offsite", "--app", "web"}, "", "backup.point.list", map[string]any{"target": "aaaaaaaaaaaaaaaa", "app": "web"}},
		{[]string{"backup", "restore", "cancel", "--id", "r1"}, "", "backup.restore.cancel", map[string]any{"id": "r1"}},
		{[]string{"backup", "history", "-j", "nightly"}, "", "backup.history", map[string]any{"job": "j1", "limit": float64(20)}},
	}
	for _, tc := range tests {
		srv, addr := newBackupServer(t, map[string]string{"backup.target.list": reply(targetList), "backup.job.list": jobList, "backup.point.list": reply(`[]`), "backup.history": reply(`[]`)})
		a, out, errOut := testApp(t, addr)
		a.in = strings.NewReader(tc.stdin)
		if code := a.dispatch(tc.argv); code != 0 {
			t.Errorf("%v: exit %d\n%s%s", tc.argv, code, out.String(), errOut.String())
			continue
		}
		if got := srv.payload(t, tc.action); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: payload %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestBackupListsRender(t *testing.T) {
	_, addr := newBackupServer(t, map[string]string{
		"backup.target.list": reply(targetList),
		"backup.job.list":    reply(`[]`),
		"backup.coverage":    reply(`{"unprotected":["db"],"noJobs":true}`),
		"backup.status":      reply(`{"jobs":[],"targets":[]}`),
	})
	a, out, _ := testApp(t, addr)
	for _, argv := range [][]string{{"backup", "target", "list"}, {"backup", "job", "list"}, {"backup", "coverage"}, {"backup", "status"}} {
		if code := a.dispatch(argv); code != 0 {
			t.Fatalf("%v: exit %d", argv, code)
		}
	}
	for _, want := range []string{"s3://b/ (s3.example.com)", "No backup jobs.", "no job backs up: db"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
