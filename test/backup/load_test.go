package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
)

// load loads a dump into a running database the way a restore does: the
// planner's load in a dump container, credentials and stream on stdin.
func (e *env) load(t *testing.T, p *consistency.Planner, a consistency.App, name, level string, dump []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	l, err := p.Load(ctx, a, name, level)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if err := l.WaitReady(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	r, err := runner.New(runner.Config{Docker: e.dumper, Binary: "/unused", DumpRate: 1 << 30, DumpLimits: e.limits})
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := r.Load(ctx, *l.Spec, bytes.NewReader(dump), nil)
	if err == nil && l.Judge != nil {
		err = l.Judge(stderr)
	}
	if err != nil {
		t.Fatalf("load: %v\n%s", err, stderr)
	}
	if err := l.Finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestLoadPostgres(t *testing.T) {
	t.Parallel()
	e := setup(t, "postgres:16-alpine")
	envs := []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=" + password}
	db := start(t, "postgres:16-alpine", envs)
	ready := func(c string) func() bool {
		return func() bool { return execOK(c, "psql", "-U", "app", "-h", "127.0.0.1", "-c", "SELECT 1") }
	}
	waitFor(t, "postgres", ready(db))
	dockerCmd(t, nil, "exec", "-e", "PGPASSWORD="+password, db, "psql", "-U", "app", "-h", "127.0.0.1", "-c",
		"CREATE TABLE t (id int primary key, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1, 5000) g; CREATE ROLE reader LOGIN PASSWORD 'r';")
	dockerCmd(t, nil, "exec", "-e", "PGPASSWORD="+password, db, "psql", "-U", "app", "-h", "127.0.0.1", "-c", "CREATE DATABASE shop")
	p := e.planner(t, consistency.Config{})
	pl := mustPlan(t, p, app(t, db, "postgres:16-alpine", nil))
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh server from the same environment, as the restore's emptied
	// volume gives it, takes the dump back.
	fresh := start(t, "postgres:16-alpine", envs)
	e.load(t, p, app(t, fresh, "postgres:16-alpine", nil), "postgres", consistency.Consistent, out)
	if got := strings.TrimSpace(dockerCmd(t, nil, "exec", fresh, "psql", "-U", "app", "-tA", "-c", "SELECT count(*), sum(length(v)) FROM t")); got != "5000|160000" {
		t.Fatalf("restored %q", got)
	}
	if got := strings.TrimSpace(dockerCmd(t, nil, "exec", fresh, "psql", "-U", "app", "-tA", "-c", "SELECT count(*) FROM pg_roles WHERE rolname = 'reader'")); got != "1" {
		t.Fatalf("role %q", got)
	}
	// A real error is not taken for the expected ones.
	if err := func() error {
		l, err := p.Load(context.Background(), app(t, fresh, "postgres:16-alpine", nil), "postgres", consistency.Consistent)
		if err != nil {
			return err
		}
		r, _ := runner.New(runner.Config{Docker: e.dumper, Binary: "/unused", DumpRate: 1 << 30, DumpLimits: e.limits})
		stderr, err := r.Load(context.Background(), *l.Spec, strings.NewReader("SELECT 1;\nINSERT INTO missing VALUES (1);\nSELECT 2;\n"), nil)
		if err != nil {
			return err
		}
		return l.Judge(stderr)
	}(); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failing statement passed: %v", err)
	}
	// A stream that breaks kills the load instead of ending its input.
	l, err := p.Load(context.Background(), app(t, fresh, "postgres:16-alpine", nil), "postgres", consistency.Consistent)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := runner.New(runner.Config{Docker: e.dumper, Binary: "/unused", DumpRate: 1 << 30, DumpLimits: e.limits})
	broken := io.MultiReader(strings.NewReader("CREATE TABLE half (id int);\n"), failingReader{})
	if _, err := r.Load(context.Background(), *l.Spec, broken, nil); err == nil || !strings.Contains(err.Error(), "stream failed") {
		t.Fatalf("a broken stream: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the restore helper died") }

func testLoadMySQL(t *testing.T, image, rootVar, client, pw string) {
	e := setup(t, image)
	envs := []string{rootVar + "=" + pw}
	db := start(t, image, envs)
	waitFor(t, image, mysqlReady(db, client))
	mysqlExec(t, db, client, "CREATE DATABASE shop; CREATE TABLE shop.t (id int primary key, v varchar(64)) ENGINE=InnoDB;"+
		" INSERT INTO shop.t VALUES (1, 'a'), (2, 'b'), (3, 'it''s'); CREATE USER 'clerk'@'%' IDENTIFIED BY 'c1'; GRANT SELECT ON shop.* TO 'clerk'@'%';")
	p := e.planner(t, consistency.Config{})
	pl := mustPlan(t, p, app(t, db, image, nil))
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatal(err)
	}
	fresh := start(t, image, envs)
	e.load(t, p, app(t, fresh, image, nil), string(pl.Engine), consistency.Consistent, out)
	if got := strings.TrimSpace(mysqlExec(t, fresh, client, "SELECT group_concat(v ORDER BY id) FROM shop.t")); got != "a,b,it's" {
		t.Fatalf("restored %q", got)
	}
	// The grant tables came back and were read again.
	if !execOK(fresh, client, "-uclerk", "-pc1", "-h127.0.0.1", "-e", "SELECT count(*) FROM shop.t") {
		t.Fatal("the restored user cannot log in")
	}
}

func TestLoadMySQL(t *testing.T) {
	t.Parallel()
	testLoadMySQL(t, "mysql:8.4", "MYSQL_ROOT_PASSWORD", "mysql", mysqlPassword)
}

func TestLoadMariaDB(t *testing.T) {
	t.Parallel()
	testLoadMySQL(t, "mariadb:11.4", "MARIADB_ROOT_PASSWORD", "mariadb", password)
}

func TestLoadMongo(t *testing.T) {
	t.Parallel()
	e := setup(t, "mongo:7.0")
	envs := []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=" + password}
	db := start(t, "mongo:7.0", envs)
	shell := func(c, js string) string {
		out, _ := exec.Command("docker", "exec", c, "mongosh", "--quiet", "-u", "root", "-p", password, "--authenticationDatabase", "admin", "--eval", js).Output()
		return strings.TrimSpace(string(out))
	}
	waitFor(t, "mongo", func() bool { return shell(db, "db.runCommand({ping: 1}).ok") == "1" })
	shell(db, `db.getSiblingDB("shop").items.insertMany([{n: 1}, {n: 2}, {n: 3}]); db.getSiblingDB("shop").createUser({user: "clerk", pwd: "c1", roles: ["read"]})`)
	p := e.planner(t, consistency.Config{})
	pl := mustPlan(t, p, app(t, db, "mongo:7.0", nil))
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatal(err)
	}
	fresh := start(t, "mongo:7.0", envs)
	e.load(t, p, app(t, fresh, "mongo:7.0", nil), "mongo", pl.Dump.Consistency, out)
	if got := shell(fresh, `db.getSiblingDB("shop").items.countDocuments()`); got != "3" {
		t.Fatalf("restored %q", got)
	}
	if got := shell(fresh, `db.getSiblingDB("shop").getUser("clerk") != null`); got != "true" {
		t.Fatalf("user %q", got)
	}
}

