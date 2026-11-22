package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// snapshotCommand is `odac snapshot`. Snapshots are the system's own: the
// operator can see them, take, delete and restore one, and change their
// share of the disk or turn them off.
func snapshotCommand() *command {
	return &command{
		title: "SNAPSHOT",
		sub: []entry{
			{"list", &command{
				description: "List the snapshots a restore can return to, of one app with --app",
				args:        []string{"--app"},
				action:      (*app).snapshotList,
			}},
			{"status", &command{
				description: "Show the snapshots' share of the disk, their schedule and their warnings",
				action:      (*app).snapshotStatus,
			}},
			{"history", &command{
				description: "List the snapshots taken and given up",
				args:        []string{"--app", "--limit"},
				action:      (*app).snapshotHistory,
			}},
			{"take", &command{
				description: "Take a snapshot of an app and the apps it links, now",
				args:        []string{"--app"},
				action: func(a *app, args []string) int {
					name := parseArg(args, "--app")
					if name == "" {
						name = a.question(__("Enter the app: "))
					}
					return a.callMessage("snapshot.take", []any{map[string]any{"app": name}})
				},
			}},
			{"delete", &command{
				description: "Delete a snapshot",
				args:        []string{"--run", "--yes"},
				action:      (*app).snapshotDelete,
			}},
			{"restore", &command{
				description: "Restore a snapshot, in place by default (a deleted app comes back as a new app). Shows what it gives up first",
				args:        []string{"--run", "--app", "--apps", "--in-place", "--new-app", "--name", "--skip-safety", "--yes", "--no-wait"},
				action:      (*app).snapshotRestore,
			}},
			{"settings", &command{
				description: "Show the snapshot settings, or change them: --on, --off, --max-disk <percent>",
				args:        []string{"--on", "--off", "--max-disk"},
				action:      (*app).snapshotSettings,
			}},
		},
	}
}

type snapshotPoint struct {
	Run   string `json:"run"`
	App   string `json:"app"`
	Event string `json:"event"`
	Time  int64  `json:"time"`
	Apps  []struct {
		App         string `json:"app"`
		Consistency string `json:"consistency"`
	} `json:"apps"`
	Server  bool  `json:"server"`
	Bytes   int64 `json:"bytes"`
	Gone    int64 `json:"gone"`
	Expires int64 `json:"expires"`
}

func (a *app) snapshotPoints(app string) ([]snapshotPoint, bool) {
	var payload any
	if app != "" {
		payload = map[string]any{"app": app}
	}
	var list []snapshotPoint
	return list, a.fetchInto("snapshot.list", payload, &list)
}

func (a *app) printSnapshots(list []snapshotPoint) {
	type row struct {
		Run   string `json:"run"`
		Time  string `json:"time"`
		App   string `json:"app"`
		Event string `json:"event"`
		Data  string `json:"data"`
		Note  string `json:"note"`
	}
	rows := make([]row, 0, len(list))
	for _, p := range list {
		var data []string
		for _, x := range p.Apps {
			data = append(data, x.App+" ("+x.Consistency+")")
		}
		if p.Server {
			data = append(data, __("ODAC's own state"))
		}
		note := ""
		if p.Gone > 0 {
			note = __("deleted app, kept until %s", when(p.Expires))
		}
		rows = append(rows, row{p.Run, when(p.Time), p.App, p.Event, strings.Join(data, ", "), note})
	}
	printRows(a, rows, __("No snapshots."))
}

func (a *app) snapshotList(args []string) int {
	list, ok := a.snapshotPoints(parseArg(args, "--app"))
	if !ok {
		return 1
	}
	a.printSnapshots(list)
	return 0
}

func (a *app) snapshotStatus(args []string) int {
	var st struct {
		Enabled      bool  `json:"enabled"`
		MaxDiskPct   int   `json:"maxDiskPct"`
		Available    bool  `json:"available"`
		Used         int64 `json:"used"`
		Garbage      int64 `json:"garbage"`
		Limit        int64 `json:"limit"`
		Free         int64 `json:"free"`
		Total        int64 `json:"total"`
		Floor        int64 `json:"floor"`
		NextMaintain int64 `json:"nextMaintain"`
		Running      *struct {
			Run   string `json:"run"`
			App   string `json:"app"`
			Event string `json:"event"`
			Start int64  `json:"start"`
		} `json:"running"`
		Periodic []struct {
			App   string `json:"app"`
			Next  int64  `json:"next"`
			Every int    `json:"every"`
		} `json:"periodic"`
		Deploys []struct {
			App    string `json:"app"`
			At     int64  `json:"at"`
			Status string `json:"status"`
			Run    string `json:"run"`
			Reason string `json:"reason"`
		} `json:"deploys"`
		Warnings []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			At      int64  `json:"at"`
		} `json:"warnings"`
	}
	if !a.fetchInto("snapshot.status", nil, &st) {
		return 1
	}
	state := __("on")
	if !st.Enabled {
		state = __("off")
	}
	fmt.Fprintln(a.out, __("Snapshots: %s, at most %s%s of the disk", state, strconv.Itoa(st.MaxDiskPct), "%"))
	used := size(st.Used)
	if st.Garbage > 0 {
		used += " " + __("(%s on its way back)", size(st.Garbage))
	}
	fmt.Fprintln(a.out, __("Repository: %s, limit now %s", used, size(st.Limit)))
	if st.Total > 0 {
		fmt.Fprintln(a.out, __("Disk: %s free of %s, floor %s", size(st.Free), size(st.Total), size(st.Floor)))
	}
	if st.NextMaintain > 0 {
		fmt.Fprintln(a.out, __("Next maintenance: %s", when(st.NextMaintain)))
	}
	if r := st.Running; r != nil {
		fmt.Fprintln(a.out, icon("progress")+__("Taking snapshot %s (%s) of %s since %s", r.Run, r.Event, r.App, when(r.Start)))
	}
	for _, w := range st.Warnings {
		fmt.Fprintln(a.out, color("! "+w.Message, ansiYellow))
	}
	type periodic struct {
		App   string `json:"app"`
		Next  string `json:"next"`
		Every string `json:"every"`
	}
	var rows []periodic
	for _, p := range st.Periodic {
		e := "-"
		if p.Every > 0 {
			e = every(p.Every)
		}
		rows = append(rows, periodic{p.App, when(p.Next), e})
	}
	fmt.Fprintln(a.out)
	printRows(a, rows, __("No app gets periodic snapshots."))
	for _, d := range st.Deploys {
		if d.Status != "ok" {
			fmt.Fprintln(a.out, color(__("The last deploy of %s (%s) went on without a snapshot: %s", d.App, when(d.At), d.Reason), ansiYellow))
		}
	}
	return 0
}

