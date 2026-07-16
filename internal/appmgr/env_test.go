package appmgr

// Ports of App.test.js "environment management" (incl. the cd7f08b masking
// change: `pass` no longer masks).

import "testing"

func envFixture(t *testing.T) *fixture {
	return newFixture(t, []any{
		map[string]any{
			"id": float64(1), "name": "app-main", "type": "container",
			"env": map[string]any{
				"manual": map[string]any{
					"NODE_ENV": "production",
					"API_KEY":  "secret-key-123",
					"DB_PASS":  "password123",
				},
				"linked": []any{},
			},
		},
		map[string]any{
			"id": float64(2), "name": "app-db", "type": "container",
			"env": map[string]any{
				"manual": map[string]any{
					"POSTGRES_USER":     "admin",
					"POSTGRES_PASSWORD": "db-secret-password",
				},
			},
		},
		map[string]any{
			"id": float64(3), "name": "app-legacy", "type": "container",
			"env": map[string]any{"LEGACY_VAR": "old-value"},
		},
	})
}

func TestGetEnvMasksSensitiveValues(t *testing.T) {
	fx := envFixture(t)

	r := fx.m.GetEnv("app-main")
	if !r.Status {
		t.Fatalf("getEnv failed: %v", r.Message)
	}
	data, _ := r.Data.(map[string]any)
	manual, _ := data["manual"].(map[string]any)

	if manual["NODE_ENV"] != "production" {
		t.Fatalf("NODE_ENV = %v", manual["NODE_ENV"])
	}
	if manual["API_KEY"] != "***" {
		t.Fatalf("API_KEY should be masked, got %v", manual["API_KEY"])
	}
	// dev cd7f08b: `pass` is NOT masked anymore.
	if manual["DB_PASS"] != "password123" {
		t.Fatalf("DB_PASS should be unmasked, got %v", manual["DB_PASS"])
	}
	if linked, _ := data["linked"].([]any); len(linked) != 0 {
		t.Fatalf("linked should start empty, got %v", linked)
	}
}

func TestSetEnvMergesAndMigratesLegacy(t *testing.T) {
	fx := envFixture(t)

	// Legacy migration.
	if r := fx.m.SetEnv("app-legacy", map[string]any{"NEW_VAR": "new-value"}); !r.Status {
		t.Fatalf("setEnv legacy failed: %v", r.Message)
	}
	legacy := fx.app(2)
	env, _ := legacy["env"].(map[string]any)
	manual, _ := env["manual"].(map[string]any)
	if manual == nil || manual["LEGACY_VAR"] != "old-value" || manual["NEW_VAR"] != "new-value" {
		t.Fatalf("legacy migration wrong: %v", env)
	}

	// Normal merge.
	if r := fx.m.SetEnv("app-main", map[string]any{"NODE_ENV": "development", "EXTRA": "foo"}); !r.Status {
		t.Fatalf("setEnv failed: %v", r.Message)
	}
	main := fx.app(0)
	env, _ = main["env"].(map[string]any)
	manual, _ = env["manual"].(map[string]any)
	if manual["NODE_ENV"] != "development" || manual["API_KEY"] != "secret-key-123" || manual["EXTRA"] != "foo" {
		t.Fatalf("merge wrong: %v", manual)
	}
}

func TestSetEnvRejectsNonObject(t *testing.T) {
	fx := envFixture(t)
	if r := fx.m.SetEnv("app-main", nil); r.Status {
		t.Fatal("setEnv(nil) should fail")
	}
}

func TestDeleteEnvRemovesKeys(t *testing.T) {
	fx := envFixture(t)

	if r := fx.m.DeleteEnv("app-main", []string{"API_KEY", "NON_EXISTENT"}); !r.Status {
		t.Fatalf("deleteEnv failed: %v", r.Message)
	}
	env, _ := fx.app(0)["env"].(map[string]any)
	manual, _ := env["manual"].(map[string]any)
	if _, present := manual["API_KEY"]; present {
		t.Fatalf("API_KEY not removed: %v", manual)
	}
	if manual["NODE_ENV"] != "production" {
		t.Fatalf("NODE_ENV lost: %v", manual)
	}
}

