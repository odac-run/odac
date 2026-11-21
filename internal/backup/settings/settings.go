// Package settings is the operator's side of backups: the targets backups
// go to, the jobs that send them there and the snapshot settings, kept in
// the "backup" config module. It validates what is declared and answers
// what the manager (scheduling, runs) and the API need, and runs nothing
// itself: the only run it asks for is a target's init, through the
// Initializer it is given.
//
// Secrets (a target's key and its storage credentials) live in the module
// and nowhere else. Every view this package returns is a separate type
// without those fields, so a list cannot carry them by accident; the key
// is read only through Key, which the API keeps for the operator.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/logx"
)

// Module is the config key the settings live under.
const Module = "backup"

// ConfigStore is the slice of the config store the settings use;
// *config.Store implements it.
type ConfigStore interface {
	View(fn func())
	Mutate(fn func())
	Get(key string) any
	Set(key string, value any)
	SaveDirty() error
	BaseDir() string
}

// Initializer runs init on a target's repository, which creates it or
// opens it with the key: a target is saved only once that worked. The
// manager implements it with the runner; Access is complete except for
// the cache, which is the manager's.
type Initializer interface {
	Init(ctx context.Context, a Access) (*proto.Result, error)
}

// Locator tells whether a host is this server, which is what makes a
// network target "local". HostLocator is the real one.
type Locator interface {
	IsLocal(ctx context.Context, host string) bool
}

// Apps lists the apps as backups see them. The app manager implements it.
type Apps interface {
	BackupApps() []App
}

// App is one app as backups see it.
type App struct {
	Name string
	// Stateful apps hold data of their own, which is what a backup is for.
	Stateful bool
	// Linked are the apps named in its env.linked, most often its
	// database: their data changes with it.
	Linked []string
}

// Config wires Settings.
type Config struct {
	Store    ConfigStore
	ServerID string
	// ServerName labels the server directory on a target for a person
	// browsing it; nil means none.
	ServerName func() string
	Init       Initializer
	Apps       Apps
	// Locate defaults to HostLocator.
	Locate Locator
	// Protected are host paths no target may be at or inside: ODAC's own
	// data, see ProtectedPaths.
	Protected []string
	Log       *logx.Logger
	Now       func() time.Time
}

// Settings holds the backup settings. Its methods are safe for concurrent
// use; changes are serialized, and the slow part of one (a target's init)
// runs without holding anything other changes wait on.
type Settings struct {
	cfg     Config
	owners  *owners
	mu      sync.Mutex
	pending map[string]*pendingTarget
}

// New checks cfg and fills its defaults.
func New(cfg Config) (*Settings, error) {
	if cfg.Store == nil || cfg.Init == nil || cfg.Apps == nil {
		return nil, errors.New("settings: a config store, an initializer and the apps are required")
	}
	if cfg.ServerID == "" {
		return nil, errors.New("settings: a server id is required")
	}
	if cfg.ServerName == nil {
		cfg.ServerName = func() string { return "" }
	}
	if cfg.Locate == nil {
		cfg.Locate = HostLocator{}
	}
	if cfg.Log == nil {
		cfg.Log = logx.New("Backup", "Settings")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Settings{
		cfg:     cfg,
		owners:  newOwners(ownerDir(cfg.Store.BaseDir())),
		pending: map[string]*pendingTarget{},
	}, nil
}

// Error is a refusal the API passes on. Code is stable; Message is for the
// operator and never carries a secret.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Error codes.
const (
	CodeInvalid  = "invalid"
	CodeNotFound = "not_found"
	CodeExists   = "exists"
	CodeInUse    = "in_use"
	CodeExpired  = "expired"
	// CodeOwnerConflict: another machine writes under this server id on
	// the target, see Adopt.
	CodeOwnerConflict = proto.CodeOwnerConflict
	// CodeUnreachable: the target's init failed, so it was not saved.
	CodeUnreachable = "unreachable"
)

func errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// module is the persisted shape of the config module.
type module struct {
	Targets   map[string]*Target  `json:"targets,omitempty"`
	Jobs      map[string]*Job     `json:"jobs,omitempty"`
	Retired   map[string]*Retired `json:"retired,omitempty"`
	Snapshots *Snapshots          `json:"snapshots,omitempty"`
}

// decode reads the module. Caller holds the config lock. A malformed
// module reads as empty here, and update refuses to write over it.
func (s *Settings) decodeLocked() (module, error) {
	var m module
	raw := s.cfg.Store.Get(Module)
	if raw != nil {
		b, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		if err != nil {
			return module{}, fmt.Errorf("settings: the %s config module is malformed: %w", Module, err)
		}
	}
	if m.Targets == nil {
		m.Targets = map[string]*Target{}
	}
	if m.Jobs == nil {
		m.Jobs = map[string]*Job{}
	}
	if m.Retired == nil {
		m.Retired = map[string]*Retired{}
	}
	return m, nil
}

// read returns a copy of the module.
func (s *Settings) read() (module, error) {
	var (
		m   module
		err error
	)
	s.cfg.Store.View(func() { m, err = s.decodeLocked() })
	return m, err
}

// update applies fn to the module and saves it. The value is replaced
// whole, in the JSON shape the store reloads, so no reader ever holds a
// map this package changes. Caller holds s.mu.
func (s *Settings) update(fn func(*module) error) error {
	var err error
	s.cfg.Store.Mutate(func() {
		var m module
		if m, err = s.decodeLocked(); err != nil {
			return
		}
		if err = fn(&m); err != nil {
			return
		}
		var raw any
		var b []byte
		if b, err = json.Marshal(m); err != nil {
			return
		}
		if err = json.Unmarshal(b, &raw); err != nil {
			return
		}
		s.cfg.Store.Set(Module, raw)
	})
	if err != nil {
		return err
	}
	// A target's key is useless to the operator if it is lost to a crash
	// after they were told it was saved.
	if serr := s.cfg.Store.SaveDirty(); serr != nil {
		s.cfg.Log.Error("Failed to save the backup settings: %s", serr.Error())
	}
	return nil
}

func (s *Settings) nowMs() int64 { return s.cfg.Now().UnixMilli() }
