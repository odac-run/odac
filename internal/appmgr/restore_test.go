package appmgr

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"odac/internal/ports"
)

func TestHoldAppStopsAndGivesBack(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "db", "type": "container", "image": "postgres:16", "active": true}})
	fx.cfg.Mutate(func() { fx.m.apps = fx.m.loadAppsFromConfig() })
	w := &deployLog{}
	fx.m.SetDeployWatcher(w)
	h, err := fx.m.HoldApp("db")
	if err != nil || !h.WasActive() {
		t.Fatalf("hold: %v", err)
	}
	if _, err := fx.m.HoldApp("db"); err == nil {
		t.Fatal("an app was held twice")
	}
	if r := fx.m.Restart("db"); r.Status {
		t.Fatal("a held app restarted")
	}
	var active any
	fx.cfg.View(func() { active = fx.m.getLocked("db")["active"] })
	if active != false || !slices.Contains(fx.dock.stopped, "db") {
		t.Fatalf("held app: active %v, stopped %v", active, fx.dock.stopped)
	}
	// Started for a load, it stays inactive: the check pulse leaves it.
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	fx.cfg.View(func() { active = fx.m.getLocked("db")["active"] })
	if active != false || len(fx.dock.runCalls) != 1 {
		t.Fatalf("started: active %v, runs %d", active, len(fx.dock.runCalls))
	}
	if err := h.Release(true); err != nil {
		t.Fatal(err)
	}
	fx.cfg.View(func() { active = fx.m.getLocked("db")["active"] })
	if active != true || strings.Join(w.events, ",") != "start db,end db" {
		t.Fatalf("released: active %v, events %v", active, w.events)
	}
	h2, err := fx.m.HoldApp("db")
	if err != nil {
		t.Fatal(err)
	}
	h2.Abandon()
	fx.cfg.View(func() { active = fx.m.getLocked("db")["active"] })
	if active != false {
		t.Fatal("an abandoned hold left the app active")
	}
	if !fx.m.SetAppActive("db", true) || fx.m.SetAppActive("nope", true) {
		t.Fatal("SetAppActive")
	}
}

func TestCreateRestoredAppKeepsNothingShared(t *testing.T) {
	fx := newFixture(t, nil)
	apps := filepath.Join(fx.cfg.BaseDir(), "apps")
	conf := filepath.Join(apps, "web", "configs", "nginx.conf")
	os.MkdirAll(filepath.Dir(conf), 0o755)
	os.WriteFile(conf, []byte("server {}"), 0o644)
	def := map[string]any{
		"id": float64(7), "name": "web", "type": "container", "image": "nginx", "active": true, "status": "running",
		"api": true, "networkMode": "host", "devices": []any{map[string]any{"host": "/dev/ttyUSB0"}},
		"ports": []any{map[string]any{"container": float64(80), "host": float64(8080), "public": true}, map[string]any{"container": float64(9000), "host": "proxy"}},
		"volumes": []any{
			map[string]any{"host": "/srv/elsewhere/web/data", "container": "/data"},
			map[string]any{"host": "/mnt/media", "container": "/media"},
			map[string]any{"host": conf, "container": "/etc/nginx/nginx.conf"},
		},
		"env": map[string]any{
			"manual": map[string]any{"DB_HOST": "db", "URL": "postgres://u:p@db:5432/app", "PATH_TO": "/srv/db/x", "CACHE": "redis://cache"},
			"linked": []any{"db", "other"},
		},
		"template":     map[string]any{"group": "blog", "name": "blog", "role": "web"},
		imageChangeKey: map[string]any{"image": "nginx:2", "previous": "nginx", "hold": "odac-previous:app-7"},
	}
	r, err := fx.m.CreateRestoredApp(def, "web-restored", map[string]string{"web": "web-restored", "db": "db-restored"}, "blog-restored-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Dropped) != 4 {
		t.Fatalf("dropped %v", r.Dropped)
	}
	var app map[string]any
	fx.cfg.View(func() { app = copyMap(fx.m.getLocked("web-restored")) })
	if app == nil || app["active"] != false || app["api"] != nil || app["networkMode"] != nil || app["devices"] != nil || app["status"] != nil || app["id"] == float64(7) || app[imageChangeKey] != nil {
		t.Fatalf("record %v", app)
	}
	for _, p := range app["ports"].([]any) {
		if ports.IsPublished(p.(map[string]any)) {
			t.Fatalf("a published port stayed: %v", p)
		}
	}
	vols := app["volumes"].([]any)
	want := []string{filepath.Join(apps, "web-restored", "data"), filepath.Join(apps, "web-restored", "volumes", "media"), filepath.Join(apps, "web-restored", "configs", "nginx.conf")}
	for i, v := range vols {
		if got := v.(map[string]any)["host"]; got != want[i] {
			t.Errorf("volume %d: %v, want %s", i, got, want[i])
		}
	}
	if b, _ := os.ReadFile(want[2]); string(b) != "server {}" {
		t.Fatal("the config file was not copied")
	}
	env := app["env"].(map[string]any)
	manual := env["manual"].(map[string]any)
	if manual["DB_HOST"] != "db-restored" || manual["URL"] != "postgres://u:p@db-restored:5432/app" || manual["PATH_TO"] != "/srv/db/x" || manual["CACHE"] != "redis://cache" {
		t.Fatalf("env %v", manual)
	}
	if !slices.Equal(env["linked"].([]any), []any{"db-restored", "other"}) {
		t.Fatalf("linked %v", env["linked"])
	}
	if app["template"].(map[string]any)["group"] != "blog-restored-x" {
		t.Fatalf("template %v", app["template"])
	}
	if _, err := fx.m.CreateRestoredApp(def, "web-restored", nil, ""); err == nil {
		t.Fatal("a name was taken twice")
	}
	def["type"] = "git"
	if _, err := fx.m.CreateRestoredApp(def, "web-2", nil, ""); err == nil {
		t.Fatal("a git app was copied")
	}
	if d, ok := fx.m.AppDefinition("web-restored"); !ok || d["name"] != "web-restored" || d["status"] != nil {
		t.Fatalf("definition %v", d)
	}
	if fx.m.UniqueAppName("web-restored") != "web-restored-1" {
		t.Fatal("UniqueAppName")
	}
	if err := fx.m.DeleteRestoredApp("web-restored"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(apps, "web-restored")); !os.IsNotExist(err) {
		t.Fatal("the app directory stayed")
	}
}

