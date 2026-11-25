package thin

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func hourly(job string, hours int) []Backup {
	out := make([]Backup, hours)
	for h := range out {
		out[h] = Backup{ID: fmt.Sprintf("%s-%d", job, h), Job: job, Time: epoch.Add(time.Duration(h) * time.Hour)}
	}
	return out
}

func without(s []Backup, ids []string) []Backup {
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	var out []Backup
	for _, b := range s {
		if !gone[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

func ids(s []Backup) []string {
	out := make([]string, len(s))
	for i, b := range s {
		out[i] = b.ID
	}
	slices.Sort(out)
	return out
}

// randomJob is a job with irregular gaps (missed runs, manual runs) over
// about two years.
func randomJob(r *rand.Rand, job string) []Backup {
	var out []Backup
	t := epoch
	for i := range 3000 {
		t = t.Add(time.Duration(r.Int64N(int64(12*time.Hour))) + time.Minute)
		out = append(out, Backup{ID: fmt.Sprintf("%s-%d", job, i), Job: job, Time: t})
	}
	return out
}

// The stability the plan asks for: thinning every day and thinning once at
// the end delete the same backups, so a backup that survives today is
// never deleted for a reason that a later run would not also have, and
// nothing deleted would have been wanted back.
func TestThinningIsPathIndependent(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, k := range []float64{Low, Normal, High, 0.7, 13} {
		all := randomJob(r, "j")
		policies := map[string]Policy{"j": {K: k}}
		end := all[len(all)-1].Time

		var kept []Backup
		next := 0
		for now := epoch; !now.After(end); now = now.Add(time.Duration(r.Int64N(int64(48*time.Hour))) + time.Hour) {
			for next < len(all) && !all[next].Time.After(now) {
				kept = append(kept, all[next])
				next++
			}
			kept = without(kept, Thin(now, kept, policies))
		}
		kept = append(kept, all[next:]...)
		later := end.Add(400 * 24 * time.Hour)
		kept = without(kept, Thin(later, kept, policies))
		once := without(all, Thin(later, all, policies))

		if !slices.Equal(ids(kept), ids(once)) {
			t.Fatalf("k=%v: incremental thinning kept %d backups, one pass %d", k, len(kept), len(once))
		}
	}
}

// A survivor only ever gives way to an older backup, so each job's oldest
// backup is never lost to the density rule, only to MaxAge.
func TestOldestSurvivesDensity(t *testing.T) {
	all := hourly("j", 24*365*3)
	now := all[len(all)-1].Time.Add(10 * 365 * 24 * time.Hour)
	del := Thin(now, all, map[string]Policy{"j": {K: Low}})
	if slices.Contains(del, all[0].ID) {
		t.Fatal("density rule deleted the oldest backup")
	}
	del = Thin(now, all, map[string]Policy{"j": {K: Low, MaxAge: 365 * 24 * time.Hour}})
	if !slices.Contains(del, all[0].ID) {
		t.Fatal("MaxAge kept a backup past it")
	}
}

func TestPresetCountsOverTenYears(t *testing.T) {
	all := hourly("j", 24*365*10)
	now := all[len(all)-1].Time
	for _, c := range []struct {
		k        float64
		min, max int
	}{{Low, 30, 48}, {Normal, 55, 68}, {High, 95, 120}} {
		kept := len(all) - len(Thin(now, all, map[string]Policy{"j": {K: c.k}}))
		if kept < c.min || kept > c.max {
			t.Errorf("k=%v kept %d backups over ten years, want %d..%d", c.k, kept, c.min, c.max)
		}
	}
}

// The gap between kept backups tracks age/k: never much denser or much
// sparser than the rule says, wherever in the history it is.
func TestGapsTrackAge(t *testing.T) {
	all := hourly("j", 24*365*10)
	now := all[len(all)-1].Time
	kept := without(all, Thin(now, all, map[string]Policy{"j": {K: Normal}}))
	for i := 1; i < len(kept); i++ {
		gap := kept[i].Time.Sub(kept[i-1].Time)
		age := now.Sub(kept[i].Time)
		if gap > time.Hour && float64(gap) > 4*float64(age)/Normal+float64(time.Hour) {
			t.Errorf("gap %v at age %v is far sparser than age/k", gap, age)
		}
		if gap > 2*time.Hour && float64(gap) < float64(age)/(4*Normal) {
			t.Errorf("gap %v at age %v is far denser than age/k", gap, age)
		}
	}
}

func TestNewestIsAlwaysKept(t *testing.T) {
	all := hourly("j", 50)
	now := all[len(all)-1].Time.Add(20 * 365 * 24 * time.Hour)
	del := Thin(now, all, map[string]Policy{"j": {K: Low, MaxAge: time.Hour}})
	if len(del) != len(all)-1 || slices.Contains(del, all[len(all)-1].ID) {
		t.Fatalf("a job failing past MaxAge must keep exactly its newest backup, deleted %d of %d", len(del), len(all))
	}
}

func TestYoungBackupsAreAllKept(t *testing.T) {
	var all []Backup
	for i := range 30 {
		all = append(all, Backup{ID: fmt.Sprint(i), Job: "j", Time: epoch.Add(time.Duration(i) * time.Second)})
	}
	if del := Thin(epoch.Add(time.Minute), all, map[string]Policy{"j": {K: Normal}}); len(del) != 0 {
		t.Fatalf("backups younger than k minutes were thinned: %v", del)
	}
}

// A job is thinned with its own policy and only ever names its own
// backups; a job without a policy (deleted, its backups kept) loses none.
func TestJobsAreIsolated(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	a, b, kept := randomJob(r, "a"), randomJob(r, "b"), randomJob(r, "kept")
	all := slices.Concat(a, b, kept)
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	now := epoch.Add(3 * 365 * 24 * time.Hour)
	policies := map[string]Policy{"a": {K: Low}, "b": {K: High, MaxAge: 90 * 24 * time.Hour}}

	got := Thin(now, all, policies)
	if len(got) == 0 {
		t.Fatal("nothing was thinned")
	}
	want := slices.Concat(
		Thin(now, a, map[string]Policy{"a": policies["a"]}),
		Thin(now, b, map[string]Policy{"b": policies["b"]}),
	)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatal("thinning several jobs together differs from thinning each alone")
	}
	keptIDs := ids(kept)
	for _, id := range got {
		if _, found := slices.BinarySearch(keptIDs, id); found {
			t.Fatalf("a job without a policy lost %s", id)
		}
	}
	if Thin(now, all, map[string]Policy{"a": {K: 0}}) != nil {
		t.Fatal("an invalid policy thinned")
	}
}

func TestPolicyValidate(t *testing.T) {
	for _, p := range []Policy{{K: 0}, {K: -1}, {K: 2000}, {K: Normal, MaxAge: -time.Hour}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if err := (Policy{K: Normal, MaxAge: time.Hour}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBucketBeforeEpoch(t *testing.T) {
	old := time.Date(1960, 6, 1, 0, 0, 0, 0, time.UTC)
	all := []Backup{
		{ID: "a", Job: "j", Time: old},
		{ID: "b", Job: "j", Time: old.Add(time.Minute)},
		{ID: "c", Job: "j", Time: epoch},
	}
	if del := Thin(epoch, all, map[string]Policy{"j": {K: Normal}}); !slices.Equal(del, []string{"b"}) {
		t.Fatalf("deleted %v, want [b]", del)
	}
}