func (a *app) snapshotHistory(args []string) int {
	req := map[string]any{}
	if v := parseArg(args, "--app"); v != "" {
		req["app"] = v
	}
	if v := parseArg(args, "--limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fmt.Fprintln(a.errOut, __("--limit takes a positive number."))
			return 1
		}
		req["limit"] = n
	}
	var list []struct {
		Kind   string `json:"kind"`
		Run    string `json:"run"`
		App    string `json:"app"`
		Event  string `json:"event"`
		Status string `json:"status"`
		Reason string `json:"reason"`
		Start  int64  `json:"start"`
		End    int64  `json:"end"`
		Added  int64  `json:"added"`
		Error  string `json:"error"`
	}
	if !a.fetchInto("snapshot.history", req, &list) {
		return 1
	}
	type row struct {
		Started string `json:"started"`
		What    string `json:"what"`
		Run     string `json:"run"`
		Status  string `json:"status"`
		Took    string `json:"took"`
		Added   string `json:"added"`
		Note    string `json:"note"`
	}
	rows := make([]row, 0, len(list))
	for _, r := range list {
		what := r.Event + " " + r.App
		switch r.Kind {
		case "maintain":
			what = __("maintenance")
		case "verify":
			what = __("verification")
		}
		rows = append(rows, row{when(r.Start), strings.TrimSpace(what), r.Run, r.Status, span(r.End - r.Start), size(r.Added), r.Error})
	}
	printRows(a, rows, __("No snapshots yet."))
	return 0
}

func (a *app) snapshotDelete(args []string) int {
	run := parseArg(args, "--run")
	if run == "" {
		list, ok := a.snapshotPoints("")
		if !ok {
			return 1
		}
		if len(list) == 0 {
			fmt.Fprintln(a.out, __("No snapshots."))
			return 0
		}
		a.printSnapshots(list)
		run = a.question(__("Enter the run to delete: "))
	}
	if !confirmed(a, args, __(`Type "yes" to delete snapshot %s: `, run)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	return a.callMessage("snapshot.delete", []any{map[string]any{"run": run}})
}

func (a *app) snapshotRestore(args []string) int {
	run := parseArg(args, "--run")
	if run == "" {
		list, ok := a.snapshotPoints(parseArg(args, "--app"))
		if !ok {
			return 1
		}
		if len(list) == 0 {
			fmt.Fprintln(a.errOut, __("No snapshots to restore from."))
			return 1
		}
		a.printSnapshots(list[:min(len(list), 10)])
		run = a.question(__("Enter the run to restore: "))
	}
	req := map[string]any{"run": run}
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
	if !a.fetchInto("snapshot.restore.preview", req, &plan) {
		return 1
	}
	a.describeRestore(plan, skipSafety)
	if !confirmed(a, args, __(`Type "yes" to restore: `)) {
		fmt.Fprintln(a.out, __("Aborted."))
		return 1
	}
	// The mode the preview settled on, so the restore cannot differ from
	// what was shown.
	req["mode"] = plan.Mode
	resp, ok := a.fetch("snapshot.restore", req)
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

func (a *app) snapshotSettings(args []string) int {
	patch := map[string]any{}
	switch {
	case slices.Contains(args, "--on") && slices.Contains(args, "--off"):
		fmt.Fprintln(a.errOut, __("Snapshots are either --on or --off."))
		return 1
	case slices.Contains(args, "--on"):
		patch["enabled"] = true
	case slices.Contains(args, "--off"):
		patch["enabled"] = false
	}
	if v := parseArg(args, "--max-disk"); v != "" {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
		if err != nil {
			fmt.Fprintln(a.errOut, __("--max-disk takes a percentage."))
			return 1
		}
		patch["maxDiskPct"] = n
	}
	if len(patch) > 0 {
		return a.callMessage("snapshot.settings.update", []any{patch})
	}
	var s struct {
		Enabled    bool `json:"enabled"`
		MaxDiskPct int  `json:"maxDiskPct"`
	}
	if !a.fetchInto("snapshot.settings", nil, &s) {
		return 1
	}
	state := __("on")
	if !s.Enabled {
		state = __("off")
	}
	fmt.Fprintln(a.out, __("Snapshots: %s, at most %s%s of the disk", state, strconv.Itoa(s.MaxDiskPct), "%"))
	return 0
}
