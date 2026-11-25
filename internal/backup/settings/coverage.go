package settings

import (
	"slices"
)

// Coverage is what the Hub warns about. Every member is absent when there
// is nothing to say, so an unchanged state serializes the same way.
type Coverage struct {
	// Unprotected are stateful apps no job backs up.
	Unprotected []string `json:"unprotected,omitempty"`
	// MissingLinks are apps a job backs up whose linked app, which holds
	// data of its own, the job leaves out: a restore would bring the app
	// back without the database it changed with.
	MissingLinks []MissingLink `json:"missingLinks,omitempty"`
	// NoJobs: there is no job at all, so the only copies are snapshots,
	// which are lost with the server.
	NoJobs bool `json:"noJobs,omitempty"`
	// OnlyLocal: every target is on this server, which does not survive
	// the server, the account or ransomware.
	OnlyLocal bool `json:"onlyLocal,omitempty"`
	// SameDisk are targets on the disk that holds the data itself.
	SameDisk []string `json:"sameDisk,omitempty"`
	// Conflicts are targets whose writes are stopped; see Adopt.
	Conflicts []string `json:"conflicts,omitempty"`
}

// MissingLink is one suggestion: add Linked to Job, which has App.
type MissingLink struct {
	Job    string `json:"job"`
	App    string `json:"app"`
	Linked string `json:"linked"`
}

// Coverage reports the gaps in what the jobs protect.
func (s *Settings) Coverage() (Coverage, error) {
	m, err := s.read()
	if err != nil {
		return Coverage{}, err
	}
	// Asked after the config lock is released: the app manager reads the
	// same store.
	apps := s.cfg.Apps.BackupApps()
	byName := make(map[string]App, len(apps))
	for _, a := range apps {
		byName[a.Name] = a
	}

	var c Coverage
	covered := map[string]bool{}
	all := false
	jobIDs := make([]string, 0, len(m.Jobs))
	for id, j := range m.Jobs {
		jobIDs = append(jobIDs, id)
		all = all || j.AllApps
		for _, a := range j.Apps {
			covered[a] = true
		}
	}
	slices.Sort(jobIDs)
	c.NoJobs = len(m.Jobs) == 0
	for _, a := range apps {
		if a.Stateful && !all && !covered[a.Name] {
			c.Unprotected = append(c.Unprotected, a.Name)
		}
	}
	for _, id := range jobIDs {
		j := m.Jobs[id]
		if j.AllApps {
			continue
		}
		in := map[string]bool{}
		for _, a := range j.Apps {
			in[a] = true
		}
		for _, a := range j.Apps {
			for _, l := range byName[a].Linked {
				if linked, ok := byName[l]; ok && linked.Stateful && !in[l] {
					c.MissingLinks = append(c.MissingLinks, MissingLink{Job: id, App: a, Linked: l})
					in[l] = true
				}
			}
		}
	}

	local := len(m.Targets) > 0
	for id, t := range m.Targets {
		local = local && t.Location == Local
		if t.SameDisk {
			c.SameDisk = append(c.SameDisk, id)
		}
		if s.owners.conflict(id) != nil {
			c.Conflicts = append(c.Conflicts, id)
		}
	}
	c.OnlyLocal = local
	slices.Sort(c.Unprotected)
	slices.Sort(c.SameDisk)
	slices.Sort(c.Conflicts)
	return c, nil
}
