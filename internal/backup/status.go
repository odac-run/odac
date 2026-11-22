package backup

import "slices"

// History returns the newest records first, of one job when job is set,
// at most limit of them (zero for all that are kept).
func (m *Manager) History(job string, limit int) []RunRecord {
	var out []RunRecord
	m.state.view(func(d *stateData) {
		for i := len(d.History) - 1; i >= 0; i-- {
			r := d.History[i]
			if job != "" && r.Job != job {
				continue
			}
			r.Apps = slices.Clone(r.Apps)
			out = append(out, r)
			if limit > 0 && len(out) == limit {
				return
			}
		}
	})
	return out
}

// JobStatus is where a job stands in the schedule. Times are Unix
// milliseconds.
type JobStatus struct {
	Job     string `json:"job"`
	Next    int64  `json:"next"`
	Running string `json:"running,omitempty"`
	// Failures counts the failed runs in a row.
	Failures       int   `json:"failures,omitempty"`
	PostponedSince int64 `json:"postponedSince,omitempty"`
	// Waiting are apps whose dump a deploy cancelled, stored once it ends.
	Waiting []string `json:"waiting,omitempty"`
	// Manual is when a run the operator asked for was requested, while it
	// waits for its turn.
	Manual int64 `json:"manual,omitempty"`
}

// TargetStatus is a target's last measurement and maintenance.
type TargetStatus struct {
	Target       string `json:"target"`
	NextMaintain int64  `json:"nextMaintain,omitempty"`
	LastMaintain int64  `json:"lastMaintain,omitempty"`
	Used         int64  `json:"used,omitempty"`
	Garbage      int64  `json:"garbage,omitempty"`
	Free         int64  `json:"free,omitempty"`
	Total        int64  `json:"total,omitempty"`
}

// Status reports the schedule, sorted by job and target id.
func (m *Manager) Status() ([]JobStatus, []TargetStatus) {
	var jobs []JobStatus
	var targets []TargetStatus
	m.state.view(func(d *stateData) {
		for id, js := range d.Jobs {
			s := JobStatus{Job: id, Next: js.Next, Running: js.Running, Failures: js.Failures, PostponedSince: js.PostponedSince, Manual: js.Manual}
			if f := d.FollowUps[id]; f != nil {
				s.Waiting = slices.Clone(f.Apps)
			}
			jobs = append(jobs, s)
		}
		for id, t := range d.Targets {
			targets = append(targets, TargetStatus{Target: id, NextMaintain: t.NextMaintain, LastMaintain: t.LastMaintain, Used: t.Used, Garbage: t.Garbage, Free: t.Free, Total: t.Total})
		}
	})
	slices.SortFunc(jobs, func(a, b JobStatus) int { return cmpString(a.Job, b.Job) })
	slices.SortFunc(targets, func(a, b TargetStatus) int { return cmpString(a.Target, b.Target) })
	return jobs, targets
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
