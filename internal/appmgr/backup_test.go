package appmgr

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestBackupApp(t *testing.T) {
	fx := newFixture(t, nil)
	apps := filepath.Join(fx.cfg.BaseDir(), "apps")
	fx.cfg.Mutate(func() {
		fx.cfg.Set("apps", []any{
			map[string]any{"id": float64(1), "name": "db", "image": "postgres:17", "backup": map[string]any{"engine": "postgres"},
				"volumes": []any{
					map[string]any{"host": filepath.Join(apps, "db", "data"), "container": "/var/lib/postgresql"},
					map[string]any{"host": filepath.Join(apps, "db", "configs", "pg.conf"), "container": "/etc/pg.conf"},
				}},
		})
		fx.m.apps = fx.m.loadAppsFromConfig()
	})
	a, ok := fx.m.BackupApp("db")
	if !ok || a.Container != "db" || a.Image != "postgres:17" || a.Spec == nil || a.Spec.Engine != "postgres" || len(a.Volumes) != 1 || a.Volumes[0].Container != "/var/lib/postgresql" {
		t.Fatalf("got %+v", a)
	}
	if _, ok := fx.m.BackupApp("nope"); ok {
		t.Fatal("an unknown app was found")
	}
}

type deployLog struct {
	mu     sync.Mutex
	events []string
}

func (d *deployLog) DeployStarted(app string) { d.add("start " + app) }
func (d *deployLog) DeployEnded(app string)   { d.add("end " + app) }
func (d *deployLog) add(s string) {
	d.mu.Lock()
	d.events = append(d.events, s)
	d.mu.Unlock()
}

func TestProcessingLockIsReportedAsADeploy(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "web"}})
	fx.cfg.Mutate(func() { fx.m.apps = fx.m.loadAppsFromConfig() })
	w := &deployLog{}
	fx.m.SetDeployWatcher(w)
	if !fx.m.tryLockProcessing(1) || fx.m.tryLockProcessing(1) {
		t.Fatal("processing lock")
	}
	fx.m.unlockProcessing(1)
	fx.m.unlockProcessing(1)
	if strings.Join(w.events, ",") != "start web,end web" {
		t.Fatalf("events %v", w.events)
	}
}
