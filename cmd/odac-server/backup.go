package main

import (
	"os"
	"path/filepath"

	"odac/internal/appmgr"
	"odac/internal/backup"
	"odac/internal/backup/consistency"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
	"odac/internal/config"
	"odac/internal/docker"
	"odac/internal/logx"
	"odac/internal/sysinfo"
)

// newBackups wires the backup manager, its settings and the runner. The
// server links none of the engine: every run goes through odac-backup in a
// helper container, or a child process when the server is not in a
// container (development). Nothing here talks to Docker; the manager
// waits for it in the background once started.
func newBackups(log *logx.Logger, cfg *config.Store, baseDir, serverID string, containers *docker.Client, apps *appmgr.Manager) *backup.Manager {
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
		return nil
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
		Load:    sysinfo.HostLoad,
		BaseDir: baseDir,
	})
	if err != nil {
		log.Error("Backups are disabled:", err.Error())
		return nil
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
		return nil
	}
	mgr.SetSettings(set)
	apps.SetDeployWatcher(mgr)
	return mgr
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
