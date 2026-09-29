package appmgr

// Ports of App.test.js "git configuration" + "template deployment".

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"odac/internal/docker"
	"odac/internal/gpu"
	"odac/internal/kernel"
)

func (fx *fixture) setRecipe(recipe map[string]any) {
	fx.hub.mu.Lock()
	defer fx.hub.mu.Unlock()
	fx.hub.recipe = recipe
}

// findApp returns the persisted app with the given name.
func (fx *fixture) findApp(name string) map[string]any {
	var out map[string]any
	fx.cfg.View(func() {
		apps, _ := fx.cfg.Get("apps").([]any)
		for _, a := range apps {
			if app, _ := a.(map[string]any); app != nil && app["name"] == name {
				out = app
				return
			}
		}
	})
	return out
}

// ---- git configuration ----

func TestCreateFromGitBuildsGitObject(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "git", "url": "https://github.com/user/my-repo.git",
		"name": "my-git-app", "branch": "develop",
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("my-git-app")
	if app == nil {
		t.Fatal("app not persisted")
	}
	git, _ := app["git"].(map[string]any)
	if git["repo"] != "user/my-repo" || git["branch"] != "develop" || git["provider"] != "github" {
		t.Fatalf("git = %v", git)
	}
	if app["image"] != "odac-app-my-git-app" || app["branch"] != "develop" {
		t.Fatalf("app = %v", app)
	}
	p, _ := app["ports"].([]any)
	portEq(t, p[0], map[string]any{"host": "proxy", "container": float64(3000)})
}

func TestCreateFromGitValidation(t *testing.T) {
	check := func(t *testing.T, cfg map[string]any, wantMsg string) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(cfg)
		if r.Status {
			t.Fatal("expected failure")
		}
		if !strings.Contains(jsString(r.Message), wantMsg) {
			t.Fatalf("message %q does not contain %q", jsString(r.Message), wantMsg)
		}
	}

	t.Run("missing url", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "name": "x"}, "Missing git URL")
	})
	t.Run("illegal characters", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "https://x.com/a;rm -rf /", "name": "x"}, "illegal characters")
	})
	t.Run("unsupported protocol", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "file:///etc/passwd", "name": "x"}, "Unsupported protocol")
	})
	t.Run("branch injection", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "https://github.com/a/b.git", "name": "x", "branch": "--upload-pack=/bin/sh"}, "Invalid branch name")
	})
	t.Run("missing name", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "https://github.com/a/b.git"}, "Missing app name")
	})
	t.Run("path traversal name", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "https://github.com/a/b.git", "name": "../evil"}, "Invalid app name")
	})
	t.Run("command injection name", func(t *testing.T) {
		check(t, map[string]any{"type": "git", "url": "https://github.com/a/b.git", "name": "x;touch /pwned"}, "Invalid app name")
	})
	t.Run("duplicate name", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "taken"}})
		r := fx.m.Create(map[string]any{"type": "git", "url": "https://github.com/a/b.git", "name": "taken"})
		if r.Status || !strings.Contains(jsString(r.Message), "already exists") {
			t.Fatalf("r = %+v", r)
		}
	})
}

func TestCreateFromGitCleansUpOnFailure(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.dock.mu.Lock()
	fx.dock.buildErr = errTest
	fx.dock.mu.Unlock()

	r := fx.m.Create(map[string]any{"type": "git", "url": "https://github.com/a/b.git", "name": "failing"})
	if r.Status {
		t.Fatal("expected failure")
	}
	if fx.appCount() != 0 {
		t.Fatalf("app leaked into config")
	}
}

func TestCreateStringDispatch(t *testing.T) {
	t.Run("git url string", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create("https://github.com/user/some-repo.git")
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)
		if fx.findApp("some-repo") == nil {
			t.Fatalf("expected app named some-repo")
		}
	})

	t.Run("recipe name string", func(t *testing.T) {
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine"})
		r := fx.m.Create("redis")
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)
		if fx.findApp("redis") == nil {
			t.Fatal("recipe app missing")
		}
	})
}

// ---- redeploy ----

func TestRedeployUpdatesGitMetadata(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "redeploy-app", "type": "git",
		"url": "https://gitlab.com/user/repo.git", "branch": "main",
		"git": map[string]any{"repo": "user/repo", "branch": "main", "provider": "gitlab"},
	}})

	r := fx.m.Redeploy(RedeployPayload{Container: "redeploy-app", Branch: "feature-abc"})
	if !r.Status {
		t.Fatalf("redeploy failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("redeploy-app")
	if app["branch"] != "feature-abc" {
		t.Fatalf("branch = %v", app["branch"])
	}
	git, _ := app["git"].(map[string]any)
	if git["branch"] != "feature-abc" || git["repo"] != "user/repo" || git["provider"] != "gitlab" {
		t.Fatalf("git = %v", git)
	}
}

func TestRedeployValidation(t *testing.T) {
	newGit := func(t *testing.T) *fixture {
		return newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "app", "type": "git", "url": "https://github.com/a/b.git",
		}})
	}

	t.Run("missing container name", func(t *testing.T) {
		fx := newGit(t)
		if r := fx.m.Redeploy(RedeployPayload{}); r.Status {
			t.Fatal("expected failure")
		}
	})
	t.Run("unknown app", func(t *testing.T) {
		fx := newGit(t)
		if r := fx.m.Redeploy(RedeployPayload{Container: "ghost"}); r.Status {
			t.Fatal("expected failure")
		}
	})
	t.Run("non-git app", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "cont", "type": "container"}})
		r := fx.m.Redeploy(RedeployPayload{Container: "cont"})
		if r.Status || !strings.Contains(jsString(r.Message), "only supported for git apps") {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("bad url override", func(t *testing.T) {
		fx := newGit(t)
		if r := fx.m.Redeploy(RedeployPayload{Container: "app", URL: "notaurl"}); r.Status {
			t.Fatal("expected failure")
		}
	})
	t.Run("bad commit sha", func(t *testing.T) {
		fx := newGit(t)
		r := fx.m.Redeploy(RedeployPayload{Container: "app", CommitSha: "xyz!"})
		if r.Status || !strings.Contains(jsString(r.Message), "Invalid commit SHA") {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("bad branch", func(t *testing.T) {
		fx := newGit(t)
		if r := fx.m.Redeploy(RedeployPayload{Container: "app", Branch: "-x"}); r.Status {
			t.Fatal("expected failure")
		}
	})
}

func TestRedeployBuildFailureSetsErrored(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "app", "type": "git", "url": "https://github.com/a/b.git",
	}})
	fx.dock.mu.Lock()
	fx.dock.buildErr = errTest
	fx.dock.mu.Unlock()

	r := fx.m.Redeploy(RedeployPayload{Container: "app"})
	if r.Status || !strings.Contains(jsString(r.Message), "Redeploy failed") {
		t.Fatalf("r = %+v", r)
	}
}

