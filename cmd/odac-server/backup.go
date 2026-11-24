package main

import (
	"os"
	"path/filepath"
	"sync/atomic"

	"odac/internal/api"
	"odac/internal/appmgr"
	"odac/internal/backup"
	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
	"odac/internal/backup/service"
	"odac/internal/backup/settings"
	"odac/internal/config"
	"odac/internal/docker"
	"odac/internal/hub"
	"odac/internal/logx"
	"odac/internal/sysinfo"
)

// newBackups wires the backup manager, its settings and the runner. The
// server links none of the engine: every run goes through odac-backup in a
// helper container, or a child process when the server is not in a
// container (development). Nothing here talks to Docker; the manager
// waits for it in the background once started.
func newBackups(log *logx.Logger, cfg *config.Store, baseDir, serverID string, containers *docker.Client, apps *appmgr.Manager, relay *hubRelay) (*backup.Manager, *service.Service) {
	host := sysinfo.HostLoad()
	helperLimits, dumpLimits := runner.DefaultLimits(host.MemTotal, host.Cores)
	dockerSide := runner.FromClient(containers)
	planner, err := consistency.NewPlanner(consistency.Config{
		Prober: consistency.NewProber(dockerSide, dumpLimits),
		// Unknown memory reads as none, which turns a Redis fork down
		// and reads its last save instead.
		AvailableMemory: func() (int64, error) { return sysinfo.HostLoad().MemAvailable, nil },
	})
	if err != nil {
		log.Error("Backups are disabled:", err.Error())
		return nil, nil
	}
	local := !runner.InContainer()
	binary := filepath.Join(moduleBinDir(), "odac-backup")
	mgr, err := backup.New(backup.Config{
		Apps:    apps,
		States:  containers,
		Planner: planner,
		NewRunner: func() (backup.Runner, error) {
			r, err := runner.New(runner.Config{
				Docker: dockerSide, Local: local, Binary: binary,
				Limits: helperLimits, DumpLimits: dumpLimits,
				DumpRate: runner.DefaultDumpRate, DumpIdle: runner.DefaultDumpIdle,
			})
			if err != nil {
				return nil, err
			}
			return backup.FromRunner(r), nil
		},
		Load:       sysinfo.HostLoad,
		BaseDir:    baseDir,
		ServerID:   serverID,
		Control:    appControl{apps},
		Loader:     planner,
		OnRecord:   relay.record,
		OnRestore:  relay.restore,
		OnSnapshot: relay.snapshot,
	})
	if err != nil {
		log.Error("Backups are disabled:", err.Error())
		return nil, nil
	}
	set, err := settings.New(settings.Config{
		Store:      cfg,
		ServerID:   serverID,
		ServerName: serverName,
		Init:       mgr,
		Apps:       apps,
		Protected:  settings.ProtectedPaths(cfg.BaseDir(), os.Getenv("ODAC_HOST_ROOT")),
	})
	if err != nil {
		log.Error("Backups are disabled:", err.Error())
		return nil, nil
	}
	mgr.SetSettings(set)
	apps.SetDeployWatcher(mgr)
	apps.SetSnapshotter(snapshotHooks{mgr})
	svc, err := service.New(service.Config{Manager: mgr, Settings: set, ServerID: serverID, BaseDir: baseDir})
	if err != nil {
		log.Error("Backups are disabled:", err.Error())
		return nil, nil
	}
	return mgr, svc
}

// hubRelay hands the backup manager's events to the Hub, which is built
// after the backups (its command table calls them). Events before the Hub
// exists go nowhere: the first connection sends every list anyway.
type hubRelay struct{ h atomic.Pointer[hub.Hub] }

func (r *hubRelay) set(h *hub.Hub) { r.h.Store(h) }

func (r *hubRelay) trigger(lists ...string) {
	h := r.h.Load()
	if h == nil {
		return
	}
	for _, l := range lists {
		h.Trigger(l)
	}
}

