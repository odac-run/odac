package appmgr

import (
	"os"
	"path/filepath"
	"strings"

	"odac/internal/applog"
	"odac/internal/docker"
)

// A git app built before the builder reported its runtime has no `runtime`
// on its record, and nothing writes one until it happens to be rebuilt. The
// backfill derives it once per process from what the host holds (the
// checkout's strategy, the running image's Env) and stores it where a build
// would, so app.list reports it through the normal path. It claims only what
// those artifacts show (docker.ObservedRuntime) and records nothing when
// there is any doubt that the checkout is what the container runs.
//
// It is queued by Check, the reload that already walks every app, for a
// running app no operation holds, and drained by one worker so a host coming
// up with many such apps probes them one at a time. It never takes the
// processing lock: a redeploy arriving meanwhile must not be refused, since
// the Cloud treats a refusal as a failed deployment. Instead every processing
// claim bumps opEpoch, and the write is dropped if one happened since the
// job was queued.

// backfillJob is one queued app and the opEpoch it was queued under.
type backfillJob struct {
	id    float64
	epoch uint64
}

// needsRuntimeBackfill reports whether a record is a candidate: a live,
// non-dev git app with no valid runtime. A dev app runs its source from a
// bind mount that is edited live, so its checkout says nothing about the
// image. Caller holds cfg.View or cfg.Mutate.
//
// A record that names its build (buildIDKey) is never a candidate: it was
// written by a build or a rollback that knew the runtime, so an absent one
// is that build's answer, and after a rollback the checkout is ahead of the
// running image, which is exactly what the backfill cannot tell.
func needsRuntimeBackfill(app map[string]any) bool {
	if app["type"] != "git" || app["dev"] == true || app["_deleted"] == true {
		return false
	}
	if id, _ := app[buildIDKey].(string); id != "" {
		return false
	}
	rt, _ := app["runtime"].(map[string]any)
	strategy, _ := rt["strategy"].(string)
	return !docker.RuntimeStrategies[strategy]
}

// queueRuntimeBackfill queues an app for its one attempt of this process and
// starts the worker when none is draining.
func (m *Manager) queueRuntimeBackfill(id float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtimeTried[id] || m.processing[id] {
		return
	}
	m.runtimeTried[id] = true
	m.backfillQueue = append(m.backfillQueue, backfillJob{id: id, epoch: m.opEpoch[id]})
	if !m.backfillRunning {
		m.backfillRunning = true
		m.spawn(m.drainRuntimeBackfill)
	}
}

// drainRuntimeBackfill runs queued jobs in order, then tells the Cloud once
// if any record changed.
func (m *Manager) drainRuntimeBackfill() {
	wrote := false
	for {
		m.mu.Lock()
		if len(m.backfillQueue) == 0 {
			m.backfillRunning = false
			m.mu.Unlock()
			break
		}
		job := m.backfillQueue[0]
		m.backfillQueue = m.backfillQueue[1:]
		m.mu.Unlock()
		if m.backfillRuntime(job) {
			wrote = true
		}
	}
	if wrote {
		m.hubTrigger("app.list")
	}
}

// untouchedSince reports whether no operation holds the app now or has
// claimed it since the job was queued.
func (m *Manager) untouchedSince(job backfillJob, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.processing[job.id] && !m.creating[name] && m.opEpoch[job.id] == job.epoch
}

// backfillRuntime makes one attempt and reports whether it wrote the record.
func (m *Manager) backfillRuntime(job backfillJob) bool {
	var name, commit string
	m.cfg.View(func() {
		if app := m.getLocked(job.id); app != nil && needsRuntimeBackfill(app) {
			name, _ = app["name"].(string)
			commit = recordedCommit(app)
		}
	})
	if name == "" || !m.untouchedSince(job, name) {
		return false
	}

	appsPath := m.appsPath()
	dir := filepath.Join(appsPath, name)
	if appsPath == "" || !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(appsPath)+string(filepath.Separator)) {
		return false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}

	status := m.deps.Docker.GetStatus(name)
	if !status.Running || status.ImageID == "" {
		return false
	}
	if !m.checkoutIsRunning(name, dir, commit) {
		return false
	}
	rt, err := m.deps.Docker.ObservedRuntime(dir, status.ImageID)
	if err != nil {
		return false
	}
	row := rt.Map()
	if row == nil {
		return false
	}

	wrote := false
	m.cfg.Mutate(func() {
		app := m.getLocked(job.id)
		if app == nil || app["name"] != name || !needsRuntimeBackfill(app) || recordedCommit(app) != commit {
			return
		}
		// A build that ran while this probed owns the field, even if it
		// failed and left it empty.
		if !m.untouchedSince(job, name) {
			return
		}
		app["runtime"] = row
		m.saveAppsLocked()
		wrote = true
	})
	return wrote
}

// checkoutIsRunning decides whether the checkout is the source of the
// running image. A recorded commit must be the checkout's HEAD: a redeploy
// that fetched and then failed to build leaves the checkout ahead. Without
// one (records from before commits were kept), the newest build attempt must
// have succeeded, since a failed or unfinished one may have fetched past the
// image; no build history at all is no evidence either way.
func (m *Manager) checkoutIsRunning(name, dir, commit string) bool {
	if commit != "" {
		head, err := m.deps.Docker.CheckoutHead(dir)
		return err == nil && head == commit
	}
	return applog.New(m.logsRoot, name).LastAttemptStatus() == "success"
}

// recordedCommit is the record's commitSha when it is a full one, "" else.
func recordedCommit(app map[string]any) string {
	sha, _ := app["commitSha"].(string)
	if sha = strings.ToLower(sha); fullCommitRE.MatchString(sha) {
		return sha
	}
	return ""
}
