package backup

import (
	"context"
	"slices"
	"time"

	"odac/internal/backup/proto"
)

// retryAfter is how soon a failed job is tried again, by consecutive
// failures; never later than its next regular run.
var retryAfter = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}

func backoff(failures int) time.Duration {
	return retryAfter[min(max(failures, 1), len(retryAfter))-1]
}

// finishRun decides the run's status, tidies its target, updates the
// schedule and records it.
func (m *Manager) finishRun(ctx context.Context, rs runSpec, rec *RunRecord, apps []*appRun, snaps []proto.Snapshot) {
	rec.Snapshots = len(snaps)
	for _, s := range snaps {
		rec.Bytes += s.Bytes
	}
	var deployDeferred []string
	failures, deferred := 0, 0
	for _, a := range apps {
		switch {
		case a.deferred != nil:
			a.res.Status, a.res.Error = StatusDeferred, a.deferred.reason
			if a.deferred.deploy {
				deployDeferred = append(deployDeferred, a.res.App)
			} else {
				deferred++
			}
		case a.res.Status == StatusFailed:
			failures++
			if rec.Error == "" {
				rec.Error = a.res.App + ": " + a.res.Error
			}
		case a.res.Status == "":
			a.res.Status = StatusOK
		}
		rec.Apps = append(rec.Apps, a.res)
	}
	if rec.Server != nil && rec.Server.Status == StatusFailed {
		failures++
		if rec.Error == "" {
			rec.Error = "server: " + rec.Server.Error
		}
	}
	switch {
	case failures == 0 && deferred == 0 && len(deployDeferred) == 0:
		rec.Status = StatusOK
	case len(snaps) > 0:
		rec.Status = StatusPartial
	case failures == 0:
		rec.Status = StatusDeferred
	default:
		rec.Status = StatusFailed
	}

	now := m.now()
	// The run's own marks come first: the tidy after it must already see
	// it as partial, or it would read it as the newest complete run of
	// its job and drop older partial runs for a newer one that is not.
	err := m.state.update(func(d *stateData) {
		js := d.job(rs.job.ID)
		js.Running = ""
		regular := fromMs(js.Next)
		retry := time.Time{}
		switch rec.Status {
		case StatusOK, StatusPartial:
			js.Failures = 0
		case StatusFailed:
			js.Failures++
			retry = now.Add(backoff(js.Failures))
		}
		if deferred > 0 {
			retry = now.Add(m.t.dockerRetry)
		}
		if !retry.IsZero() && retry.Before(regular) {
			js.Next = ms(retry)
		}

		t := d.target(rs.job.Target)
		prev := d.FollowUps[rs.job.ID]
		clean := failures == 0 && deferred == 0
		switch {
		case len(deployDeferred) > 0:
			if rs.apps != nil && prev != nil {
				clean = clean && prev.Clean
			}
			d.FollowUps[rs.job.ID] = &followUp{Run: rs.run, Apps: deployDeferred, Until: js.Next, Scheduled: ms(now), Clean: clean}
		case rs.apps != nil:
			delete(d.FollowUps, rs.job.ID)
		}
		complete := rec.Status == StatusOK && (rs.apps == nil || prev != nil && prev.Clean)
		if len(snaps) > 0 || rs.apps != nil {
			if complete {
				t.Partial = slices.DeleteFunc(t.Partial, func(r string) bool { return r == rs.run })
			} else if !slices.Contains(t.Partial, rs.run) {
				t.Partial = append(t.Partial, rs.run)
			}
		}
		if len(snaps) > 0 && t.NextMaintain == 0 {
			t.NextMaintain = ms(now.Add(m.t.firstMaintain))
		}
	})
	if err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}

	if len(snaps) > 0 && !m.isReleased() {
		rec.Deleted = m.tidy(ctx, rs.job.Target, nil).deleted
	}
	rec.End = ms(m.now())
	if err := m.state.update(func(d *stateData) { d.History = append(d.History, *rec) }); err != nil {
		m.log.Error("Failed to save the backup state: %s", err.Error())
	}
	m.report(rs.job.Name, rec)
}

// report logs a record and hands it to the Hub's side.
func (m *Manager) report(name string, rec *RunRecord) {
	what := "Backup job " + name
	switch rec.Kind {
	case KindMaintain:
		what = "Maintenance of backup target " + rec.Target
	case KindVerify:
		what = "Verification of backup target " + rec.Target
	}
	switch rec.Status {
	case StatusOK:
		m.log.Log("%s finished: %s snapshots, %s bytes read", what, rec.Snapshots, rec.Bytes)
	case StatusDeferred:
		m.log.Log("%s deferred: %s", what, firstReason(rec))
	default:
		m.log.Warn("%s %s: %s", what, rec.Status, firstReason(rec))
	}
	for _, a := range rec.Apps {
		for _, w := range a.Warnings {
			m.log.Warn("%s: %s: %s", what, a.App, w.Message)
		}
	}
	if m.cfg.OnRecord != nil {
		m.cfg.OnRecord(*rec)
	}
}

func firstReason(rec *RunRecord) string {
	if rec.Error != "" {
		return rec.Error
	}
	for _, a := range rec.Apps {
		if a.Error != "" {
			return a.App + ": " + a.Error
		}
	}
	return rec.Status
}
