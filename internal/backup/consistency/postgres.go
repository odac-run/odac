package consistency

import (
	"context"
	"strconv"
	"strings"
)

// postgresRoots are where Postgres images keep their data. Postgres 18's
// image mounts its volume at /var/lib/postgresql and puts the data in a
// versioned directory under it.
var postgresRoots = []string{"/var/lib/postgresql", "/bitnami/postgresql"}

type postgresAuth struct {
	user, password, db string
	port               int
	superuser          bool
	trust              bool
}

// postgresCredentials reads the official image's variables, then
// Bitnami's. A password that exists only as a file (POSTGRES_PASSWORD_FILE)
// is out of reach: the dump container has none of the app's mounts.
func postgresCredentials(env map[string]string) (postgresAuth, bool) {
	a := postgresAuth{port: port(env, 5432, "PGPORT", "POSTGRESQL_PORT_NUMBER")}
	switch {
	case env["POSTGRESQL_POSTGRES_PASSWORD"] != "":
		a.user, a.password, a.superuser = "postgres", env["POSTGRESQL_POSTGRES_PASSWORD"], true
	case env["POSTGRESQL_PASSWORD"] != "":
		a.user = first(env["POSTGRESQL_USERNAME"], "postgres")
		a.password, a.db = env["POSTGRESQL_PASSWORD"], first(env["POSTGRESQL_DATABASE"], a.user)
		a.superuser = a.user == "postgres"
	default:
		a.user = first(env["POSTGRES_USER"], "postgres")
		a.password = env["POSTGRES_PASSWORD"]
		a.superuser = true
		a.trust = env["POSTGRES_HOST_AUTH_METHOD"] == "trust"
		if a.password == "" && !a.trust {
			return a, false
		}
	}
	return a, true
}

func (p *Planner) planPostgres(ctx context.Context, app App, plan *Plan) ([]string, error) {
	roots := postgresRoots
	if d := app.Env["PGDATA"]; strings.HasPrefix(d, "/") {
		roots = append([]string{d}, roots...)
	}
	auth, ok := postgresCredentials(app.Env)
	if !ok {
		plan.warn(WarnCredentials, "no Postgres password in the container's environment (POSTGRES_PASSWORD)")
		return nil, nil
	}
	if err := plainSecret(auth.password); err != nil {
		plan.warn(WarnCredentials, "%s", err.Error())
		return nil, nil
	}
	tool := "pg_dumpall"
	if !auth.superuser {
		tool = "pg_dump"
	}
	name, _, err := p.tool(ctx, app, tool)
	if err != nil {
		return nil, err
	}
	if name == "" {
		plan.warn(WarnToolMissing, "%s is not in the image", tool)
		return nil, nil
	}
	c := command{
		argv: []string{tool,
			"--host=127.0.0.1",
			"--port=" + strconv.Itoa(auth.port),
			"--username=" + auth.user,
			"--no-password",
			// Waits for the ACCESS SHARE lock on each table are bounded; a
			// table a migration holds fails the dump instead of queueing it.
			"--lock-wait-timeout=" + strconv.FormatInt(p.cfg.LockWait.Milliseconds(), 10),
		},
		env: []string{"PGAPPNAME=odac-backup", "PGCONNECT_TIMEOUT=10"},
	}
	if !auth.superuser {
		c.argv = append(c.argv, "--dbname="+auth.db)
	}
	if auth.password != "" {
		c.env = append(c.env, "PGPASSFILE="+secretFile)
		c.secret = []byte("*:*:*:" + pgpassField(auth.user) + ":" + pgpassField(auth.password) + "\n")
		c.mode = secretAsFile
	}
	plan.Dump = &Dump{Name: "postgres", File: "postgres.sql", Consistency: Consistent, Spec: c.spec(app)}
	return roots, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func port(env map[string]string, def int, names ...string) int {
	for _, n := range names {
		if v, err := strconv.Atoi(env[n]); err == nil && v > 0 && v < 65536 {
			return v
		}
	}
	return def
}

// flag reads "--name value" or "--name=value" from a command line.
func flag(cmd []string, name string) string {
	for i, a := range cmd {
		if a == name && i+1 < len(cmd) {
			return cmd[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return a[len(name)+1:]
		}
	}
	return ""
}
