package thin

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func snap(app, event string, at time.Time) Snapshot {
	return Snapshot{ID: fmt.Sprintf("%s-%s-%d", app, event, at.Unix()), App: app, Event: event, Time: at}
}

func snapIDs(s []Snapshot) []string {
	out := make([]string, len(s))
	for i, sn := range s {
		out[i] = sn.ID
	}
	return out
}

func TestSnapshotEventsKeepTheLastFivePerKind(t *testing.T) {
	var snaps []Snapshot
	for i := range 8 {
		snaps = append(snaps, snap("web", "deploy", epoch.Add(time.Duration(i)*time.Hour)))
	}
	snaps = append(snaps, snap("web", "manual", epoch), snap("db", "deploy", epoch))
	now := epoch.Add(30 * 24 * time.Hour)

	got := ThinSnapshots(now, snaps, nil, DefaultSnapshotRules)
	want := snapIDs(snaps[:3])
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want the three oldest deploys %v", got, want)
	}
}

func TestPeriodicSnapshotsThinAndExpire(t *testing.T) {
	var snaps []Snapshot
	for h := range 24 * 14 {
		snaps = append(snaps, snap("db", "", epoch.Add(time.Duration(h)*time.Hour)))
	}
	now := snaps[len(snaps)-1].Time
	del := ThinSnapshots(now, snaps, nil, DefaultSnapshotRules)
	kept := map[string]bool{}
	for _, s := range snaps {
		kept[s.ID] = !slices.Contains(del, s.ID)
	}
	for _, s := range snaps {
		if now.Sub(s.Time) > 7*24*time.Hour && kept[s.ID] {
			t.Fatalf("periodic snapshot of age %v kept", now.Sub(s.Time))
		}
	}
	n := 0
	for _, k := range kept {
		if k {
			n++
		}
	}
	if n < 15 || n > 40 {
		t.Fatalf("a week of hourly periodic snapshots kept %d", n)
	}
	if !kept[snaps[len(snaps)-1].ID] {
		t.Fatal("the newest periodic snapshot was deleted")
	}
}

func TestDeletedAppSnapshotsLastSevenDays(t *testing.T) {
	deletedAt := epoch.Add(10 * time.Hour)
	snaps := []Snapshot{
		snap("old", "deploy", epoch),
		snap("old", "", epoch.Add(time.Hour)),
		snap("old", "delete", deletedAt),
		snap("live", "deploy", epoch),
	}
	gone := map[string]time.Time{"old": deletedAt}

	if got := ThinSnapshots(deletedAt.Add(6*24*time.Hour), snaps, gone, DefaultSnapshotRules); len(got) != 0 {
		t.Fatalf("a deleted app lost %v inside its window", got)
	}
	got := ThinSnapshots(deletedAt.Add(8*24*time.Hour), snaps, gone, DefaultSnapshotRules)
	want := snapIDs(snaps[:3])
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("after the window deleted %v, want all of the app's snapshots %v", got, want)
	}
}

func TestSqueezeSnapshotsOrder(t *testing.T) {
	now := epoch.Add(7 * 24 * time.Hour)
	var snaps []Snapshot
	for h := range 48 {
		snaps = append(snaps, snap("web", "", now.Add(-time.Duration(h)*time.Hour)))
		snaps = append(snaps, snap("db", "", now.Add(-time.Duration(h)*time.Hour-time.Minute)))
	}
	events := []Snapshot{
		snap("web", "deploy", epoch.Add(time.Hour)),
		snap("web", "deploy", epoch.Add(2*time.Hour)),
		snap("db", "deploy", epoch),
		snap("web", "manual", epoch.Add(3*time.Hour)),
	}
	snaps = append(snaps, events...)
	order := SqueezeSnapshots(now, snaps)

	all := snapIDs(snaps)
	got := slices.Clone(order)
	slices.Sort(got)
	slices.Sort(all)
	if !slices.Equal(got, all) {
		t.Fatalf("squeeze named %d of %d snapshots", len(got), len(all))
	}
	tail := order[len(order)-len(events):]
	want := []string{events[0].ID, events[1].ID, events[2].ID, events[3].ID}
	if !slices.Equal(tail, want) {
		t.Fatalf("event order %v, want older events oldest first, then each app's newest %v", tail, want)
	}
	anchors := order[len(order)-len(events)-2 : len(order)-len(events)]
	if !slices.Equal(anchors, []string{snaps[1].ID, snaps[0].ID}) {
		t.Fatalf("each app's newest periodic snapshot must close the periodic part, got %v", anchors)
	}
}
