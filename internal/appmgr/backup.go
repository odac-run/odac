package appmgr

import (
	"path/filepath"
	"strings"

	"odac/internal/backup/consistency"
	"odac/internal/backup/settings"
)

// BackupApps reports every app as the backup settings see it, for the
// warnings about stateful apps no job backs up and links a job leaves out.
// An app is stateful when it has a volume other than the config files
// ODAC writes from its recipe, or a backup declaration.
func (m *Manager) BackupApps() []settings.App {
	appsPath := m.appsPath()
	var out []settings.App
	m.cfg.View(func() {
		for _, app := range m.apps {
			if app["_deleted"] == true {
				continue
			}
			name, _ := app["name"].(string)
			if name == "" {
				continue
			}
			a := settings.App{Name: name, Stateful: app["backup"] != nil}
			for _, v := range toMounts(app["volumes"]) {
				if !configFile(appsPath, v.Host) {
					a.Stateful = true
				}
			}
			if env, _ := app["env"].(map[string]any); isNewEnvStructure(env) {
				for _, l := range toCmd(env["linked"]) {
					if l != "" && l != name {
						a.Linked = append(a.Linked, l)
					}
				}
			}
			out = append(out, a)
		}
	})
	return out
}

// configFile reports a volume that is one of the files writeConfigFiles
// puts under <apps>/<app>/configs, which the recipe recreates.
func configFile(appsPath, host string) bool {
	if appsPath == "" || host == "" {
		return false
	}
	rel, err := filepath.Rel(appsPath, host)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) >= 3 && parts[1] == "configs"
}

// BackupApp returns what the app record says about one app for a backup:
// its image, its data volumes and its recipe's backup declaration. The
// container's own state (image id, environment) is the backup manager's to
// read from the engine. ok is false for an app that does not exist. The
// config files ODAC writes from the recipe are left out: the recipe makes
// them again, and a file is not a directory a backup can read.
func (m *Manager) BackupApp(name string) (consistency.App, bool) {
	appsPath := m.appsPath()
	var (
		out consistency.App
		ok  bool
	)
	m.cfg.View(func() {
		for _, app := range m.apps {
			if n, _ := app["name"].(string); n != name || app["_deleted"] == true {
				continue
			}
			ok = true
			out = consistency.App{Name: name, Container: name}
			out.Image, _ = app["image"].(string)
			out.Cmd = toCmd(app["cmd"])
			for _, v := range toMounts(app["volumes"]) {
				if v.Host != "" && v.Container != "" && !configFile(appsPath, v.Host) {
					out.Volumes = append(out.Volumes, consistency.Volume{Host: v.Host, Container: v.Container})
				}
			}
			if app["backup"] != nil {
				if spec, err := consistency.Parse(app); err == nil {
					out.Spec = spec
				} else {
					m.log.Error("Ignoring the backup declaration of %s: %s", name, err.Error())
				}
			}
			return
		}
	})
	return out, ok
}