// TestRedisLoadsItsSaveWithAOF checks the file a Redis restore places: an
// emptied /data holding only the RDB is loaded, AOF on or off.
func TestRedisLoadsItsSaveWithAOF(t *testing.T) {
	t.Parallel()
	e := setup(t, "redis:7.4-alpine")
	cmd := []string{"redis-server", "--requirepass", password, "--appendonly", "yes"}
	db := start(t, "redis:7.4-alpine", nil, cmd...)
	waitFor(t, "redis", func() bool { return execOK(db, "redis-cli", "-a", password, "--no-auth-warning", "PING") })
	dockerCmd(t, nil, "exec", db, "redis-cli", "-a", password, "--no-auth-warning", "SET", "greeting", "hello")
	pl := mustPlan(t, e.planner(t, consistency.Config{}), app(t, db, "redis:7.4-alpine", cmd, consistency.Volume{Host: "/srv/r", Container: "/data"}))
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatal(err)
	}
	l, err := consistency.Shape(app(t, db, "redis:7.4-alpine", cmd), "redis")
	if err != nil || l.File != "/data/dump.rdb" || l.AOF != "/data/appendonly.aof" {
		t.Fatalf("shape %+v, %v", l, err)
	}
	out = out[:len(out)-consistency.RDBTrailerSize(out)]
	f := filepath.Join(t.TempDir(), "dump.rdb")
	os.WriteFile(f, out, 0o644)
	name := "odac-dbtest-redis-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	dockerCmd(t, nil, append([]string{"create", "--name", name, "redis:7.4-alpine"}, cmd...)...)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", name).Run() })
	dockerCmd(t, nil, "cp", f, name+":"+l.File)
	dockerCmd(t, nil, "cp", f, name+":"+l.AOF)
	dockerCmd(t, nil, "start", name)
	deadline := time.Now().Add(time.Minute)
	for !execOK(name, "redis-cli", "-a", password, "--no-auth-warning", "PING") {
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("restored redis does not answer:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
	if got := strings.TrimSpace(dockerCmd(t, nil, "exec", name, "redis-cli", "-a", password, "--no-auth-warning", "GET", "greeting")); got != "hello" {
		t.Fatalf("restored %q", got)
	}
}

// TestLoadClickHouse replays the definitions and attaches the frozen
// parts, moved into each table's detached directory the way a restore
// moves them.
func TestLoadClickHouse(t *testing.T) {
	t.Parallel()
	const image = "clickhouse/clickhouse-server:24.8"
	e := setup(t, image)
	envs := []string{"CLICKHOUSE_PASSWORD=" + password}
	db := start(t, image, envs)
	query := func(c, q string) (string, error) {
		out, err := exec.Command("docker", "exec", c, "clickhouse-client", "--password", password, "--multiquery", "--query", q).CombinedOutput()
		return string(out), err
	}
	waitFor(t, "clickhouse", func() bool { _, err := query(db, "SELECT 1"); return err == nil })
	if out, err := query(db, "CREATE DATABASE shop; CREATE TABLE shop.events (id UInt64, s String) ENGINE = MergeTree ORDER BY id;"+
		" INSERT INTO shop.events SELECT number, toString(number) FROM numbers(1000); INSERT INTO shop.events SELECT number + 1000, 'x' FROM numbers(10);"+
		" CREATE TABLE shop.notes (id UInt64) ENGINE = Log; INSERT INTO shop.notes VALUES (1), (2);"); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	a := app(t, db, image, nil, consistency.Volume{Host: "/srv/ch", Container: "/var/lib/clickhouse"})
	p := e.planner(t, consistency.Config{})
	pl := mustPlan(t, p, a)
	if err := pl.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := e.dump(t, pl.Dump)
	if err != nil {
		t.Fatal(err)
	}
	parts := t.TempDir()
	dockerCmd(t, nil, "cp", db+":/var/lib/clickhouse/shadow/odac_run_1/.", parts)
	pl.Finish(context.Background())

	fresh := start(t, image, envs)
	fa := app(t, fresh, image, nil, consistency.Volume{Host: "/srv/ch", Container: "/var/lib/clickhouse"})
	e.load(t, p, fa, "clickhouse", consistency.Consistent, out)
	l, err := p.Load(context.Background(), fa, "clickhouse", consistency.Consistent)
	if err != nil {
		t.Fatal(err)
	}
	tables, disk, err := l.Tables(context.Background())
	if err != nil || disk != "/var/lib/clickhouse/" {
		t.Fatalf("tables %v, disk %q, %v", tables, disk, err)
	}
	attach := map[string][]string{}
	stores, _ := os.ReadDir(filepath.Join(parts, "store"))
	for _, pre := range stores {
		uuids, _ := os.ReadDir(filepath.Join(parts, "store", pre.Name()))
		for _, u := range uuids {
			ref := tables[u.Name()]
			if ref == "" {
				t.Fatalf("no table for frozen %s in %v", u.Name(), tables)
			}
			names, _ := os.ReadDir(filepath.Join(parts, "store", pre.Name(), u.Name()))
			detached := "/var/lib/clickhouse/store/" + pre.Name() + "/" + u.Name() + "/detached/"
			for _, n := range names {
				dockerCmd(t, nil, "cp", filepath.Join(parts, "store", pre.Name(), u.Name(), n.Name()), fresh+":"+detached)
				attach[ref] = append(attach[ref], n.Name())
			}
			dockerCmd(t, nil, "exec", fresh, "chown", "-R", "clickhouse:clickhouse", detached)
		}
	}
	if err := l.Attach(context.Background(), attach); err != nil {
		t.Fatal(err)
	}
	got, err := query(fresh, "SELECT count() FROM shop.events; SELECT count() FROM shop.notes")
	if err != nil || got != "1010\n2\n" {
		t.Fatalf("restored %q, %v", got, err)
	}
}