func TestDeleteEnvRejectsEmptyKeys(t *testing.T) {
	fx := envFixture(t)
	if r := fx.m.DeleteEnv("app-main", nil); r.Status {
		t.Fatal("deleteEnv(nil) should fail")
	}
}

func TestLinkEnvValidatesAndLinks(t *testing.T) {
	fx := envFixture(t)

	if r := fx.m.LinkEnv("app-main", "app-main"); r.Status {
		t.Fatal("self-link should fail")
	}
	if r := fx.m.LinkEnv("app-main", "ghost"); r.Status {
		t.Fatal("link to unknown target should fail")
	}

	if r := fx.m.LinkEnv("app-main", "app-db"); !r.Status {
		t.Fatalf("linkEnv failed: %v", r.Message)
	}
	env, _ := fx.app(0)["env"].(map[string]any)
	linked, _ := env["linked"].([]any)
	if len(linked) != 1 || linked[0] != "app-db" {
		t.Fatalf("linked = %v", linked)
	}

	// getEnv resolves the linked app's (sanitized) envs.
	r := fx.m.GetEnv("app-main")
	data, _ := r.Data.(map[string]any)
	linkedOut, _ := data["linked"].([]any)
	if len(linkedOut) != 1 {
		t.Fatalf("linked resolution = %v", linkedOut)
	}
	entry, _ := linkedOut[0].(map[string]any)
	if entry["app"] != "app-db" {
		t.Fatalf("linked app = %v", entry["app"])
	}
	linkedEnv, _ := entry["env"].(map[string]any)
	if linkedEnv["POSTGRES_USER"] != "admin" {
		t.Fatalf("POSTGRES_USER = %v", linkedEnv["POSTGRES_USER"])
	}
	// POSTGRES_PASSWORD contains `pass` only — not masked post-cd7f08b.
	if linkedEnv["POSTGRES_PASSWORD"] != "db-secret-password" {
		t.Fatalf("POSTGRES_PASSWORD = %v", linkedEnv["POSTGRES_PASSWORD"])
	}
}

func TestUnlinkEnvRemovesLink(t *testing.T) {
	fx := envFixture(t)
	fx.m.LinkEnv("app-main", "app-db")

	if r := fx.m.UnlinkEnv("app-main", "app-db"); !r.Status {
		t.Fatalf("unlinkEnv failed: %v", r.Message)
	}
	env, _ := fx.app(0)["env"].(map[string]any)
	linked, _ := env["linked"].([]any)
	if len(linked) != 0 {
		t.Fatalf("link not removed: %v", linked)
	}

	if r := fx.m.UnlinkEnv("app-main", "app-db"); r.Status {
		t.Fatal("unlinking a non-linked app should fail")
	}
}

func TestResolveEnvMergesLinkedThenManual(t *testing.T) {
	// Linked env is applied first, own manual env overrides it, system
	// defaults (HOST/ODAC_APP) always present — verified through the actual
	// container run env.
	fx := newFixture(t, []any{
		map[string]any{
			"id": float64(1), "name": "web", "type": "container", "image": "app", "active": true,
			"env": map[string]any{
				"manual": map[string]any{"SHARED": "own-wins"},
				"linked": []any{"db"},
			},
		},
		map[string]any{
			"id": float64(2), "name": "db", "type": "container",
			"env": map[string]any{"manual": map[string]any{"SHARED": "from-db", "DB_HOST": "db"}},
		},
	})

	fx.checkAndSettle(t)

	env := fx.dock.runCallAt(0).options.Env
	if env["SHARED"] != "own-wins" {
		t.Fatalf("manual should override linked: %v", env["SHARED"])
	}
	if env["DB_HOST"] != "db" {
		t.Fatalf("linked env missing: %v", env)
	}
	if env["HOST"] != "0.0.0.0" || env["ODAC_APP"] != "true" {
		t.Fatalf("system envs missing: %v", env)
	}
}
