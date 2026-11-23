package appmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/api"
	"odac/internal/docker"
	"odac/internal/rundir"
)

// templateWithVolumes is a one-member template payload whose member binds the
// given volumes, with an optional config file.
func templateWithVolumes(volumes []any, configs []any) map[string]any {
	member := map[string]any{
		"container": "made-by-app",
		"image":     "alpine:3",
		"volumes":   volumes,
	}
	if configs != nil {
		member["configs"] = configs
	}
	return map[string]any{"type": "template", "name": "stack", "apps": map[string]any{"main": member}}
}

func vol(host, container string) any {
	return map[string]any{"host": host, "container": container}
}

// An app holding app.create must not reach the host through the container it
// creates: every bind source stays a named volume in that container's own
// directory, separate from the others and off any symlink left on disk.
func TestAppCreatedContainerKeepsItsBindsInItsOwnDirectory(t *testing.T) {
	app := api.Caller{App: "panel"}
	refused := map[string][]any{
		"host root":          {vol("/", "/host")},
		"docker socket":      {vol("/var/run/docker.sock", "/var/run/docker.sock")},
		"climbs out":         {vol("../../..", "/host")},
		"climbs out deeper":  {vol("data/../../other-app", "/steal")},
		"empty source":       {vol("", "/x")},
		"nested":             {vol("data", "/data"), vol("data/x", "/x")},
		"app dir over child": {vol(".", "/all"), vol("data", "/data")},
	}
	for name, volumes := range refused {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, []any{})
			r := fx.m.Create(app, templateWithVolumes(volumes, nil))
			if r.Status {
				t.Fatalf("an app created a container binding %v: %s", volumes, r.Message)
			}
			if fx.findApp("made-by-app") != nil || fx.dock.runCallCount() != 0 {
				t.Fatal("a refused create still left an app or a container behind")
			}
		})
	}

	t.Run("volume holding the config directory", func(t *testing.T) {
		fx := newFixture(t, []any{})
		configs := []any{map[string]any{"path": "/etc/app.conf", "content": "x"}}
		if r := fx.m.Create(app, templateWithVolumes([]any{vol(".", "/all")}, configs)); r.Status {
			t.Fatal("a volume over the configs tree lets the container redirect a config mount")
		}
	})

	// app.delete with purge false keeps the directory, named after the app,
	// so an app could create one of that name and bind the old one's data.
	leftovers := map[string]func(t *testing.T, appDir string){
		"data kept by a delete": func(t *testing.T, appDir string) {
			if err := os.MkdirAll(filepath.Join(appDir, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(appDir, "data", "db.sqlite"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink inside": func(t *testing.T, appDir string) {
			if err := os.MkdirAll(filepath.Join(appDir, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/", filepath.Join(appDir, "data", "x")); err != nil {
				t.Fatal(err)
			}
		},
		"directory is a symlink": func(t *testing.T, appDir string) {
			if err := os.Symlink(t.TempDir(), appDir); err != nil {
				t.Fatal(err)
			}
		},
		"a file in its place": func(t *testing.T, appDir string) {
			if err := os.WriteFile(appDir, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, leave := range leftovers {
		t.Run("left behind: "+name, func(t *testing.T) {
			fx := newFixture(t, []any{})
			if err := os.MkdirAll(fx.m.appsPath(), 0o755); err != nil {
				t.Fatal(err)
			}
			leave(t, filepath.Join(fx.m.appsPath(), "made-by-app"))
			r := fx.m.Create(app, templateWithVolumes([]any{vol("data", "/data")}, nil))
			if msg, _ := r.Message.(string); r.Status || !strings.Contains(msg, "still on disk") {
				t.Fatalf("an app created over a directory left behind: %+v", r)
			}
			if fx.findApp("made-by-app") != nil || fx.dock.runCallCount() != 0 {
				t.Fatal("a refused create still left an app or a container behind")
			}
		})
	}

	t.Run("empty directory", func(t *testing.T) {
		fx := newFixture(t, []any{})
		if err := os.MkdirAll(filepath.Join(fx.m.appsPath(), "made-by-app"), 0o755); err != nil {
			t.Fatal(err)
		}
		if r := fx.m.Create(app, templateWithVolumes([]any{vol("data", "/data")}, nil)); !r.Status {
			t.Fatalf("an empty directory refused: %s", r.Message)
		}
		fx.waitIdle(t)
	})

	t.Run("operator over a directory left behind", func(t *testing.T) {
		fx := newFixture(t, []any{})
		leftovers["data kept by a delete"](t, filepath.Join(fx.m.appsPath(), "made-by-app"))
		if r := fx.m.Create(api.Operator, templateWithVolumes([]any{vol("data", "/data")}, nil)); !r.Status {
			t.Fatalf("the operator's create refused: %s", r.Message)
		}
		fx.waitIdle(t)
	})

	t.Run("separate named volumes", func(t *testing.T) {
		fx := newFixture(t, []any{})
		configs := []any{map[string]any{"path": "/etc/app.conf", "content": "x"}}
		volumes := []any{vol("data", "/data"), vol("cache/tmp", "/tmp/cache"), vol("data", "/also-data")}
		r := fx.m.Create(app, templateWithVolumes(volumes, configs))
		if !r.Status {
			t.Fatalf("named volumes refused: %s", r.Message)
		}
		fx.waitIdle(t)
		if fx.findApp("made-by-app") == nil {
			t.Fatal("app not created")
		}
	})
}

// A recipe from the catalog is checked too: an app could otherwise pick one
// that mounts the Docker socket and override its image.
func TestAppCannotCreateARecipeThatBindsTheHost(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "manager", "image": "manager:1",
		"volumes": []any{vol("/var/run/docker.sock", "/var/run/docker.sock")},
	})
	cfg := map[string]any{"type": "app", "app": "manager", "name": "mine", "image": "attacker:latest"}
	if r := fx.m.Create(api.Caller{App: "panel"}, cfg); r.Status {
		t.Fatal("an app created a recipe that binds the Docker socket")
	}
	if fx.findApp("mine") != nil {
		t.Fatal("refused recipe left an app record")
	}
}

// The operator, and the zero Caller only in the other direction, are where the
// line is drawn: the CLI and the Cloud bind host paths on purpose, a call site
// that forgot to fill the Caller gets nothing.
func TestOperatorKeepsHostBindsAndTheZeroCallerDoesNot(t *testing.T) {
	fx := newFixture(t, []any{})
	if r := fx.m.Create(api.Caller{}, templateWithVolumes([]any{vol("/srv/media", "/media")}, nil)); r.Status {
		t.Fatal("the zero Caller bound a host path")
	}
	r := fx.m.Create(api.Operator, templateWithVolumes([]any{vol("/srv/media", "/media")}, nil))
	if !r.Status {
		t.Fatalf("operator host bind refused: %s", r.Message)
	}
	fx.waitIdle(t)
	var got []docker.Mount
	for _, v := range fx.dock.runCallAt(0).options.Volumes {
		if v.Container != rundir.BackendMount {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0].Host != "/srv/media" {
		t.Fatalf("operator volumes = %v", got)
	}
}

// The create log line names what is being deployed and nothing it carries:
// a git token, env values (top level and per template member) and config
// file contents all stay out.
func TestCreateLogSummaryKeepsSecretsOut(t *testing.T) {
	cfg := map[string]any{
		"type": "template", "name": "stack", "url": "https://github.com/a/b.git", "token": "ghp_secret",
		"env": map[string]any{"web": map[string]any{"DB_PASSWORD": "hunter2"}},
		"apps": map[string]any{
			"web": map[string]any{"env": map[string]any{"API_SECRET": "s3"}, "configs": []any{map[string]any{"path": "/etc/x", "content": "pw=s4"}}},
			"db":  map[string]any{},
		},
	}
	raw, err := json.Marshal(createLogSummary(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ghp_secret", "hunter2", "s3", "s4"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("summary carries %q: %s", secret, raw)
		}
	}
	if want := `{"apps":["db","web"],"name":"stack","type":"template","url":"https://github.com/a/b.git"}`; string(raw) != want {
		t.Errorf("summary = %s, want %s", raw, want)
	}
}
