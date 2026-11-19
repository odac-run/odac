package consistency

import (
	"context"
	"strconv"
	"strings"
)

var mongoRoots = []string{"/data/db", "/data/configdb", "/bitnami/mongodb"}

const setMarker = "odac-set:"

func replicaSet(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), setMarker); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mongoPort(app App) int {
	if v, err := strconv.Atoi(flag(app.Cmd, "--port")); err == nil && v > 0 && v < 65536 {
		return v
	}
	return port(app.Env, 27017, "MONGODB_PORT_NUMBER")
}

// planMongo dumps with mongodump. Only a replica set member has an oplog,
// and only with --oplog is the dump one point in time; a standalone server
// is dumped collection by collection, which is said in its label.
func (p *Planner) planMongo(ctx context.Context, app App, plan *Plan) ([]string, error) {
	user, pw := app.Env["MONGO_INITDB_ROOT_USERNAME"], app.Env["MONGO_INITDB_ROOT_PASSWORD"]
	if pw == "" && app.Env["MONGODB_ROOT_PASSWORD"] != "" {
		user, pw = first(app.Env["MONGODB_ROOT_USER"], "root"), app.Env["MONGODB_ROOT_PASSWORD"]
	}
	if err := plainSecret(pw); err != nil {
		plan.warn(WarnCredentials, "%s", err.Error())
		return nil, nil
	}
	tool, _, err := p.tool(ctx, app, "mongodump")
	if err != nil {
		return nil, err
	}
	if tool == "" {
		plan.warn(WarnToolMissing, "mongodump is not in the image")
		return nil, nil
	}
	portArg := strconv.Itoa(mongoPort(app))

	replica := false
	shell, _, err := p.tool(ctx, app, "mongosh", "mongo")
	if err != nil {
		return nil, err
	}
	if shell != "" {
		// hello answers without authentication. The shell prints warnings
		// of its own to stdout (a home directory it cannot write), so the
		// answer is the line with the marker.
		out, ok, err := p.probe(ctx, app, plan, command{
			argv: []string{shell, "--quiet", "--norc", "--host", "127.0.0.1", "--port", portArg,
				"--eval", "print('" + setMarker + "' + (db.hello().setName || ''))"},
			env: []string{"HOME=/tmp"},
		})
		if err != nil {
			return nil, err
		}
		replica = ok && replicaSet(out) != ""
	}

	argv := []string{tool, "--host=127.0.0.1", "--port=" + portArg, "--archive"}
	c := command{env: []string{"HOME=/tmp"}}
	if pw != "" {
		argv = append(argv, "--username="+user, "--authenticationDatabase=admin", "--config="+secretFile)
		c.secret, c.mode = []byte("password: "+yamlString(pw)+"\n"), secretAsFile
	}
	consistency := Consistent
	if replica {
		argv = append(argv, "--oplog")
	} else {
		consistency = Live
		plan.warn(WarnStandalone, "MongoDB runs without a replica set, so its collections are dumped one after another rather than at one point in time")
	}
	c.argv = argv
	plan.Dump = &Dump{Name: "mongo", File: "mongo.archive", Consistency: consistency, Spec: c.spec(app)}
	return mongoRoots, nil
}
