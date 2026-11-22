package appmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"odac/internal/api"
	"odac/internal/netmode"
	"odac/internal/ports"
)

// AppHold is an app a restore holds: stopped, and kept away from the check
// pulse and from every other operation (deploys, restarts) until Release.
// A deploy that starts during a restore would run its migration against
// data that is being replaced.
type AppHold struct {
	m         *Manager
	id        float64
	name      string
	wasActive bool
	released  bool
}

// ErrAppBusy is an app another operation holds, such as a deploy.
var ErrAppBusy = errors.New("the app is being processed by another operation")

// HoldApp stops the app and holds it for a restore. The app is marked
// inactive while held and that is persisted, so a server that dies during
// the restore does not have its check pulse start a database on data
// that is half replaced; the restore's recovery marks it active again.
func (m *Manager) HoldApp(name string) (*AppHold, error) {
	var (
		id     float64
		active bool
		found  bool
	)
	m.cfg.View(func() {
		if app := m.getLocked(name); app != nil && app["name"] == name {
			found = true
			id, _ = app["id"].(float64)
			active = app["active"] == true
		}
	})
	if !found {
		return nil, fmt.Errorf("app %s not found", name)
	}
	if m.isCreating(name) || !m.tryLockProcessing(id) {
		return nil, fmt.Errorf("%s: %w", name, ErrAppBusy)
	}
	h := &AppHold{m: m, id: id, name: name, wasActive: active}
	h.Stop()
	return h, nil
}

// WasActive reports whether the app was meant to run before the hold.
func (h *AppHold) WasActive() bool { return h.wasActive }

// Start starts the app's container while it is still held and inactive:
// a database must run for its dump to be loaded.
func (h *AppHold) Start() error {
	return h.m.runHeld(h.id, nil)
}

// Stop stops the app's container and keeps it inactive.
func (h *AppHold) Stop() {
	m := h.m
	if m.deps.Docker.Available() {
		m.deps.Docker.Stop(h.name)
	}
	m.set(h.id, map[string]any{"status": "stopped", "pid": nil, "active": false})
	m.mu.Lock()
	stream := m.logStreams[h.name]
	delete(m.logStreams, h.name)
	m.mu.Unlock()
	if stream != nil {
		stream.end()
	}
}

// Release ends the hold. With active the app runs afterwards (it is
// started unless it already runs) and is marked active; without, it is
// left stopped.
func (h *AppHold) Release(active bool) error {
	if h.released {
		return nil
	}
	h.released = true
	defer h.m.unlockProcessing(h.id)
	if !active {
		h.Stop()
		return nil
	}
	h.m.set(h.id, map[string]any{"active": true})
	if h.m.isAppRunning(h.id) {
		return nil
	}
	err := h.m.runHeld(h.id, nil)
	h.m.hubTrigger("app.list")
	return err
}

// Abandon drops the hold without touching the app, for a server that
// stops in the middle of a restore: the app stays stopped and inactive,
// and the next server's recovery takes it from there.
func (h *AppHold) Abandon() {
	if h.released {
		return
	}
	h.released = true
	h.m.unlockProcessing(h.id)
}

// SetAppActive persists an app's active flag. A restore that did not
// finish leaves its apps inactive; its recovery gives each its flag back,
// and the check pulse starts the active ones.
func (m *Manager) SetAppActive(name string, active bool) bool {
	ok := false
	m.cfg.Mutate(func() {
		if app := m.getLocked(name); app != nil && app["name"] == name {
			app["active"] = active
			m.saveAppsLocked()
			ok = true
		}
	})
	return ok
}

// AppDefinition returns the app's record as it is persisted, which a
// backup stores beside the app's data so that it can be restored as a new
// app, here or on another server. It holds the app's environment, and so
// its credentials; the repository it goes to is encrypted.
func (m *Manager) AppDefinition(name string) (map[string]any, bool) {
	var out map[string]any
	m.cfg.View(func() {
		app := m.getLocked(name)
		if app == nil || app["name"] != name || app["_deleted"] == true {
			return
		}
		b, err := json.Marshal(app)
		if err != nil {
			return
		}
		if json.Unmarshal(b, &out) != nil {
			out = nil
			return
		}
		for _, k := range ephemeralFields {
			delete(out, k)
		}
		delete(out, "activeContainerId")
	})
	return out, out != nil
}

