package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"odac/internal/api"
	"odac/internal/config"
	"odac/internal/hub"
)

// Every backup action is closed to apps, and the API and the Hub offer the
// same ones under the same names, except the staging of ODAC's own state,
// which is the local CLI's alone.
func TestBackupActionsMatchTheHubAndAreClosedToApps(t *testing.T) {
	src, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, m := range regexp.MustCompile(`reg\("([a-z.]+)"`).FindAllStringSubmatch(string(src), -1) {
		actions[m[1]] = true
	}
	if len(actions) < 20 {
		t.Fatalf("found %d backup actions; the pattern no longer matches", len(actions))
	}
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(cfg, "http://127.0.0.1:1", hub.Deps{})
	for action := range actions {
		if api.AppMayCall(action) {
			t.Errorf("%s is grantable to an app", action)
		}
		if !strings.HasPrefix(action, "backup.") {
			t.Errorf("%s is outside the backup namespace", action)
		}
		_, onHub := h.Triggers(action)
		if local := strings.HasPrefix(action, "backup.server."); local == onHub {
			t.Errorf("%s: on the hub %v, want %v", action, onHub, !local)
		}
	}
}
