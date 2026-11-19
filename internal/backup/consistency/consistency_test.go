package consistency

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"odac/internal/docker"
)

// fakeProber stands in for dump containers: answer decides what each
// command prints, by the tool it runs.
type fakeProber struct {
	mu     sync.Mutex
	calls  []docker.DumpSpec
	answer func(tool string, args []string, stdin string) (string, error)
}

func (f *fakeProber) Run(_ context.Context, spec docker.DumpSpec) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
	tool, args := spec.Cmd[0], spec.Cmd[1:]
	if tool == "sh" {
		tool, args = spec.Cmd[3], spec.Cmd[4:]
	}
	out, err := f.answer(tool, args, string(spec.Stdin))
	return []byte(out), err
}

func (f *fakeProber) ran(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Cmd[0] == tool || len(c.Cmd) > 3 && c.Cmd[0] == "sh" && c.Cmd[3] == tool {
			n++
		}
	}
	return n
}

// has answers --version for the tools an image has and refuses the rest
// the way the runtime does.
func has(tools ...string) func(string, []string, string) (string, error) {
	return func(tool string, args []string, _ string) (string, error) {
		if !slices.Contains(tools, tool) {
			return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
		}
		if len(args) == 1 && args[0] == "--version" {
			return tool + " 1.0", nil
		}
		return "", nil
	}
}

