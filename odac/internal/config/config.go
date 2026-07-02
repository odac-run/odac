// Package config implements ODAC's modular JSON config store as specified in
// docs/migration/contracts/config-schema.md: one file per module under
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
	"io"
	"os"
	"path/filepath"
	"sync"
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
	"proxy":    {"tunnels"},
	"server":   {"server"},
	"service":  {"services"},
	"ssl":      {"ssl"},
}

// Store holds the merged in-memory configuration.
type Store struct {
	baseDir string
	data    map[string]any
	dirty   map[string]bool
	mu      sync.Mutex
}

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
	if err := os.MkdirAll(s.configDir(), 0o755); err != nil {
		return nil, fmt.Errorf("config: create config dir: %w", err)
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

// Touch marks the module owning key as dirty. Use after mutating a value
// obtained from Map/Get in place.
func (s *Store) Touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDirty(key)
}

// SaveDirty writes every dirty module file atomically. Modules that fail to
// write stay dirty so a later save retries them.
func (s *Store) SaveDirty() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for module, dirty := range s.dirty {
		if !dirty {
			continue
		}
		if err := s.writeModule(module); err != nil {
			errs = append(errs, fmt.Errorf("config: save %s: %w", module, err))
			continue
		}
		s.dirty[module] = false
	}
	return errors.Join(errs...)
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
	os.WriteFile(mainFile, raw, 0o644)
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
			default:
				c[k] = map[string]any{}
			}
		}
	}
}

// writeModule atomically persists one module file: tmp write, backup of the
// previous version into .bak/, rename over the main file. Caller holds s.mu.
//
// Note: encoding/json sorts map keys alphabetically, while Node writes keys
// in insertion order. The files are semantically identical; only the key
// order differs, and Node restores its own order on its next save.
func (s *Store) writeModule(module string) error {
	content := map[string]any{}
	for _, k := range moduleKeys[module] {
		if v, ok := s.data[k]; ok {
			content[k] = v
		}
	}

	payload, err := json.MarshalIndent(content, "", "    ")
	if err != nil {
		return err
	}

	file := filepath.Join(s.configDir(), module+".json")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}

	if _, err := os.Stat(file); err == nil {
		// Backup failures are non-fatal: better to save without a backup
		// than not save at all (same policy as Node's #atomicWrite).
		if err := os.MkdirAll(s.bakDir(), 0o755); err == nil {
			copyFile(file, filepath.Join(s.bakDir(), module+".json.bak"))
		}
	}

	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