// record re-sends what a finished run, maintenance or verification
// changed. A run that did not go well may also have stopped writes to its
// target (an owner conflict) or found damage, which the target list and
// the coverage show.
func (r *hubRelay) record(rec backup.RunRecord) {
	r.trigger("backup.status", "backup.history")
	switch {
	case rec.Status != backup.StatusOK && rec.Status != backup.StatusDeferred:
		r.trigger("backup.target.list", "backup.coverage")
	case rec.Kind == backup.KindVerify, rec.Kind == backup.KindMaintain:
		// A clean verification can clear a corruption warning, and a
		// maintenance can find another server's directory orphaned.
		r.trigger("backup.coverage")
	}
}

// restore re-sends the restores and the apps a finished restore changed.
func (r *hubRelay) restore(res backup.RestoreResult) {
	r.trigger("backup.restore.list", "backup.status", "app.list")
	if res.Target == backup.SnapshotTarget {
		r.trigger("snapshot.status")
	}
}

// snapshot re-sends what a snapshot or a maintenance of the snapshot
// repository changed; the restore points change only with what was
// stored or deleted.
func (r *hubRelay) snapshot(rec backup.SnapshotRecord) {
	r.trigger("snapshot.status", "snapshot.history")
	if rec.Status == backup.StatusOK || rec.Deleted > 0 {
		r.trigger("snapshot.list")
	}
}

// snapshotHooks adapts the backup manager to the app manager's snapshot
// seam.
type snapshotHooks struct{ m *backup.Manager }

// BeforeDeploy never hands a nil *PendingSnapshot through the interface,
// which would read as a snapshot that is there.
func (s snapshotHooks) BeforeDeploy(app, event string) (appmgr.DeploySnapshot, string) {
	p, note := s.m.BeforeDeploy(app, event)
	if p == nil {
		return nil, note
	}
	return p, ""
}

func (s snapshotHooks) BeforeDelete(app string) string { return s.m.BeforeDelete(app) }

// appHub is the app manager's Hub seam. Whenever the apps change, the
// coverage may too: a new app with data and no job is the warning the
// dashboard shows.
type appHub struct {
	*hub.Hub
	backups bool
}

func (a appHub) Trigger(event string) {
	a.Hub.Trigger(event)
	if a.backups && event == "app.list" {
		a.Hub.Trigger("backup.coverage")
	}
}