func TestRenameHosts(t *testing.T) {
	r := map[string]string{"db": "db-2", "cache": "cache-2"}
	for in, want := range map[string]string{
		"db":                           "db-2",
		"db:5432":                      "db-2:5432",
		"postgres://u:p@db:5432/x":     "postgres://u:p@db-2:5432/x",
		"http://db/health":             "http://db-2/health",
		"redis://cache:6379,db:6379":   "redis://cache-2:6379,db-2:6379",
		"/var/db/data":                 "/var/db/data",
		"dbx":                          "dbx",
		"mydb:5432":                    "mydb:5432",
		"the db is down":               "the db-2 is down",
		"HOSTS=db;cache":               "HOSTS=db-2;cache-2",
		"postgres://u:p@db-old:5432/x": "postgres://u:p@db-old:5432/x",
	} {
		if got := RenameHosts(in, r); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// A switch's marker belongs to this server's switch in flight: a stored
// definition never carries it, and a hold refuses an app whose marker no
// operation owns, since its container may run an image the record does not
// name until Check's recovery decides.
func TestImageChangeMarkerStaysOutOfRestores(t *testing.T) {
	marker := map[string]any{"image": "postgres:17", "previous": "postgres:16", "previousId": "sha256:" + strings.Repeat("1", 64)}
	fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "db", "type": "container", "image": "postgres:16", "active": true, imageChangeKey: marker}})
	fx.cfg.Mutate(func() { fx.m.apps = fx.m.loadAppsFromConfig() })
	def, ok := fx.m.AppDefinition("db")
	if !ok || def[imageChangeKey] != nil || def["image"] != "postgres:16" {
		t.Fatalf("definition %v", def)
	}
	w := &deployLog{}
	fx.m.SetDeployWatcher(w)
	if _, err := fx.m.HoldApp("db"); !errors.Is(err, ErrImageChangePending) {
		t.Fatalf("hold of a marked app: %v", err)
	}
	if len(fx.dock.stopped) != 0 {
		t.Fatalf("a refused hold stopped the app: %v", fx.dock.stopped)
	}
	if !fx.m.tryLockProcessing(1) {
		t.Fatal("a refused hold kept the processing lock")
	}
	fx.m.unlockProcessing(1)
	var active any
	fx.cfg.View(func() { active = fx.m.getLocked("db")["active"] })
	if active != true {
		t.Fatal("a refused hold marked the app inactive")
	}
	fx.cfg.Mutate(func() { delete(fx.m.getLocked("db"), imageChangeKey) })
	h, err := fx.m.HoldApp("db")
	if err != nil {
		t.Fatalf("hold once recovered: %v", err)
	}
	h.Release(true)
}
