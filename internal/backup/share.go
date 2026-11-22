package backup

import (
	"slices"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
)

// shares reports a request that writes this server's usage file and
// reads the others': maintenance or usage of a filesystem target this
// server claims. The snapshot repository claims nothing and is never
// shared.
func shares(req *proto.Request) bool {
	return req.Repo.Owner != nil && req.Repo.Storage.Filesystem != nil
}

// sharesTarget reports a target whose usage requests share it, which
// makes them writes: they claim the server directory first.
func (m *Manager) sharesTarget(target string) bool {
	if target == SnapshotTarget {
		return false
	}
	a, err := m.access(target, settings.AccessOptions{})
	return err == nil && a.Repo.Storage.Filesystem != nil
}

// peerActive reports a server whose usage file is recent enough for it
// to count as writing to the target. Clocks of other machines are taken
// as they are, within the window either way. A server that stopped
// updating its file is not active: its space counts as taken, and nobody
// deletes its backups.
func peerActive(p proto.PeerUsage, now time.Time, within time.Duration) bool {
	if p.Error != "" || p.Updated.IsZero() {
		return false
	}
	d := now.Sub(p.Updated)
	return d <= within && d >= -within
}

func (m *Manager) activePeers(list []proto.PeerUsage, now time.Time) []thin.Peer {
	var out []thin.Peer
	for _, p := range list {
		if peerActive(p, now, m.t.peerActive) {
			out = append(out, thin.Peer{Used: p.Bytes, Garbage: p.Garbage})
		}
	}
	return out
}

// ServerShare is another server's directory on a shared filesystem
// target, as its usage file reports it. Times are Unix milliseconds:
// Updated is its last measurement, Seen its last write of any kind.
type ServerShare struct {
	Server  string `json:"server"`
	Name    string `json:"name,omitempty"`
	Used    int64  `json:"used,omitempty"`
	Garbage int64  `json:"garbage,omitempty"`
	Updated int64  `json:"updated,omitempty"`
	Seen    int64  `json:"seen,omitempty"`
	// Active: it measured itself recently and takes part in the fair
	// share. One that is not is an orphan: its backups are kept, and only
	// the operator may remove them.
	Active bool   `json:"active,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (m *Manager) serverShares(list []proto.PeerUsage, now time.Time) []ServerShare {
	if len(list) == 0 {
		return nil
	}
	out := make([]ServerShare, 0, len(list))
	for _, p := range list {
		s := ServerShare{Server: p.ID, Name: p.Name, Used: p.Bytes, Garbage: p.Garbage, Active: peerActive(p, now, m.t.peerActive), Error: p.Error}
		if !p.Updated.IsZero() {
			s.Updated = ms(p.Updated)
		}
		if !p.Seen.IsZero() {
			s.Seen = ms(p.Seen)
		}
		out = append(out, s)
	}
	return out
}

// OrphanServer is a server directory on one of this server's targets
// whose server no longer measures itself there: moved, reinstalled or
// gone. Its backups stay until the operator removes them, which no server
// does on its own, since they may be the only copy of a lost machine.
type OrphanServer struct {
	Target string `json:"target"`
	ServerShare
}

// Health is what the Hub warns about beyond the settings' coverage:
// targets whose verification found damage, orphaned server directories on
// shared targets, and targets shared with other active servers.
type Health struct {
	Corrupt []string
	Orphans []OrphanServer
	Shared  []string
}

// Health reports the warnings that come from the targets themselves.
func (m *Manager) Health() Health {
	now := m.now()
	var h Health
	m.state.view(func(d *stateData) {
		for id, t := range d.Targets {
			if id == SnapshotTarget {
				continue
			}
			if t.Corrupt != nil {
				h.Corrupt = append(h.Corrupt, id)
			}
			shared := false
			for _, s := range m.serverShares(t.Peers, now) {
				if s.Active {
					shared = true
					continue
				}
				h.Orphans = append(h.Orphans, OrphanServer{Target: id, ServerShare: s})
			}
			if shared {
				h.Shared = append(h.Shared, id)
			}
		}
	})
	slices.Sort(h.Corrupt)
	slices.Sort(h.Shared)
	slices.SortFunc(h.Orphans, func(a, b OrphanServer) int {
		if c := cmpString(a.Target, b.Target); c != 0 {
			return c
		}
		return cmpString(a.Server, b.Server)
	})
	return h
}
