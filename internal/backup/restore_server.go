package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
)

// ODAC's own state (config, certificates, its databases, mail) is restored
// in two steps, because the running server holds all of it open: the
// state is first restored beside the live one (StageServerRestore), and
// the next start of the server swaps it in before anything reads it
// (ApplyServerRestore). It is meant for a freshly installed server taking
// over from one that is gone, and refuses a server that has apps.

// keptModules are the config modules a server restore leaves as they are:
// server describes this machine (the watchdog rewrites it from its own
// copy), and backup holds this server's targets and keys, the ones the
// restore itself was made with, whose owner sequence files stay with this
// server.
var keptModules = []string{"server", "backup"}

// ServerRestoreRequest names the run whose copy of ODAC's state is
// restored; Server and Key are as in a RestoreRequest.
type ServerRestoreRequest struct {
	Target string `json:"target"`
	Server string `json:"server,omitempty"`
	Key    string `json:"-"`
	Run    string `json:"run"`
}

// ServerRestore is a staged restore of ODAC's own state. Times are Unix
// milliseconds.
type ServerRestore struct {
	ID     string   `json:"id"`
	Target string   `json:"target"`
	Server string   `json:"server,omitempty"`
	Run    string   `json:"run"`
	Dirs   []string `json:"dirs"`
	Point  int64    `json:"point"`
	Staged int64    `json:"staged"`
	// Applied lists the directories already swapped in, so a start that
	// dies in the middle finishes the swap the next time.
	Applied []string `json:"applied,omitempty"`
	// Previous is where the state it replaced is kept.
	Previous string `json:"previous,omitempty"`
}

func serverRestoreRoot(base string) string { return filepath.Join(base, "backup", "server-restore") }

func pendingFile(base string) string { return filepath.Join(serverRestoreRoot(base), "pending.json") }

// StageServerRestore restores ODAC's state from a run beside the live one
// and leaves it for the next start of the server to apply. The server
// must have no apps: their records would be replaced by the restored
// ones. A staged restore that was not applied yet is replaced.
func (m *Manager) StageServerRestore(ctx context.Context, req ServerRestoreRequest) (*ServerRestore, error) {
	if len(m.cfg.Apps.BackupApps()) > 0 {
		return nil, restoreErr("ODAC's own state is restored only onto a freshly installed server, and this one has apps.")
	}
	if req.Target == "" || req.Run == "" {
		return nil, restoreErr("A restore names a target and a run.")
	}
	a, err := m.accessFor(req.Target, req.Server, req.Key)
	if err != nil {
		return nil, err
	}
	snaps, err := m.listWith(ctx, a, req.Target, proto.List{Host: hostOf(a, req.Server), App: serverApp, Run: req.Run})
	if err != nil {
		return nil, err
	}
	var dirs []proto.Snapshot
	for _, s := range snaps {
		name, ok := strings.CutPrefix(s.Source.Name, "server-")
		if ok && s.Kind == "dir" && slices.Contains(serverDirNames, name) {
			dirs = append(dirs, s)
		}
	}
	if len(dirs) == 0 {
		return nil, &settings.Error{Code: settings.CodeNotFound, Message: fmt.Sprintf("Run %s holds no copy of ODAC's own state.", req.Run)}
	}
	if err := m.acquireRestore(ctx, req.Target); err != nil {
		return nil, err
	}
	defer m.releaseRestore(req.Target)

	root := serverRestoreRoot(m.cfg.BaseDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := CancelServerRestore(m.cfg.BaseDir); err != nil {
		return nil, err
	}
	sr := &ServerRestore{ID: newRestoreID(m.now()), Target: req.Target, Server: req.Server, Run: req.Run, Staged: ms(m.now())}
	stage := filepath.Join(root, sr.ID)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, err
	}
	for _, s := range dirs {
		name := strings.TrimPrefix(s.Source.Name, "server-")
		dest := filepath.Join(stage, name)
		if err := os.Mkdir(dest, 0o700); err != nil {
			os.RemoveAll(stage)
			return nil, err
		}
		if err := m.restoreDir(ctx, a, req.Target, s.ID, dest); err != nil {
			os.RemoveAll(stage)
			return nil, fmt.Errorf("backup: restore %s: %w", s.Source.Name, err)
		}
		sr.Dirs = append(sr.Dirs, name)
		if sr.Point == 0 || ms(s.Start) < sr.Point {
			sr.Point = ms(s.Start)
		}
	}
	slices.Sort(sr.Dirs)
	if err := writeJSON(pendingFile(m.cfg.BaseDir), sr); err != nil {
		os.RemoveAll(stage)
		return nil, err
	}
	m.log.Log("Staged ODAC's own state from run %s (%s); it is applied on the next start of the server", req.Run, strings.Join(sr.Dirs, ", "))
	return sr, nil
}

