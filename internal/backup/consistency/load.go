package consistency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"odac/internal/docker"
)

// Load is how a stored dump goes back into its database: the engine's own
// client reads it on stdin inside a dump container (the app's image, in
// its network namespace, entrypoint replaced), with the credentials ahead
// of it on the same stdin and nowhere else.
type Load struct {
	App    string
	Engine Engine
	// Fresh: the volumes holding the engine's data (DataVolumes) are
	// emptied before the app starts, so the image initializes an empty
	// database from the app's environment and the dump is all it holds. A
	// dump loaded over existing tables would duplicate their rows. A
	// hook's restore command is the recipe's and gets the data as it is.
	Fresh bool
	// Spec loads the stream it is given as Input; the caller adds limits
	// and labels. Nil when the dump is placed as a file instead.
	Spec *docker.DumpSpec
	// File is where the dump goes inside the app while it is stopped, for
	// an engine that loads its data from a file at start (Redis).
	File string
	// AOF is where the same dump goes too when the command line turns
	// Redis's append-only file on: Redis then reads only that file, and an
	// RDB is a valid one (its preamble); Redis 7 converts it into its AOF
	// directory at start.
	AOF string
	// Judge reads a load's stderr when its tool reports errors it does not
	// fail on; nil trusts the exit code.
	Judge func(stderr string) error
	// Warnings are conditions the Hub shows next to the restore.
	Warnings []Warning

	ready  *command
	finish *command
	p      *Planner
	app    App
	attach bool
}

// WarnAOF: a Redis restore cannot tell whether the append-only file is
// on, which decides the file Redis loads.
const WarnAOF = "aof_unknown"

// DumpEngine names the engine behind a dump's source name, which is the
// engine itself, or "hook".
func DumpEngine(name string) (Engine, bool) {
	switch e := Engine(name); e {
	case Postgres, MySQL, MariaDB, Redis, Mongo, ClickHouse:
		return e, true
	case "hook":
		return "", true
	}
	return "", false
}

// secretLinesScript reads the first $0 lines of stdin into secretFile and
// execs the tool with the rest of stdin, which is the stream to load. The
// shell's read takes one byte at a time from a pipe, so it never consumes
// what follows the last line. Credentials are refused when they hold a
// line break (plainSecret), so the count is exact.
const secretLinesScript = `n=$0; umask 077 && : >` + secretFile + ` || exit 70; while [ "$n" -gt 0 ]; do IFS= read -r l || exit 70; printf '%s\n' "$l" >>` + secretFile + ` || exit 70; n=$((n-1)); done; exec "$@"`

// loadSpec runs argv with stdin: the secret's lines first, then the
// stream.
func (c command) loadSpec(app App) docker.DumpSpec {
	s := docker.DumpSpec{Image: app.image(), App: app.Container, Env: c.env, StderrTail: 64 << 10}
	if c.mode == secretAsFile && c.secret != nil {
		lines := bytes.Count(c.secret, []byte("\n"))
		s.Cmd = append([]string{"sh", "-c", secretLinesScript, strconv.Itoa(lines)}, c.argv...)
		s.Stdin = c.secret
		return s
	}
	s.Cmd = c.argv
	return s
}