// UniqueAppName returns base when no app has that name, else base-N.
func (m *Manager) UniqueAppName(base string) string {
	var name string
	m.cfg.View(func() { name = m.generateUniqueNameLocked(base) })
	return name
}

// AppsDir is the directory app directories live in, as this server sees
// it.
func (m *Manager) AppsDir() string { return m.appsPath() }

// DeleteRestoredApp removes an app a restore created and could not finish,
// with its directory.
func (m *Manager) DeleteRestoredApp(name string) error {
	if r := m.Delete(api.Operator, name, true); r != nil && !r.Status {
		return fmt.Errorf("delete %s: %v", name, r.Message)
	}
	return nil
}

// RestoredApp is the record CreateRestoredApp made.
type RestoredApp struct {
	Name string
	// Dropped says what of the definition the new app does not keep, for
	// the operator.
	Dropped []string
}

// CreateRestoredApp adds an app made from another app's definition under
// a new name, stopped and inactive, for a restore to fill with data. It
// runs next to the app it copies, so nothing it would share with that
// app's host-wide state is kept: published ports go back to the proxy,
// host networking to the bridge, host devices and the API grant are
// dropped (the operator grants them to the copy explicitly), and volumes
// get the new app's own directories. Domains belong to their records, not
// to the app, so none follows. renames maps the names of the group being
// restored to their new names: env.linked follows them, and so does
// every env value that names an old one as a host (see renameHosts).
// group is the template group the copy joins instead of its original's.
func (m *Manager) CreateRestoredApp(def map[string]any, name string, renames map[string]string, group string) (*RestoredApp, error) {
	if !validAppName(name) {
		return nil, fmt.Errorf("invalid app name %q", name)
	}
	rec, err := cloneRecord(def)
	if err != nil {
		return nil, err
	}
	old, _ := rec["name"].(string)
	switch typ, _ := rec["type"].(string); typ {
	case "container":
	case "git":
		return nil, errors.New("a git app is built from its repository: create it from there and restore its data in place")
	default:
		return nil, fmt.Errorf("an app of type %q cannot be restored as a new app", typ)
	}
	out := &RestoredApp{Name: name}
	drop := func(format string, a ...any) { out.Dropped = append(out.Dropped, fmt.Sprintf(format, a...)) }

	for _, k := range ephemeralFields {
		delete(rec, k)
	}
	delete(rec, "activeContainerId")
	if rec["api"] != nil {
		delete(rec, "api")
		drop("the API grant")
	}
	if devs, _ := rec["devices"].([]any); len(devs) > 0 {
		drop("%d host devices", len(devs))
	}
	delete(rec, "devices")
	if netmode.IsHost(rec["networkMode"]) {
		drop("host networking")
	}
	delete(rec, "networkMode")
	if list, _ := rec["ports"].([]any); list != nil {
		for _, p := range list {
			if pm, _ := p.(map[string]any); ports.IsPublished(pm) {
				drop("the published host port %v", pm["host"])
				pm["host"] = ports.Proxy
				delete(pm, "public")
			}
		}
	}
	if tpl, _ := rec["template"].(map[string]any); tpl != nil && group != "" {
		tpl["group"] = group
	}
	renameEnv(rec, renames)

	appsPath := m.appsPath()
	appDir := filepath.Join(appsPath, name)
	var copies [][2]string
	if list, _ := rec["volumes"].([]any); list != nil {
		kept := make([]any, 0, len(list))
		for _, v := range list {
			vm, _ := v.(map[string]any)
			if vm == nil {
				continue
			}
			host, _ := vm["host"].(string)
			container, _ := vm["container"].(string)
			if host == "" || container == "" {
				continue
			}
			if configFile(appsPath, host) {
				if st, err := os.Stat(host); err != nil || !st.Mode().IsRegular() {
					drop("the config file %s, which is not on this server", container)
					continue
				}
				rel := relocate(host, old, container)
				dst := filepath.Join(appDir, rel)
				copies = append(copies, [2]string{host, dst})
				vm["host"] = dst
				kept = append(kept, vm)
				continue
			}
			vm["host"] = filepath.Join(appDir, relocate(host, old, container))
			kept = append(kept, vm)
		}
		rec["volumes"] = kept
	}

	rec["name"] = name
	rec["active"] = false
	rec["created"] = nowMs()
	var refusal error
	m.cfg.View(func() {
		if m.getLocked(name) != nil {
			refusal = fmt.Errorf("app %s already exists", name)
		}
	})
	if refusal != nil {
		return nil, refusal
	}
	if !m.tryLockCreating(name) {
		return nil, fmt.Errorf("app %s is already being created", name)
	}
	defer m.unlockCreating(name)
	// A directory left by an app of the same name, deleted without its
	// data, would mix that app's files into the restore.
	if entries, err := os.ReadDir(appDir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("the directory of app %s exists already and is not empty", name)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return nil, err
	}
	for _, c := range copies {
		if err := copyFile(c[0], c[1]); err != nil {
			os.RemoveAll(appDir)
			return nil, fmt.Errorf("copy config file: %w", err)
		}
	}
	m.cfg.Mutate(func() {
		if m.getLocked(name) != nil {
			refusal = fmt.Errorf("app %s already exists", name)
			return
		}
		rec["id"] = m.getNextIDLocked()
		m.apps = append(m.apps, rec)
		m.saveAppsLocked()
	})
	if refusal != nil {
		os.RemoveAll(appDir)
		return nil, refusal
	}
	m.hubTrigger("app.list")
	return out, nil
}

