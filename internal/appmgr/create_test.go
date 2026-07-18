package appmgr

// Ports of App.test.js "git configuration" + "template deployment".

import (
	"strings"
	"testing"
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