// Shape is what a load of the dump stored under name does to app before
// its database starts, worked out without running anything: whether its
// data volumes are emptied, and where a dump loaded from a file goes. A
// restore needs this while the app is stopped; the rest of the load
// (Load) needs the database running.
func Shape(app App, name string) (*Load, error) {
	engine, ok := DumpEngine(name)
	if !ok {
		return nil, fmt.Errorf("consistency: %q is not a dump ODAC knows how to load", name)
	}
	l := &Load{App: app.Name, Engine: engine, app: app}
	switch engine {
	case "":
		if app.Spec == nil || app.Spec.Hook == nil || len(app.Spec.Hook.Restore) == 0 {
			return nil, errors.New("consistency: the recipe declares no restore command for its dump")
		}
	case Redis:
		l.Fresh = true
		dir := first(flag(app.Cmd, "--dir"), redisDir(app))
		l.File = path.Join(dir, first(flag(app.Cmd, "--dbfilename"), "dump.rdb"))
		if strings.EqualFold(flag(app.Cmd, "--appendonly"), "yes") {
			l.AOF = path.Join(dir, first(flag(app.Cmd, "--appendfilename"), "appendonly.aof"))
		} else if len(app.Cmd) > 1 && strings.HasSuffix(app.Cmd[1], ".conf") {
			l.Warnings = append(l.Warnings, Warning{Code: WarnAOF, Message: "Redis reads a config file; if it turns appendonly on there, Redis ignores the restored save and starts empty"})
		}
	case ClickHouse:
		l.Fresh, l.attach = true, true
	default:
		l.Fresh = true
	}
	return l, nil
}

// Load decides how the dump stored under name is loaded into app, whose
// container runs the image the dump was taken with or a newer one. level
// is the snapshot's consistency, which says whether a Mongo dump carries
// an oplog to replay.
func (p *Planner) Load(ctx context.Context, app App, name, level string) (*Load, error) {
	if app.Name == "" || app.Container == "" || app.image() == "" {
		return nil, errors.New("consistency: an app needs a name, a container and an image")
	}
	l, err := Shape(app, name)
	if err != nil {
		return nil, err
	}
	l.p = p
	switch l.Engine {
	case Postgres:
		err = p.loadPostgres(ctx, app, l)
	case MySQL, MariaDB:
		err = p.loadMySQL(ctx, app, l)
	case Mongo:
		err = p.loadMongo(ctx, app, level, l)
	case ClickHouse:
		err = p.loadClickHouse(ctx, app, l)
	case "":
		h := app.Spec.Hook
		env := make([]string, 0, len(app.Env))
		for k, v := range app.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env)
		s := command{argv: h.Restore, env: env}.loadSpec(app)
		l.Spec = &s
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// redisDir is where the image keeps its save: the official image runs in
// /data, Bitnami's in its own tree.
func redisDir(app App) string {
	switch repo := Repository(app.Image); {
	case repo == "bitnami/valkey":
		return "/bitnami/valkey/data"
	case strings.HasPrefix(repo, "bitnami/"):
		return "/bitnami/redis/data"
	}
	return "/data"
}

func (p *Planner) loadPostgres(ctx context.Context, app App, l *Load) error {
	auth, ok := postgresCredentials(app.Env)
	if !ok {
		return errors.New("consistency: no Postgres password in the container's environment (POSTGRES_PASSWORD)")
	}
	if err := plainSecret(auth.password); err != nil {
		return err
	}
	if tool, _, err := p.tool(ctx, app, "psql"); err != nil {
		return err
	} else if tool == "" {
		return errors.New("consistency: psql is not in the image")
	}
	db := "postgres"
	if !auth.superuser {
		db = auth.db
	}
	base := []string{"psql", "--host=127.0.0.1", "--port=" + strconv.Itoa(auth.port), "--username=" + auth.user, "--no-password", "--dbname=" + db, "-X", "-q"}
	c := command{env: []string{"PGAPPNAME=odac-restore", "PGCONNECT_TIMEOUT=10"}}
	if auth.password != "" {
		c.env = append(c.env, "PGPASSFILE="+secretFile)
		c.secret = []byte("*:*:*:" + pgpassField(auth.user) + ":" + pgpassField(auth.password) + "\n")
		c.mode = secretAsFile
	}
	ready := c
	ready.argv = append(append([]string{}, base...), "--command=SELECT 1")
	l.ready = &ready
	// pg_dumpall recreates the roles and databases the image's init made
	// already, which psql reports and moves past; any other error fails
	// the restore.
	load := c
	load.argv = append(append([]string{}, base...), "--variable=ON_ERROR_STOP=0")
	s := load.loadSpec(app)
	l.Spec = &s
	l.Judge = judgePostgres
	return nil
}

func judgePostgres(stderr string) error {
	var bad []string
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "ERROR:") || strings.Contains(line, "already exists") {
			continue
		}
		bad = append(bad, strings.TrimSpace(line))
	}
	if len(bad) == 0 {
		return nil
	}
	more := ""
	if len(bad) > 5 {
		bad, more = bad[:5], fmt.Sprintf(" and %d more", len(bad)-5)
	}
	return fmt.Errorf("consistency: psql reported errors: %s%s", strings.Join(bad, "; "), more)
}