// ---- template deployment ----

func TestTemplateDeploysMultiAppStack(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "wordpress",
		"apps": map[string]any{
			"db": map[string]any{
				"image":   "mariadb:10.6",
				"volumes": []any{map[string]any{"host": "data", "container": "/var/lib/mysql"}},
				"env": map[string]any{
					"MYSQL_DATABASE":      "wordpress",
					"MYSQL_PASSWORD":      map[string]any{"generate": true, "length": float64(16)},
					"MYSQL_ROOT_PASSWORD": map[string]any{"generate": true, "length": float64(24)},
					"MYSQL_USER":          "wordpress",
				},
			},
			"web": map[string]any{
				"image":  "wordpress:latest",
				"ports":  []any{map[string]any{"container": float64(80), "host": "auto"}},
				"linked": []any{"db"},
				"env": map[string]any{
					"WORDPRESS_DB_HOST":     "${db.name}",
					"WORDPRESS_DB_NAME":     "${db.env.MYSQL_DATABASE}",
					"WORDPRESS_DB_PASSWORD": "${db.env.MYSQL_PASSWORD}",
					"WORDPRESS_DB_USER":     "${db.env.MYSQL_USER}",
				},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "wordpress", "name": "myblog"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if fx.appCount() != 2 {
		t.Fatalf("appCount = %d", fx.appCount())
	}

	dbApp := fx.findApp("myblog-db")
	if dbApp == nil || dbApp["image"] != "mariadb:10.6" {
		t.Fatalf("db app = %v", dbApp)
	}
	tpl, _ := dbApp["template"].(map[string]any)
	if tpl["group"] != "myblog" || tpl["name"] != "wordpress" || tpl["role"] != "db" {
		t.Fatalf("db template = %v", tpl)
	}

	webApp := fx.findApp("myblog-web")
	if webApp == nil || webApp["image"] != "wordpress:latest" {
		t.Fatalf("web app = %v", webApp)
	}
	env, _ := webApp["env"].(map[string]any)
	linked, _ := env["linked"].([]any)
	if len(linked) != 1 || linked[0] != "myblog-db" {
		t.Fatalf("web linked = %v", linked)
	}

	if fx.dock.runCallCount() != 2 {
		t.Fatalf("runApp calls = %d", fx.dock.runCallCount())
	}
	// Interpolation reached the actual container env.
	webEnv := fx.dock.runCallAt(1).options.Env
	if webEnv["WORDPRESS_DB_HOST"] != "myblog-db" || webEnv["WORDPRESS_DB_NAME"] != "wordpress" {
		t.Fatalf("web env = %v", webEnv)
	}
	if strings.Contains(webEnv["WORDPRESS_DB_PASSWORD"], "${") || len(webEnv["WORDPRESS_DB_PASSWORD"]) != 16 {
		t.Fatalf("password not interpolated: %q", webEnv["WORDPRESS_DB_PASSWORD"])
	}
}

func TestTemplateContainerDirective(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "container-ref",
		"apps": map[string]any{
			"db": map[string]any{
				"image": "postgres:alpine",
				"env": map[string]any{
					"POSTGRES_PASSWORD": map[string]any{"generate": true},
					"SERVICE_NAME":      map[string]any{"type": "container"},
				},
			},
			"web": map[string]any{
				"image":  "node:lts",
				"linked": []any{"db"},
				"env": map[string]any{
					"APP_NAME": map[string]any{"type": "container"},
					"DB_HOST":  "${db.name}",
				},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "container-ref", "name": "mystack"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if env := fx.dock.runCallAt(0).options.Env; env["SERVICE_NAME"] != "mystack-db" {
		t.Fatalf("db SERVICE_NAME = %q", env["SERVICE_NAME"])
	}
	webEnv := fx.dock.runCallAt(1).options.Env
	if webEnv["APP_NAME"] != "mystack-web" || webEnv["DB_HOST"] != "mystack-db" {
		t.Fatalf("web env = %v", webEnv)
	}
}

func TestRecipeContainerDirectiveSingleApp(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "redis", "image": "redis:alpine",
		"env": map[string]any{
			"HOSTNAME": map[string]any{"type": "container"},
			"MODE":     "standalone",
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "myredis"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	env := fx.dock.runCallAt(0).options.Env
	if env["HOSTNAME"] != "myredis" || env["MODE"] != "standalone" {
		t.Fatalf("env = %v", env)
	}
}

func TestCreateRejectsUnsafeNames(t *testing.T) {
	t.Run("recipe malicious name", func(t *testing.T) {
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine", "env": map[string]any{}})
		r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "x;touch /pwned"})
		if r.Status || !strings.Contains(jsString(r.Message), "Invalid app name") {
			t.Fatalf("expected rejection, got %+v", r)
		}
	})
	t.Run("template malicious container name", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(map[string]any{
			"type": "template",
			"name": "stack",
			"apps": map[string]any{
				"db": map[string]any{"image": "postgres:alpine", "container": "evil$(id)"},
			},
		})
		if r.Status || !strings.Contains(jsString(r.Message), "Invalid app name") {
			t.Fatalf("expected rejection, got %+v", r)
		}
	})
}

func TestValidAppName(t *testing.T) {
	valid := []string{"myapp", "my-app", "App123", "a", strings.Repeat("a", 63)}
	for _, n := range valid {
		if !validAppName(n) {
			t.Errorf("validAppName(%q) = false, want true", n)
		}
	}
	invalid := []string{
		"", "../evil", "x;touch", "a b", "evil$(id)", "back`tick`",
		"a/b", "-lead", ".", "..", "under_score", strings.Repeat("a", 64),
	}
	for _, n := range invalid {
		if validAppName(n) {
			t.Errorf("validAppName(%q) = true, want false", n)
		}
	}
}

func TestRecipeImageOverride(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "redis", "image": "redis:alpine",
		"env": map[string]any{},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "myredis", "image": "redis:7-bookworm"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if img := fx.dock.runCallAt(0).options.Image; img != "redis:7-bookworm" {
		t.Fatalf("image = %q, want the override", img)
	}
}

func TestTemplateTypedDirectives(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "typed-stack",
		"apps": map[string]any{
			"db": map[string]any{
				"image": "postgres:alpine",
				"env": map[string]any{
					"CONTAINER_NAME": map[string]any{"type": "container"},
					"DB_PASS":        map[string]any{"type": "generate", "length": float64(24)},
					"LEGACY_PASS":    map[string]any{"generate": true, "length": float64(12)},
					"STATIC_VAR":     "hello",
				},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "typed-stack", "name": "mytyped"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	env := fx.dock.runCallAt(0).options.Env
	if env["CONTAINER_NAME"] != "mytyped-db" || len(env["DB_PASS"]) != 24 || len(env["LEGACY_PASS"]) != 12 || env["STATIC_VAR"] != "hello" {
		t.Fatalf("env = %v", env)
	}
}

func TestTemplateDependencyOrder(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "three-tier",
		"apps": map[string]any{
			"cache": map[string]any{"image": "redis:alpine", "env": map[string]any{}},
			"db": map[string]any{
				"image": "postgres:alpine",
				"env":   map[string]any{"POSTGRES_PASSWORD": map[string]any{"generate": true}},
			},
			"web": map[string]any{
				"image":  "node:lts",
				"linked": []any{"db", "cache"},
				"env":    map[string]any{"CACHE_HOST": "${cache.name}", "DB_HOST": "${db.name}"},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "three-tier", "name": "myapp"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if fx.dock.runCallCount() != 3 {
		t.Fatalf("runApp calls = %d", fx.dock.runCallCount())
	}
	if last := fx.dock.runCallAt(2).name; last != "myapp-web" {
		t.Fatalf("last started = %q", last)
	}
}

func TestTemplateRollbackOnPartialFailure(t *testing.T) {
	fx := newFixture(t, []any{})
	calls := 0
	fx.dock.mu.Lock()
	fx.dock.runErr = func(string) error {
		calls++
		if calls >= 2 {
			return errTest
		}
		return nil
	}
	fx.dock.mu.Unlock()

	fx.setRecipe(map[string]any{
		"name": "fail-stack",
		"apps": map[string]any{
			"db": map[string]any{"image": "db:latest", "env": map[string]any{"DB_PASS": map[string]any{"generate": true}}},
			"web": map[string]any{
				"image": "web:latest", "linked": []any{"db"},
				"env": map[string]any{"DB_HOST": "${db.name}"},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "fail-stack", "name": "mybad"})
	if r.Status {
		t.Fatal("expected failure")
	}
	fx.waitIdle(t)

	if fx.appCount() != 0 {
		t.Fatalf("rollback incomplete: %d apps left", fx.appCount())
	}
}

func TestTemplateCircularDependency(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "circular",
		"apps": map[string]any{
			"a": map[string]any{"image": "a:latest", "linked": []any{"b"}, "env": map[string]any{}},
			"b": map[string]any{"image": "b:latest", "linked": []any{"a"}, "env": map[string]any{}},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "circular", "name": "loop"})
	if r.Status || !strings.Contains(jsString(r.Message), "Circular dependency") {
		t.Fatalf("r = %+v", r)
	}
}

func TestTemplateUndefinedDependency(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "broken",
		"apps": map[string]any{
			"web": map[string]any{"image": "web:latest", "linked": []any{"nonexistent"}, "env": map[string]any{}},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "broken", "name": "nope"})
	if r.Status || !strings.Contains(jsString(r.Message), "not defined") {
		t.Fatalf("r = %+v", r)
	}
}

func TestSingleAppRecipeIsNotATemplate(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "redis", "image": "redis:alpine",
		"ports": []any{map[string]any{"container": float64(6379), "host": "auto"}},
		"env":   map[string]any{},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "myredis"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if fx.appCount() != 1 {
		t.Fatalf("appCount = %d", fx.appCount())
	}
	app := fx.findApp("myredis")
	if _, present := app["template"]; present {
		t.Fatalf("template metadata leaked: %v", app)
	}
}

func TestTemplateLeavesUnresolvableVarsAsIs(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "partial",
		"apps": map[string]any{
			"app": map[string]any{
				"image": "test:latest",
				"env": map[string]any{
					"VALID":       "hello",
					"BROKEN_REF":  "${nonexistent.env.FOO}",
					"BROKEN_PATH": "${app.env.NOPE}",
				},
			},
		},
	})

	r := fx.m.Create(map[string]any{"type": "app", "app": "partial", "name": "mypartial"})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	env := fx.dock.runCallAt(0).options.Env
	if env["VALID"] != "hello" || env["BROKEN_REF"] != "${nonexistent.env.FOO}" {
		t.Fatalf("env = %v", env)
	}
}

func TestTemplateUserEnvOverrides(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "override-test",
		"apps": map[string]any{
			"db": map[string]any{"image": "db:latest", "env": map[string]any{"DB_NAME": "default_db"}},
		},
	})

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "override-test", "name": "myoverride",
		"env": map[string]any{"db": map[string]any{"DB_NAME": "custom_db"}},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if env := fx.dock.runCallAt(0).options.Env; env["DB_NAME"] != "custom_db" {
		t.Fatalf("env = %v", env)
	}
}

func TestEchoedGenerateDirectiveDoesNotClobber(t *testing.T) {
	t.Run("single-app recipe", func(t *testing.T) {
		recipeEnv := map[string]any{
			"MARIADB_DATABASE":      "odac",
			"MARIADB_HOST":          map[string]any{"type": "container"},
			"MARIADB_ROOT_PASSWORD": map[string]any{"length": float64(16), "generate": true},
			"MARIADB_USER":          "root",
		}
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{"name": "mariadb", "image": "mariadb:latest", "env": recipeEnv})

		r := fx.m.Create(map[string]any{"type": "app", "app": "mariadb", "name": "mymariadb", "env": recipeEnv})
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)

		env := fx.dock.runCallAt(0).options.Env
		if env["MARIADB_ROOT_PASSWORD"] == "[object Object]" || len(env["MARIADB_ROOT_PASSWORD"]) != 16 {
			t.Fatalf("password = %q", env["MARIADB_ROOT_PASSWORD"])
		}
		if env["MARIADB_HOST"] != "mymariadb" {
			t.Fatalf("host = %q", env["MARIADB_HOST"])
		}
	})

	t.Run("template", func(t *testing.T) {
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{
			"name": "stack",
			"apps": map[string]any{
				"db": map[string]any{
					"image": "mariadb:latest",
					"env":   map[string]any{"DB_PASSWORD": map[string]any{"generate": true, "length": float64(16)}},
				},
			},
		})

		r := fx.m.Create(map[string]any{
			"type": "app", "app": "stack", "name": "mystack",
			"env": map[string]any{"db": map[string]any{"DB_PASSWORD": map[string]any{"generate": true, "length": float64(16)}}},
		})
		if !r.Status {
			t.Fatalf("create failed: %v", r.Message)
		}
		fx.waitIdle(t)

		env := fx.dock.runCallAt(0).options.Env
		if env["DB_PASSWORD"] == "[object Object]" || len(env["DB_PASSWORD"]) != 16 {
			t.Fatalf("password = %q", env["DB_PASSWORD"])
		}
	})
}

