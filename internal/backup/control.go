package backup

import (
	"errors"

	"odac/internal/backup/settings"
)

// RunNow asks for a run of a job outside its schedule. The run waits for
// its turn like any other (one backup at a time, never beside a restore or
// maintenance in its repository) and leaves the schedule's own clock
// alone. The request is kept in the state file, so it survives a restart.
func (m *Manager) RunNow(job string) (JobStatus, error) {
	set := m.settings()
	if set == nil {
		return JobStatus{}, errors.New("backup: backups are not available on this server")
	}
	jobs, err := set.Jobs()
	if err != nil {
		return JobStatus{}, err
	}
	known := false
	for _, j := range jobs {
		known = known || j.ID == job
	}
	if !known {
		return JobStatus{}, refusal(settings.CodeNotFound, "Backup job %s not found.", job)
	}
	var out JobStatus
	var running bool
	err = m.state.update(func(d *stateData) {
		js := d.job(job)
		if js.Running != "" {
			running = true
			return
		}
		if js.Manual == 0 {
			js.Manual = ms(m.now())
		}
		out = JobStatus{Job: job, Next: js.Next, Failures: js.Failures, PostponedSince: js.PostponedSince, Manual: js.Manual}
	})
	if running {
		return JobStatus{}, refusal(settings.CodeInUse, "Backup job %s is running already.", job)
	}
	if err != nil {
		return JobStatus{}, err
	}
	m.poke()
	return out, nil
}

// Wake has the schedule look again now rather than at its next poll, so a
// job just created makes its first run at once.
func (m *Manager) Wake() { m.poke() }