func (p *Planner) loadMySQL(ctx context.Context, app App, l *Load) error {
	auth, ok := mysqlCredentials(app.Env)
	if !ok {
		return errors.New("consistency: no MySQL root password or app user in the container's environment")
	}
	for _, s := range []string{auth.user, auth.password} {
		if err := plainSecret(s); err != nil {
			return err
		}
	}
	client, _, err := p.tool(ctx, app, "mariadb", "mysql")
	if err != nil {
		return err
	}
	if client == "" {
		return errors.New("consistency: neither mariadb nor mysql is in the image")
	}
	secret := auth.clientFile(max(1, int64(p.cfg.LockWait.Seconds())))
	opt := "--defaults-extra-file=" + secretFile
	l.ready = &command{argv: []string{client, opt, "--batch", "--execute=SELECT 1"}, secret: secret, mode: secretAsFile}
	s := command{argv: []string{client, opt, "--binary-mode"}, secret: secret, mode: secretAsFile}.loadSpec(app)
	l.Spec = &s
	if auth.root {
		// A dump of every database carries the grant tables, which the
		// server reads again only when told to.
		l.finish = &command{argv: []string{client, opt, "--batch", "--execute=FLUSH PRIVILEGES"}, secret: secret, mode: secretAsFile}
	}
	return nil
}

func (p *Planner) loadMongo(ctx context.Context, app App, level string, l *Load) error {
	user, pw := app.Env["MONGO_INITDB_ROOT_USERNAME"], app.Env["MONGO_INITDB_ROOT_PASSWORD"]
	if pw == "" && app.Env["MONGODB_ROOT_PASSWORD"] != "" {
		user, pw = first(app.Env["MONGODB_ROOT_USER"], "root"), app.Env["MONGODB_ROOT_PASSWORD"]
	}
	if err := plainSecret(pw); err != nil {
		return err
	}
	tool, _, err := p.tool(ctx, app, "mongorestore")
	if err != nil {
		return err
	}
	if tool == "" {
		return errors.New("consistency: mongorestore is not in the image")
	}
	shell, _, err := p.tool(ctx, app, "mongosh", "mongo")
	if err != nil {
		return err
	}
	if shell == "" {
		return errors.New("consistency: neither mongosh nor mongo is in the image")
	}
	portArg := strconv.Itoa(mongoPort(app))
	l.ready = &command{argv: []string{shell, "--quiet", "--norc", "--host", "127.0.0.1", "--port", portArg, "--eval", "db.runCommand({ping: 1}).ok"}, env: []string{"HOME=/tmp"}}
	argv := []string{tool, "--host=127.0.0.1", "--port=" + portArg, "--archive", "--drop", "--quiet"}
	c := command{env: []string{"HOME=/tmp"}}
	if pw != "" {
		argv = append(argv, "--username="+user, "--authenticationDatabase=admin", "--config="+secretFile)
		c.secret, c.mode = []byte("password: "+yamlString(pw)+"\n"), secretAsFile
	}
	if level == Consistent {
		// A consistent Mongo dump was taken with --oplog.
		argv = append(argv, "--oplogReplay")
	}
	c.argv = argv
	s := c.loadSpec(app)
	l.Spec = &s
	return nil
}

