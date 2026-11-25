// Package backup_test runs the consistency planner against real databases
// in a Docker daemon: each engine's dump is taken from a live server the
// way a backup takes it (dump container, credentials on stdin), replayed
// into a fresh server, and compared.
//
// It is off unless ODAC_DB_TESTS=1, because it needs a daemon and the
// images below, which it does not pull: a missing image would make the run
// wait on a pull. Pull them first:
//
//	docker pull postgres:16-alpine mysql:8.4 mariadb:11.4 redis:7.4-alpine mongo:7.0 clickhouse/clickhouse-server:24.8
//	ODAC_DB_TESTS=1 go test -timeout 15m ./test/backup/
package backup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
	"odac/internal/docker"
)

const testsEnv = "ODAC_DB_TESTS"

type env struct {
	client *docker.Client
	dumper runner.Docker
	limits docker.Limits
}

func setup(t *testing.T, images ...string) *env {
	t.Helper()
	if os.Getenv(testsEnv) != "1" {
		t.Skipf("set %s=1 to run against real databases", testsEnv)
	}
	for _, img := range images {
		if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
			t.Fatalf("image %s is not local; pull it first", img)
		}
	}
	c, err := docker.Connect(docker.Options{})
	if err != nil || !c.Available() {
		t.Fatalf("Docker: %v", err)
	}
	_, dump := runner.DefaultLimits(4<<30, 2)
	return &env{client: c, dumper: runner.FromClient(c), limits: dump}
}