func newPlanner(t *testing.T, f *fakeProber, avail int64) *Planner {
	t.Helper()
	var removed []string
	p, err := NewPlanner(Config{
		Prober:          f,
		AvailableMemory: func() (int64, error) { return avail, nil },
		RemoveAll: func(s string) error {
			removed = append(removed, s)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func plan(t *testing.T, p *Planner, app App) *Plan {
	t.Helper()
	pl, err := p.Plan(context.Background(), app, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func warned(pl *Plan, code string) bool {
	for _, w := range pl.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// hidesSecret fails when a credential shows in what `docker inspect` or `ps`
// would show of the dump container.
func hidesSecret(t *testing.T, spec docker.DumpSpec, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, v := range append(append([]string{}, spec.Cmd...), spec.Env...) {
			if strings.Contains(v, s) {
				t.Errorf("secret %q in %q", s, v)
			}
		}
	}
}

const tricky = `p:a\ss"w'o$(rm -rf /)rd`

func TestEngineOfImage(t *testing.T) {
	for image, want := range map[string]Engine{
		"postgres:16-alpine":                         Postgres,
		"docker.io/library/postgres:17@sha256:abcd":  Postgres,
		"registry.example.com:5000/team/postgres:15": Postgres,
		"postgis/postgis:16-3.4":                     Postgres,
		"bitnami/postgresql":                         Postgres,
		"mysql:8.4":                                  MySQL,
		"mariadb:11":                                 MariaDB,
		"redis:7-alpine":                             Redis,
		"valkey/valkey:8":                            Redis,
		"mongo:7.0":                                  Mongo,
		"clickhouse/clickhouse-server:24.8":          ClickHouse,
		"ghcr.io/example/redis":                      Redis,
		"nginx:alpine":                               "",
		"odac-app-blog":                              "",
		"sha256:0123456789abcdef":                    "",
		"localhost/mysql":                            MySQL,
		"example/postgres-exporter":                  "",
	} {
		if got := EngineOfImage(image); got != want {
			t.Errorf("%s: %q, want %q", image, got, want)
		}
	}
}

func TestSpecParse(t *testing.T) {
	s, err := Parse(map[string]any{"backup": map[string]any{"engine": "postgres"}})
	if err != nil || s == nil || s.Engine != Postgres {
		t.Fatalf("engine: %+v %v", s, err)
	}
	s, err = Parse(map[string]any{"backup": map[string]any{"hook": map[string]any{
		"dump": []any{"/bin/export", "--all"}, "restore": []any{"/bin/import"},
		"file": "export.bin", "consistency": "consistent", "covers": []any{"/var/lib/app"},
	}}})
	if err != nil || s.Hook == nil || s.Hook.File != "export.bin" || s.Hook.Consistency != Consistent || len(s.Hook.Covers) != 1 {
		t.Fatalf("hook: %+v %v", s, err)
	}
	app := map[string]any{}
	s.Apply(app)
	again, err := Parse(app)
	if err != nil || again.Hook == nil || !slices.Equal(again.Hook.Dump, s.Hook.Dump) || !slices.Equal(again.Hook.Restore, s.Hook.Restore) {
		t.Fatalf("round trip: %+v %v", again, err)
	}
	h, _ := Parse(map[string]any{"backup": map[string]any{"hook": map[string]any{"dump": []any{"x"}}}})
	if h.Hook.Consistency != Live || h.Hook.File != "dump" {
		t.Fatalf("hook defaults: %+v", h.Hook)
	}

	if s, err := Parse(map[string]any{"name": "x"}); s != nil || err != nil {
		t.Fatalf("no declaration: %+v %v", s, err)
	}
	var none *Spec
	app = map[string]any{"backup": "stale"}
	none.Apply(app)
	if _, ok := app["backup"]; ok {
		t.Fatal("Apply of no declaration left the member")
	}

	for name, bad := range map[string]any{
		"not an object":    "postgres",
		"unknown engine":   map[string]any{"engine": "oracle"},
		"unknown member":   map[string]any{"engine": "postgres", "pause": true},
		"both":             map[string]any{"engine": "postgres", "hook": map[string]any{"dump": []any{"x"}}},
		"empty dump":       map[string]any{"hook": map[string]any{"dump": []any{}}},
		"no dump":          map[string]any{"hook": map[string]any{"file": "x"}},
		"bad file":         map[string]any{"hook": map[string]any{"dump": []any{"x"}, "file": "../x"}},
		"bad consistency":  map[string]any{"hook": map[string]any{"dump": []any{"x"}, "consistency": "perfect"}},
		"relative covers":  map[string]any{"hook": map[string]any{"dump": []any{"x"}, "covers": []any{"data"}}},
		"root covers":      map[string]any{"hook": map[string]any{"dump": []any{"x"}, "covers": []any{"/"}}},
		"NUL in argument":  map[string]any{"hook": map[string]any{"dump": []any{"x", "a\x00b"}}},
		"number argument":  map[string]any{"hook": map[string]any{"dump": []any{"x", 1.0}}},
		"blank executable": map[string]any{"hook": map[string]any{"dump": []any{" "}}},
	} {
		if _, err := Parse(map[string]any{"backup": bad}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPostgres(t *testing.T) {
	f := &fakeProber{answer: has("pg_dumpall", "pg_dump")}
	p := newPlanner(t, f, 8<<30)
	app := App{
		Name: "db", Container: "db", Image: "postgres:16", ImageID: "sha256:pg",
		Env: map[string]string{"POSTGRES_USER": "app", "POSTGRES_PASSWORD": tricky},
		Volumes: []Volume{
			{Host: "/app/apps/db/data", Container: "/var/lib/postgresql/data"},
			{Host: "/app/apps/db/init", Container: "/docker-entrypoint-initdb.d"},
		},
	}
	pl := plan(t, p, app)
	if pl.Dump == nil || pl.Dump.Consistency != Consistent || pl.Dump.File != "postgres.sql" {
		t.Fatalf("dump %+v, warnings %v", pl.Dump, pl.Warnings)
	}
	spec := pl.Dump.Spec
	hidesSecret(t, spec, tricky, "rm -rf")
	if spec.Image != "sha256:pg" || spec.App != "db" {
		t.Errorf("image %q app %q: the dump runs the container's image in its namespace", spec.Image, spec.App)
	}
	if spec.Cmd[0] != "sh" || spec.Cmd[2] != fileScript || spec.Cmd[3] != "pg_dumpall" {
		t.Errorf("cmd %q", spec.Cmd)
	}
	if !slices.Contains(spec.Cmd, "--lock-wait-timeout=30000") || !slices.Contains(spec.Cmd, "--username=app") {
		t.Errorf("cmd %q", spec.Cmd)
	}
	if want := `*:*:*:app:p\:a\\ss"w'o$(rm -rf /)rd` + "\n"; string(spec.Stdin) != want {
		t.Errorf("pgpass %q, want %q", spec.Stdin, want)
	}
	if !slices.Contains(spec.Env, "PGPASSFILE="+secretFile) {
		t.Errorf("env %q", spec.Env)
	}
	if len(pl.Covered) != 1 || pl.Covered[0].Container != "/var/lib/postgresql/data" {
		t.Errorf("covered %+v", pl.Covered)
	}
	if len(pl.Dirs) != 1 || pl.Dirs[0].Path != "/app/apps/db/init" || pl.Dirs[0].Consistency != Live || pl.Dirs[0].Name != "files-docker-entrypoint-initdb.d" {
		t.Errorf("dirs %+v", pl.Dirs)
	}

	// The tool is verified once per image.
	plan(t, p, app)
	if n := f.ran("pg_dumpall"); n != 1 {
		t.Errorf("pg_dumpall probed %d times", n)
	}

	// Postgres 18 mounts its volume above the versioned data directory.
	app18 := app
	app18.Volumes = []Volume{{Host: "/app/apps/db/pg", Container: "/var/lib/postgresql"}}
	if pl := plan(t, p, app18); len(pl.Covered) != 1 || len(pl.Dirs) != 0 {
		t.Errorf("postgres 18: covered %+v dirs %+v", pl.Covered, pl.Dirs)
	}
	custom := app
	custom.Env = map[string]string{"POSTGRES_PASSWORD": "x", "PGDATA": "/pgdata"}
	custom.Volumes = []Volume{{Host: "/app/apps/db/pg", Container: "/pgdata"}}
	if pl := plan(t, p, custom); len(pl.Covered) != 1 {
		t.Errorf("PGDATA volume not covered: %+v", pl)
	}

	noPass := app
	noPass.Env = map[string]string{"POSTGRES_PASSWORD_FILE": "/run/secrets/pg"}
	pl = plan(t, p, noPass)
	if pl.Dump != nil || !warned(pl, WarnCredentials) || len(pl.Dirs) != 2 {
		t.Errorf("no password: dump %+v warnings %v dirs %d", pl.Dump, pl.Warnings, len(pl.Dirs))
	}

	trust := app
	trust.Env = map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}
	if pl := plan(t, p, trust); pl.Dump == nil || pl.Dump.Spec.Cmd[0] != "pg_dumpall" || pl.Dump.Spec.Stdin != nil {
		t.Errorf("trust: %+v", pl.Dump)
	}

	bitnami := app
	bitnami.Image = "bitnami/postgresql:16"
	bitnami.Env = map[string]string{"POSTGRESQL_USERNAME": "blog", "POSTGRESQL_PASSWORD": "pw", "POSTGRESQL_DATABASE": "blogdb"}
	if pl := plan(t, p, bitnami); pl.Dump == nil || pl.Dump.Spec.Cmd[3] != "pg_dump" || !slices.Contains(pl.Dump.Spec.Cmd, "--dbname=blogdb") {
		t.Errorf("bitnami app user: %+v", pl.Dump)
	}

	missing := newPlanner(t, &fakeProber{answer: has()}, 8<<30)
	pl = plan(t, missing, app)
	if pl.Dump != nil || !warned(pl, WarnToolMissing) || len(pl.Covered) != 0 {
		t.Errorf("tool missing: %+v", pl)
	}
}

func TestMySQL(t *testing.T) {
	var query string
	f := &fakeProber{answer: func(tool string, args []string, stdin string) (string, error) {
		switch tool {
		case "mysqldump":
			return "mysqldump  Ver 8.4.3 for Linux (MySQL Community Server - GPL)", nil
		case "mysql":
			query = args[len(args)-1]
			return "", nil
		}
		return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
	}}
	p := newPlanner(t, f, 8<<30)
	app := App{
		Name: "db", Container: "db", Image: "mysql:8.4",
		Env:     map[string]string{"MYSQL_ROOT_PASSWORD": tricky},
		Volumes: []Volume{{Host: "/app/apps/db/data", Container: "/var/lib/mysql"}},
	}
	pl := plan(t, p, app)
	if pl.Engine != MySQL || pl.Dump == nil || pl.Dump.Consistency != Consistent || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	spec := pl.Dump.Spec
	hidesSecret(t, spec, tricky, "rm -rf")
	for _, a := range []string{"mysqldump", "--defaults-extra-file=" + secretFile, "--single-transaction", "--all-databases", "--set-gtid-purged=OFF", "--routines", "--no-tablespaces"} {
		if !slices.Contains(spec.Cmd, a) {
			t.Errorf("cmd %q lacks %s", spec.Cmd, a)
		}
	}
	if slices.Contains(spec.Cmd, "--lock-tables") || slices.Contains(spec.Cmd, "--lock-all-tables") {
		t.Errorf("cmd %q locks tables", spec.Cmd)
	}
	cnf := string(spec.Stdin)
	if !strings.Contains(cnf, `password="p:a\\ss\"w'o$(rm -rf /)rd"`) || !strings.Contains(cnf, `loose-init-command="SET SESSION lock_wait_timeout=30"`) {
		t.Errorf("option file:\n%s", cnf)
	}
	if !strings.Contains(query, "NOT IN ('InnoDB','ROCKSDB')") {
		t.Errorf("engine query %q", query)
	}

	maria := &fakeProber{answer: func(tool string, args []string, _ string) (string, error) {
		switch tool {
		case "mariadb-dump":
			return "mariadb-dump from 11.4.3-MariaDB", nil
		case "mariadb":
			return "shop.sessions MyISAM\nshop.cache MEMORY\n", nil
		}
		return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
	}}
	app.Image = "mariadb:11"
	app.Env = map[string]string{"MARIADB_USER": "shop", "MARIADB_PASSWORD": "pw", "MARIADB_DATABASE": "shop"}
	pl = plan(t, newPlanner(t, maria, 8<<30), app)
	if pl.Engine != MariaDB || pl.Dump == nil || pl.Dump.Consistency != Live || !warned(pl, WarnNonTransactional) {
		t.Fatalf("mariadb: %+v", pl)
	}
	if !strings.Contains(pl.Warnings[0].Message, "shop.sessions MyISAM") {
		t.Errorf("warning %q", pl.Warnings[0].Message)
	}
	cmd := pl.Dump.Spec.Cmd
	if cmd[3] != "mariadb-dump" || slices.Contains(cmd, "--set-gtid-purged=OFF") || !slices.Contains(cmd, "--databases") || slices.Contains(cmd, "--all-databases") {
		t.Errorf("mariadb app user cmd %q", cmd)
	}

	failing := &fakeProber{answer: func(tool string, args []string, _ string) (string, error) {
		if tool == "mysqldump" {
			return "mysqldump Ver 8", nil
		}
		if tool == "mysql" {
			return "", &CommandError{Msg: "Access denied for user 'root' using password " + tricky}
		}
		return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
	}}
	app.Env = map[string]string{"MYSQL_ROOT_PASSWORD": tricky}
	pl = plan(t, newPlanner(t, failing, 8<<30), app)
	if pl.Dump != nil || !warned(pl, WarnProbe) || len(pl.Dirs) != 1 {
		t.Fatalf("probe failure: %+v", pl)
	}
	if strings.Contains(pl.Warnings[0].Message, tricky) {
		t.Errorf("warning carries the password: %q", pl.Warnings[0].Message)
	}

	app.Env = map[string]string{"MYSQL_RANDOM_ROOT_PASSWORD": "yes"}
	if pl := plan(t, p, app); pl.Dump != nil || !warned(pl, WarnCredentials) {
		t.Errorf("random root password: %+v", pl)
	}
}

func redisInfo(used, rss int64) string {
	return fmt.Sprintf("# Memory\r\nused_memory:%d\r\nused_memory_human:1M\r\nused_memory_rss:%d\r\n", used, rss)
}

func TestRedis(t *testing.T) {
	var stdin string
	f := &fakeProber{answer: func(tool string, args []string, in string) (string, error) {
		if tool != "redis-cli" {
			return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
		}
		if slices.Contains(args, "INFO") {
			stdin = in
			return redisInfo(1<<30, 1<<30+100), nil
		}
		return "redis-cli 7.4.0", nil
	}}
	app := App{
		Name: "cache", Container: "cache", Image: "redis:7",
		Cmd:     []string{"redis-server", "--requirepass", tricky, "--port", "6380"},
		Volumes: []Volume{{Host: "/app/apps/cache/data", Container: "/data"}},
	}
	pl := plan(t, newPlanner(t, f, 4<<30), app)
	if pl.Dump == nil || pl.Dump.Consistency != Consistent || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	spec := pl.Dump.Spec
	hidesSecret(t, spec, tricky, "rm -rf")
	if spec.Cmd[2] != redisScript || !slices.Contains(spec.Cmd, "--rdb") || !slices.Contains(spec.Cmd, "6380") {
		t.Errorf("cmd %q", spec.Cmd)
	}
	if string(spec.Stdin) != tricky+"\n" || stdin != tricky+"\n" {
		t.Errorf("stdin %q / probe %q", spec.Stdin, stdin)
	}

	// Not enough room on the host for the fork's worst case.
	pl = plan(t, newPlanner(t, f, 1<<30+100<<20), app)
	if pl.Dump != nil || !warned(pl, WarnMemory) || len(pl.Dirs) != 1 || pl.Dirs[0].Path != "/app/apps/cache/data" {
		t.Fatalf("low memory: %+v", pl)
	}
	// Or inside the container's own limit, where the fork is charged.
	limited := app
	limited.MemoryLimit = 3 << 29
	if pl := plan(t, newPlanner(t, f, 64<<30), limited); pl.Dump != nil || !warned(pl, WarnMemory) {
		t.Fatalf("container limit: %+v", pl)
	}

	noInfo := &fakeProber{answer: func(tool string, args []string, _ string) (string, error) {
		if slices.Contains(args, "INFO") {
			return "NOAUTH Authentication required.", nil
		}
		return "redis-cli 7", nil
	}}
	app.Cmd = nil
	if pl := plan(t, newPlanner(t, noInfo, 8<<30), app); pl.Dump != nil || !warned(pl, WarnProbe) {
		t.Fatalf("no info: %+v", pl)
	}
}

func TestMongo(t *testing.T) {
	setName := ""
	f := &fakeProber{answer: func(tool string, args []string, _ string) (string, error) {
		switch tool {
		case "mongodump", "mongosh":
			if slices.Contains(args, "--eval") {
				return "Warning: Could not access file: EACCES\n\nodac-set:" + setName + "\n", nil
			}
			return tool + " 2.0", nil
		}
		return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
	}}
	app := App{
		Name: "m", Container: "m", Image: "mongo:7",
		Env:     map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": tricky},
		Volumes: []Volume{{Host: "/app/apps/m/db", Container: "/data/db"}},
	}
	pl := plan(t, newPlanner(t, f, 8<<30), app)
	if pl.Dump == nil || pl.Dump.Consistency != Live || !warned(pl, WarnStandalone) || slices.Contains(pl.Dump.Spec.Cmd, "--oplog") {
		t.Fatalf("standalone: %+v", pl)
	}
	hidesSecret(t, pl.Dump.Spec, tricky, "rm -rf")
	if want := `password: "p:a\\ss\"w'o$(rm -rf /)rd"` + "\n"; string(pl.Dump.Spec.Stdin) != want {
		t.Errorf("config %q, want %q", pl.Dump.Spec.Stdin, want)
	}
	setName = "rs0"
	pl = plan(t, newPlanner(t, f, 8<<30), app)
	if pl.Dump.Consistency != Consistent || !slices.Contains(pl.Dump.Spec.Cmd, "--oplog") || warned(pl, WarnStandalone) {
		t.Fatalf("replica set: %+v", pl)
	}
}

func TestClickHouse(t *testing.T) {
	listing := strings.Join([]string{
		`{"k":"disk","name":"default","path":"/var/lib/clickhouse/","type":"Local"}`,
		`{"k":"db","name":"default","engine":"Atomic"}`,
		`{"k":"db","name":"system","engine":"Atomic"}`,
		`{"k":"db","name":"an` + "`" + `alytics","engine":"Atomic"}`,
		`{"k":"db","name":"remote","engine":"MySQL"}`,
		`{"k":"table","db":"an` + "`" + `alytics","name":"events","engine":"ReplacingMergeTree"}`,
		`{"k":"table","db":"an` + "`" + `alytics","name":"daily","engine":"MaterializedView"}`,
		`{"k":"table","db":"an` + "`" + `alytics","name":".inner_id.1234","engine":"SummingMergeTree"}`,
		`{"k":"table","db":"default","name":"log","engine":"Log"}`,
		`{"k":"table","db":"default","name":"queue","engine":"Kafka"}`,
		`{"k":"table","db":"system","name":"query_log","engine":"MergeTree"}`,
		`{"k":"table","db":"remote","name":"users","engine":"MySQL"}`,
	}, "\n")
	var ran []string
	f := &fakeProber{answer: func(tool string, args []string, stdin string) (string, error) {
		if tool != "clickhouse-client" {
			return "", fmt.Errorf("%w: %s", ErrToolMissing, tool)
		}
		if args[0] == "--version" {
			return "ClickHouse client version 24.8", nil
		}
		q := args[len(args)-1]
		ran = append(ran, q)
		if strings.Contains(q, "system.disks") {
			return listing, nil
		}
		return "", nil
	}}
	var removed []string
	p, _ := NewPlanner(Config{Prober: f, AvailableMemory: func() (int64, error) { return 8 << 30, nil },
		RemoveAll: func(s string) error { removed = append(removed, s); return nil }})
	app := App{
		Name: "ch", Container: "ch", Image: "clickhouse/clickhouse-server:24.8",
		Env: map[string]string{"CLICKHOUSE_PASSWORD": "a<b&c"},
		Volumes: []Volume{
			{Host: "/app/apps/ch/data", Container: "/var/lib/clickhouse"},
			{Host: "/app/apps/ch/conf", Container: "/etc/clickhouse-server/config.d"},
		},
	}
	pl, err := p.Plan(context.Background(), app, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Dump == nil || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	if string(pl.Dump.Spec.Stdin) != "<config><user>default</user><password>a&lt;b&amp;c</password></config>\n" {
		t.Errorf("client config %q", pl.Dump.Spec.Stdin)
	}
	hidesSecret(t, pl.Dump.Spec, "a<b&c")
	var dirs []string
	for _, d := range pl.Dirs {
		dirs = append(dirs, d.Name+"="+d.Path+"/"+d.Consistency)
	}
	want := []string{
		"clickhouse-parts=/app/apps/ch/data/shadow/odac_run_1/" + Consistent,
		"clickhouse-access=/app/apps/ch/data/access/" + Live,
		"files-etc-clickhouse-server-config.d=/app/apps/ch/conf/" + Live,
	}
	if !slices.Equal(dirs, want) {
		t.Errorf("dirs %q\nwant %q", dirs, want)
	}

	script := pl.Dump.Spec.Cmd[len(pl.Dump.Spec.Cmd)-1]
	for _, s := range []string{
		"SHOW CREATE DATABASE `an\\`alytics`",
		"SHOW CREATE TABLE `an\\`alytics`.`events`",
		"SHOW CREATE TABLE `default`.`queue`",
		"SELECT * FROM `default`.`log` FORMAT SQLInsert SETTINGS output_format_sql_insert_table_name = '`default`.`log`'",
	} {
		if !strings.Contains(script, s) {
			t.Errorf("script lacks %q:\n%s", s, script)
		}
	}
	// A remote database engine keeps its definition but names no local
	// table; a Kafka table is never read.
	for _, s := range []string{"CREATE DATABASE `default`", "query_log", "`remote`.`users`", "FROM `default`.`queue`", ".inner_id"} {
		if strings.Contains(script, s) {
			t.Errorf("script has %q:\n%s", s, script)
		}
	}
	if strings.Index(script, "`events`") > strings.Index(script, "`daily`") {
		t.Errorf("a view is created before its table:\n%s", script)
	}
	if !slices.Contains(pl.Dump.Spec.Cmd, "--show_table_uuid_in_table_create_query_if_not_nil=1") {
		t.Errorf("cmd %q", pl.Dump.Spec.Cmd)
	}

	ran = nil
	if err := pl.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || !strings.Contains(ran[0], "ALTER TABLE `an\\`alytics`.`events` FREEZE WITH NAME 'odac_run_1'") || !strings.Contains(ran[0], "`.inner_id.1234` FREEZE") || strings.Contains(ran[0], "`log`") {
		t.Errorf("freeze %q", ran)
	}
	ran = nil
	if err := pl.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || !strings.Contains(ran[0], "UNFREEZE WITH NAME 'odac_run_1'") || !slices.Equal(removed, []string{"/app/apps/ch/data/shadow/odac_run_1"}) {
		t.Errorf("unfreeze %q removed %q", ran, removed)
	}

	// Data the helper cannot reach is not frozen for nothing.
	app.Volumes = app.Volumes[1:]
	pl, err = p.Plan(context.Background(), app, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Dump != nil || !warned(pl, WarnNotOnVolume) || len(pl.Dirs) != 1 {
		t.Fatalf("not on a volume: %+v", pl)
	}
}

func TestHook(t *testing.T) {
	f := &fakeProber{answer: has()}
	app := App{
		Name: "vault", Container: "vault", Image: "example/vault",
		Env:  map[string]string{"TOKEN": "t"},
		Spec: &Spec{Hook: &Hook{Dump: []string{"/bin/export"}, File: "vault.snap", Consistency: Consistent, Covers: []string{"/vault/data"}}},
		Volumes: []Volume{
			{Host: "/app/apps/vault/data", Container: "/vault/data"},
			{Host: "/app/apps/vault/logs", Container: "/vault/logs"},
		},
	}
	pl := plan(t, newPlanner(t, f, 8<<30), app)
	if pl.Dump == nil || pl.Dump.File != "vault.snap" || pl.Dump.Consistency != Consistent || pl.Dump.Spec.Cmd[0] != "/bin/export" {
		t.Fatalf("hook: %+v", pl.Dump)
	}
	if !slices.Equal(pl.Dump.Spec.Env, []string{"TOKEN=t"}) || len(pl.Covered) != 1 || len(pl.Dirs) != 1 {
		t.Errorf("hook env %q covered %+v dirs %+v", pl.Dump.Spec.Env, pl.Covered, pl.Dirs)
	}
	if len(f.calls) != 0 {
		t.Errorf("a hook was probed: %+v", f.calls)
	}

	declared := App{Name: "x", Container: "x", Image: "example/x", Spec: &Spec{Engine: Postgres}, Env: map[string]string{"POSTGRES_PASSWORD": "pw"}}
	if pl := plan(t, newPlanner(t, &fakeProber{answer: has("pg_dumpall")}, 8<<30), declared); pl.Engine != Postgres || pl.Dump == nil {
		t.Errorf("declared engine ignored: %+v", pl)
	}
	files := App{Name: "x", Container: "x", Image: "postgres:16", Spec: &Spec{Engine: None}, Volumes: []Volume{{Host: "/h", Container: "/var/lib/postgresql/data"}}}
	if pl := plan(t, newPlanner(t, &fakeProber{answer: has()}, 8<<30), files); pl.Dump != nil || len(pl.Dirs) != 1 {
		t.Errorf("declared none: %+v", pl)
	}
}

func TestPlanErrors(t *testing.T) {
	down := errors.New("Cannot connect to the Docker daemon")
	p := newPlanner(t, &fakeProber{answer: func(string, []string, string) (string, error) { return "", down }}, 8<<30)
	app := App{Name: "db", Container: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "pw"}}
	if _, err := p.Plan(context.Background(), app, "r1"); !errors.Is(err, down) {
		t.Fatalf("Docker down: %v; a failure to ask must not read as a missing tool", err)
	}
	if _, err := p.Plan(context.Background(), app, "bad run!"); err == nil {
		t.Error("invalid run accepted")
	}
	if _, err := p.Plan(context.Background(), App{Name: "x"}, "r1"); err == nil {
		t.Error("an app without a container accepted")
	}
	if _, err := NewPlanner(Config{}); err == nil {
		t.Error("planner without a prober")
	}
	newline := App{Name: "db", Container: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "a\nb"}}
	if pl := plan(t, newPlanner(t, &fakeProber{answer: has("pg_dumpall")}, 8<<30), newline); pl.Dump != nil || !warned(pl, WarnCredentials) {
		t.Errorf("a password no client file can hold: %+v", pl)
	}
}

func TestVolumes(t *testing.T) {
	vols := dataVolumes([]Volume{
		{Host: "/app/apps/a", Container: "/srv"},
		{Host: "/app/apps/a/uploads", Container: "/srv/uploads"},
		{Host: "/app/apps/a", Container: "/again"},
		{Host: "/app/apps/b", Container: "/b"},
		{Host: "", Container: "/x"},
	})
	if len(vols) != 2 || vols[0].Container != "/srv" || vols[1].Container != "/b" {
		t.Fatalf("volumes %+v", vols)
	}
	if h, _, ok := hostPath([]Volume{{Host: "/h/a", Container: "/var/lib"}, {Host: "/h/b", Container: "/var/lib/ch"}}, "/var/lib/ch/shadow/x"); !ok || h != "/h/b/shadow/x" {
		t.Fatalf("hostPath %q %v", h, ok)
	}
	names := map[string]bool{}
	for _, c := range []string{"/var/lib/x y", "/var/lib/x-y", "/", "/data", "/" + strings.Repeat("long/", 40)} {
		n := dirName(c)
		if n != dirName(c) || names[n] || len(n) > 128 || !filePattern.MatchString(n) {
			t.Errorf("dirName(%q) = %q", c, n)
		}
		names[n] = true
	}
}