func (p *Planner) loadClickHouse(ctx context.Context, app App, l *Load) error {
	user := first(app.Env["CLICKHOUSE_USER"], app.Env["CLICKHOUSE_ADMIN_USER"], "default")
	pw := first(app.Env["CLICKHOUSE_PASSWORD"], app.Env["CLICKHOUSE_ADMIN_PASSWORD"])
	for _, s := range []string{user, pw} {
		if err := plainSecret(s); err != nil {
			return err
		}
	}
	cli, _, err := p.tool(ctx, app, "clickhouse-client")
	if err != nil {
		return err
	}
	if cli == "" {
		return errors.New("consistency: clickhouse-client is not in the image")
	}
	secret := []byte("<config><user>" + xmlText(user) + "</user><password>" + xmlText(pw) + "</password></config>\n")
	base := []string{cli, "--config-file=" + secretFile, "--host=127.0.0.1", "--port=" + strconv.Itoa(port(app.Env, 9000, "CLICKHOUSE_TCP_PORT"))}
	l.ready = &command{argv: append(append([]string{}, base...), "--query=SELECT 1"), secret: secret, mode: secretAsFile}
	s := command{argv: append(append([]string{}, base...), "--multiquery"), secret: secret, mode: secretAsFile}.loadSpec(app)
	l.Spec = &s
	return nil
}

// Ready timing: the database must answer every probe for readyStable in
// a row. An image's first start runs a server of its own to initialize
// the data directory (Mongo's listens on loopback) and stops it again; a
// single answer can come from that one.
const (
	readyEvery  = 2 * time.Second
	readyStable = 10 * time.Second
)

// WaitReady blocks until the database answers steadily, or ctx ends. alive,
// when given, is asked after every probe that fails and ends the wait with
// its error: a database that crashes on start never answers, and waiting
// out ctx for it only stretches the restore's downtime.
func (l *Load) WaitReady(ctx context.Context, alive func(context.Context) error) error {
	if l.ready == nil {
		return nil
	}
	var since time.Time
	var last error
	for {
		_, err := l.p.cfg.Prober.Run(ctx, l.ready.spec(l.app))
		switch {
		case err == nil && since.IsZero():
			since = time.Now()
		case err == nil && time.Since(since) >= readyStable:
			return nil
		case err != nil:
			since, last = time.Time{}, err
			if alive != nil {
				if aerr := alive(ctx); aerr != nil {
					return fmt.Errorf("consistency: %s did not become ready: %w (last probe: %s)", l.Engine, aerr, err.Error())
				}
			}
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("consistency: %s did not become ready: %s", l.Engine, last.Error())
			}
			return ctx.Err()
		case <-time.After(readyEvery):
		}
	}
}

// Finish runs what the server needs after the load.
func (l *Load) Finish(ctx context.Context) error {
	if l.finish == nil {
		return nil
	}
	if _, err := l.p.cfg.Prober.Run(ctx, l.finish.spec(l.app)); err != nil {
		return fmt.Errorf("consistency: after the load: %w", err)
	}
	return nil
}

// AttachesParts reports a load whose MergeTree data comes back as frozen
// parts after the definitions (ClickHouse).
func (l *Load) AttachesParts() bool { return l.attach }

