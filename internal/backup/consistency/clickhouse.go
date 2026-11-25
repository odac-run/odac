package consistency

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

var clickhouseRoots = []string{"/var/lib/clickhouse", "/bitnami/clickhouse"}

// clickhouseSystemDBs hold the server's own state, which a new server
// makes for itself.
var clickhouseSystemDBs = map[string]bool{"system": true, "INFORMATION_SCHEMA": true, "information_schema": true}

// clickhouseLocalDBs are the database engines whose tables live on this
// server; a MySQL or PostgreSQL database engine only names remote tables.
var clickhouseLocalDBs = map[string]bool{"Atomic": true, "Ordinary": true, "Replicated": true, "Lazy": true, "Memory": true}

// clickhouseDataEngines are the non-MergeTree engines whose rows are read
// with SELECT. Everything else (Kafka, URL, S3, Distributed, Buffer, ...) is
// stored as its definition only: selecting from a Kafka table would consume
// its messages.
var clickhouseDataEngines = map[string]bool{"Log": true, "TinyLog": true, "StripeLog": true, "Memory": true, "EmbeddedRocksDB": true}

type chRow struct {
	K      string `json:"k"`
	DB     string `json:"db"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Path   string `json:"path"`
	Type   string `json:"type"`
}

type chTable struct{ db, name, engine string }

func (t chTable) ref() string { return chIdent(t.db) + "." + chIdent(t.name) }

func (t chTable) mergeTree() bool { return strings.HasSuffix(t.engine, "MergeTree") }

// order puts tables first, then dictionaries, then views, so a restore
// creates what a view reads before the view.
func (t chTable) order() int {
	switch {
	case t.engine == "Dictionary":
		return 1
	case strings.HasSuffix(t.engine, "View"):
		return 2
	}
	return 0
}

// chIdent quotes a ClickHouse identifier.
func chIdent(s string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
}

// chString quotes a ClickHouse string literal.
func chString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

var chNameChars = strings.NewReplacer("-", "_")

const chListQuery = "SELECT 'disk' AS k, name, path, type FROM system.disks FORMAT JSONEachRow;" +
	" SELECT 'db' AS k, name, engine FROM system.databases FORMAT JSONEachRow;" +
	" SELECT 'table' AS k, database AS db, name, engine FROM system.tables WHERE NOT is_temporary FORMAT JSONEachRow"

// planClickHouse freezes every MergeTree table (Prepare), which hard-links
// its current parts into the disk's shadow directory without locking
// anything, and reads that directory as files. Definitions, and the rows of
// the few engines that are not MergeTree, come as one SQL stream that a
// client replays; users made with SQL live in the access directory, read
// live.
func (p *Planner) planClickHouse(ctx context.Context, app App, vols []Volume, run string, plan *Plan) ([]string, error) {
	user := first(app.Env["CLICKHOUSE_USER"], app.Env["CLICKHOUSE_ADMIN_USER"], "default")
	pw := first(app.Env["CLICKHOUSE_PASSWORD"], app.Env["CLICKHOUSE_ADMIN_PASSWORD"])
	for _, s := range []string{user, pw} {
		if err := plainSecret(s); err != nil {
			plan.warn(WarnCredentials, "%s", err.Error())
			return nil, nil
		}
	}
	cli, _, err := p.tool(ctx, app, "clickhouse-client")
	if err != nil {
		return nil, err
	}
	if cli == "" {
		plan.warn(WarnToolMissing, "clickhouse-client is not in the image")
		return nil, nil
	}
	secret := []byte("<config><user>" + xmlText(user) + "</user><password>" + xmlText(pw) + "</password></config>\n")
	client := func(args ...string) command {
		return command{
			argv:   append([]string{cli, "--config-file=" + secretFile, "--host=127.0.0.1", "--port=" + strconv.Itoa(port(app.Env, 9000, "CLICKHOUSE_TCP_PORT")), "--multiquery"}, args...),
			secret: secret, mode: secretAsFile,
		}
	}

	out, ok, err := p.probe(ctx, app, plan, client("--query="+chListQuery), pw)
	if err != nil || !ok {
		return nil, err
	}
	var disks []chRow
	localDB := map[string]bool{}
	var dbs []string
	var tables []chTable
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), probeOutput)
	for sc.Scan() {
		var r chRow
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.K {
		case "disk":
			disks = append(disks, r)
		case "db":
			if !clickhouseSystemDBs[r.Name] {
				dbs = append(dbs, r.Name)
				localDB[r.Name] = clickhouseLocalDBs[r.Engine]
			}
		case "table":
			if !clickhouseSystemDBs[r.DB] && localDB[r.DB] {
				tables = append(tables, chTable{db: r.DB, name: r.Name, engine: r.Engine})
			}
		}
	}
	sort.Strings(dbs)
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].order() < tables[j].order() })

	freezeName := "odac_" + chNameChars.Replace(run)
	var defaultPath string
	var shadows []string
	var dirs []Dir
	covered := append([]string{}, clickhouseRoots...)
	for _, d := range disks {
		if d.Name == "default" {
			defaultPath = d.Path
		}
		if !strings.EqualFold(d.Type, "local") {
			plan.warn(WarnNotOnVolume, "ClickHouse disk %q is %s storage; its parts are not frozen", d.Name, d.Type)
			continue
		}
		shadow, vol, ok := hostPath(vols, path.Join(d.Path, "shadow", freezeName))
		if !ok {
			plan.warn(WarnNotOnVolume, "ClickHouse disk %q (%s) is not on a volume", d.Name, d.Path)
			return nil, nil
		}
		covered = append(covered, vol.Container)
		name := "clickhouse-parts"
		if d.Name != "default" {
			name += "-" + shortHash(d.Name)
		}
		dirs = append(dirs, Dir{Name: name, Path: shadow, Container: path.Join(d.Path, "shadow"), Consistency: Consistent, Prepared: true})
		shadows = append(shadows, shadow)
	}
	if defaultPath == "" {
		plan.warn(WarnProbe, "ClickHouse reported no default disk")
		return nil, nil
	}
	if access, _, ok := hostPath(vols, path.Join(defaultPath, "access")); ok {
		dirs = append(dirs, Dir{Name: "clickhouse-access", Path: access, Container: path.Join(defaultPath, "access"), Consistency: Live})
	}

	var script, freeze, unfreeze strings.Builder
	for _, db := range dbs {
		// The default database exists on every server.
		if db != "default" {
			fmt.Fprintf(&script, "SHOW CREATE DATABASE %s FORMAT TSVRaw; SELECT ';' FORMAT TSVRaw;\n", chIdent(db))
		}
	}
	for _, t := range tables {
		if t.mergeTree() {
			fmt.Fprintf(&freeze, "ALTER TABLE %s FREEZE WITH NAME %s;\n", t.ref(), chString(freezeName))
			fmt.Fprintf(&unfreeze, "ALTER TABLE %s UNFREEZE WITH NAME %s;\n", t.ref(), chString(freezeName))
		}
		// A materialized view creates its own inner table.
		if strings.HasPrefix(t.name, ".inner") {
			continue
		}
		kind := "TABLE"
		if t.engine == "Dictionary" {
			kind = "DICTIONARY"
		}
		fmt.Fprintf(&script, "SHOW CREATE %s %s FORMAT TSVRaw; SELECT ';' FORMAT TSVRaw;\n", kind, t.ref())
	}
	for _, t := range tables {
		if clickhouseDataEngines[t.engine] {
			fmt.Fprintf(&script, "SELECT * FROM %s FORMAT SQLInsert SETTINGS output_format_sql_insert_table_name = %s, output_format_sql_insert_include_column_names = 0;\n",
				t.ref(), chString(t.ref()))
		}
	}
	if script.Len() == 0 {
		script.WriteString("SELECT '' FORMAT TSVRaw;\n")
	}

	if freeze.Len() > 0 {
		f := client("--query=" + freeze.String())
		u := client("--ignore-error", "--query="+unfreeze.String())
		plan.AddPrepare(func(ctx context.Context) error {
			if _, err := p.cfg.Prober.Run(ctx, f.spec(app)); err != nil {
				return fmt.Errorf("consistency: freeze ClickHouse tables: %s", mask(err.Error(), []string{pw}))
			}
			return nil
		}, func(ctx context.Context) error {
			_, err := p.cfg.Prober.Run(ctx, u.spec(app))
			for _, s := range shadows {
				if rerr := p.cfg.RemoveAll(s); rerr != nil && err == nil {
					err = rerr
				}
			}
			if err != nil {
				return fmt.Errorf("consistency: unfreeze ClickHouse tables: %s", mask(err.Error(), []string{pw}))
			}
			return nil
		})
	} else {
		// Nothing frozen means no shadow directory to read.
		kept := dirs[:0]
		for _, d := range dirs {
			if d.Consistency != Consistent {
				kept = append(kept, d)
			}
		}
		dirs = kept
	}
	plan.Dirs = append(plan.Dirs, dirs...)

	s := client("--show_table_uuid_in_table_create_query_if_not_nil=1", "--query="+script.String())
	plan.Dump = &Dump{Name: "clickhouse", File: "clickhouse.sql", Consistency: Consistent, Spec: s.spec(app)}
	return covered, nil
}
