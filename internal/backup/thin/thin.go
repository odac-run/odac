// Package thin decides which backups go. It is pure: it is handed the
// backups, the time and the rules, and returns IDs. It reads no disk,
// network or clock, so every decision is reproducible in a test.
//
// The rule is continuous rather than tiered: the gap between a kept backup
// and its neighbour grows with its age, gap ≈ age/k. It is anchored to the
// calendar, not to the age. Buckets are aligned dyadic intervals of
// absolute time (unit << level from the Unix epoch), and a backup's bucket
// is the largest such interval whose newest edge is at least k times its
// width in the past. Over time a bucket can only merge with its neighbour,
// never shift. Each bucket keeps its oldest backup, which therefore stays
// the survivor when buckets merge. Thinning is path independent: running
// it every hour and running it once a year later delete exactly the same
// backups, and nothing deleted would ever have been wanted back.
package thin

import (
	"cmp"
	"errors"
	"slices"
	"time"
)

// unit is the smallest bucket. Backups younger than k units are all kept.
const unit = int64(time.Minute / time.Second)

// maxLevel bounds the bucket ladder: unit << 40 is about two million years.
const maxLevel = 40

// Preset densities offered by the Hub. For an hourly job over ten years
// they keep 39, 62 and 107 backups; normal is close to what a fixed
// hourly/daily/weekly/monthly/yearly scheme keeps, about 60.
const (
	Low    = 2
	Normal = 3.5
	High   = 7
)

// Backup is one restore point: everything one job run stored, deleted
// together so that an app and the database it links to never come back
// from different moments.
type Backup struct {
	ID   string
	Job  string
	Time time.Time
}

// Policy is one job's retention.
type Policy struct {
	// K is the density: a kept backup's distance to its older neighbour is
	// about its age divided by K.
	K float64
	// MaxAge, when set, drops backups older than it.
	MaxAge time.Duration
}

// Validate reports a policy the rule cannot apply.
func (p Policy) Validate() error {
	if !(p.K > 0 && p.K <= 1000) {
		return errors.New("thin: density must be above 0 and at most 1000")
	}
	if p.MaxAge < 0 {
		return errors.New("thin: negative maximum age")
	}
	return nil
}

// Thin returns the IDs the rule deletes. Each job is thinned with its own
// policy. A job without a policy (a deleted job whose backups were kept) is
// left alone, and no job's decision ever names another job's backup. Every
// job keeps its newest backup whatever its age, so a job that has been
// failing for longer than MaxAge still has something to restore.
func Thin(now time.Time, backups []Backup, policies map[string]Policy) []string {
	var del []string
	for job, series := range byJob(backups) {
		p, ok := policies[job]
		if !ok || p.Validate() != nil {
			continue
		}
		del = append(del, thinSeries(now, series, p)...)
	}
	slices.Sort(del)
	return del
}

// thinSeries applies one policy to one job's backups, sorted oldest first.
func thinSeries(now time.Time, series []Backup, p Policy) []string {
	var del []string
	newest := len(series) - 1
	type key struct{ level, index int64 }
	seen := map[key]bool{}
	for i, b := range series {
		if i == newest {
			break
		}
		if p.MaxAge > 0 && now.Sub(b.Time) > p.MaxAge {
			del = append(del, b.ID)
			continue
		}
		level, index, ok := bucket(now.Unix(), b.Time.Unix(), p.K)
		if !ok {
			continue
		}
		k := key{level, index}
		if seen[k] {
			del = append(del, b.ID)
			continue
		}
		seen[k] = true
	}
	return del
}

// bucket finds the largest aligned interval holding t whose newest edge is
// at least k widths before now. The condition only gets harder as the
// level grows (the width doubles while the edge can only move closer to
// now), so the first failure ends the search. ok is false for a backup too
// young for even one unit: it is kept as is.
func bucket(now, t int64, k float64) (level, index int64, ok bool) {
	level = -1
	for l := int64(0); l <= maxLevel; l++ {
		w := unit << l
		i := floorDiv(t, w)
		end := (i + 1) * w
		if float64(w)*k > float64(now-end) {
			break
		}
		level, index = l, i
	}
	return level, index, level >= 0
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// byJob groups backups per job, each group sorted oldest first. Equal times
// are ordered by ID so that the result never depends on input order.
func byJob(backups []Backup) map[string][]Backup {
	out := map[string][]Backup{}
	for _, b := range backups {
		out[b.Job] = append(out[b.Job], b)
	}
	for _, s := range out {
		sortSeries(s)
	}
	return out
}

func sortSeries(s []Backup) {
	slices.SortFunc(s, func(a, b Backup) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}