// serverDirNames are the directories of ODAC's state a backup takes.
var serverDirNames = []string{"config", "cert", "db", "mail"}

// PendingServerRestore returns the staged restore of ODAC's state, if any.
func PendingServerRestore(base string) (*ServerRestore, error) {
	raw, err := os.ReadFile(pendingFile(base))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sr ServerRestore
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("backup: the staged server restore is damaged: %w", err)
	}
	if !validStageID(sr.ID) {
		return nil, errors.New("backup: the staged server restore names no valid directory")
	}
	return &sr, nil
}

func validStageID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.HasPrefix(id, ".")
}

// CancelServerRestore drops a staged restore that has not been applied.
// One that a start began to apply is finished by the next start instead.
func CancelServerRestore(base string) error {
	sr, err := PendingServerRestore(base)
	if err != nil || sr == nil {
		return err
	}
	if len(sr.Applied) > 0 {
		return errors.New("backup: the staged server restore is being applied")
	}
	if err := os.RemoveAll(filepath.Join(serverRestoreRoot(base), sr.ID)); err != nil {
		return err
	}
	return os.Remove(pendingFile(base))
}

// ApplyServerRestore swaps a staged restore of ODAC's state in. The
// server calls it at start, before it opens its config, and with nothing
// staged it does nothing. The state it replaces is kept under
// backup/server-restore/<id>/previous. This server's own server and
// backup modules stay, every restored app is left inactive (its data is
// restored next, in place), and each step is recorded first, so a start
// that dies in the middle finishes the swap on the next one.
func ApplyServerRestore(base string) (*ServerRestore, error) {
	sr, err := PendingServerRestore(base)
	if err != nil || sr == nil {
		return nil, err
	}
	stage := filepath.Join(serverRestoreRoot(base), sr.ID)
	prev := filepath.Join(stage, "previous")
	sr.Previous = prev
	if err := os.MkdirAll(prev, 0o700); err != nil {
		return nil, err
	}
	for _, name := range sr.Dirs {
		if !slices.Contains(serverDirNames, name) || slices.Contains(sr.Applied, name) {
			continue
		}
		live, staged, kept := filepath.Join(base, name), filepath.Join(stage, name), filepath.Join(prev, name)
		if _, err := os.Lstat(kept); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(live, kept); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("backup: set %s aside: %w", live, err)
			}
		}
		if err := os.Rename(staged, live); err != nil {
			return nil, fmt.Errorf("backup: swap in %s: %w", live, err)
		}
		if name == "config" {
			if err := fixRestoredConfig(base, filepath.Join(prev, "config")); err != nil {
				return nil, err
			}
		}
		sr.Applied = append(sr.Applied, name)
		if err := writeJSON(pendingFile(base), sr); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(pendingFile(base)); err != nil {
		return nil, err
	}
	return sr, nil
}

// fixRestoredConfig keeps this server's own modules and stops every
// restored app, and drops the backups of the replaced modules, which
// would otherwise stand in for a restored module that does not parse.
func fixRestoredConfig(base, previous string) error {
	config := filepath.Join(base, "config")
	for _, mod := range keptModules {
		raw, err := os.ReadFile(filepath.Join(previous, mod+".json"))
		switch {
		case errors.Is(err, os.ErrNotExist):
			os.Remove(filepath.Join(config, mod+".json"))
		case err != nil:
			return err
		default:
			if err := os.WriteFile(filepath.Join(config, mod+".json"), raw, 0o600); err != nil {
				return err
			}
		}
	}
	if list, err := os.ReadDir(filepath.Join(base, ".bak")); err == nil {
		for _, e := range list {
			mod := strings.TrimSuffix(e.Name(), ".json.bak")
			if mod != e.Name() && !slices.Contains(keptModules, mod) {
				os.Remove(filepath.Join(base, ".bak", e.Name()))
			}
		}
	}
	appFile := filepath.Join(config, "app.json")
	raw, err := os.ReadFile(appFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var mod map[string]any
	if json.Unmarshal(raw, &mod) != nil {
		return nil
	}
	apps, _ := mod["apps"].([]any)
	for _, a := range apps {
		if app, ok := a.(map[string]any); ok {
			app["active"] = false
		}
	}
	out, err := json.MarshalIndent(mod, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(appFile, out, 0o600)
}
