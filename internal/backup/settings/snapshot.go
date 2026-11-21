package settings

import "fmt"

// Snapshot defaults: on, and at most a quarter of the disk the swap rule
// would also count (free space plus the snapshot store's own size).
const (
	DefaultMaxDiskPct = 25
	MaxMaxDiskPct     = 50
)

// Snapshots are the two things an operator may change about snapshots,
// which the system otherwise manages alone.
type Snapshots struct {
	Enabled    bool `json:"enabled"`
	MaxDiskPct int  `json:"maxDiskPct"`
}

// SnapshotsPatch changes the members it sets.
type SnapshotsPatch struct {
	Enabled    *bool `json:"enabled,omitempty"`
	MaxDiskPct *int  `json:"maxDiskPct,omitempty"`
}

func (m *module) snapshots() Snapshots {
	if m.Snapshots == nil {
		return Snapshots{Enabled: true, MaxDiskPct: DefaultMaxDiskPct}
	}
	return *m.Snapshots
}

// Snapshots returns the snapshot settings.
func (s *Settings) Snapshots() (Snapshots, error) {
	m, err := s.read()
	if err != nil {
		return Snapshots{}, err
	}
	return m.snapshots(), nil
}

// SetSnapshots changes the snapshot settings.
func (s *Settings) SetSnapshots(p SnapshotsPatch) (Snapshots, error) {
	if p.MaxDiskPct != nil && (*p.MaxDiskPct < 1 || *p.MaxDiskPct > MaxMaxDiskPct) {
		return Snapshots{}, invalid("Snapshots may take 1%% to %d%% of the disk.", MaxMaxDiskPct)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Snapshots
	err := s.update(func(m *module) error {
		out = m.snapshots()
		if p.Enabled != nil {
			out.Enabled = *p.Enabled
		}
		if p.MaxDiskPct != nil {
			out.MaxDiskPct = *p.MaxDiskPct
		}
		m.Snapshots = &out
		return nil
	})
	if err != nil {
		return Snapshots{}, err
	}
	s.cfg.Log.Log(fmt.Sprintf("Snapshots: enabled %v, at most %d%% of the disk", out.Enabled, out.MaxDiskPct))
	return out, nil
}