// registerBackupActions adds the backup.* and snapshot.* actions to the
// local API. Each
// takes one object argument, the payload of the Hub command of the same
// name, and a change re-sends the Hub lists that command would. Every one is
// closed to app tokens (api.AppMayCall). The staging of ODAC's own state is
// here and not on the Hub: it is for a freshly installed server and the CLI.
func registerBackupActions(apiSrv *api.Server, svc *service.Service, hubSvc *hub.Hub) {
	reg := func(action string, fn func(a api.Args) api.Result) {
		apiSrv.Register(action, func(a api.Args, _ api.Progress) (*api.Result, error) {
			r := fn(a)
			if lists, _ := hubSvc.Triggers(action); r.Status {
				for _, l := range lists {
					hubSvc.Trigger(l)
				}
			}
			return &r, nil
		})
	}
	reg("backup.target.list", func(api.Args) api.Result { return svc.Targets() })
	reg("backup.target.prepare", func(a api.Args) api.Result { return svc.PrepareTarget(a.At(0)) })
	reg("backup.target.confirm", func(a api.Args) api.Result { return svc.ConfirmTarget(a.At(0)) })
	reg("backup.target.cancel", func(a api.Args) api.Result { return svc.CancelTarget(a.At(0)) })
	reg("backup.target.update", func(a api.Args) api.Result { return svc.UpdateTarget(a.At(0)) })
	reg("backup.target.delete", func(a api.Args) api.Result { return svc.DeleteTarget(a.At(0)) })
	reg("backup.target.adopt", func(a api.Args) api.Result { return svc.AdoptTarget(a.At(0)) })
	reg("backup.target.servers", func(a api.Args) api.Result { return svc.TargetServers(a.At(0)) })
	reg("backup.key", func(a api.Args) api.Result { return svc.Key(a.Caller(), a.At(0)) })
	reg("backup.job.list", func(api.Args) api.Result { return svc.Jobs() })
	reg("backup.job.create", func(a api.Args) api.Result { return svc.CreateJob(a.At(0)) })
	reg("backup.job.update", func(a api.Args) api.Result { return svc.UpdateJob(a.At(0)) })
	reg("backup.job.delete", func(a api.Args) api.Result { return svc.DeleteJob(a.At(0)) })
	reg("backup.job.run", func(a api.Args) api.Result { return svc.RunJob(a.At(0)) })
	reg("backup.retired.list", func(api.Args) api.Result { return svc.Retired() })
	reg("backup.retired.purge", func(a api.Args) api.Result { return svc.PurgeRetired(a.At(0)) })
	reg("backup.coverage", func(api.Args) api.Result { return svc.Coverage() })
	reg("backup.status", func(api.Args) api.Result { return svc.Status() })
	reg("backup.history", func(a api.Args) api.Result { return svc.History(a.At(0)) })
	reg("backup.point.list", func(a api.Args) api.Result { return svc.Points(a.At(0)) })
	reg("backup.restore.preview", func(a api.Args) api.Result { return svc.PreviewRestore(a.At(0)) })
	reg("backup.restore", func(a api.Args) api.Result { return svc.StartRestore(a.At(0)) })
	reg("backup.restore.cancel", func(a api.Args) api.Result { return svc.CancelRestore(a.At(0)) })
	reg("backup.restore.list", func(api.Args) api.Result { return svc.Restores() })
	reg("backup.server.restore", func(a api.Args) api.Result { return svc.StageServerRestore(a.Caller(), a.At(0)) })
	reg("backup.server.pending", func(a api.Args) api.Result { return svc.PendingServerRestore(a.Caller()) })
	reg("backup.server.cancel", func(a api.Args) api.Result { return svc.CancelServerRestore(a.Caller()) })
	reg("snapshot.status", func(api.Args) api.Result { return svc.SnapshotStatus() })
	reg("snapshot.list", func(a api.Args) api.Result { return svc.SnapshotPoints(a.At(0)) })
	reg("snapshot.history", func(a api.Args) api.Result { return svc.SnapshotHistory(a.At(0)) })
	reg("snapshot.settings", func(api.Args) api.Result { return svc.SnapshotSettings() })
	reg("snapshot.settings.update", func(a api.Args) api.Result { return svc.UpdateSnapshotSettings(a.At(0)) })
	reg("snapshot.take", func(a api.Args) api.Result { return svc.TakeSnapshot(a.At(0)) })
	reg("snapshot.delete", func(a api.Args) api.Result { return svc.DeleteSnapshot(a.At(0)) })
	reg("snapshot.restore.preview", func(a api.Args) api.Result { return svc.PreviewSnapshotRestore(a.At(0)) })
	reg("snapshot.restore", func(a api.Args) api.Result { return svc.StartSnapshotRestore(a.At(0)) })
}

// serverName labels this server's directory on a target for a person
// browsing it: ODAC_SERVER_NAME, or the host name, which ODAC's container
// shares with the host because it runs in the host's network namespace.
func serverName() string {
	if n := os.Getenv("ODAC_SERVER_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// appControl adapts the app manager to the restore's narrow interface.
type appControl struct{ m *appmgr.Manager }

func (a appControl) HoldApp(name string) (backup.AppHold, error) {
	h, err := a.m.HoldApp(name)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (a appControl) SetAppActive(name string, active bool) bool {
	return a.m.SetAppActive(name, active)
}

func (a appControl) AppDefinition(name string) (map[string]any, bool) { return a.m.AppDefinition(name) }

func (a appControl) UniqueAppName(base string) string { return a.m.UniqueAppName(base) }

func (a appControl) AppsDir() string { return a.m.AppsDir() }

func (a appControl) CreateRestoredApp(def map[string]any, name string, renames map[string]string, group string) ([]string, error) {
	r, err := a.m.CreateRestoredApp(def, name, renames, group)
	if err != nil {
		return nil, err
	}
	return r.Dropped, nil
}

func (a appControl) DeleteRestoredApp(name string) error { return a.m.DeleteRestoredApp(name) }
