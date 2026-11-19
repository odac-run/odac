package thin

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func TestSqueezeNamesEveryBackupButEachNewest(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	a, b, other := randomJob(r, "a"), randomJob(r, "b"), randomJob(r, "other")
	all := slices.Concat(a, b, other)
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	now := epoch.Add(3 * 365 * 24 * time.Hour)

	order := Squeeze(now, all, map[string]bool{"a": true, "b": true})
	want := slices.Concat(ids(a[:len(a)-1]), ids(b[:len(b)-1]))
	slices.Sort(want)
	got := slices.Clone(order)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("squeeze named %d backups, want every one of the listed jobs but their newest (%d)", len(got), len(want))
	}
	if !slices.Equal(order, Squeeze(now, all, map[string]bool{"a": true, "b": true})) {
		t.Fatal("squeeze order is not deterministic")
	}
}

// Hourly backups everywhere, but an extra burst of manual runs a month ago:
// that burst is where backups are densest for their age, so it goes first.
func TestSqueezeTakesTheDensestRegionFirst(t *testing.T) {
	now := epoch.Add(365 * 24 * time.Hour)
	var all []Backup
	for d := range 365 {
		all = append(all, Backup{ID: fmt.Sprintf("day-%d", d), Job: "j", Time: epoch.Add(time.Duration(d) * 24 * time.Hour)})
	}
	burstAt := now.Add(-30 * 24 * time.Hour).Add(time.Hour)
	var burst []string
	for i := range 10 {
		id := fmt.Sprintf("burst-%d", i)
		burst = append(burst, id)
		all = append(all, Backup{ID: id, Job: "j", Time: burstAt.Add(time.Duration(i) * time.Minute)})
	}
	order := Squeeze(now, all, map[string]bool{"j": true})
	for _, id := range order[:9] {
		if !slices.Contains(burst, id) {
			t.Fatalf("squeeze took %s before the burst: %v", id, order[:9])
		}
	}
}

// Under the density rule itself, squeezing lowers the density evenly: the
// most recent hours are not emptied first, and the oldest backup stays
// until only it and the newest are left.
func TestSqueezeLowersDensityEvenly(t *testing.T) {
	all := hourly("j", 24*365*10)
	now := all[len(all)-1].Time
	kept := without(all, Thin(now, all, map[string]Policy{"j": {K: High}}))
	order := Squeeze(now, kept, map[string]bool{"j": true})
	half := without(kept, order[:len(kept)/2])

	recent, older := 0, 0
	for _, b := range half {
		if now.Sub(b.Time) < 7*24*time.Hour {
			recent++
		} else {
			older++
		}
	}
	if recent < 3 || older < 15 {
		t.Fatalf("after squeezing half: %d backups in the last week, %d before it", recent, older)
	}
	if half[0].ID != kept[0].ID {
		t.Fatal("squeezing half lost the oldest backup")
	}
	if order[len(order)-1] != kept[0].ID {
		t.Fatal("the oldest backup is not the last to go")
	}
}

func TestSqueezeSmallSeries(t *testing.T) {
	now := epoch.Add(time.Hour)
	one := []Backup{{ID: "x", Job: "j", Time: epoch}}
	if got := Squeeze(now, one, map[string]bool{"j": true}); len(got) != 0 {
		t.Fatalf("a job's only backup was offered: %v", got)
	}
	two := append(one, Backup{ID: "y", Job: "j", Time: epoch.Add(time.Minute)})
	if got := Squeeze(now, two, map[string]bool{"j": true}); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("got %v, want [x]", got)
	}
}

func TestExcess(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct {
		name string
		s    Space
		want int64
	}{
		{"fits", Space{Used: 10 * gb, Quota: 20 * gb, Free: 500 * gb, Total: 1000 * gb}, 0},
		{"over quota", Space{Used: 30 * gb, Quota: 20 * gb, Free: 500 * gb, Total: 1000 * gb}, 10 * gb},
		{"garbage on its way back", Space{Used: 30 * gb, Garbage: 10 * gb, Quota: 20 * gb, Free: 500 * gb, Total: 1000 * gb}, 0},
		{"below the 10% floor", Space{Used: 400 * gb, Free: 60 * gb, Total: 1000 * gb}, 40 * gb},
		{"floor with garbage", Space{Used: 400 * gb, Garbage: 30 * gb, Free: 60 * gb, Total: 1000 * gb}, 10 * gb},
		{"small disk uses 5 GB", Space{Used: 10 * gb, Free: 2 * gb, Total: 20 * gb}, 3 * gb},
		{"floor under a quota", Space{Used: 10 * gb, Quota: 50 * gb, Free: 50 * gb, Total: 1000 * gb}, 50 * gb},
		{"S3 without quota", Space{Used: 900 * gb, Free: -1}, 0},
		{"S3 over quota", Space{Used: 900 * gb, Quota: 800 * gb, Free: -1}, 100 * gb},
		{"garbage beyond used", Space{Used: 5 * gb, Garbage: 50 * gb, Quota: 1 * gb, Free: -1}, 0},
	} {
		if got := c.s.Excess(); got != c.want {
			t.Errorf("%s: excess %d, want %d", c.name, got, c.want)
		}
	}
	if Floor(0) != 5<<30 || Floor(1000<<30) != 100<<30 {
		t.Fatal("floor is not max(10%, 5 GB)")
	}
}
