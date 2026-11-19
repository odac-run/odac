package consistency

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

var mysqlRoots = []string{"/var/lib/mysql", "/bitnami/mysql", "/bitnami/mariadb"}

// transactionalEngines are read from the snapshot --single-transaction
// opens; any other engine's tables are read as they are at that moment.
var transactionalEngines = []string{"InnoDB", "ROCKSDB"}

type mysqlAuth struct {
	user, password, db string
	port               int
	root               bool
}

// mysqlCredentials prefers root, which can dump every database with its
// users and grants, and falls back to the image's app user, limited to its
// own database.
func mysqlCredentials(env map[string]string) (mysqlAuth, bool) {
	a := mysqlAuth{port: port(env, 3306, "MYSQL_TCP_PORT", "MARIADB_PORT_NUMBER", "MYSQL_PORT_NUMBER")}
	if pw := first(env["MARIADB_ROOT_PASSWORD"], env["MYSQL_ROOT_PASSWORD"]); pw != "" {
		a.user, a.password, a.root = "root", pw, true
		return a, true
	}
	if env["MARIADB_ALLOW_EMPTY_ROOT_PASSWORD"] != "" || env["MYSQL_ALLOW_EMPTY_PASSWORD"] != "" || env["ALLOW_EMPTY_PASSWORD"] == "yes" {
		a.user, a.root = "root", true
		return a, true
	}
	user := first(env["MARIADB_USER"], env["MYSQL_USER"])
	pw := first(env["MARIADB_PASSWORD"], env["MYSQL_PASSWORD"])
	db := first(env["MARIADB_DATABASE"], env["MYSQL_DATABASE"])
	if user != "" && pw != "" && db != "" {
		a.user, a.password, a.db = user, pw, db
		return a, true
	}
	return a, false
}

// clientFile is the option file every client reads from secretFile. MySQL's
// clients run init-command from it, which bounds metadata lock waits;
// MariaDB's dump ignores it (verified on 11.4), so there the wait is
// bounded only by the run's own time limit.
func (a mysqlAuth) clientFile(lockWait int64) []byte {
	var b strings.Builder
	b.WriteString("[client]\n")
	b.WriteString("user=" + cnfValue(a.user) + "\n")
	if a.password != "" {
		b.WriteString("password=" + cnfValue(a.password) + "\n")
	}
	b.WriteString("host=127.0.0.1\nprotocol=TCP\nport=" + strconv.Itoa(a.port) + "\n")
	fmt.Fprintf(&b, "loose-init-command=\"SET SESSION lock_wait_timeout=%d\"\n", lockWait)
	return []byte(b.String())
}

func (p *Planner) planMySQL(ctx context.Context, app App, plan *Plan) ([]string, error) {
	auth, ok := mysqlCredentials(app.Env)
	if !ok {
		plan.warn(WarnCredentials, "no MySQL root password or app user in the container's environment")
		return nil, nil
	}
	for _, s := range []string{auth.user, auth.password} {
		if err := plainSecret(s); err != nil {
			plan.warn(WarnCredentials, "%s", err.Error())
			return nil, nil
		}
	}
	dumpTool, version, err := p.tool(ctx, app, "mariadb-dump", "mysqldump")
	if err != nil {
		return nil, err
	}
	if dumpTool == "" {
		plan.warn(WarnToolMissing, "neither mariadb-dump nor mysqldump is in the image")
		return nil, nil
	}
	maria := dumpTool == "mariadb-dump" || strings.Contains(version, "MariaDB")
	client := "mysql"
	if dumpTool == "mariadb-dump" {
		client = "mariadb"
	}
	if maria {
		plan.Engine = MariaDB
	} else {
		plan.Engine = MySQL
	}
	secret := auth.clientFile(max(1, int64(p.cfg.LockWait.Seconds())))
	optFile := "--defaults-extra-file=" + secretFile

	consistency := Consistent
	scope := "TABLE_SCHEMA NOT IN ('mysql','sys','information_schema','performance_schema')"
	if !auth.root {
		scope = "TABLE_SCHEMA = '" + strings.ReplaceAll(strings.ReplaceAll(auth.db, `\`, `\\`), `'`, `''`) + "'"
	}
	query := "SELECT CONCAT(TABLE_SCHEMA, '.', TABLE_NAME, ' ', ENGINE) FROM information_schema.TABLES" +
		" WHERE TABLE_TYPE = 'BASE TABLE' AND ENGINE IS NOT NULL AND ENGINE NOT IN ('" + strings.Join(transactionalEngines, "','") + "')" +
		" AND " + scope + " ORDER BY 1 LIMIT 21"
	out, ok, err := p.probe(ctx, app, plan, command{
		argv:   []string{client, optFile, "--batch", "--skip-column-names", "--execute=" + query},
		secret: secret, mode: secretAsFile,
	}, auth.password)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if listed := strings.TrimSpace(string(out)); listed != "" {
		lines := strings.Split(listed, "\n")
		more := ""
		if len(lines) > 20 {
			lines, more = lines[:20], " and more"
		}
		// Consistency for these would take LOCK TABLES, which stops the
		// app's writes for the whole dump; they are read without it.
		plan.warn(WarnNonTransactional, "tables without snapshot reads are dumped as they are while being read: %s%s", strings.Join(lines, ", "), more)
		consistency = Live
	}

	argv := []string{dumpTool, optFile,
		"--single-transaction", "--quick", "--hex-blob", "--triggers",
		"--default-character-set=utf8mb4", "--no-tablespaces",
	}
	if auth.root {
		argv = append(argv, "--routines", "--events", "--all-databases")
	} else {
		argv = append(argv, "--databases", auth.db)
	}
	if !maria {
		// Without it, MySQL takes a global read lock to read the GTID set
		// on a server with GTIDs on.
		argv = append(argv, "--set-gtid-purged=OFF")
	}
	name := string(plan.Engine)
	plan.Dump = &Dump{Name: name, File: name + ".sql", Consistency: consistency,
		Spec: command{argv: argv, secret: secret, mode: secretAsFile}.spec(app)}
	return mysqlRoots, nil
}
