package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"odac/internal/apiproto"
	"odac/internal/resources"
)

// backupCommand is `odac backup`. Every action takes one object, the same
// payload the dashboard sends, so the CLI builds that object from flags.
func backupCommand() *command {
	return &command{
		title: "BACKUP",
		sub: []entry{
			{"target", &command{
				sub: []entry{
					{"list", &command{
						description: "List backup targets",
						action:      (*app).backupTargetList,
					}},
					{"add", &command{
						description: "Add a backup target: --disk <host path>, --smb //<host>/<share>, --nfs <host>:<export>, --s3 <bucket> --endpoint <host> or --webdav <url>. Shows its key once; --key uses your own",
						args:        []string{"--name", "--disk", "--smb", "--nfs", "--s3", "--webdav", "--path", "--username", "--password", "--domain", "--version", "--endpoint", "--region", "--prefix", "--access-key", "--secret-key", "--no-tls", "--key", "--adopt", "--recovery-file", "--yes"},
						action:      (*app).backupTargetAdd,
					}},
					{"update", &command{
						description: "Rename a backup target or replace its credentials",
						args:        []string{"-t", "--target", "--name", "--username", "--password", "--domain", "--version", "--region", "--access-key", "--secret-key", "--no-tls"},
						action:      (*app).backupTargetUpdate,
					}},
					{"delete", &command{
						description: "Delete a backup target. Its backups stay on the storage",
						args:        []string{"-t", "--target"},
						action: func(a *app, args []string) int {
							id, code := a.backupTargetID(args)
							if code != 0 {
								return code
							}
							return a.call("backup.target.delete", []any{map[string]any{"id": id}}, false)
						},
					}},
					{"adopt", &command{
						description: "Let this server write to a target again after another machine wrote under its id there",
						args:        []string{"-t", "--target"},
						action:      (*app).backupTargetAdopt,
					}},
					{"servers", &command{
						description: "List the server directories on a backup target",
						args:        []string{"-t", "--target"},
						action:      (*app).backupTargetServers,
					}},
				},
			}},
			{"key", &command{
				description: "Show a backup target's key again, with what a new server needs to open its backups. --out writes it to a recovery file",
				args:        []string{"-t", "--target", "--out"},
				action:      (*app).backupKey,
			}},
			{"job", &command{
				sub: []entry{
					{"list", &command{
						description: "List backup jobs",
						action:      (*app).backupJobList,
					}},
					{"create", &command{
						description: "Create a backup job: --target, --apps a,b or --all, --server for ODAC's own state, --every 1h, --density low|normal|high, --max-age <days>, --budget 50g",
						args:        []string{"--name", "--target", "--apps", "--all", "--server", "--every", "--density", "--max-age", "--budget"},
						action:      (*app).backupJobCreate,
					}},
					{"update", &command{
						description: "Change a backup job; options left out keep their value",
						args:        []string{"-j", "--job", "--name", "--apps", "--all", "--server", "--no-server", "--every", "--density", "--max-age", "--budget"},
						action:      (*app).backupJobUpdate,
					}},
					{"delete", &command{
						description: "Delete a backup job. Its backups are kept unless --purge",
						args:        []string{"-j", "--job", "--purge", "--yes"},
						action:      (*app).backupJobDelete,
					}},
					{"run", &command{
						description: "Run a backup job now",
						args:        []string{"-j", "--job"},
						action: func(a *app, args []string) int {
							id, code := a.backupJobID(args)
							if code != 0 {
								return code
							}
							return a.callMessage("backup.job.run", []any{map[string]any{"id": id}})
						},
					}},
					{"retired", &command{
						description: "List deleted backup jobs whose backups are kept",
						action:      (*app).backupRetiredList,
					}},
					{"purge", &command{
						description: "Delete the kept backups of a deleted backup job",
						args:        []string{"-j", "--job", "--yes"},
						action:      (*app).backupRetiredPurge,
					}},
				},
			}},
			{"status", &command{
				description: "Show when each backup job runs and how each target stands",
				action:      (*app).backupStatus,
			}},
			{"history", &command{
				description: "List finished backup runs and maintenance",
				args:        []string{"-j", "--job", "--limit"},
				action:      (*app).backupHistory,
			}},
			{"coverage", &command{
				description: "Show what the backup jobs leave unprotected",
				action:      (*app).backupCoverage,
			}},
			{"points", &command{
				description: "List the runs a restore can return to. --server reads another server's directory",
				args:        []string{"-t", "--target", "--server", "--key", "--app"},
				action:      (*app).backupPoints,
			}},
			{"restore", &command{
				sub: []entry{
					{"apps", &command{
						description: "Restore apps from a backup run, as new apps (default) or --in-place. Shows what it does and what it gives up first",
						args:        []string{"-t", "--target", "--run", "--apps", "--in-place", "--new-app", "--name", "--server", "--key", "--skip-safety", "--yes", "--no-wait"},
						action:      (*app).backupRestoreApps,
					}},
					{"list", &command{
						description: "List restores",
						action:      (*app).backupRestoreList,
					}},
					{"cancel", &command{
						description: "Stop the restore in progress and roll it back",
						args:        []string{"--id"},
						action: func(a *app, args []string) int {
							id := parseArg(args, "--id")
							if id == "" {
								id = a.question(__("Enter the restore id: "))
							}
							return a.callMessage("backup.restore.cancel", []any{map[string]any{"id": id}})
						},
					}},
					{"state", &command{
						description: "Restore ODAC's own state (config, certificates, databases, mail) onto a freshly installed server. --cancel drops a staged one",
						args:        []string{"-t", "--target", "--server", "--run", "--key", "--yes", "--cancel"},
						action:      (*app).backupRestoreState,
					}},
				},
			}},
		},
	}
}

