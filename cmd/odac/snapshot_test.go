package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const snapshotPlan = `{"id":"","mode":"in-place","target":"snapshots","run":"s1","status":"planned","point":1700000000000,"lost":600000,"apps":[{"app":"db","as":"db","method":"dump","point":1700000000000}],"start":0}`

// A snapshot restore leaves the mode to the server, shows what it gives
// up, and then restores in exactly the mode it showed.
func TestSnapshotRestoreShowsWhatItGivesUpThenFollows(t *testing.T) {
	restorePollInterval = time.Millisecond
	started := strings.Replace(snapshotPlan, `"id":""`, `"id":"r1"`, 1)
	srv, addr := newBackupServer(t, map[string]string{
		"snapshot.restore.preview": reply(snapshotPlan),
		"snapshot.restore":         reply(started),
		"backup.restore.list":      reply(`[` + strings.Replace(started, `"status":"planned"`, `"status":"ok"`, 1) + `]`),
	})
	a, out, _ := testApp(t, addr)
	a.in = strings.NewReader("yes\n")
	if code := a.dispatch([]string{"snapshot", "restore", "--run", "s1"}); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	for _, want := range []string{"in place", "is lost (10 min)", "Restore r1 finished"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if got := srv.payload(t, "snapshot.restore.preview"); !reflect.DeepEqual(got, map[string]any{"run": "s1"}) {
		t.Errorf("preview %v", got)
	}
	if got := srv.payload(t, "snapshot.restore"); !reflect.DeepEqual(got, map[string]any{"run": "s1", "mode": "in-place"}) {
		t.Errorf("restore %v", got)
	}
}

func TestSnapshotDispatch(t *testing.T) {
	tests := []struct {
		argv   []string
		stdin  string
		action string
		want   map[string]any
	}{
		{[]string{"snapshot", "take", "--app", "web"}, "", "snapshot.take", map[string]any{"app": "web"}},
		{[]string{"snapshot", "delete", "--run", "s1"}, "yes\n", "snapshot.delete", map[string]any{"run": "s1"}},
		{[]string{"snapshot", "list", "--app", "db"}, "", "snapshot.list", map[string]any{"app": "db"}},
		{[]string{"snapshot", "history", "--app", "db", "--limit", "5"}, "", "snapshot.history", map[string]any{"app": "db", "limit": float64(5)}},
		{[]string{"snapshot", "settings", "--off"}, "", "snapshot.settings.update", map[string]any{"enabled": false}},
		{[]string{"snapshot", "settings", "--on", "--max-disk", "30%"}, "", "snapshot.settings.update", map[string]any{"enabled": true, "maxDiskPct": float64(30)}},
	}
	for _, tc := range tests {
		srv, addr := newBackupServer(t, map[string]string{"snapshot.list": reply(`[]`), "snapshot.history": reply(`[]`)})
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

func TestSnapshotListsRender(t *testing.T) {
	_, addr := newBackupServer(t, map[string]string{
		"snapshot.list":     reply(`[{"run":"s1","app":"blog","event":"delete","time":1700000000000,"apps":[{"app":"blog","consistency":"consistent"}],"gone":1700000000000,"expires":1700604800000}]`),
		"snapshot.status":   reply(`{"enabled":true,"maxDiskPct":25,"available":true,"used":1048576,"limit":2097152,"free":10,"total":20,"floor":5,"periodic":[{"app":"db","next":1700000000000,"every":60}],"deploys":[{"app":"web","at":1700000000000,"status":"skipped","reason":"timeout"}],"warnings":[{"code":"no_space","message":"No room.","at":1}]}`),
		"snapshot.settings": reply(`{"enabled":false,"maxDiskPct":25}`),
	})
	a, out, _ := testApp(t, addr)
	for _, argv := range [][]string{{"snapshot", "list"}, {"snapshot", "status"}, {"snapshot", "settings"}} {
		if code := a.dispatch(argv); code != 0 {
			t.Fatalf("%v: exit %d\n%s", argv, code, out.String())
		}
	}
	for _, want := range []string{"deleted app, kept until", "No room.", "went on without a snapshot: timeout", "Snapshots: off"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
