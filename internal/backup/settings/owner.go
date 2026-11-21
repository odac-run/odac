package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The owner sequence numbers live in files of their own, one per target,
// and not in the config module: a config module is restored from its .bak
// copy when it is found damaged, and the watchdog saves every module from
// its own copy at start. Either would hand the server a number it already
// sent, and the next run would read its own earlier write as a clone.

func ownerDir(base string) string { return filepath.Join(base, "backup", "owner") }

// Conflict is a stop on writes to a target: its ownership file said that
// another machine writes there under this server id.
type Conflict struct {
	Message string `json:"message"`
	At      int64  `json:"at"`
}

type ownerState struct {
	Seq      int64     `json:"seq"`
	Conflict *Conflict `json:"conflict,omitempty"`
}

type owners struct {
	dir   string
	mu    sync.Mutex
	cache map[string]*ownerState
}

func newOwners(dir string) *owners {
	return &owners{dir: dir, cache: map[string]*ownerState{}}
}

func (o *owners) file(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid target id %q", id)
	}
	return filepath.Join(o.dir, id+".json"), nil
}

// loadLocked reads a target's state. A missing file is a target never
// written to; a damaged one is an error, because guessing a number low
// reads the server's own writes as a clone and guessing high hides one.
func (o *owners) loadLocked(id string) (*ownerState, error) {
	if st := o.cache[id]; st != nil {
		return st, nil
	}
	name, err := o.file(id)
	if err != nil {
		return nil, err
	}
	st := &ownerState{}
	raw, err := os.ReadFile(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, st); err != nil || st.Seq < 0 {
			return nil, fmt.Errorf("%s is damaged; adopting the target writes it again", name)
		}
	}
	o.cache[id] = st
	return st, nil
}

// storeLocked writes the state durably before anyone acts on it.
func (o *owners) storeLocked(id string, st ownerState) error {
	name, err := o.file(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(o.dir, "."+id+".tmp-*")
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
	if err := os.Rename(tmp.Name(), name); err != nil {
		return err
	}
	if d, err := os.Open(o.dir); err == nil {
		d.Sync()
		d.Close()
	}
	o.cache[id] = &st
	return nil
}

// next reserves the number the next write sends. An adopt starts over
// from a damaged state, since the number it writes is the target's.
func (o *owners) next(id string, adopt bool) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, err := o.loadLocked(id)
	if err != nil {
		if !adopt {
			return 0, err
		}
		st = &ownerState{}
	}
	n := *st
	n.Seq++
	if err := o.storeLocked(id, n); err != nil {
		return 0, err
	}
	return n.Seq, nil
}

// adopted records the number an adopt wrote and lifts the stop.
func (o *owners) adopted(id string, seq int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, err := o.loadLocked(id)
	if err != nil {
		st = &ownerState{}
	}
	return o.storeLocked(id, ownerState{Seq: max(seq, st.Seq)})
}

func (o *owners) setConflict(id, msg string, at int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, err := o.loadLocked(id)
	if err != nil {
		return err
	}
	n := *st
	n.Conflict = &Conflict{Message: msg, At: at}
	return o.storeLocked(id, n)
}

// conflict reports the stop on a target, if any. A state that cannot be
// read is a stop too.
func (o *owners) conflict(id string) *Conflict {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, err := o.loadLocked(id)
	if err != nil {
		return &Conflict{Message: err.Error()}
	}
	if st.Conflict == nil {
		return nil
	}
	c := *st.Conflict
	return &c
}

func (o *owners) forget(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cache, id)
	name, err := o.file(id)
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