// fetch runs an action and returns its data, or renders the refusal and
// returns false.
func (a *app) fetch(action string, payload any) (*apiproto.Response, bool) {
	if !a.check() {
		fmt.Fprintln(a.errOut, "Odac server is not running.")
		return nil, false
	}
	auth, _ := a.cfg.Map("api")["auth"].(string)
	data := []any{}
	if payload != nil {
		data = []any{payload}
	}
	resp, err := a.client.Call(apiproto.Request{Auth: auth, Action: action, Data: data}, nil)
	if err != nil {
		fmt.Fprintln(a.errOut, "Socket error:", err)
		return nil, false
	}
	if !resp.Result {
		(&renderer{out: a.out, errOut: a.errOut}).final(resp)
		return nil, false
	}
	return resp, true
}

// fetchInto runs an action and decodes its data into out.
func (a *app) fetchInto(action string, payload any, out any) bool {
	resp, ok := a.fetch(action, payload)
	if !ok {
		return false
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		fmt.Fprintln(a.errOut, err)
		return false
	}
	return true
}

// printRows renders rows (a slice of structs, whose field order is the
// column order) as a table, or the note when there are none.
func printRows(a *app, rows any, none string) {
	raw, err := json.Marshal(rows)
	if err != nil {
		fmt.Fprintln(a.errOut, err)
		return
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		fmt.Fprintln(a.out, none)
		return
	}
	printTable(a.out, raw, list)
}

func confirmed(a *app, args []string, prompt string) bool {
	if slices.Contains(args, "--yes") {
		return true
	}
	return strings.EqualFold(a.question(prompt), "yes")
}

func when(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// span renders a duration in its two largest units.
func span(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return __("%s s", int(d.Seconds()))
	case d < time.Hour:
		return __("%s min", int(d.Minutes()))
	case d < 48*time.Hour:
		return __("%s h %s min", int(d.Hours()), int(d.Minutes())%60)
	}
	return __("%s days %s h", int(d.Hours()/24), int(d.Hours())%24)
}

func size(n int64) string {
	if n <= 0 {
		return "-"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// --- targets ---

type backupTarget struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Disk     map[string]string `json:"disk"`
	SMB      map[string]string `json:"smb"`
	NFS      map[string]string `json:"nfs"`
	S3       map[string]any    `json:"s3"`
	WebDAV   map[string]string `json:"webdav"`
	Location string            `json:"location"`
	SameDisk bool              `json:"sameDisk"`
	Conflict *struct {
		Message string `json:"message"`
	} `json:"conflict"`
}

// where is a target's storage in one line.
func (t backupTarget) where() string {
	switch {
	case t.Disk != nil:
		return t.Disk["path"]
	case t.SMB != nil:
		return "//" + t.SMB["addr"] + "/" + t.SMB["share"] + strings.TrimSuffix(t.SMB["path"], "/")
	case t.NFS != nil:
		return t.NFS["addr"] + ":" + t.NFS["export"] + strings.TrimSuffix(t.NFS["path"], "/")
	case t.S3 != nil:
		prefix, _ := t.S3["prefix"].(string)
		return fmt.Sprintf("s3://%v/%v (%v)", t.S3["bucket"], prefix, t.S3["endpoint"])
	case t.WebDAV != nil:
		return t.WebDAV["url"]
	}
	return "-"
}

func (t backupTarget) note() string {
	var notes []string
	if t.Conflict != nil {
		notes = append(notes, __("writes stopped: %s", t.Conflict.Message))
	}
	if t.SameDisk {
		notes = append(notes, __("same disk as the data"))
	}
	return strings.Join(notes, "; ")
}

func (a *app) backupTargets() ([]backupTarget, bool) {
	var list []backupTarget
	ok := a.fetchInto("backup.target.list", nil, &list)
	return list, ok
}

func (a *app) backupTargetList(args []string) int {
	list, ok := a.backupTargets()
	if !ok {
		return 1
	}
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Where    string `json:"where"`
		Location string `json:"location"`
		Note     string `json:"note"`
	}
	rows := make([]row, 0, len(list))
	for _, t := range list {
		rows = append(rows, row{t.ID, t.Name, t.Kind, t.where(), t.Location, t.note()})
	}
	printRows(a, rows, __("No backup targets. Add one with: odac backup target add"))
	return 0
}

// backupTargetID resolves -t/--target (an id or a name), or asks.
func (a *app) backupTargetID(args []string) (string, int) {
	ref := parseArg(args, "-t", "--target")
	if ref == "" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref = args[0]
	}
	list, ok := a.backupTargets()
	if !ok {
		return "", 1
	}
	if ref == "" {
		if len(list) == 1 {
			return list[0].ID, 0
		}
		ref = a.question(__("Enter the backup target (id or name): "))
	}
	for _, t := range list {
		if t.ID == ref || strings.EqualFold(t.Name, ref) {
			return t.ID, 0
		}
	}
	fmt.Fprintln(a.errOut, __("Backup target %s not found.", ref))
	return "", 1
}

