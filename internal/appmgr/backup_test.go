package appmgr

import (
	"path/filepath"
	"reflect"
	"testing"

	"odac/internal/backup/settings"
)

func TestBackupApps(t *testing.T) {
	fx := newFixture(t, nil)
	apps := filepath.Join(fx.cfg.BaseDir(), "apps")
	fx.cfg.Mutate(func() {
		fx.cfg.Set("apps", []any{
			map[string]any{"id": float64(1), "name": "web", "env": map[string]any{"manual": map[string]any{}, "linked": []any{"db", "web"}},
				"volumes": []any{map[string]any{"host": filepath.Join(apps, "web", "configs", "etc", "nginx.conf"), "container": "/etc/nginx.conf"}}},
			map[string]any{"id": float64(2), "name": "db", "volumes": []any{map[string]any{"host": filepath.Join(apps, "db", "data"), "container": "/var/lib/postgresql"}}},
			map[string]any{"id": float64(3), "name": "kv", "backup": map[string]any{"engine": "redis"}},
			map[string]any{"id": float64(4), "name": "gone", "_deleted": true, "volumes": []any{map[string]any{"host": "/srv/x", "container": "/x"}}},
		})
		fx.m.apps = fx.m.loadAppsFromConfig()
	})
	got := fx.m.BackupApps()
	want := []settings.App{
		{Name: "web", Linked: []string{"db"}},
		{Name: "db", Stateful: true},
		{Name: "kv", Stateful: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
