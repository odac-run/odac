package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A restore never writes into a live directory. Each volume is restored
// beside its data, inside the volume itself (so every move is a rename on
// one file system, even when the volume is a mount point), and swapped in
// by moving entries; the data it replaces is kept until the whole restore
// has succeeded, so any failure, a crash included, puts the volume back
// as it was. The journal (restoreJournal) says how far each volume got.
//
// A volume emptied for a dump keeps its data next to it instead whenever
// it can (keepBeside): its database starts on it before the commit, and
// an image that initializes only an empty directory (postgres with PGDATA
// at the mount point) refuses one holding the keep directory.

// Volume modes.
const (
	// modeReplace restores a snapshot into the staging directory and
	// swaps it in.
	modeReplace = "replace"
	// modeEmpty moves the volume's data aside so its database starts
	// empty, for a dump to be loaded into.
	modeEmpty = "empty"
	// modeNew is a volume of an app the restore created: it starts empty
	// and is restored directly; a failure deletes the app.
	modeNew = "new"
)

// Volume phases. A volume moves staged -> out -> in; each phase is written
// to the journal before the work it names starts.
const (
	// volStaged: the staging directory exists and may hold restored
	// entries; the volume is untouched.
	volStaged = "staged"
	// volOut: the volume's own entries are moving into the keep
	// directory; every entry outside it is still an original.
	volOut = "out"
	// volIn: the originals are all in the keep directory; every other
	// entry of the volume is the restore's.
	volIn = "in"
)

// volumeJob is one directory a restore changes.
type volumeJob struct {
	App string `json:"app"`
	// Path is the directory as this server sees it.
	Path string `json:"path"`
	// Root is a directory above Path that the restore trusts; no element
	// between them may be a symlink. Empty trusts every element but Path.
	Root  string `json:"root,omitempty"`
	Mode  string `json:"mode"`
	Phase string `json:"phase,omitempty"`
	// Staging and Keep are entry names inside Path, or Keep one in
	// Path's parent when Beside is set.
	Staging string `json:"staging,omitempty"`
	Keep    string `json:"keep,omitempty"`
	Beside  bool   `json:"beside,omitempty"`

	// source is the snapshot that fills the volume.
	source string
}

// ErrUnsafePath is a restore destination reached through a symlink, which
// an app could have placed to send the restore into another app's data.
var ErrUnsafePath = errors.New("backup: the restore destination is reached through a symlink")

// checkPath refuses p when it, or an element between root and it, is a
// symlink. The apps whose data is restored are stopped, so nothing they
// run can swap an element after the check.
func checkPath(root, p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("backup: restore destination %q is not a clean absolute path", p)
	}
	start := p
	if root != "" && within(p, root) {
		start = root
	}
	cur := start
	rel, _ := filepath.Rel(start, p)
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	check := func(q string) error {
		st, err := os.Lstat(q)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrUnsafePath, q)
		}
		if !st.IsDir() {
			return fmt.Errorf("backup: restore destination %s is not a directory", q)
		}
		return nil
	}
	if start == p {
		return check(p)
	}
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		if err := check(cur); err != nil {
			return err
		}
	}
	return nil
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// ensureDir makes p and its missing parents below root, refusing a
// symlink anywhere on the way.
func ensureDir(root, p string, perm os.FileMode) error {
	if err := checkPath(root, p); err != nil {
		return err
	}
	if err := os.MkdirAll(p, perm); err != nil {
		return err
	}
	return checkPath(root, p)
}

func (v *volumeJob) staging() string { return filepath.Join(v.Path, v.Staging) }
func (v *volumeJob) keep() string {
	if v.Beside {
		return filepath.Join(filepath.Dir(v.Path), v.Keep)
	}
	return filepath.Join(v.Path, v.Keep)
}

// keepBeside moves v's keep directory next to the volume when a rename
// still works there: the parent is below the trusted root, on the same
// file system, and inside no other volume of the restore, whose own swap
// would carry the directory away.
func (v *volumeJob) keepBeside(all []*volumeJob) {
	parent := filepath.Dir(v.Path)
	if v.Root == "" || parent == v.Root || !within(parent, v.Root) {
		return
	}
	for _, o := range all {
		if o != v && within(parent, o.Path) {
			return
		}
	}
	if !sameDevice(v.Path, parent) {
		return
	}
	v.Beside, v.Keep = true, v.Keep+"-"+filepath.Base(v.Path)
}