func TestTemplatePayloadFromHub(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template",
		"name": "wordpress",
		"apps": map[string]any{
			"db": map[string]any{
				"container": "wordpress-db-a3f2c1",
				"image":     "mariadb:10.6",
				"env": map[string]any{
					"MYSQL_DATABASE": "wordpress",
					"MYSQL_PASSWORD": map[string]any{"generate": true, "length": float64(16)},
					"MYSQL_USER":     "wordpress",
				},
				"ports":   []any{},
				"volumes": []any{map[string]any{"host": "data", "container": "/var/lib/mysql"}},
				"linked":  []any{},
			},
			"web": map[string]any{
				"container": "wordpress-web-a3f2c1",
				"image":     "wordpress:latest",
				"env": map[string]any{
					"WORDPRESS_DB_HOST":     "${db.name}",
					"WORDPRESS_DB_NAME":     "${db.env.MYSQL_DATABASE}",
					"WORDPRESS_DB_PASSWORD": "${db.env.MYSQL_PASSWORD}",
					"WORDPRESS_DB_USER":     "${db.env.MYSQL_USER}",
				},
				"ports":   []any{map[string]any{"container": float64(80), "host": "auto"}},
				"volumes": []any{map[string]any{"host": "data", "container": "/var/www/html"}},
				"linked":  []any{"db"},
			},
		},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	// Cloud-provided names used verbatim.
	if fx.findApp("wordpress-db-a3f2c1") == nil || fx.findApp("wordpress-web-a3f2c1") == nil {
		t.Fatal("cloud container names not used")
	}

	webEnv := fx.dock.runCallAt(1).options.Env
	if webEnv["WORDPRESS_DB_HOST"] != "wordpress-db-a3f2c1" || webEnv["WORDPRESS_DB_NAME"] != "wordpress" {
		t.Fatalf("web env = %v", webEnv)
	}
	if strings.Contains(webEnv["WORDPRESS_DB_PASSWORD"], "${") || len(webEnv["WORDPRESS_DB_PASSWORD"]) != 16 {
		t.Fatalf("password = %q", webEnv["WORDPRESS_DB_PASSWORD"])
	}
}