// backupTargetSpec builds a target spec from the flags. With asking set
// (a new target) a missing name, location or secret is asked for.
func (a *app) backupTargetSpec(args []string, asking bool) (map[string]any, error) {
	spec := map[string]any{}
	name := parseArg(args, "--name")
	if name == "" && asking {
		name = a.question(__("Enter a name for the target: "))
	}
	if name != "" {
		spec["name"] = name
	}
	put := func(m map[string]any, key, flag string) {
		if v := parseArg(args, flag); v != "" {
			m[key] = v
		}
	}
	secret := func(m map[string]any, key, flag, prompt string, required bool) {
		if v := parseArg(args, flag); v != "" {
			m[key] = v
			return
		}
		if asking && (required || m["username"] != nil) {
			if v := a.secret(prompt); v != "" {
				m[key] = v
			}
		}
	}

	kinds := 0
	if v := parseArg(args, "--disk"); v != "" {
		kinds++
		spec["disk"] = map[string]any{"path": v}
	}
	if v := parseArg(args, "--smb"); v != "" {
		kinds++
		addr, share, ok := strings.Cut(strings.TrimPrefix(v, "//"), "/")
		if !ok || addr == "" || share == "" {
			return nil, errors.New(__("An SMB share is written //<host>/<share>."))
		}
		m := map[string]any{"addr": addr, "share": share}
		put(m, "path", "--path")
		put(m, "username", "--username")
		put(m, "domain", "--domain")
		put(m, "version", "--version")
		secret(m, "password", "--password", __("Enter the SMB password: "), false)
		spec["smb"] = m
	}
	if v := parseArg(args, "--nfs"); v != "" {
		kinds++
		addr, export, ok := strings.Cut(v, ":")
		if !ok || addr == "" || export == "" {
			return nil, errors.New(__("An NFS export is written <host>:<export>."))
		}
		m := map[string]any{"addr": addr, "export": export}
		put(m, "path", "--path")
		put(m, "version", "--version")
		spec["nfs"] = m
	}
	if v := parseArg(args, "--s3"); v != "" {
		kinds++
		m := map[string]any{"bucket": v}
		put(m, "endpoint", "--endpoint")
		put(m, "region", "--region")
		put(m, "prefix", "--prefix")
		put(m, "accessKeyId", "--access-key")
		if m["endpoint"] == nil && asking {
			m["endpoint"] = a.question(__("Enter the S3 endpoint (e.g. s3.eu-central-1.amazonaws.com): "))
		}
		if m["accessKeyId"] == nil && asking {
			m["accessKeyId"] = a.question(__("Enter the S3 access key id: "))
		}
		secret(m, "secretAccessKey", "--secret-key", __("Enter the S3 secret key: "), true)
		if slices.Contains(args, "--no-tls") {
			m["disableTLS"] = true
		}
		spec["s3"] = m
	}
	if v := parseArg(args, "--webdav"); v != "" {
		kinds++
		m := map[string]any{"url": v}
		put(m, "username", "--username")
		secret(m, "password", "--password", __("Enter the WebDAV password: "), false)
		spec["webdav"] = m
	}
	if asking && kinds != 1 {
		return nil, errors.New(__("Name exactly one place: --disk, --smb, --nfs, --s3 or --webdav."))
	}
	if key := parseArg(args, "--key"); key != "" {
		spec["key"] = key
	}
	return spec, nil
}

type backupRecovery struct {
	ServerID   string       `json:"serverId"`
	ServerName string       `json:"serverName"`
	Target     backupTarget `json:"target"`
	Directory  string       `json:"directory"`
	Key        string       `json:"key"`
}

func (a *app) printRecovery(r backupRecovery) {
	fmt.Fprintln(a.out, __("Key: %s", color(r.Key, ansiYellow)))
	fmt.Fprintln(a.out, __("Target: %s (%s)", r.Target.Name, r.Target.where()))
	server := r.ServerID
	if r.ServerName != "" {
		server += " (" + r.ServerName + ")"
	}
	fmt.Fprintln(a.out, __("Server: %s", server))
	fmt.Fprintln(a.out, __("Directory on the target: %s", r.Directory))
}

// writeRecovery saves what a new server needs to open the backups. The
// file holds the key, so only its owner reads it.
func (a *app) writeRecovery(path string, raw json.RawMessage) bool {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintln(a.errOut, __("%s already exists.", path))
		return false
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		fmt.Fprintln(a.errOut, __("Failed to write %s: %s", path, err.Error()))
		return false
	}
	fmt.Fprintln(a.out, __("Saved to %s", path))
	if warning := ephemeralWarning(path); warning != "" {
		fmt.Fprintln(a.errOut, warning)
	}
	return true
}

func (a *app) backupTargetAdd(args []string) int {
	spec, err := a.backupTargetSpec(args, true)
	if err != nil {
		fmt.Fprintln(a.errOut, err.Error())
		return 1
	}
	resp, ok := a.fetch("backup.target.prepare", spec)
	if !ok {
		return 1
	}
	var pending struct {
		Token    string          `json:"token"`
		Recovery json.RawMessage `json:"recovery"`
	}
	var rec backupRecovery
	if json.Unmarshal(resp.Data, &pending) != nil || json.Unmarshal(pending.Recovery, &rec) != nil {
		fmt.Fprintln(a.errOut, __("The server sent an unexpected reply."))
		return 1
	}
	fmt.Fprintln(a.out, resp.Message)
	a.printRecovery(rec)
	if file := parseArg(args, "--recovery-file"); file != "" && !a.writeRecovery(file, pending.Recovery) {
		a.fetch("backup.target.cancel", map[string]any{"token": pending.Token})
		return 1
	}
	if !confirmed(a, args, __(`Type "yes" once the key is saved: `)) {
		a.fetch("backup.target.cancel", map[string]any{"token": pending.Token})
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	confirm := map[string]any{"token": pending.Token}
	if slices.Contains(args, "--adopt") {
		confirm["adopt"] = true
	}
	return a.callMessage("backup.target.confirm", []any{confirm})
}

// backupTargetUpdate sends only what the flags name, inside the target's
// own kind; the server keeps everything else.
func (a *app) backupTargetUpdate(args []string) int {
	id, code := a.backupTargetID(args)
	if code != 0 {
		return code
	}
	list, _ := a.backupTargets()
	kind := ""
	for _, t := range list {
		if t.ID == id {
			kind = t.Kind
		}
	}
	patch := map[string]any{"id": id}
	if name := parseArg(args, "--name"); name != "" {
		patch["name"] = name
	}
	creds := map[string]any{}
	set := func(key, flag string) {
		if v := parseArg(args, flag); v != "" {
			creds[key] = v
		}
	}
	switch kind {
	case "smb":
		set("username", "--username")
		set("password", "--password")
		set("domain", "--domain")
		set("version", "--version")
	case "nfs":
		set("version", "--version")
	case "s3":
		set("accessKeyId", "--access-key")
		set("secretAccessKey", "--secret-key")
		set("region", "--region")
		if slices.Contains(args, "--no-tls") {
			creds["disableTLS"] = true
		}
	case "webdav":
		set("username", "--username")
		set("password", "--password")
	}
	if len(creds) > 0 {
		patch[kind] = creds
	}
	if len(patch) == 1 {
		fmt.Fprintln(a.errOut, __("Nothing to change: give --name or new credentials."))
		return 1
	}
	return a.callMessage("backup.target.update", []any{patch})
}

func (a *app) backupTargetAdopt(args []string) int {
	id, code := a.backupTargetID(args)
	if code != 0 {
		return code
	}
	fmt.Fprintln(a.out, __("WARNING: Adopt only when this server wrote those backups, for example after the target was added again or ODAC's state was restored from an older copy. If a copy of this server still writes there, stop it or give it a new id first, or the conflict comes back."))
	if !confirmed(a, args, __(`Type "yes" to continue: `)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	return a.callMessage("backup.target.adopt", []any{map[string]any{"id": id}})
}

type backupServerDir struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Self       bool   `json:"self"`
	Updated    int64  `json:"updated"`
	Repository bool   `json:"repository"`
	Error      string `json:"error"`
}

func (a *app) printServerDirs(dirs []backupServerDir) {
	type row struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Updated string `json:"updated"`
		Note    string `json:"note"`
	}
	rows := make([]row, 0, len(dirs))
	for _, d := range dirs {
		note := d.Error
		switch {
		case d.Self:
			note = __("this server")
		case !d.Repository && note == "":
			note = __("no backups")
		}
		rows = append(rows, row{d.ID, d.Name, when(d.Updated), note})
	}
	printRows(a, rows, __("No server has written to this target yet."))
}