// entries lists dir without the given names.
func entries(dir string, except ...string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	out := names[:0]
	for _, n := range names {
		if !slices.Contains(except, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// prepare makes the directory the helper restores into: the staging
// directory, or the new app's volume itself.
func (v *volumeJob) prepare() error {
	switch v.Mode {
	case modeNew:
		if err := ensureDir(v.Root, v.Path, 0o755); err != nil {
			return err
		}
		names, err := entries(v.Path)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return fmt.Errorf("backup: the new app's directory %s is not empty", v.Path)
		}
		return nil
	case modeReplace:
		if err := ensureDir(v.Root, v.Path, 0o755); err != nil {
			return err
		}
		return os.Mkdir(v.staging(), 0o700)
	}
	return ensureDir(v.Root, v.Path, 0o755)
}

// moveOut moves every original entry of the volume into the keep
// directory.
func (v *volumeJob) moveOut() error {
	if err := checkPath(v.Root, v.Path); err != nil {
		return err
	}
	if err := os.Mkdir(v.keep(), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := checkPath(v.Root, v.keep()); err != nil {
		return err
	}
	names, err := entries(v.Path, v.Staging, v.Keep)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := os.Rename(filepath.Join(v.Path, n), filepath.Join(v.keep(), n)); err != nil {
			return err
		}
	}
	return nil
}

// moveIn moves the staged entries into the volume and drops the empty
// staging directory.
func (v *volumeJob) moveIn() error {
	if v.Mode != modeReplace {
		return nil
	}
	if err := checkPath(v.Root, v.Path); err != nil {
		return err
	}
	names, err := entries(v.staging())
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := os.Rename(filepath.Join(v.staging(), n), filepath.Join(v.Path, n)); err != nil {
			return err
		}
	}
	return os.Remove(v.staging())
}

// rollback puts the volume back as it was before the restore, from
// whatever phase it reached.
func (v *volumeJob) rollback() error {
	if v.Mode == modeNew {
		return nil
	}
	if err := checkPath(v.Root, v.Path); err != nil {
		return err
	}
	if v.Phase == volIn {
		names, err := entries(v.Path, v.Keep)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := os.RemoveAll(filepath.Join(v.Path, n)); err != nil {
				return err
			}
		}
	}
	if v.Phase == volOut || v.Phase == volIn {
		names, err := entries(v.keep())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, n := range names {
			if err := os.Rename(filepath.Join(v.keep(), n), filepath.Join(v.Path, n)); err != nil {
				return err
			}
		}
	}
	if v.Keep != "" {
		if err := os.RemoveAll(v.keep()); err != nil {
			return err
		}
	}
	if v.Staging != "" {
		return os.RemoveAll(v.staging())
	}
	return nil
}

// commit drops what the restore kept for a rollback.
func (v *volumeJob) commit() error {
	if v.Mode == modeNew {
		return nil
	}
	if err := checkPath(v.Root, v.Path); err != nil {
		return err
	}
	var first error
	if v.Keep != "" {
		first = os.RemoveAll(v.keep())
	}
	if v.Staging != "" {
		if err := os.RemoveAll(v.staging()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// restoreJournal is the restore in flight, in <base>/backup/restore.json,
// so that a restore cut short by a crash or an update is rolled back by
// the next server, and its apps are given back their state.
type restoreJournal struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Target  string `json:"target"`
	Run     string `json:"run"`
	Phase   string `json:"phase"`
	Started int64  `json:"started"`
	// Apps are the apps the restore holds, with whether each was meant
	// to run, and the ones it created.
	Apps    []journalApp `json:"apps,omitempty"`
	Volumes []*volumeJob `json:"volumes,omitempty"`
	// Planned is what the restore set out to do, so the record a later
	// server writes for it names the same apps.
	Planned   []AppRestore `json:"planned,omitempty"`
	Restarted []string     `json:"restarted,omitempty"`
	Server    string       `json:"server,omitempty"`
	Job       string       `json:"job,omitempty"`
	Point     int64        `json:"point,omitempty"`
	// Temp are directories on ODAC's own disk the restore works in.
	Temp []string `json:"temp,omitempty"`
}

type journalApp struct {
	Name      string `json:"name"`
	WasActive bool   `json:"wasActive,omitempty"`
	Created   bool   `json:"created,omitempty"`
	Held      bool   `json:"held,omitempty"`
}

// Restore phases.
const (
	phasePrepare   = "prepare"
	phaseStage     = "stage"
	phaseSwap      = "swap"
	phaseLoad      = "load"
	phaseCommitted = "committed"
)

func journalFile(base string) string { return filepath.Join(base, "backup", "restore.json") }

func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func readJournal(base string) (*restoreJournal, error) {
	raw, err := os.ReadFile(journalFile(base))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j restoreJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("backup: the restore journal is damaged: %w", err)
	}
	return &j, nil
}

// writeDefinition stores an app's definition for its backup. It holds the
// app's environment, so it is the server's to read only.
func writeDefinition(dir string, def map[string]any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, definitionFile), raw, 0o600)
}

// maxDefinition bounds a definition read back from a repository.
const maxDefinition = 4 << 20

func readDefinitionFile(p string) (map[string]any, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxDefinition+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDefinition {
		return nil, errors.New("backup: the stored definition is too large")
	}
	var def map[string]any
	if err := json.Unmarshal(raw, &def); err != nil || def == nil {
		return nil, fmt.Errorf("backup: the stored definition is unreadable: %v", err)
	}
	return def, nil
}