func TestTemplatePayloadValidation(t *testing.T) {
	t.Run("missing apps", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(map[string]any{"type": "template", "name": "empty"})
		if r.Status || !strings.Contains(jsString(r.Message), "no apps defined") {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("missing name", func(t *testing.T) {
		fx := newFixture(t, []any{})
		r := fx.m.Create(map[string]any{
			"type": "template",
			"apps": map[string]any{"db": map[string]any{"image": "db:latest", "env": map[string]any{}}},
		})
		if r.Status || !strings.Contains(jsString(r.Message), "Missing template name") {
			t.Fatalf("r = %+v", r)
		}
	})
}

// ---- GPU requests ----

// The Cloud's single-app shape (Hub.js:672): the request is persisted and
// reaches the container run untouched.
func TestCreateWithGPURequest(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})

	r := fx.m.Create(map[string]any{
		"type": "app", "name": "comfyui-a1b2c3", "app": "comfyui",
		"image": "x/comfyui:cuda", "ports": []any{}, "volumes": []any{}, "env": map[string]any{},
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("comfyui-a1b2c3")
	if app == nil {
		t.Fatal("app not persisted")
	}
	persisted, _ := app["gpu"].(map[string]any)
	if persisted["vendor"] != "nvidia" || persisted["runtime"] != "nvidia" || persisted["count"] != "all" {
		t.Fatalf("persisted gpu = %v", persisted)
	}

	spec := fx.dock.runCallAt(0).options.GPU
	if spec == nil || spec.Runtime != gpu.RuntimeNvidia || spec.Count != gpu.CountAll {
		t.Fatalf("RunOptions.GPU = %+v", spec)
	}
}

// A recipe may declare the GPU itself; the payload still wins over it.
func TestCreateGPUPayloadOverridesRecipe(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "whisper", "image": "x/whisper",
		"gpu": map[string]any{"runtime": "nvidia", "count": "all"},
	})

	if r := fx.m.Create(map[string]any{"type": "app", "app": "whisper", "name": "recipe-gpu"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)
	if spec := fx.dock.runCallAt(0).options.GPU; spec == nil || spec.Runtime != gpu.RuntimeNvidia {
		t.Fatalf("recipe gpu ignored: %+v", spec)
	}

	fx2 := newFixture(t, []any{})
	fx2.setRecipe(map[string]any{
		"name": "whisper", "image": "x/whisper",
		"gpu": map[string]any{"runtime": "nvidia", "count": "all"},
	})
	if r := fx2.m.Create(map[string]any{
		"type": "app", "app": "whisper", "name": "payload-gpu",
		"gpu": map[string]any{"vendor": "amd", "runtime": "rocm", "count": float64(1)},
	}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx2.waitIdle(t)
	spec := fx2.dock.runCallAt(0).options.GPU
	if spec == nil || spec.Runtime != gpu.RuntimeROCm || spec.Count != 1 {
		t.Fatalf("payload gpu did not win: %+v", spec)
	}
}

// A CPU app must carry no GPU key at all — the Cloud reads app.list and must
// not see a phantom request.
func TestCreateWithoutGPU(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine"})

	if r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "plain"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if app := fx.findApp("plain"); app == nil {
		t.Fatal("app not persisted")
	} else if _, present := app["gpu"]; present {
		t.Fatalf("CPU app carries a gpu key: %v", app["gpu"])
	}
	if spec := fx.dock.runCallAt(0).options.GPU; spec != nil {
		t.Fatalf("RunOptions.GPU = %+v, want nil", spec)
	}
}

// A malformed request fails the create instead of silently starting a
// CUDA-only image on the CPU.
func TestCreateRejectsInvalidGPU(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "bad-gpu",
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "cuda", "count": "all"},
	})
	if r.Status || !strings.Contains(jsString(r.Message), "Invalid GPU configuration") {
		t.Fatalf("r = %+v", r)
	}
	if fx.appCount() != 0 {
		t.Fatalf("a rejected create must persist nothing: %d apps", fx.appCount())
	}
	if fx.dock.runCallCount() != 0 {
		t.Fatal("a rejected create must not start a container")
	}
}

