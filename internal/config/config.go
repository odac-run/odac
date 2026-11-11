// Package config implements ODAC's modular JSON config store as specified in
// one file per module under
// <base>/config/, merged into a single view keyed by top-level config keys,
// with per-module dirty tracking, atomic writes, .bak backups and corruption
// recovery.
//
// Single-writer by convention: only the server (or the watchdog, before the
// server starts) saves; the CLI reads but must never write.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// moduleKeys maps each module file (name without .json) to the top-level
// config keys it owns. Mirrors #moduleMap in core/Config.js.
var moduleKeys = map[string][]string{
	"api":      {"api"},
	"app":      {"apps", "app"},
	"dns":      {"dns"},
	"domain":   {"domains"},
	"firewall": {"firewall"},
	"hub":      {"hub"},
	"mail":     {"mail"},
	"mtls":     {"mtls"},
	"proxy":    {"tunnels"},
	"server":   {"server"},
	"service":  {"services"},
	"ssl":      {"ssl"},
	"system":   {"swap"},
}

// Store holds the merged in-memory configuration.
//
// Two locks with a fixed order (valueMu before mu, never the reverse):
//
//   - mu guards the top-level data/dirty maps (every accessor takes it).
//   - valueMu guards the VALUES hanging off those keys. Node's handlers
//     mutated config objects freely under the single-threaded event loop;
//     in Go, API command handlers (task 3.3+) mutate deep maps while the
//     autosave loop and the data-plane sync builders marshal them. Writers
//     wrap mutations in Mutate; readers that walk or marshal deep values
//     (including SaveDirty itself) hold the read side via View.
//
// Get/Set/Map/Touch alone do NOT take valueMu: replacing or reading a
// top-level slot is safe under mu, but touching anything inside a value
// from a goroutine requires View/Mutate.
type Store struct {
	baseDir string
	data    map[string]any
	dirty   map[string]bool
	mu      sync.Mutex
	valueMu sync.RWMutex
	// saveMu serializes saves so an older snapshot never lands after a
	// newer one; the disk writes run under it alone, off mu and valueMu.
	saveMu sync.Mutex
}

// Module files hold the root API token, the Hub secret and tunnel tokens,
// so the tree is private to the server's user.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// DefaultBaseDir returns ~/.odac, the base directory shared with the Node
// implementation and the data-plane binaries.
func DefaultBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".odac")
}

// Open loads every module file from <baseDir>/config, applying defaults for
// missing or unrecoverable modules.
func Open(baseDir string) (*Store, error) {
	s := &Store{baseDir: baseDir, data: map[string]any{}, dirty: map[string]bool{}}
	if err := privateDir(s.configDir()); err != nil {
		return nil, fmt.Errorf("config: create config dir: %w", err)
	}
	if _, err := os.Stat(s.bakDir()); err == nil {
		if err := os.Chmod(s.bakDir(), dirMode); err != nil {
			return nil, fmt.Errorf("config: restrict backup dir: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s, nil
}

// BaseDir returns the base directory (e.g. ~/.odac) this store operates in.
func (s *Store) BaseDir() string { return s.baseDir }

// Reload re-reads all module files from disk, discarding unsaved in-memory
// changes. The watchdog calls this after a server crash because the server
// may have persisted config changes since our last load.
func (s *Store) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	s.dirty = map[string]bool{}
}

// Get returns the value of a top-level config key, or nil when absent.
func (s *Store) Get(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

// Map returns the map value of a top-level key (nil when absent or not a
// map). The returned map is live: callers that mutate it must call Touch so
// the owning module is saved.
func (s *Store) Map(key string) map[string]any {
	v, _ := s.Get(key).(map[string]any)
	return v
}

// Set stores a top-level key and marks its owning module dirty.
func (s *Store) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	s.markDirty(key)
}

// Delete removes a top-level key and marks its owning module dirty, the
// port of `delete config.hub` (Hub credential invalidation) — the module
// file persists without the key on the next save.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	s.markDirty(key)
}

// Touch marks the module owning key as dirty. Use after mutating a value
// obtained from Map/Get in place.
func (s *Store) Touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDirty(key)
}