func (a *app) backupTargetServers(args []string) int {
	id, code := a.backupTargetID(args)
	if code != 0 {
		return code
	}
	var dirs []backupServerDir
	if !a.fetchInto("backup.target.servers", map[string]any{"id": id}, &dirs) {
		return 1
	}
	a.printServerDirs(dirs)
	return 0
}

func (a *app) backupKey(args []string) int {
	id, code := a.backupTargetID(withoutFlagValue(args, "--out"))
	if code != 0 {
		return code
	}
	resp, ok := a.fetch("backup.key", map[string]any{"id": id})
	if !ok {
		return 1
	}
	var rec backupRecovery
	if err := json.Unmarshal(resp.Data, &rec); err != nil {
		fmt.Fprintln(a.errOut, err)
		return 1
	}
	fmt.Fprintln(a.out, resp.Message)
	a.printRecovery(rec)
	if out := parseArg(args, "--out"); out != "" && !a.writeRecovery(out, resp.Data) {
		return 1
	}
	return 0
}

// --- jobs ---

type backupJob struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Target     string   `json:"target"`
	Apps       []string `json:"apps"`
	AllApps    bool     `json:"allApps"`
	Server     bool     `json:"server"`
	Every      int      `json:"every"`
	Density    float64  `json:"density"`
	MaxAgeDays int      `json:"maxAgeDays"`
	Budget     int64    `json:"budget"`
}

func (a *app) backupJobs() ([]backupJob, bool) {
	var list []backupJob
	ok := a.fetchInto("backup.job.list", nil, &list)
	return list, ok
}

func every(minutes int) string {
	switch {
	case minutes%(24*60) == 0:
		return __("%s d", minutes/(24*60))
	case minutes%60 == 0:
		return __("%s h", minutes/60)
	}
	return __("%s min", minutes)
}

func density(k float64) string {
	switch k {
	case 2:
		return "low"
	case 3.5:
		return "normal"
	case 7:
		return "high"
	}
	return strconv.FormatFloat(k, 'f', -1, 64)
}

func (j backupJob) sources() string {
	var parts []string
	if j.AllApps {
		parts = append(parts, __("all apps"))
	} else {
		parts = append(parts, j.Apps...)
	}
	if j.Server {
		parts = append(parts, __("ODAC's state"))
	}
	return strings.Join(parts, ", ")
}

func (a *app) backupJobList(args []string) int {
	jobs, ok := a.backupJobs()
	if !ok {
		return 1
	}
	targets, _ := a.backupTargets()
	names := map[string]string{}
	for _, t := range targets {
		names[t.ID] = t.Name
	}
	type row struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Target  string `json:"target"`
		Backs   string `json:"backs up"`
		Every   string `json:"every"`
		Density string `json:"density"`
		Limits  string `json:"limits"`
	}
	rows := make([]row, 0, len(jobs))
	for _, j := range jobs {
		var limits []string
		if j.MaxAgeDays > 0 {
			limits = append(limits, __("%s days", j.MaxAgeDays))
		}
		if j.Budget > 0 {
			limits = append(limits, size(j.Budget))
		}
		target := names[j.Target]
		if target == "" {
			target = j.Target
		}
		rows = append(rows, row{j.ID, j.Name, target, j.sources(), every(j.Every), density(j.Density), strings.Join(limits, ", ")})
	}
	printRows(a, rows, __("No backup jobs. Until there is one, the only copies are snapshots, which are lost with the server. Create one with: odac backup job create"))
	return 0
}

// backupJobID resolves -j/--job (an id or a name), or asks.
func (a *app) backupJobID(args []string) (string, int) {
	ref := parseArg(args, "-j", "--job")
	if ref == "" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref = args[0]
	}
	jobs, ok := a.backupJobs()
	if !ok {
		return "", 1
	}
	if ref == "" {
		if len(jobs) == 1 {
			return jobs[0].ID, 0
		}
		ref = a.question(__("Enter the backup job (id or name): "))
	}
	for _, j := range jobs {
		if j.ID == ref || strings.EqualFold(j.Name, ref) {
			return j.ID, 0
		}
	}
	fmt.Fprintln(a.errOut, __("Backup job %s not found.", ref))
	return "", 1
}

// parseEvery reads an interval as minutes, or as 30m, 6h, 1d.
func parseEvery(text string) (int, error) {
	if n, err := strconv.Atoi(text); err == nil {
		return n, nil
	}
	if days, ok := strings.CutSuffix(text, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return n * 24 * 60, nil
		}
	}
	d, err := time.ParseDuration(text)
	if err != nil || d%time.Minute != 0 {
		return 0, errors.New(__("An interval is minutes, or written like 30m, 6h or 1d."))
	}
	return int(d / time.Minute), nil
}