// The Cloud's template shape (Hub.js:904): gpu lives per container under
// apps.<key> and only that member gets it.
func TestTemplateGPUPerContainer(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "immich",
		"apps": map[string]any{
			"immich-server": map[string]any{
				"container": "immich-server", "image": "immich/server",
			},
			"immich-ml": map[string]any{
				"container": "immich-ml", "image": "immich/ml:cuda",
				"linked": []any{"immich-server"},
				"gpu":    map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
			},
		},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	ml := fx.findApp("immich-ml")
	if ml == nil {
		t.Fatal("ml app not persisted")
	}
	if persisted, _ := ml["gpu"].(map[string]any); persisted["runtime"] != "nvidia" {
		t.Fatalf("ml gpu = %v", ml["gpu"])
	}
	server := fx.findApp("immich-server")
	if _, present := server["gpu"]; present {
		t.Fatalf("the CPU member must not inherit a GPU: %v", server["gpu"])
	}

	// Both containers ran; only the ML one asked for a GPU.
	byName := map[string]*gpu.Spec{}
	for i := 0; i < fx.dock.runCallCount(); i++ {
		call := fx.dock.runCallAt(i)
		byName[call.name] = call.options.GPU
	}
	if spec := byName["immich-ml"]; spec == nil || spec.Runtime != gpu.RuntimeNvidia {
		t.Fatalf("immich-ml GPU = %+v", spec)
	}
	if spec := byName["immich-server"]; spec != nil {
		t.Fatalf("immich-server GPU = %+v, want nil", spec)
	}
}

// A bad GPU request anywhere in a template fails the whole stack before a
// single container is created — no half-deploy, no rollback.
func TestTemplateRejectsInvalidGPUBeforeDeploying(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "immich",
		"apps": map[string]any{
			"immich-server": map[string]any{"container": "immich-server", "image": "immich/server"},
			"immich-ml": map[string]any{
				"container": "immich-ml", "image": "immich/ml:cuda",
				"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": float64(0)},
			},
		},
	})
	if r.Status || !strings.Contains(jsString(r.Message), "Invalid GPU configuration for immich-ml") {
		t.Fatalf("r = %+v", r)
	}
	if fx.appCount() != 0 || fx.dock.runCallCount() != 0 {
		t.Fatalf("nothing may be created: %d apps, %d runs", fx.appCount(), fx.dock.runCallCount())
	}
}

// The pre-flight must fire before anything expensive happens: no directory,
// no logger, no image pull, no container. The Cloud gets the missing piece
// named instead of "failed to start".
func TestCreateGPUPreflightFailsEarly(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})
	fx.gpuHost.allow() // this host can pass nothing through

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "comfyui-a1b2c3",
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
	})

	if r.Status {
		t.Fatal("expected the create to fail")
	}
	message := jsString(r.Message)
	if !strings.Contains(message, "nvidia-container-toolkit") || !strings.Contains(message, "nvidia-ctk runtime configure") {
		t.Fatalf("message must name the fix, got %q", message)
	}
	if fx.appCount() != 0 {
		t.Errorf("nothing may be persisted: %d apps", fx.appCount())
	}
	// RunApp is what pulls the image and creates the container; never
	// reaching it is exactly "no multi-gigabyte CUDA pull".
	if fx.dock.runCallCount() != 0 {
		t.Errorf("RunApp must not be reached: %d calls", fx.dock.runCallCount())
	}
	if len(fx.dock.registered) != 0 {
		t.Errorf("no build logger may be registered: %v", fx.dock.registered)
	}
}

// Same gate for templates, before a single member is created.
func TestTemplateGPUPreflightFailsEarly(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.gpuHost.allow(gpu.RuntimeROCm) // AMD host, NVIDIA app

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "immich",
		"apps": map[string]any{
			"immich-server": map[string]any{"container": "immich-server", "image": "immich/server"},
			"immich-ml": map[string]any{
				"container": "immich-ml", "image": "immich/ml:cuda",
				"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
			},
		},
	})

	if r.Status || !strings.Contains(jsString(r.Message), "nvidia-container-toolkit") {
		t.Fatalf("r = %+v", r)
	}
	if fx.appCount() != 0 || fx.dock.runCallCount() != 0 {
		t.Fatalf("nothing may be created: %d apps, %d runs", fx.appCount(), fx.dock.runCallCount())
	}
}

// A capable host is not blocked, and CPU apps never consult the pre-flight.
func TestCreateGPUPreflightPasses(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})
	fx.gpuHost.allow(gpu.RuntimeNvidia)

	if r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "gpu-ok",
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
	}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)
	if spec := fx.dock.runCallAt(0).options.GPU; spec == nil {
		t.Fatal("GPU request lost")
	}

	fx2 := newFixture(t, []any{})
	fx2.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine"})
	fx2.gpuHost.allow() // nothing allowed — a CPU app must not care
	if r := fx2.m.Create(map[string]any{"type": "app", "app": "redis", "name": "cpu-app"}); !r.Status {
		t.Fatalf("CPU app blocked by the GPU pre-flight: %v", r.Message)
	}
	fx2.waitIdle(t)
	if len(fx2.gpuHost.asked) != 0 {
		t.Errorf("pre-flight consulted for a CPU app: %v", fx2.gpuHost.asked)
	}
}

// When the daemon refuses at start time anyway (toolkit removed after the
// pre-flight, or a redeploy), the Cloud still gets the actionable message
// rather than the generic one.
func TestCreateGPURefusedAtStartMessage(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})
	fx.gpuHost.allow(gpu.RuntimeNvidia)   // pre-flight says yes...
	fx.dock.runErr = func(string) error { // ...but the daemon disagrees
		return fmt.Errorf("%w (nvidia): could not select device driver", docker.ErrGPUUnavailable)
	}

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "late-fail",
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
	})

	if r.Status {
		t.Fatal("expected failure")
	}
	if !strings.Contains(jsString(r.Message), "nvidia-container-toolkit") {
		t.Fatalf("message = %q", jsString(r.Message))
	}
	if fx.appCount() != 0 {
		t.Errorf("the failed app must be rolled back: %d apps", fx.appCount())
	}
}