// Tables maps the UUID of every table on a loaded ClickHouse server to
// its quoted name, for attaching parts the restore moved into each
// table's detached directory, and returns the default disk's path.
func (l *Load) Tables(ctx context.Context) (map[string]string, string, error) {
	if !l.attach {
		return nil, "", errors.New("consistency: not a ClickHouse load")
	}
	q := *l.ready
	q.argv = append(q.argv[:len(q.argv)-1:len(q.argv)-1],
		"--query=SELECT 'table' AS k, toString(uuid) AS uuid, database AS db, name FROM system.tables WHERE NOT is_temporary FORMAT JSONEachRow;"+
			" SELECT 'disk' AS k, name, path FROM system.disks WHERE name = 'default' FORMAT JSONEachRow", "--multiquery")
	out, err := l.p.cfg.Prober.Run(ctx, q.spec(l.app))
	if err != nil {
		return nil, "", fmt.Errorf("consistency: list ClickHouse tables: %w", err)
	}
	tables := map[string]string{}
	disk := ""
	for _, line := range strings.Split(string(out), "\n") {
		var r struct {
			K, UUID, DB, Name, Path string
		}
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		switch r.K {
		case "table":
			if r.UUID != "" && r.UUID != "00000000-0000-0000-0000-000000000000" && !clickhouseSystemDBs[r.DB] {
				tables[r.UUID] = chTable{db: r.DB, name: r.Name}.ref()
			}
		case "disk":
			disk = r.Path
		}
	}
	if disk == "" {
		return nil, "", errors.New("consistency: ClickHouse reported no default disk")
	}
	return tables, disk, nil
}

// Attach attaches the parts named per table; every part must already be
// in the table's detached directory.
func (l *Load) Attach(ctx context.Context, parts map[string][]string) error {
	if len(parts) == 0 {
		return nil
	}
	refs := make([]string, 0, len(parts))
	for ref := range parts {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	var b strings.Builder
	for _, ref := range refs {
		names := append([]string(nil), parts[ref]...)
		sort.Strings(names)
		for _, part := range names {
			fmt.Fprintf(&b, "ALTER TABLE %s ATTACH PART %s;\n", ref, chString(part))
		}
	}
	q := *l.ready
	q.argv = append(q.argv[:len(q.argv)-1:len(q.argv)-1], "--multiquery", "--query="+b.String())
	if _, err := l.p.cfg.Prober.Run(ctx, q.spec(l.app)); err != nil {
		return fmt.Errorf("consistency: attach ClickHouse parts: %w", err)
	}
	return nil
}

// DataVolumes are the volumes of app that hold its engine's data
// directory: what a dump stands in for, which a backup leaves out of its
// files and a fresh load empties. It mirrors the roots Plan covers,
// without asking the database (a restore runs while it is stopped).
func DataVolumes(app App, engine Engine) []Volume {
	var roots []string
	switch engine {
	case Postgres:
		roots = postgresRoots
		if d := app.Env["PGDATA"]; strings.HasPrefix(d, "/") {
			roots = append([]string{d}, roots...)
		}
	case MySQL, MariaDB:
		roots = mysqlRoots
	case Redis:
		roots = redisRoots
	case Mongo:
		roots = mongoRoots
	case ClickHouse:
		roots = clickhouseRoots
	default:
		if app.Spec != nil && app.Spec.Hook != nil {
			roots = app.Spec.Hook.Covers
		}
	}
	var out []Volume
	for _, v := range dataVolumes(app.Volumes) {
		if underAny(v.Container, roots) {
			out = append(out, v)
		}
	}
	return out
}

// DirName is the source name a backup gives the volume a container sees
// at container, so a restore finds which snapshot belongs to it.
func DirName(container string) string { return dirName(container) }

// ClickHouseRoot is the container path of ClickHouse's default disk in
// the image, where SQL users (access) live; "" for an unknown layout.
func ClickHouseRoot(app App) string {
	for _, v := range dataVolumes(app.Volumes) {
		switch {
		case within("/var/lib/clickhouse", v.Container):
			return "/var/lib/clickhouse"
		case within("/bitnami/clickhouse/data", v.Container):
			return "/bitnami/clickhouse/data"
		}
	}
	return ""
}

// HostPath maps a path inside app to this server's view through the
// deepest volume holding it.
func HostPath(app App, p string) (string, bool) {
	h, _, ok := hostPath(dataVolumes(app.Volumes), p)
	return h, ok
}