// backupJobSpec builds a job spec from the flags given.
func (a *app) backupJobSpec(args []string) (map[string]any, error) {
	spec := map[string]any{}
	if v := parseArg(args, "--name"); v != "" {
		spec["name"] = v
	}
	if v := parseArg(args, "--apps"); v != "" {
		spec["apps"] = splitHosts(v)
	}
	if slices.Contains(args, "--all") {
		spec["allApps"] = true
	}
	if slices.Contains(args, "--server") {
		spec["server"] = true
	}
	if slices.Contains(args, "--no-server") {
		spec["server"] = false
	}
	if v := parseArg(args, "--every"); v != "" {
		n, err := parseEvery(v)
		if err != nil {
			return nil, err
		}
		spec["every"] = n
	}
	if v := parseArg(args, "--density"); v != "" {
		if k, err := strconv.ParseFloat(v, 64); err == nil {
			spec["density"] = k
		} else {
			spec["density"] = strings.ToLower(v)
		}
	}
	if v := parseArg(args, "--max-age"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, errors.New(__("A maximum age is a number of days."))
		}
		spec["maxAgeDays"] = n
	}
	if v := parseArg(args, "--budget"); v != "" {
		n, err := resources.ParseBytes(v)
		if err != nil {
			return nil, err
		}
		spec["budget"] = n
	}
	return spec, nil
}

func (a *app) backupJobCreate(args []string) int {
	spec, err := a.backupJobSpec(args)
	if err != nil {
		fmt.Fprintln(a.errOut, err.Error())
		return 1
	}
	if spec["name"] == nil {
		spec["name"] = a.question(__("Enter a name for the job: "))
	}
	target, code := a.backupTargetID(withoutFlagValue(args, "--name"))
	if code != 0 {
		return code
	}
	spec["target"] = target
	if spec["apps"] == nil && spec["allApps"] == nil && spec["server"] == nil {
		apps := a.question(__("Enter the apps to back up (comma separated, or * for all): "))
		if strings.TrimSpace(apps) == "*" {
			spec["allApps"] = true
		} else {
			spec["apps"] = splitHosts(apps)
		}
	}
	return a.callMessage("backup.job.create", []any{spec})
}

func (a *app) backupJobUpdate(args []string) int {
	id, code := a.backupJobID(args)
	if code != 0 {
		return code
	}
	spec, err := a.backupJobSpec(args)
	if err != nil {
		fmt.Fprintln(a.errOut, err.Error())
		return 1
	}
	if len(spec) == 0 {
		fmt.Fprintln(a.errOut, __("Nothing to change."))
		return 1
	}
	spec["id"] = id
	return a.callMessage("backup.job.update", []any{spec})
}

func (a *app) backupJobDelete(args []string) int {
	id, code := a.backupJobID(args)
	if code != 0 {
		return code
	}
	req := map[string]any{"id": id}
	if slices.Contains(args, "--purge") {
		fmt.Fprintln(a.out, __("WARNING: --purge deletes every backup this job made. They cannot be brought back."))
		if !confirmed(a, args, __(`Type "yes" to continue: `)) {
			fmt.Fprintln(a.out, __("Aborted."))
			return 1
		}
		req["purge"] = true
	}
	return a.callMessage("backup.job.delete", []any{req})
}

func (a *app) backupRetiredList(args []string) int {
	var list []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Target  string `json:"target"`
		Deleted int64  `json:"deleted"`
		Purge   bool   `json:"purge"`
	}
	if !a.fetchInto("backup.retired.list", nil, &list) {
		return 1
	}
	type row struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Target  string `json:"target"`
		Deleted string `json:"deleted"`
		State   string `json:"state"`
	}
	rows := make([]row, 0, len(list))
	for _, r := range list {
		state := __("kept")
		if r.Purge {
			state = __("being deleted")
		}
		rows = append(rows, row{r.ID, r.Name, r.Target, when(r.Deleted), state})
	}
	printRows(a, rows, __("No deleted backup jobs with kept backups."))
	return 0
}