// An ordinary start failure keeps the generic message — the GPU must not be
// blamed for unrelated problems.
func TestCreateNonGPUFailureMessage(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "comfyui", "image": "x/comfyui:cuda"})
	fx.dock.runErr = func(string) error { return errors.New("port already allocated") }

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "comfyui", "name": "other-fail",
		"gpu": map[string]any{"vendor": "nvidia", "runtime": "nvidia", "count": "all"},
	})
	if r.Status || strings.Contains(jsString(r.Message), "nvidia-container-toolkit") {
		t.Fatalf("r = %+v", r)
	}
}

// A slow image pull must leave the app visible and correctly labelled: the
// record is broadcast before RunApp, and List reports "installing" instead of
// the live container state until the pull finishes.
func TestCreateStaysVisibleWhileImagePulls(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "redis", "image": "redis:7"})

	block := make(chan struct{})
	release := sync.OnceFunc(func() { close(block) })
	defer release()
	fx.dock.mu.Lock()
	fx.dock.runBlock = block
	fx.dock.mu.Unlock()

	done := make(chan bool, 1)
	go func() {
		done <- fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "slow-pull"}).Status
	}()

	waitFor(t, "the pull to start", func() bool { return fx.dock.runCallCount() == 1 })

	if !fx.hub.sawTrigger("app.list") {
		t.Fatal("app.list was not broadcast before the pull")
	}
	// run() relabels the record "starting" before RunApp, so that is the
	// status the pull runs under. What matters is that it is not "stopped".
	if got := fx.listStatus("slow-pull"); got != "starting" {
		t.Fatalf("status during pull = %v, want starting", got)
	}

	release()
	if !<-done {
		t.Fatal("create failed")
	}
	fx.waitIdle(t)

	if got := fx.listStatus("slow-pull"); got != "running" {
		t.Fatalf("status after create = %v, want running", got)
	}
}

// ---- recipe config files ----

// A recipe config file must land on disk verbatim and reach Docker as a bind
// with an absolute container target, which is the only form the daemon
// accepts. Multi-line content may arrive as a string or as a list of lines.
func TestCreateWritesRecipeConfigFiles(t *testing.T) {
	yaml := "logger:\n  level: warning\n  size: 100M\n"
	cases := []struct {
		name    string
		content any
	}{
		{"string", yaml},
		{"lines", []any{"logger:", "  level: warning", "  size: 100M"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, []any{})
			fx.setRecipe(map[string]any{
				"name":  "clickhouse",
				"image": "clickhouse/clickhouse-server:24",
				"configs": []any{map[string]any{
					"path":    "/etc/clickhouse-server/config.d/odac-logs.yaml",
					"content": tc.content,
				}},
			})

			r := fx.m.Create(map[string]any{"type": "app", "app": "clickhouse", "name": "ch"})
			if !r.Status {
				t.Fatalf("create failed: %v", r.Message)
			}
			fx.waitIdle(t)

			app := fx.findApp("ch")
			if app == nil {
				t.Fatal("app not persisted")
			}
			vols, _ := app["volumes"].([]any)
			var mount map[string]any
			for _, v := range vols {
				if entry, _ := v.(map[string]any); entry != nil &&
					entry["container"] == "/etc/clickhouse-server/config.d/odac-logs.yaml" {
					mount = entry
				}
			}
			if mount == nil {
				t.Fatalf("config mount missing, volumes = %v", vols)
			}

			hostPath, _ := mount["host"].(string)
			wantHost := filepath.Join(fx.m.appsPath(), "ch", "configs",
				"etc", "clickhouse-server", "config.d", "odac-logs.yaml")
			if hostPath != wantHost {
				t.Fatalf("host = %q, want %q", hostPath, wantHost)
			}
			data, err := os.ReadFile(hostPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			if string(data) != yaml {
				t.Fatalf("content = %q, want %q", string(data), yaml)
			}

			// The bind must survive into the actual container run.
			var bound bool
			fx.dock.mu.Lock()
			for _, call := range fx.dock.runCalls {
				for _, vol := range call.options.Volumes {
					if vol.Host == wantHost && vol.Container == "/etc/clickhouse-server/config.d/odac-logs.yaml" {
						bound = true
					}
				}
			}
			fx.dock.mu.Unlock()
			if !bound {
				t.Fatal("config bind never reached RunApp")
			}
		})
	}
}

// Paths that cannot become a valid mount target, or that try to escape the
// per-app sandbox, are dropped instead of producing a broken container.
func TestWriteConfigFilesRejectsUnusablePaths(t *testing.T) {
	fx := newFixture(t, []any{})
	appDir := filepath.Join(fx.m.appsPath(), "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"conf/app.yaml", "app.yaml", "", "../../etc/passwd"} {
		vols, err := fx.m.writeConfigFiles([]any{
			map[string]any{"path": p, "content": "x"},
		}, appDir)
		if err != nil {
			t.Fatalf("path %q: %v", p, err)
		}
		if len(vols) != 0 {
			t.Fatalf("path %q produced %v, want no mount", p, vols)
		}
	}

	// An absolute path with ".." resolves inside the sandbox, it does not escape.
	vols, err := fx.m.writeConfigFiles([]any{
		map[string]any{"path": "/etc/../etc/app.conf", "content": "x"},
	}, appDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 {
		t.Fatalf("volumes = %v, want 1", vols)
	}
	entry, _ := vols[0].(map[string]any)
	if entry["container"] != "/etc/app.conf" {
		t.Fatalf("container = %v, want /etc/app.conf", entry["container"])
	}
	host, _ := entry["host"].(string)
	if !strings.HasPrefix(host, filepath.Join(appDir, "configs")+string(filepath.Separator)) {
		t.Fatalf("host escaped the sandbox: %q", host)
	}
}

func TestConfigContent(t *testing.T) {
	cases := []struct {
		name    string
		content any
		want    string
	}{
		{"string verbatim", "a: 1\nb: 2", "a: 1\nb: 2"},
		{"lines joined", []any{"a: 1", "b: 2"}, "a: 1\nb: 2\n"},
		{"empty list", []any{}, ""},
		{"mixed list falls back to json", []any{"a", float64(1)}, "[\n  \"a\",\n  1\n]"},
		{"object falls back to json", map[string]any{"a": "1"}, "{\n  \"a\": \"1\"\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configContent(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("configContent = %q, want %q", got, tc.want)
			}
		})
	}
}

