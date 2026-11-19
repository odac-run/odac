package thin

import (
	"cmp"
	"slices"
	"time"
)

// Snapshot is one restore point in the server's own snapshot repository.
// App names the series: the app whose deploy, deletion or schedule took
// it (a group snapshot of linked apps is filed under the app that
// triggered it). Event is empty for a periodic snapshot and otherwise
// names what took it; each app keeps its last few of each kind.
type Snapshot struct {
	ID    string
	App   string
	Event string
	Time  time.Time
}

// SnapshotRules are the system-managed retention of snapshots.
type SnapshotRules struct {
	// Periodic thins periodic snapshots per app.
	Periodic Policy
	// KeepEvents is how many snapshots of each event kind an app keeps.
	KeepEvents int
	// Gone is how long a deleted app's snapshots outlive the app, which is
	// the window in which it can be brought back.
	Gone time.Duration
}

// DefaultSnapshotRules: periodic snapshots on the normal density for a
// week, the last five of each event per app, a deleted app for a week.
var DefaultSnapshotRules = SnapshotRules{
	Periodic:   Policy{K: Normal, MaxAge: 7 * 24 * time.Hour},
	KeepEvents: 5,
	Gone:       7 * 24 * time.Hour,
}

// ThinSnapshots returns the snapshots the rules delete. gone holds, for
// each app that no longer exists, when it was deleted; an app absent from
// it is live. Once a deleted app's window has passed, all of its snapshots
// go.
func ThinSnapshots(now time.Time, snaps []Snapshot, gone map[string]time.Time, r SnapshotRules) []string {
	var del []string
	for app, s := range snapshotsByApp(snaps) {
		if at, ok := gone[app]; ok && now.Sub(at) > r.Gone {
			for _, sn := range s {
				del = append(del, sn.ID)
			}
			continue
		}
		periodic, events := splitEvents(s)
		if r.Periodic.Validate() == nil {
			del = append(del, thinSeries(now, periodic, r.Periodic)...)
		}
		for _, ev := range events {
			if extra := len(ev) - max(r.KeepEvents, 1); extra > 0 {
				for _, sn := range ev[:extra] {
					del = append(del, sn.ID)
				}
			}
		}
	}
	slices.Sort(del)
	return del
}

// SqueezeSnapshots orders what ThinSnapshots left for deletion when the
// snapshot budget is exceeded: periodic snapshots first (the same order as
// Squeeze, then each app's newest periodic one, oldest first), then every
// event snapshot except each app's newest, oldest first, and last each
// app's newest event snapshot, oldest first. Unlike Squeeze it names every
// snapshot, because at the floor the apps and their databases come before
// any snapshot of them.
func SqueezeSnapshots(now time.Time, snaps []Snapshot) []string {
	var periodic [][]Backup
	var anchors, older, newest []Backup
	for _, s := range snapshotsByApp(snaps) {
		p, events := splitEvents(s)
		if len(p) > 0 {
			periodic = append(periodic, p)
			anchors = append(anchors, p[len(p)-1])
		}
		var ev []Backup
		for _, e := range events {
			ev = append(ev, e...)
		}
		if len(ev) == 0 {
			continue
		}
		sortSeries(ev)
		older = append(older, ev[:len(ev)-1]...)
		newest = append(newest, ev[len(ev)-1])
	}
	order := squeeze(now, periodic)
	for _, group := range [][]Backup{anchors, older, newest} {
		sortSeries(group)
		for _, b := range group {
			order = append(order, b.ID)
		}
	}
	return order
}

func snapshotsByApp(snaps []Snapshot) map[string][]Snapshot {
	out := map[string][]Snapshot{}
	for _, s := range snaps {
		out[s.App] = append(out[s.App], s)
	}
	return out
}

// splitEvents separates an app's periodic series from its event series,
// one per event kind, each sorted oldest first.
func splitEvents(s []Snapshot) (periodic []Backup, events [][]Backup) {
	byEvent := map[string][]Backup{}
	for _, sn := range s {
		b := Backup{ID: sn.ID, Job: sn.App, Time: sn.Time}
		if sn.Event == "" {
			periodic = append(periodic, b)
		} else {
			byEvent[sn.Event] = append(byEvent[sn.Event], b)
		}
	}
	sortSeries(periodic)
	kinds := make([]string, 0, len(byEvent))
	for k := range byEvent {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, cmp.Compare)
	for _, k := range kinds {
		ev := byEvent[k]
		sortSeries(ev)
		events = append(events, ev)
	}
	return periodic, events
}