// View runs fn holding the value read-lock: safe to walk or JSON-marshal
// deep config values while no Mutate is in flight. fn may call the plain
// accessors (Get/Map/...) but must not call Mutate or save methods.
func (s *Store) View(fn func()) {
	s.valueMu.RLock()
	defer s.valueMu.RUnlock()
	fn()
}

// Mutate runs fn holding the value write-lock: the required wrapper for any
// in-place mutation of deep config values (API command handlers). fn may
// call the plain accessors including Touch, but must not call View, Mutate
// or the save methods (persist after Mutate returns).
func (s *Store) Mutate(fn func()) {
	s.valueMu.Lock()
	defer s.valueMu.Unlock()
	fn()
}

// SaveDirty writes every dirty module file atomically. Modules that fail to
// write stay dirty so a later save retries them. The value read-lock is held
// only while the modules are marshaled (that walks live deep values); the
// writes and their fsyncs run after both locks are released, so a slow disk
// never stalls config readers.
func (s *Store) SaveDirty() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	type pending struct {
		module  string
		payload []byte
	}
	var writes []pending
	var errs []error
	s.valueMu.RLock()
	s.mu.Lock()
	for module, dirty := range s.dirty {
		if !dirty {
			continue
		}
		payload, err := s.marshalModule(module)
		if err != nil {
			errs = append(errs, fmt.Errorf("config: save %s: %w", module, err))
			continue
		}
		s.dirty[module] = false
		writes = append(writes, pending{module, payload})
	}
	s.mu.Unlock()
	s.valueMu.RUnlock()

	for _, w := range writes {
		if err := s.writeModule(w.module, w.payload); err != nil {
			errs = append(errs, fmt.Errorf("config: save %s: %w", w.module, err))
			s.mu.Lock()
			s.dirty[w.module] = true
			s.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// AutoSave starts a background loop persisting dirty modules every interval,
// mirroring Node's 500ms server-side autosave (contract 0.6). Modules whose
// write fails stay dirty and are retried on the next tick. Only the server
// process may run autosave (single-writer invariant — the CLI never writes).
// The returned stop function is idempotent and returns once the loop has
// exited, so no save from it is still in flight.
func (s *Store) AutoSave(interval time.Duration, onError func(error)) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := s.SaveDirty(); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// ForceSave marks every module dirty and saves, mirroring Config.force() in
// Node (used before handover/exit so nothing is lost).
func (s *Store) ForceSave() error {
	s.mu.Lock()
	for module := range moduleKeys {
		s.dirty[module] = true
	}
	s.mu.Unlock()
	return s.SaveDirty()
}

func (s *Store) configDir() string { return filepath.Join(s.baseDir, "config") }
func (s *Store) bakDir() string    { return filepath.Join(s.baseDir, ".bak") }

func (s *Store) markDirty(key string) {
	for module, keys := range moduleKeys {
		for _, k := range keys {
			if k == key {
				s.dirty[module] = true
				return
			}
		}
	}
}

func (s *Store) load() {
	merged := map[string]any{}
	for module, keys := range moduleKeys {
		content := s.loadModuleFile(module)
		if content == nil {
			continue
		}
		for _, k := range keys {
			if v, ok := content[k]; ok {
				merged[k] = v
			}
		}
	}
	applyDefaults(merged)
	s.data = merged
}

// loadModuleFile reads one module file, falling back to its .bak copy when
// the main file is missing content or unparseable.
func (s *Store) loadModuleFile(module string) map[string]any {
	mainFile := filepath.Join(s.configDir(), module+".json")
	raw, err := os.ReadFile(mainFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && len(raw) >= 2 {
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			return parsed
		}
	}
	return s.recoverFromBackup(module, mainFile)
}

func (s *Store) recoverFromBackup(module, mainFile string) map[string]any {
	backupFile := filepath.Join(s.bakDir(), module+".json.bak")
	corruptedFile := mainFile + ".corrupted"

	raw, err := os.ReadFile(backupFile)
	var parsed map[string]any
	if err != nil || len(raw) < 2 || json.Unmarshal(raw, &parsed) != nil {
		// Main and backup both unusable: keep forensic copies, use defaults.
		copyFile(mainFile, corruptedFile)
		copyFile(backupFile, backupFile+".corrupted")
		return nil
	}

	// Backup is valid: preserve the broken main, then restore from backup.
	// A failed restore is non-fatal — the parsed backup data is still used.
	copyFile(mainFile, corruptedFile)
	writeDurable(mainFile, raw)
	return parsed
}

// applyDefaults fills missing module keys with the defaults defined by
// core/Config.js #initializeDefaultModuleConfig.
func applyDefaults(c map[string]any) {
	for _, keys := range moduleKeys {
		for _, k := range keys {
			if _, ok := c[k]; ok {
				continue
			}
			switch k {
			case "server":
				c[k] = map[string]any{"pid": nil, "started": nil, "watchdog": nil}
			case "apps":
				c[k] = []any{}
			case "domains", "app":
				c[k] = map[string]any{}
			case "firewall":
				c[k] = map[string]any{
					"enabled":   true,
					"blacklist": []any{},
					"whitelist": []any{},
					"rateLimit": map[string]any{"enabled": true, "windowMs": 60000, "max": 1000},
				}
			case "swap":
				// Elastic swap manager (internal/system/swap); Linux-only at
				// runtime.
				c[k] = map[string]any{
					"autoManage":    true,
					"persist":       true,
					"maxDiskPct":    25,
					"maxIncrements": 8,
					"allowShrink":   true,
				}
			default:
				c[k] = map[string]any{}
			}
		}
	}
}

// marshalModule encodes one module's keys. Caller holds s.mu and the value
// read-lock.
//
// Note: encoding/json sorts map keys alphabetically, while Node writes keys
// in insertion order. The files are semantically identical; only the key
// order differs, and Node restores its own order on its next save.
func (s *Store) marshalModule(module string) ([]byte, error) {
	content := map[string]any{}
	for _, k := range moduleKeys[module] {
		if v, ok := s.data[k]; ok {
			content[k] = v
		}
	}
	return json.MarshalIndent(content, "", "    ")
}

// writeModule atomically persists one module file: tmp write and fsync,
// backup of the previous version into .bak/, rename over the main file,
// fsync of the directory so the rename itself survives a power loss.
// Without the fsyncs a crash could keep the rename and lose the data,
// leaving a zero-length module next to an equally unsynced backup.
func (s *Store) writeModule(module string, payload []byte) error {
	file := filepath.Join(s.configDir(), module+".json")
	tmp := file + ".tmp"
	if err := writeDurable(tmp, payload); err != nil {
		os.Remove(tmp)
		return err
	}

	if _, err := os.Stat(file); err == nil {
		// Backup failures are non-fatal: better to save without a backup
		// than not save at all (same policy as Node's #atomicWrite).
		if err := privateDir(s.bakDir()); err == nil {
			copyFile(file, filepath.Join(s.bakDir(), module+".json.bak"))
		}
	}

	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(s.configDir())
	return nil
}

// privateDir creates dir with dirMode, and narrows an existing one: installs
// from before this rule have 0755 directories holding 0644 secrets.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	return os.Chmod(dir, dirMode)
}

// writeDurable writes data to path with fileMode and fsyncs it before
// returning. An existing file keeps its inode, so its mode is set again.
func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	if err := f.Chmod(fileMode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable. Best effort: some filesystems
// refuse fsync on a directory, and the rename has already happened.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeDurable(dst, data)
}