// The WireGuard shape end to end: caps + sysctls at the payload root and a
// published UDP port, persisted on the record and handed to the engine.
func TestCreateWithKernelRequest(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "wireguard", "image": "linuxserver/wireguard",
		"ports": []any{map[string]any{
			"host": float64(51820), "container": float64(51820), "proto": "udp", "public": true,
		}},
	})

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "wireguard", "name": "wg-a1b2c3",
		"caps":    []any{"NET_ADMIN"},
		"sysctls": map[string]any{"net.ipv4.ip_forward": "1"},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("wg-a1b2c3")
	if app == nil {
		t.Fatal("app not persisted")
	}
	caps, _ := app["caps"].([]any)
	if len(caps) != 1 || caps[0] != "NET_ADMIN" {
		t.Fatalf("persisted caps = %v", app["caps"])
	}
	sysctls, _ := app["sysctls"].(map[string]any)
	if len(sysctls) != 1 || sysctls["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("persisted sysctls = %v", app["sysctls"])
	}
	portList, _ := app["ports"].([]any)
	entry, _ := portList[0].(map[string]any)
	if entry["proto"] != "udp" || entry["host"] != float64(51820) || entry["public"] != true {
		t.Fatalf("persisted port = %v", entry)
	}

	opts := fx.dock.runCallAt(0).options
	if opts.Kernel == nil || len(opts.Kernel.Caps) != 1 || opts.Kernel.Caps[0] != kernel.CapNetAdmin {
		t.Fatalf("RunOptions.Kernel = %+v", opts.Kernel)
	}
	if opts.Kernel.Sysctls["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("RunOptions sysctls = %+v", opts.Kernel.Sysctls)
	}
	if len(opts.Ports) != 1 || opts.Ports[0]["proto"] != "udp" {
		t.Fatalf("published ports = %+v", opts.Ports)
	}
}

// A recipe may declare what its image needs; an explicit payload still wins,
// exactly like the GPU request.
func TestCreateKernelPayloadOverridesRecipe(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "wireguard", "image": "linuxserver/wireguard",
		"caps": []any{"NET_ADMIN"},
	})
	if r := fx.m.Create(map[string]any{"type": "app", "app": "wireguard", "name": "recipe-caps"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)
	if spec := fx.dock.runCallAt(0).options.Kernel; spec == nil || spec.Caps[0] != kernel.CapNetAdmin {
		t.Fatalf("recipe caps ignored: %+v", spec)
	}

	fx2 := newFixture(t, []any{})
	fx2.setRecipe(map[string]any{
		"name": "wireguard", "image": "linuxserver/wireguard",
		"caps": []any{"NET_ADMIN"},
	})
	if r := fx2.m.Create(map[string]any{
		"type": "app", "app": "wireguard", "name": "payload-caps",
		"caps": []any{"NET_RAW"},
	}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx2.waitIdle(t)
	spec := fx2.dock.runCallAt(0).options.Kernel
	if spec == nil || len(spec.Caps) != 1 || spec.Caps[0] != kernel.CapNetRaw {
		t.Fatalf("payload caps did not win: %+v", spec)
	}
}

// Every other app's record and container config must be what it was before
// these fields existed.
func TestCreateWithoutKernelRequest(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine"})

	if r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "plain"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("plain")
	if _, present := app["caps"]; present {
		t.Fatalf("app carries a caps key: %v", app["caps"])
	}
	if _, present := app["sysctls"]; present {
		t.Fatalf("app carries a sysctls key: %v", app["sysctls"])
	}
	if spec := fx.dock.runCallAt(0).options.Kernel; spec != nil {
		t.Fatalf("RunOptions.Kernel = %+v, want nil", spec)
	}
}

// A capability outside the allowlist fails the create. Passing it through
// would only move the refusal to container create, which the app manager
// retries on every check tick.
func TestCreateRejectsDisallowedKernelRequest(t *testing.T) {
	for _, payload := range []map[string]any{
		{"caps": []any{"SYS_ADMIN"}},
		{"caps": []any{"NET_ADMIN", "SYS_MODULE"}},
		{"sysctls": map[string]any{"vm.max_map_count": "262144"}},
		{"sysctls": map[string]any{"net.ipv4.ip_forward": []any{"1"}}},
	} {
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{"name": "wireguard", "image": "linuxserver/wireguard"})

		cfg := map[string]any{"type": "app", "app": "wireguard", "name": "bad-kernel"}
		for k, v := range payload {
			cfg[k] = v
		}
		r := fx.m.Create(cfg)
		if r.Status || !strings.Contains(jsString(r.Message), "Invalid kernel configuration") {
			t.Fatalf("payload %v: r = %+v", payload, r)
		}
		if fx.appCount() != 0 || fx.dock.runCallCount() != 0 {
			t.Fatalf("a rejected create must persist and start nothing: %d apps, %d runs", fx.appCount(), fx.dock.runCallCount())
		}
	}
}