func cloneRecord(def map[string]any) (map[string]any, error) {
	b, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("empty app definition")
	}
	return out, nil
}

var slugChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// relocate is a volume's path inside the new app's directory: the part
// below the old app's directory when it lives there (found by the old
// name as a path element, since another server keeps its apps elsewhere),
// or volumes/<container path> for a directory the operator chose, which
// the copy must never share with the app it copies.
func relocate(host, old, container string) string {
	clean := path.Clean(filepath.ToSlash(host))
	if old != "" {
		if i := strings.LastIndex(clean, "/"+old+"/"); i >= 0 {
			if rel := clean[i+len(old)+2:]; rel != "" && !strings.HasPrefix(rel, "..") {
				return filepath.FromSlash(rel)
			}
		}
	}
	slug := strings.Trim(slugChars.ReplaceAllString(strings.Trim(strings.TrimSuffix(container, ":ro"), "/"), "-"), "-.")
	if slug == "" {
		slug = "root"
	}
	return filepath.Join("volumes", slug)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// renameEnv points the record's links and env values at the group's new
// names.
func renameEnv(rec map[string]any, renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	env, _ := rec["env"].(map[string]any)
	if env == nil {
		return
	}
	if isNewEnvStructure(env) {
		if linked, _ := env["linked"].([]any); linked != nil {
			for i, l := range linked {
				if s, ok := l.(string); ok && renames[s] != "" {
					linked[i] = renames[s]
				}
			}
		}
	}
	manual := getManualEnv(env)
	for k, v := range manual {
		if s, ok := v.(string); ok {
			manual[k] = RenameHosts(s, renames)
		}
	}
}

// RenameHosts rewrites the app names in an env value: the whole value
// ("db"), or a name in the host position of an address ("postgres://u:p@db:5432/x",
// "http://db/", "db:6379"). An app reaches another by its container name,
// and a restored copy that kept an old name would read and write the live
// app it was copied from. A name elsewhere in a value (a path, a word) is
// left alone.
func RenameHosts(s string, renames map[string]string) string {
	if to := renames[s]; to != "" {
		return to
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		if hostStart(s, i) {
			j := i
			for j < len(s) && nameChar(s[j]) {
				j++
			}
			if to := renames[s[i:j]]; to != "" && j > i && hostEnd(s, j) {
				b.WriteString(to)
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func nameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// hostStart: the start of the value, or right after "@", "//" or a
// separator of host lists (",", ";", " ", "=").
func hostStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case '@', ',', ';', ' ', '=':
		return true
	case '/':
		return i >= 2 && s[i-2] == '/'
	}
	return false
}

// hostEnd: the end of the value, or a port, path, query or list separator.
func hostEnd(s string, j int) bool {
	if j == len(s) {
		return true
	}
	switch s[j] {
	case ':', '/', '?', ',', ';', ' ':
		return true
	}
	return false
}