func (e *env) planner(t *testing.T, cfg consistency.Config) *consistency.Planner {
	t.Helper()
	cfg.Prober = consistency.NewProber(e.dumper, e.limits)
	if cfg.AvailableMemory == nil {
		cfg.AvailableMemory = func() (int64, error) { return 8 << 30, nil }
	}
	if cfg.RemoveAll == nil {
		cfg.RemoveAll = func(string) error { return nil }
	}
	p, err := consistency.NewPlanner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func dockerCmd(t *testing.T, stdin io.Reader, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// start runs a database container and returns its name.
func start(t *testing.T, image string, envs []string, cmd ...string) string {
	t.Helper()
	name := fmt.Sprintf("odac-dbtest-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--name", name}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	dockerCmd(t, nil, append(args, cmd...)...)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", name).Run() })
	return name
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

func execOK(container string, args ...string) bool {
	return exec.Command("docker", append([]string{"exec", container}, args...)...).Run() == nil
}

// app describes a running container the way the server will: the image it
// runs and its environment as the engine reports them.
func app(t *testing.T, container, image string, cmd []string, vols ...consistency.Volume) consistency.App {
	t.Helper()
	var info struct {
		Image  string
		Config struct{ Env []string }
	}
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(dockerCmd(t, nil, "inspect", container)), &list); err != nil || len(list) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	if err := json.Unmarshal(list[0], &info); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return consistency.App{Name: container, Container: container, Image: image, ImageID: info.Image, Cmd: cmd, Env: env, Volumes: vols}
}

func (e *env) dump(t *testing.T, d *consistency.Dump) ([]byte, error) {
	t.Helper()
	spec := d.Spec
	spec.Limits = e.limits
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rc, err := e.dumper.StartDump(ctx, spec)
	if err != nil {
		t.Fatalf("start dump: %v", err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func mustPlan(t *testing.T, p *consistency.Planner, a consistency.App) *consistency.Plan {
	t.Helper()
	pl, err := p.Plan(context.Background(), a, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func warned(pl *consistency.Plan, code string) bool {
	for _, w := range pl.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// A password every client file format has to escape. MySQL's image cannot
// start with a root password holding a quote or a backslash (its
// entrypoint writes it into SQL and an option file unescaped), so its test
// keeps the rest.
const (
	password      = `p:a\ss"w'o rd#1`
	mysqlPassword = `p:a ss#w=rd;1`
)

func TestPostgres(t *testing.T) {
	t.Parallel()
	e := setup(t, "postgres:16-alpine")
	db := start(t, "postgres:16-alpine", []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=" + password})
	waitFor(t, "postgres", func() bool {
		return execOK(db, "psql", "-U", "app", "-h", "127.0.0.1", "-c", "SELECT 1")
	})
	dockerCmd(t, nil, "exec", "-e", "PGPASSWORD="+password, db, "psql", "-U", "app", "-h", "127.0.0.1", "-c",
		"CREATE TABLE t (id int primary key, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1, 5000) g; CREATE ROLE reader LOGIN PASSWORD 'r';")

	p := e.planner(t, consistency.Config{LockWait: 2 * time.Second})
	pl := mustPlan(t, p, app(t, db, "postgres:16-alpine", nil, consistency.Volume{Host: "/srv/db", Container: "/var/lib/postgresql/data"}))
	if pl.Engine != consistency.Postgres || pl.Dump == nil || pl.Dump.Consistency != consistency.Consistent || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if !strings.Contains(string(out), "CREATE ROLE reader") || !strings.Contains(string(out), "COPY public.t") {
		t.Fatalf("dump lacks roles or data:\n%.500s", out)
	}

	fresh := start(t, "postgres:16-alpine", []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=x"})
	waitFor(t, "fresh postgres", func() bool { return execOK(fresh, "psql", "-U", "app", "-h", "127.0.0.1", "-c", "SELECT 1") })
	cmd := exec.Command("docker", "exec", "-i", fresh, "psql", "-U", "app", "-d", "postgres", "-q", "-v", "ON_ERROR_STOP=0")
	cmd.Stdin = strings.NewReader(string(out))
	cmd.Run()
	if got := strings.TrimSpace(dockerCmd(t, nil, "exec", fresh, "psql", "-U", "app", "-tA", "-c", "SELECT count(*), sum(length(v)) FROM t")); got != "5000|160000" {
		t.Fatalf("restored %q", got)
	}

	// A table a migration holds fails the dump after the lock wait instead
	// of queueing it.
	hold := exec.Command("docker", "exec", "-e", "PGPASSWORD="+password, db, "psql", "-U", "app", "-h", "127.0.0.1", "-c",
		"BEGIN; LOCK TABLE t IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(30);")
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { hold.Process.Kill(); hold.Wait() }()
	waitFor(t, "the lock", func() bool {
		out, _ := exec.Command("docker", "exec", db, "psql", "-U", "app", "-tA", "-c", "SELECT count(*) FROM pg_locks WHERE mode = 'AccessExclusiveLock' AND relation = 't'::regclass").Output()
		return strings.TrimSpace(string(out)) == "1"
	})
	began := time.Now()
	if _, err := e.dump(t, pl.Dump); err == nil || !strings.Contains(err.Error(), "LOCK TABLE public.t") {
		t.Fatalf("dump under a held lock: %v", err)
	}
	if time.Since(began) > 20*time.Second {
		t.Fatalf("the dump waited %s for the lock", time.Since(began))
	}
}

func mysqlReady(name, client string) func() bool {
	return func() bool {
		return execOK(name, "sh", "-c", client+` -uroot -p"$MYSQL_ROOT_PASSWORD$MARIADB_ROOT_PASSWORD" -h127.0.0.1 -e 'SELECT 1'`)
	}
}

// mysqlExec returns the client's stdout only: MySQL's client warns on
// stderr about the password on its command line.
func mysqlExec(t *testing.T, name, client, sql string) string {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", name, "sh", "-c", client+` -uroot -p"$MYSQL_ROOT_PASSWORD$MARIADB_ROOT_PASSWORD" -h127.0.0.1 -N -B`)
	cmd.Stdin = strings.NewReader(sql)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", client, err, stderr.String())
	}
	return string(out)
}

func testMySQL(t *testing.T, image, rootVar, client string, engine consistency.Engine, mysqlPassword string) {
	e := setup(t, image)
	db := start(t, image, []string{rootVar + "=" + mysqlPassword})
	waitFor(t, image, mysqlReady(db, client))
	mysqlExec(t, db, client, "CREATE DATABASE shop; CREATE TABLE shop.t (id int primary key, v varchar(64)) ENGINE=InnoDB;"+
		" INSERT INTO shop.t VALUES (1, 'a'), (2, 'b'), (3, 'it''s');")

	p := e.planner(t, consistency.Config{LockWait: 2 * time.Second})
	a := app(t, db, image, nil, consistency.Volume{Host: "/srv/db", Container: "/var/lib/mysql"})
	pl := mustPlan(t, p, a)
	if pl.Engine != engine || pl.Dump == nil || pl.Dump.Consistency != consistency.Consistent || len(pl.Warnings) != 0 {
		t.Fatalf("plan %+v", pl)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	fresh := start(t, image, []string{rootVar + "=" + mysqlPassword})
	waitFor(t, "fresh "+image, mysqlReady(fresh, client))
	mysqlExec(t, fresh, client, string(out))
	if got := strings.TrimSpace(mysqlExec(t, fresh, client, "SELECT group_concat(v ORDER BY id) FROM shop.t")); got != "a,b,it's" {
		t.Fatalf("restored %q", got)
	}

	// MyISAM is read without the lock that would make it consistent.
	mysqlExec(t, db, client, "CREATE TABLE shop.legacy (id int) ENGINE=MyISAM;")
	pl = mustPlan(t, p, a)
	if !warned(pl, consistency.WarnNonTransactional) || pl.Dump.Consistency != consistency.Live {
		t.Fatalf("MyISAM: %+v", pl)
	}
	mysqlExec(t, db, client, "DROP TABLE shop.legacy;")

	// MySQL's dump reads the lock wait from the option file; MariaDB's does
	// not (see clientFile), so only MySQL is held to it.
	if engine != consistency.MySQL {
		return
	}
	hold := exec.Command("docker", "exec", "-i", db, "sh", "-c", client+` -uroot -p"$MYSQL_ROOT_PASSWORD" -h127.0.0.1`)
	hold.Stdin = strings.NewReader("LOCK TABLES shop.t WRITE; SELECT SLEEP(30);")
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { hold.Process.Kill(); hold.Wait() }()
	time.Sleep(2 * time.Second)
	began := time.Now()
	if _, err := e.dump(t, pl.Dump); err == nil || !strings.Contains(err.Error(), "Lock wait timeout") {
		t.Fatalf("dump under a held lock: %v", err)
	}
	if time.Since(began) > 20*time.Second {
		t.Fatalf("the dump waited %s for the lock", time.Since(began))
	}
}

func TestMySQL(t *testing.T) {
	t.Parallel()
	testMySQL(t, "mysql:8.4", "MYSQL_ROOT_PASSWORD", "mysql", consistency.MySQL, mysqlPassword)
}

func TestMariaDB(t *testing.T) {
	t.Parallel()
	testMySQL(t, "mariadb:11.4", "MARIADB_ROOT_PASSWORD", "mariadb", consistency.MariaDB, password)
}

func TestRedis(t *testing.T) {
	t.Parallel()
	e := setup(t, "redis:7.4-alpine")
	cmd := []string{"redis-server", "--requirepass", password}
	db := start(t, "redis:7.4-alpine", nil, cmd...)
	waitFor(t, "redis", func() bool { return execOK(db, "redis-cli", "-a", password, "--no-auth-warning", "PING") })
	dockerCmd(t, nil, "exec", db, "redis-cli", "-a", password, "--no-auth-warning", "SET", "greeting", "hello")
	dockerCmd(t, nil, "exec", db, "redis-cli", "-a", password, "--no-auth-warning", "RPUSH", "list", "a", "b", "c")

	a := app(t, db, "redis:7.4-alpine", cmd, consistency.Volume{Host: "/srv/cache", Container: "/data"})
	pl := mustPlan(t, e.planner(t, consistency.Config{}), a)
	if pl.Dump == nil || pl.Dump.Consistency != consistency.Consistent || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if !strings.HasPrefix(string(out), "REDIS") {
		t.Fatalf("not an RDB: %q", out[:min(len(out), 32)])
	}

	fresh := start(t, "redis:7.4-alpine", nil, "sleep", "600")
	f, err := os.CreateTemp(t.TempDir(), "dump-*.rdb")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(out)
	f.Close()
	dockerCmd(t, nil, "cp", f.Name(), fresh+":/data/dump.rdb")
	dockerCmd(t, nil, "exec", "-d", fresh, "redis-server", "--dir", "/data", "--dbfilename", "dump.rdb")
	waitFor(t, "restored redis", func() bool { return execOK(fresh, "redis-cli", "PING") })
	if got := strings.TrimSpace(dockerCmd(t, nil, "exec", fresh, "redis-cli", "GET", "greeting")); got != "hello" {
		t.Fatalf("restored %q", got)
	}

	// No room for the fork's copy-on-write pages: the last save on disk
	// is read as files instead.
	tight := e.planner(t, consistency.Config{AvailableMemory: func() (int64, error) { return 1 << 20, nil }})
	if pl := mustPlan(t, tight, a); pl.Dump != nil || !warned(pl, consistency.WarnMemory) || len(pl.Dirs) != 1 {
		t.Fatalf("low memory: %+v", pl)
	}
}

func TestMongo(t *testing.T) {
	t.Parallel()
	e := setup(t, "mongo:7.0")
	db := start(t, "mongo:7.0", []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=" + password})
	shell := func(c, js string) bool {
		return execOK(c, "mongosh", "--quiet", "-u", "root", "-p", password, "--authenticationDatabase", "admin", "--eval", js)
	}
	waitFor(t, "mongo", func() bool { return shell(db, "db.runCommand({ping: 1})") })
	if !shell(db, `db.getSiblingDB("shop").items.insertMany([{n: 1}, {n: 2}, {n: 3}])`) {
		t.Fatal("seed")
	}

	pl := mustPlan(t, e.planner(t, consistency.Config{}), app(t, db, "mongo:7.0", nil, consistency.Volume{Host: "/srv/m", Container: "/data/db"}))
	if pl.Dump == nil || pl.Dump.Consistency != consistency.Live || !warned(pl, consistency.WarnStandalone) || len(pl.Covered) != 1 {
		t.Fatalf("plan %+v", pl)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	fresh := start(t, "mongo:7.0", nil)
	waitFor(t, "fresh mongo", func() bool { return execOK(fresh, "mongosh", "--quiet", "--eval", "db.runCommand({ping: 1})") })
	dockerCmd(t, strings.NewReader(string(out)), "exec", "-i", fresh, "mongorestore", "--archive", "--quiet", "--nsExclude=admin.*")
	got := dockerCmd(t, nil, "exec", fresh, "mongosh", "--quiet", "--eval", `db.getSiblingDB("shop").items.countDocuments()`)
	if strings.TrimSpace(got) != "3" {
		t.Fatalf("restored %q", got)
	}
}

func TestClickHouse(t *testing.T) {
	t.Parallel()
	const image = "clickhouse/clickhouse-server:24.8"
	e := setup(t, image)
	db := start(t, image, []string{"CLICKHOUSE_PASSWORD=" + password})
	query := func(c, pw, q string) (string, error) {
		out, err := exec.Command("docker", "exec", c, "clickhouse-client", "--password", pw, "--multiquery", "--query", q).CombinedOutput()
		return string(out), err
	}
	waitFor(t, "clickhouse", func() bool { _, err := query(db, password, "SELECT 1"); return err == nil })
	if out, err := query(db, password, "CREATE DATABASE shop;"+
		" CREATE TABLE shop.events (id UInt64, s String) ENGINE = MergeTree ORDER BY id;"+
		" INSERT INTO shop.events SELECT number, toString(number) FROM numbers(1000);"+
		" CREATE TABLE shop.daily_src (d Date, n UInt64) ENGINE = MergeTree ORDER BY d;"+
		" CREATE MATERIALIZED VIEW shop.daily ENGINE = SummingMergeTree ORDER BY d AS SELECT d, sum(n) AS n FROM shop.daily_src GROUP BY d;"+
		" INSERT INTO shop.daily_src VALUES ('2026-01-01', 5);"+
		" CREATE TABLE shop.notes (id UInt64, `we\\`ird` String) ENGINE = Log;"+
		" INSERT INTO shop.notes VALUES (1, 'it''s'), (2, 'b');"+
		" CREATE TABLE shop.empty (id UInt64) ENGINE = TinyLog;"); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	a := app(t, db, image, nil, consistency.Volume{Host: "/srv/ch", Container: "/var/lib/clickhouse"})
	var removed []string
	p := e.planner(t, consistency.Config{RemoveAll: func(s string) error { removed = append(removed, s); return nil }})
	pl := mustPlan(t, p, a)
	if pl.Dump == nil || len(pl.Dirs) < 1 || pl.Dirs[0].Path != "/srv/ch/shadow/odac_run_1" || len(pl.Warnings) != 0 {
		t.Fatalf("plan %+v", pl)
	}
	if err := pl.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts := dockerCmd(t, nil, "exec", db, "find", "/var/lib/clickhouse/shadow/odac_run_1", "-name", "columns.txt")
	if n := strings.Count(parts, "columns.txt"); n < 3 {
		t.Fatalf("frozen parts: %d\n%s", n, parts)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if err := pl.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := dockerCmd(t, nil, "exec", db, "find", "/var/lib/clickhouse/shadow", "-name", "columns.txt"); strings.TrimSpace(left) != "" {
		t.Fatalf("unfreeze left parts:\n%s", left)
	}
	if len(removed) != 1 || removed[0] != "/srv/ch/shadow/odac_run_1" {
		t.Fatalf("removed %q", removed)
	}

	fresh := start(t, image, []string{"CLICKHOUSE_PASSWORD=x"})
	waitFor(t, "fresh clickhouse", func() bool { _, err := query(fresh, "x", "SELECT 1"); return err == nil })
	if res, err := query(fresh, "x", string(out)); err != nil {
		t.Fatalf("replay: %v\n%s\n--- script\n%s", err, res, out)
	}
	got, err := query(fresh, "x", "SELECT count() FROM shop.notes; SELECT `we\\`ird` FROM shop.notes WHERE id = 1 FORMAT TSVRaw; SELECT count() FROM shop.events; SELECT count() FROM shop.empty; EXISTS TABLE shop.daily")
	if err != nil {
		t.Fatalf("query restored: %v\n%s", err, got)
	}
	// MergeTree data comes back from the frozen parts, which a restore
	// attaches; the replayed script carries definitions and Log rows.
	if want := "2\nit's\n0\n0\n1\n"; got != want {
		t.Fatalf("restored %q, want %q", got, want)
	}
}