// The Frigate shape: a bigger /dev/shm asked for at the payload root,
// persisted as a canonical byte count and handed to the engine.
func TestCreateWithShmSize(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "frigate", "image": "blakeblackshear/frigate"})

	r := fx.m.Create(map[string]any{
		"type": "app", "app": "frigate", "name": "frigate-a1b2c3",
		"shmSize": "512m",
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	app := fx.findApp("frigate-a1b2c3")
	if app == nil {
		t.Fatal("app not persisted")
	}
	if app["shmSize"] != float64(512<<20) {
		t.Fatalf("persisted shmSize = %v, want %v", app["shmSize"], float64(512<<20))
	}
	if spec := fx.dock.runCallAt(0).options.Resources; spec.Shm() != 512<<20 {
		t.Fatalf("RunOptions.Resources = %+v", spec)
	}
}

// A recipe may size its own image's /dev/shm; an explicit payload still
// wins, because only the operator knows how many cameras are pointed at it.
func TestCreateShmSizePayloadOverridesRecipe(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{
		"name": "frigate", "image": "blakeblackshear/frigate",
		"shmSize": "256m",
	})
	if r := fx.m.Create(map[string]any{"type": "app", "app": "frigate", "name": "recipe-shm"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)
	if spec := fx.dock.runCallAt(0).options.Resources; spec.Shm() != 256<<20 {
		t.Fatalf("recipe shmSize ignored: %+v", spec)
	}

	fx2 := newFixture(t, []any{})
	fx2.setRecipe(map[string]any{
		"name": "frigate", "image": "blakeblackshear/frigate",
		"shmSize": "256m",
	})
	if r := fx2.m.Create(map[string]any{
		"type": "app", "app": "frigate", "name": "payload-shm",
		"shmSize": float64(1 << 30),
	}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx2.waitIdle(t)
	if spec := fx2.dock.runCallAt(0).options.Resources; spec.Shm() != 1<<30 {
		t.Fatalf("payload shmSize did not win: %+v", spec)
	}
}

// Every other app's record and container config must be what it was before
// the field existed.
func TestCreateWithoutShmSize(t *testing.T) {
	fx := newFixture(t, []any{})
	fx.setRecipe(map[string]any{"name": "redis", "image": "redis:alpine"})

	if r := fx.m.Create(map[string]any{"type": "app", "app": "redis", "name": "plain-shm"}); !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if app := fx.findApp("plain-shm"); app != nil {
		if _, present := app["shmSize"]; present {
			t.Fatalf("app carries a shmSize key: %v", app["shmSize"])
		}
	}
	if spec := fx.dock.runCallAt(0).options.Resources; spec != nil {
		t.Fatalf("RunOptions.Resources = %+v, want nil", spec)
	}
}

// An unreadable size fails the create rather than falling back to the
// default: an app sized for shared memory that silently gets 64 MiB dies
// under load with nothing naming the cause.
func TestCreateRejectsBadShmSize(t *testing.T) {
	for _, value := range []any{"512", "lots", float64(int64(1) << 40), float64(-1)} {
		fx := newFixture(t, []any{})
		fx.setRecipe(map[string]any{"name": "frigate", "image": "blakeblackshear/frigate"})

		r := fx.m.Create(map[string]any{
			"type": "app", "app": "frigate", "name": "bad-shm",
			"shmSize": value,
		})
		if r.Status || !strings.Contains(jsString(r.Message), "Invalid resource configuration") {
			t.Fatalf("shmSize %v: r = %+v", value, r)
		}
		if fx.appCount() != 0 || fx.dock.runCallCount() != 0 {
			t.Fatalf("a rejected create must persist and start nothing: %d apps, %d runs", fx.appCount(), fx.dock.runCallCount())
		}
	}
}

// Template shape: the size belongs to the container that needs it, and a bad
// one fails the stack before a single container exists.
func TestTemplateShmSizePerContainer(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "nvr",
		"apps": map[string]any{
			"ui": map[string]any{"container": "nvr-ui", "image": "nginx"},
			"detector": map[string]any{
				"container": "nvr-detector", "image": "blakeblackshear/frigate",
				"linked":  []any{"ui"},
				"shmSize": "512m",
			},
		},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	if detector := fx.findApp("nvr-detector"); detector["shmSize"] != float64(512<<20) {
		t.Fatalf("detector shmSize = %v", detector["shmSize"])
	}
	if ui := fx.findApp("nvr-ui"); ui != nil {
		if _, present := ui["shmSize"]; present {
			t.Fatalf("the plain member must not inherit shmSize: %v", ui["shmSize"])
		}
	}

	bad := newFixture(t, []any{})
	r = bad.m.Create(map[string]any{
		"type": "template", "name": "nvr",
		"apps": map[string]any{
			"ui": map[string]any{"container": "bad-ui", "image": "nginx"},
			"detector": map[string]any{
				"container": "bad-detector", "image": "blakeblackshear/frigate",
				"linked":  []any{"ui"},
				"shmSize": "512",
			},
		},
	})
	if r.Status || !strings.Contains(jsString(r.Message), "Invalid resource configuration") {
		t.Fatalf("bad template shmSize: r = %+v", r)
	}
	if bad.appCount() != 0 || bad.dock.runCallCount() != 0 {
		t.Fatalf("a rejected template must persist and start nothing: %d apps, %d runs", bad.appCount(), bad.dock.runCallCount())
	}
}

// Template shape: caps live per container under apps.<key>, and only that
// member gets them.
func TestTemplateKernelPerContainer(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "vpn",
		"apps": map[string]any{
			"web": map[string]any{"container": "vpn-web", "image": "nginx"},
			"tunnel": map[string]any{
				"container": "vpn-tunnel", "image": "linuxserver/wireguard",
				"linked":  []any{"web"},
				"caps":    []any{"NET_ADMIN"},
				"sysctls": map[string]any{"net.ipv4.ip_forward": float64(1)},
			},
		},
	})
	if !r.Status {
		t.Fatalf("create failed: %v", r.Message)
	}
	fx.waitIdle(t)

	tunnel := fx.findApp("vpn-tunnel")
	if caps, _ := tunnel["caps"].([]any); len(caps) != 1 || caps[0] != "NET_ADMIN" {
		t.Fatalf("tunnel caps = %v", tunnel["caps"])
	}
	if sysctls, _ := tunnel["sysctls"].(map[string]any); sysctls["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("tunnel sysctls = %v", tunnel["sysctls"])
	}
	web := fx.findApp("vpn-web")
	if _, present := web["caps"]; present {
		t.Fatalf("the plain member must not inherit caps: %v", web["caps"])
	}
	if _, present := web["sysctls"]; present {
		t.Fatalf("the plain member must not inherit sysctls: %v", web["sysctls"])
	}

	byName := map[string]*kernel.Spec{}
	for i := 0; i < fx.dock.runCallCount(); i++ {
		call := fx.dock.runCallAt(i)
		byName[call.name] = call.options.Kernel
	}
	if spec := byName["vpn-tunnel"]; spec == nil || spec.Caps[0] != kernel.CapNetAdmin {
		t.Fatalf("vpn-tunnel kernel = %+v", spec)
	}
	if spec := byName["vpn-web"]; spec != nil {
		t.Fatalf("vpn-web kernel = %+v, want nil", spec)
	}
}

// A bad request anywhere in a template fails the whole stack before a single
// container is created.
func TestTemplateRejectsDisallowedKernelBeforeDeploying(t *testing.T) {
	fx := newFixture(t, []any{})

	r := fx.m.Create(map[string]any{
		"type": "template", "name": "vpn",
		"apps": map[string]any{
			"web": map[string]any{"container": "vpn-web", "image": "nginx"},
			"tunnel": map[string]any{
				"container": "vpn-tunnel", "image": "linuxserver/wireguard",
				"caps": []any{"SYS_ADMIN"},
			},
		},
	})
	if r.Status || !strings.Contains(jsString(r.Message), "Invalid kernel configuration for tunnel") {
		t.Fatalf("r = %+v", r)
	}
	if fx.appCount() != 0 || fx.dock.runCallCount() != 0 {
		t.Fatalf("nothing may be created: %d apps, %d runs", fx.appCount(), fx.dock.runCallCount())
	}
}