func (a *app) backupRetiredPurge(args []string) int {
	id := parseArg(args, "-j", "--job")
	if id == "" {
		id = a.question(__("Enter the id of the deleted job: "))
	}
	fmt.Fprintln(a.out, __("WARNING: This deletes every backup the job made. They cannot be brought back."))
	if !confirmed(a, args, __(`Type "yes" to continue: `)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	return a.callMessage("backup.retired.purge", []any{map[string]any{"id": id}})
}

// --- status ---

func (a *app) backupStatus(args []string) int {
	var st struct {
		Jobs []struct {
			Job            string   `json:"job"`
			Next           int64    `json:"next"`
			Running        string   `json:"running"`
			Failures       int      `json:"failures"`
			PostponedSince int64    `json:"postponedSince"`
			Waiting        []string `json:"waiting"`
			Manual         int64    `json:"manual"`
		} `json:"jobs"`
		Targets []struct {
			Target       string `json:"target"`
			NextMaintain int64  `json:"nextMaintain"`
			LastMaintain int64  `json:"lastMaintain"`
			Used         int64  `json:"used"`
			Garbage      int64  `json:"garbage"`
			Free         int64  `json:"free"`
		} `json:"targets"`
	}
	if !a.fetchInto("backup.status", nil, &st) {
		return 1
	}
	jobs, _ := a.backupJobs()
	jobNames := map[string]string{}
	for _, j := range jobs {
		jobNames[j.ID] = j.Name
	}
	targets, _ := a.backupTargets()
	targetNames := map[string]string{}
	for _, t := range targets {
		targetNames[t.ID] = t.Name
	}
	or := func(m map[string]string, id string) string {
		if m[id] != "" {
			return m[id]
		}
		return id
	}

	type jobRow struct {
		Job   string `json:"job"`
		Next  string `json:"next run"`
		State string `json:"state"`
	}
	jrows := []jobRow{}
	for _, j := range st.Jobs {
		var state []string
		if j.Running != "" {
			state = append(state, __("running %s", j.Running))
		}
		if j.Manual > 0 {
			state = append(state, __("asked to run at %s", when(j.Manual)))
		}
		if j.Failures > 0 {
			state = append(state, __("%s failed run(s) in a row", j.Failures))
		}
		if j.PostponedSince > 0 {
			state = append(state, __("postponed since %s: the server is busy", when(j.PostponedSince)))
		}
		if len(j.Waiting) > 0 {
			state = append(state, __("waiting for a deploy: %s", strings.Join(j.Waiting, ", ")))
		}
		jrows = append(jrows, jobRow{or(jobNames, j.Job), when(j.Next), strings.Join(state, "; ")})
	}
	printRows(a, jrows, __("No backup jobs."))

	type targetRow struct {
		Target  string `json:"target"`
		Used    string `json:"used"`
		Garbage string `json:"garbage"`
		Free    string `json:"free"`
		Last    string `json:"last maintenance"`
		Next    string `json:"next maintenance"`
	}
	trows := []targetRow{}
	for _, t := range st.Targets {
		trows = append(trows, targetRow{Target: or(targetNames, t.Target), Used: size(t.Used), Garbage: size(t.Garbage), Free: size(t.Free), Last: when(t.LastMaintain), Next: when(t.NextMaintain)})
	}
	if len(trows) > 0 {
		printRows(a, trows, "")
	}
	return 0
}

type backupRecord struct {
	Kind      string `json:"kind"`
	Run       string `json:"run"`
	Job       string `json:"job"`
	Target    string `json:"target"`
	Trigger   string `json:"trigger"`
	Status    string `json:"status"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Snapshots int    `json:"snapshots"`
	Bytes     int64  `json:"bytes"`
	Apps      []struct {
		App      string `json:"app"`
		Status   string `json:"status"`
		Warnings []struct {
			Message string `json:"message"`
		} `json:"warnings"`
		Error string `json:"error"`
	} `json:"apps"`
	Deleted int    `json:"deleted"`
	Error   string `json:"error"`
}

func (a *app) backupHistory(args []string) int {
	req := map[string]any{}
	if ref := parseArg(args, "-j", "--job"); ref != "" {
		id, code := a.backupJobID(args)
		if code != 0 {
			return code
		}
		req["job"] = id
	}
	limit := 20
	if v := parseArg(args, "--limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fmt.Fprintln(a.errOut, __("A limit is a positive number."))
			return 1
		}
		limit = n
	}
	req["limit"] = limit
	var list []backupRecord
	if !a.fetchInto("backup.history", req, &list) {
		return 1
	}
	jobs, _ := a.backupJobs()
	names := map[string]string{}
	for _, j := range jobs {
		names[j.ID] = j.Name
	}
	type row struct {
		Started string `json:"started"`
		What    string `json:"what"`
		Status  string `json:"status"`
		Took    string `json:"took"`
		Stored  string `json:"stored"`
		Note    string `json:"note"`
	}
	rows := make([]row, 0, len(list))
	for _, r := range list {
		what := __("maintenance of %s", r.Target)
		if r.Kind == "backup" {
			what = names[r.Job]
			if what == "" {
				what = r.Job
			}
			if r.Trigger != "" && r.Trigger != "schedule" {
				what += " (" + r.Trigger + ")"
			}
		}
		var notes []string
		if r.Error != "" {
			notes = append(notes, r.Error)
		}
		for _, app := range r.Apps {
			for _, w := range app.Warnings {
				notes = append(notes, app.App+": "+w.Message)
			}
		}
		if r.Deleted > 0 {
			notes = append(notes, __("%s old backup(s) deleted", r.Deleted))
		}
		stored := "-"
		if r.Snapshots > 0 {
			stored = __("%s snapshot(s), %s read", r.Snapshots, size(r.Bytes))
		}
		rows = append(rows, row{when(r.Start), what, r.Status, span(r.End - r.Start), stored, strings.Join(notes, "; ")})
	}
	printRows(a, rows, __("No backup has run yet."))
	return 0
}

func (a *app) backupCoverage(args []string) int {
	var c struct {
		Unprotected  []string `json:"unprotected"`
		MissingLinks []struct {
			Job    string `json:"job"`
			App    string `json:"app"`
			Linked string `json:"linked"`
		} `json:"missingLinks"`
		NoJobs    bool     `json:"noJobs"`
		OnlyLocal bool     `json:"onlyLocal"`
		SameDisk  []string `json:"sameDisk"`
		Conflicts []string `json:"conflicts"`
	}
	if !a.fetchInto("backup.coverage", nil, &c) {
		return 1
	}
	var lines []string
	if c.NoJobs {
		lines = append(lines, __("There is no backup job: the only copies are snapshots, which are lost with the server."))
	}
	if len(c.Unprotected) > 0 {
		lines = append(lines, __("These apps hold data that no job backs up: %s.", strings.Join(c.Unprotected, ", ")))
	}
	for _, l := range c.MissingLinks {
		lines = append(lines, __("Job %s backs up %s but not %s, which it is linked to: a restore would bring %s back without it.", l.Job, l.App, l.Linked, l.App))
	}
	if c.OnlyLocal {
		lines = append(lines, __("Every backup target is on this server. That does not survive the loss of the server, the account or ransomware; add a remote target."))
	}
	if len(c.SameDisk) > 0 {
		lines = append(lines, __("These targets are on the disk that holds the data itself, which only protects against deleting by mistake: %s.", strings.Join(c.SameDisk, ", ")))
	}
	if len(c.Conflicts) > 0 {
		lines = append(lines, __("Writes to these targets are stopped, because another machine writes under this server's id there (see odac backup target adopt): %s.", strings.Join(c.Conflicts, ", ")))
	}
	if len(lines) == 0 {
		fmt.Fprintln(a.out, __("Every app with data is backed up."))
		return 0
	}
	for _, l := range lines {
		fmt.Fprintln(a.out, color(" ! ", ansiYellow)+l)
	}
	return 0
}

// --- restore ---

type backupPoint struct {
	Run     string `json:"run"`
	Job     string `json:"job"`
	Trigger string `json:"trigger"`
	Time    int64  `json:"time"`
	Apps    []struct {
		App         string `json:"app"`
		Consistency string `json:"consistency"`
		Dump        bool   `json:"dump"`
	} `json:"apps"`
	Server bool `json:"server"`
}

func (a *app) printPoints(points []backupPoint) {
	type row struct {
		Run     string `json:"run"`
		Time    string `json:"time"`
		Trigger string `json:"trigger"`
		Apps    string `json:"apps"`
		State   string `json:"odac state"`
	}
	rows := make([]row, 0, len(points))
	for _, p := range points {
		var apps []string
		for _, app := range p.Apps {
			apps = append(apps, app.App+" ("+app.Consistency+")")
		}
		state := ""
		if p.Server {
			state = __("yes")
		}
		rows = append(rows, row{p.Run, when(p.Time), p.Trigger, strings.Join(apps, ", "), state})
	}
	printRows(a, rows, __("No backups to restore from."))
}

// pointRequest reads where restore points are listed from.
func (a *app) pointRequest(args []string) (map[string]any, int) {
	target, code := a.backupTargetID(args)
	if code != 0 {
		return nil, code
	}
	req := map[string]any{"target": target}
	if v := parseArg(args, "--server"); v != "" {
		req["server"] = v
	}
	if v := parseArg(args, "--key"); v != "" {
		req["key"] = v
	}
	return req, 0
}

func (a *app) backupPoints(args []string) int {
	req, code := a.pointRequest(args)
	if code != 0 {
		return code
	}
	if v := parseArg(args, "--app"); v != "" {
		req["app"] = v
	}
	var points []backupPoint
	if !a.fetchInto("backup.point.list", req, &points) {
		return 1
	}
	a.printPoints(points)
	return 0
}

type backupRestore struct {
	ID     string `json:"id"`
	Mode   string `json:"mode"`
	Target string `json:"target"`
	Server string `json:"server"`
	Run    string `json:"run"`
	Status string `json:"status"`
	Phase  string `json:"phase"`
	Point  int64  `json:"point"`
	Lost   int64  `json:"lost"`
	Apps   []struct {
		App         string   `json:"app"`
		As          string   `json:"as"`
		Method      string   `json:"method"`
		Consistency string   `json:"consistency"`
		Point       int64    `json:"point"`
		Lost        int64    `json:"lost"`
		Warnings    []string `json:"warnings"`
		Dropped     []string `json:"dropped"`
	} `json:"apps"`
	Restarted  []string `json:"restarted"`
	Safety     string   `json:"safety"`
	Error      string   `json:"error"`
	RolledBack bool     `json:"rolledBack"`
	Start      int64    `json:"start"`
	End        int64    `json:"end"`
}

// describe prints what a planned restore does, the part an operator must
// read before saying yes.
func (a *app) describeRestore(r backupRestore, skipSafety bool) {
	if r.Mode == "in-place" {
		fmt.Fprintln(a.out, __("Restore of run %s in place, back to %s.", r.Run, when(r.Point)))
	} else {
		fmt.Fprintln(a.out, __("Restore of run %s as new apps, from %s. The live apps are not touched.", r.Run, when(r.Point)))
	}
	for _, app := range r.Apps {
		line := "  " + app.App
		if app.As != app.App {
			line += " -> " + app.As
		}
		line += ": " + app.Method
		if app.Consistency != "" {
			line += ", " + app.Consistency
		}
		fmt.Fprintln(a.out, line)
		for _, w := range app.Warnings {
			fmt.Fprintln(a.out, color("    ! ", ansiYellow)+w)
		}
		if len(app.Dropped) > 0 {
			fmt.Fprintln(a.out, "    "+__("Not copied: %s", strings.Join(app.Dropped, ", ")))
		}
	}
	if r.Mode != "in-place" {
		return
	}
	fmt.Fprintln(a.out, color(__("WARNING: The apps are stopped until the restore ends, and everything they wrote since %s is lost (%s).", when(r.Point), span(r.Lost)), ansiYellow))
	if len(r.Restarted) > 0 {
		fmt.Fprintln(a.out, __("These apps link a restored app and are stopped during the restore too, their own data untouched: %s.", strings.Join(r.Restarted, ", ")))
	}
	if skipSafety {
		fmt.Fprintln(a.out, color(__("The current state is NOT backed up first: this restore cannot be undone."), ansiRed))
	} else {
		fmt.Fprintln(a.out, __("The current state is backed up first, so the restore itself can be undone."))
	}
}

// restorePollInterval is how often a followed restore is looked at; tests
// shrink it.
var restorePollInterval = 2 * time.Second

func (a *app) backupRestoreApps(args []string) int {
	req, code := a.pointRequest(args)
	if code != 0 {
		return code
	}
	run := parseArg(args, "--run")
	if run == "" {
		var points []backupPoint
		if !a.fetchInto("backup.point.list", req, &points) {
			return 1
		}
		if len(points) == 0 {
			fmt.Fprintln(a.errOut, __("No backups to restore from."))
			return 1
		}
		a.printPoints(points[:min(len(points), 10)])
		run = a.question(__("Enter the run to restore: "))
	}
	req["run"] = run
	if v := parseArg(args, "--apps"); v != "" {
		req["apps"] = splitHosts(v)
	}
	if slices.Contains(args, "--in-place") {
		req["mode"] = "in-place"
	} else if slices.Contains(args, "--new-app") {
		req["mode"] = "new-app"
	}
	if v := parseArg(args, "--name"); v != "" {
		names := map[string]any{}
		for _, pair := range strings.Split(v, ",") {
			from, to, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || from == "" || to == "" {
				fmt.Fprintln(a.errOut, __("New names are written old=new, comma separated."))
				return 1
			}
			names[from] = to
		}
		req["names"] = names
	}
	skipSafety := slices.Contains(args, "--skip-safety")
	if skipSafety {
		req["skipSafety"] = true
	}

	var plan backupRestore
	if !a.fetchInto("backup.restore.preview", req, &plan) {
		return 1
	}
	a.describeRestore(plan, skipSafety)
	if !confirmed(a, args, __(`Type "yes" to restore: `)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	resp, ok := a.fetch("backup.restore", req)
	if !ok {
		return 1
	}
	var started backupRestore
	if err := json.Unmarshal(resp.Data, &started); err != nil {
		fmt.Fprintln(a.errOut, err)
		return 1
	}
	fmt.Fprintln(a.out, resp.Message)
	if slices.Contains(args, "--no-wait") {
		fmt.Fprintln(a.out, __("Follow it with: odac backup restore list"))
		return 0
	}
	return a.followRestore(started.ID)
}

// followRestore prints a restore's phases until it ends. Leaving early
// leaves the restore running on the server.
func (a *app) followRestore(id string) int {
	last := ""
	for {
		var list []backupRestore
		if !a.fetchInto("backup.restore.list", nil, &list) {
			return 1
		}
		var cur *backupRestore
		for i := range list {
			if list[i].ID == id {
				cur = &list[i]
				break
			}
		}
		if cur == nil {
			fmt.Fprintln(a.errOut, __("Restore %s is gone from the list.", id))
			return 1
		}
		if state := cur.Status + " " + cur.Phase; state != last {
			last = state
			fmt.Fprintln(a.out, icon("progress")+strings.TrimSpace(state))
		}
		switch cur.Status {
		case "ok":
			fmt.Fprintln(a.out, icon("success")+__("Restore %s finished.", id))
			return 0
		case "failed":
			fmt.Fprintln(a.errOut, icon("errored")+__("Restore %s failed: %s", id, cur.Error))
			if cur.RolledBack {
				fmt.Fprintln(a.errOut, __("Everything it changed was put back."))
			}
			return 1
		}
		time.Sleep(restorePollInterval)
	}
}

func (a *app) backupRestoreList(args []string) int {
	var list []backupRestore
	if !a.fetchInto("backup.restore.list", nil, &list) {
		return 1
	}
	type row struct {
		ID      string `json:"id"`
		Started string `json:"started"`
		Mode    string `json:"mode"`
		Run     string `json:"run"`
		Apps    string `json:"apps"`
		Status  string `json:"status"`
		Note    string `json:"note"`
	}
	rows := make([]row, 0, len(list))
	for _, r := range list {
		var apps []string
		for _, app := range r.Apps {
			if app.As != app.App {
				apps = append(apps, app.App+" -> "+app.As)
			} else {
				apps = append(apps, app.App)
			}
		}
		note := r.Error
		if r.Phase != "" && note == "" {
			note = r.Phase
		}
		if r.RolledBack {
			note += " " + __("(rolled back)")
		}
		rows = append(rows, row{r.ID, when(r.Start), r.Mode, r.Run, strings.Join(apps, ", "), r.Status, strings.TrimSpace(note)})
	}
	printRows(a, rows, __("No restores yet."))
	return 0
}

// backupRestoreState is the move onto a freshly installed server: the
// target is added again with its old key first, then this picks the old
// server's directory and a run that holds ODAC's state, stages it and
// restarts ODAC to apply it. The apps' data comes back afterwards, in
// place.
func (a *app) backupRestoreState(args []string) int {
	if slices.Contains(args, "--cancel") {
		return a.callMessage("backup.server.cancel", nil)
	}
	targets, ok := a.backupTargets()
	if !ok {
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(a.errOut, __("Add the target the backups are on first, with the old server's key: odac backup target add ... --key <key>"))
		return 1
	}
	req, code := a.pointRequest(args)
	if code != 0 {
		return code
	}
	if req["server"] == nil {
		var dirs []backupServerDir
		if !a.fetchInto("backup.target.servers", map[string]any{"id": req["target"]}, &dirs) {
			return 1
		}
		var others []backupServerDir
		for _, d := range dirs {
			if !d.Self && d.Repository {
				others = append(others, d)
			}
		}
		if len(others) == 0 {
			fmt.Fprintln(a.errOut, __("No other server has backups on this target."))
			return 1
		}
		a.printServerDirs(others)
		server := others[0].ID
		if len(others) > 1 {
			server = a.question(__("Enter the id of the server to restore: "))
		}
		req["server"] = server
	}
	run := parseArg(args, "--run")
	if run == "" {
		var points []backupPoint
		if !a.fetchInto("backup.point.list", req, &points) {
			return 1
		}
		for _, p := range points {
			if p.Server {
				run = p.Run
				fmt.Fprintln(a.out, __("The newest copy of ODAC's state is from %s (run %s).", when(p.Time), p.Run))
				break
			}
		}
		if run == "" {
			fmt.Fprintln(a.errOut, __("No run on this target holds ODAC's own state. Only a job with --server stores it."))
			return 1
		}
	}
	req["run"] = run
	fmt.Fprintln(a.out, __("WARNING: This replaces ODAC's own state on this server (config, certificates, databases, mail) with the one from run %s. This server's backup targets and keys stay.", run))
	if !confirmed(a, args, __(`Type "yes" to continue: `)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	resp, ok := a.fetch("backup.server.restore", req)
	if !ok {
		return 1
	}
	fmt.Fprintln(a.out, resp.Message)
	next := fmt.Sprintf("odac backup restore apps -t %v --server %v --run %s --in-place", req["target"], req["server"], run)
	if req["key"] != nil {
		next += " --key <key>"
	}
	if !confirmed(a, args, __(`Type "yes" to restart ODAC now: `)) {
		fmt.Fprintln(a.out, __("Restart ODAC with odac restart when you are ready. Then bring the apps' data back with:"))
		fmt.Fprintln(a.out, "  "+next)
		return 0
	}
	a.booted = false
	a.boot()
	fmt.Fprintln(a.out, __("Once ODAC is back, bring the apps' data back with:"))
	fmt.Fprintln(a.out, "  "+next)
	return 0
}
