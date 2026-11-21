package settings

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"time"

	"odac/internal/backup/thin"
)

// Job schedule bounds, in minutes. The lower one keeps a job from living
// under a permanent dump; the upper one keeps "never" from passing for a
// schedule.
const (
	DefaultEvery = 24 * 60
	MinEvery     = 15
	MaxEvery     = 31 * 24 * 60
	maxAgeDays   = 100 * 365
)

// Job is a saved job: what it backs up, where, how often, and how densely
// its backups are kept.
type Job struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	// Apps are named apps; AllApps takes every app, including those added
	// later, which a fixed list would silently leave out.
	Apps    []string `json:"apps,omitempty"`
	AllApps bool     `json:"allApps,omitempty"`
	// Server takes ODAC's own state: config, certificates, DKIM and CA
	// keys, mail, DNS.
	Server bool `json:"server,omitempty"`
	// Every is the interval between runs, in minutes.
	Every int `json:"every"`
	// Density is the thinning rule's k; see thin.Low, Normal and High.
	Density float64 `json:"density"`
	// MaxAgeDays drops backups older than it; zero keeps them.
	MaxAgeDays int `json:"maxAgeDays,omitempty"`
	// Budget caps the bytes the job's backups reference; zero means the
	// target's free space decides.
	Budget  int64 `json:"budget,omitempty"`
	Created int64 `json:"created"`
}

// JobSpec is what the operator gives to create or change a job. Zero
// Every and Density take the defaults: daily, normal density.
type JobSpec struct {
	Name       string   `json:"name"`
	Target     string   `json:"target"`
	Apps       []string `json:"apps,omitempty"`
	AllApps    bool     `json:"allApps,omitempty"`
	Server     bool     `json:"server,omitempty"`
	Every      int      `json:"every,omitempty"`
	Density    float64  `json:"density,omitempty"`
	MaxAgeDays int      `json:"maxAgeDays,omitempty"`
	Budget     int64    `json:"budget,omitempty"`
}

// JobView is a job as lists show it.
type JobView struct {
	ID string `json:"id"`
	Job
}

// Retired is a deleted job whose backups were kept. They are no longer
// thinned (thin.Thin leaves a job without a policy alone) and stay in the
// restore list under the job's name. Purge marks backups the operator
// asked to delete, which the manager removes and then reports with
// Purged.
type Retired struct {
	Name    string `json:"name"`
	Target  string `json:"target"`
	Deleted int64  `json:"deleted"`
	Purge   bool   `json:"purge,omitempty"`
}

// RetiredView is a deleted job as lists show it.
type RetiredView struct {
	ID string `json:"id"`
	Retired
}

// appPattern is the engine's name rule, since an app's name becomes part
// of its backups' source path.
var appPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Policy is the job's thinning rule.
func (j *Job) Policy() thin.Policy {
	return thin.Policy{K: j.Density, MaxAge: daysToDuration(j.MaxAgeDays)}
}

func (s *Settings) normalizeJob(spec JobSpec, m *module, known map[string]bool) (*Job, error) {
	name, err := checkName("job", spec.Name)
	if err != nil {
		return nil, err
	}
	if m.Targets[spec.Target] == nil {
		return nil, errorf(CodeNotFound, "Backup target %s not found.", spec.Target)
	}
	j := &Job{
		Name: name, Target: spec.Target, AllApps: spec.AllApps, Server: spec.Server,
		Every: cmp.Or(spec.Every, DefaultEvery), Density: cmp.Or(spec.Density, thin.Normal),
		MaxAgeDays: spec.MaxAgeDays, Budget: spec.Budget,
	}
	if j.AllApps && len(spec.Apps) > 0 {
		return nil, invalid("A job takes all apps or a list of apps, not both.")
	}
	seen := map[string]bool{}
	for _, app := range spec.Apps {
		if !appPattern.MatchString(app) {
			return nil, invalid("Invalid app name %q.", app)
		}
		if !known[app] {
			return nil, errorf(CodeNotFound, "App %s not found.", app)
		}
		if !seen[app] {
			seen[app] = true
			j.Apps = append(j.Apps, app)
		}
	}
	if !j.AllApps && len(j.Apps) == 0 && !j.Server {
		return nil, invalid("A job backs up at least one app or the server's own state.")
	}
	if j.Every < MinEvery || j.Every > MaxEvery {
		return nil, invalid("A job runs every %d minutes to every %d days.", MinEvery, MaxEvery/(24*60))
	}
	if err := j.Policy().Validate(); err != nil {
		return nil, invalid("Invalid density.")
	}
	if j.MaxAgeDays < 0 || j.MaxAgeDays > maxAgeDays {
		return nil, invalid("A maximum age is 0 to %d days.", maxAgeDays)
	}
	if j.Budget < 0 {
		return nil, invalid("A budget cannot be negative.")
	}
	for _, o := range m.Jobs {
		if strings.EqualFold(o.Name, j.Name) {
			return nil, errorf(CodeExists, "A job named %s already exists.", o.Name)
		}
	}
	return j, nil
}

func daysToDuration(days int) time.Duration { return time.Duration(days) * 24 * time.Hour }

func (s *Settings) knownApps() map[string]bool {
	known := map[string]bool{}
	for _, a := range s.cfg.Apps.BackupApps() {
		known[a.Name] = true
	}
	return known
}

// CreateJob saves a new job.
func (s *Settings) CreateJob(spec JobSpec) (JobView, error) {
	known := s.knownApps()
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID()
	var j *Job
	err := s.update(func(m *module) error {
		var err error
		if j, err = s.normalizeJob(spec, m, known); err != nil {
			return err
		}
		j.Created = s.nowMs()
		m.Jobs[id] = j
		return nil
	})
	if err != nil {
		return JobView{}, err
	}
	s.cfg.Log.Log("Created backup job %s", j.Name)
	return JobView{ID: id, Job: *j}, nil
}

// UpdateJob replaces a job's settings. Its target cannot change: the
// job's backups are on that target and its thinning runs there, so moving
// is a new job, and deleting this one keeps its backups.
func (s *Settings) UpdateJob(id string, spec JobSpec) (JobView, error) {
	known := s.knownApps()
	s.mu.Lock()
	defer s.mu.Unlock()
	var j *Job
	err := s.update(func(m *module) error {
		cur := m.Jobs[id]
		if cur == nil {
			return errorf(CodeNotFound, "Backup job %s not found.", id)
		}
		if spec.Target == "" {
			spec.Target = cur.Target
		}
		if spec.Target != cur.Target {
			return invalid("A job's target cannot change, because its backups are there. Create another job for the new target.")
		}
		others := *m
		others.Jobs = map[string]*Job{}
		for oid, o := range m.Jobs {
			if oid != id {
				others.Jobs[oid] = o
			}
		}
		// Apps that are gone stay on the list, as they were: a job only
		// loses an app when the operator takes it off.
		for _, app := range cur.Apps {
			known[app] = true
		}
		var err error
		if j, err = s.normalizeJob(spec, &others, known); err != nil {
			return err
		}
		j.Created = cur.Created
		m.Jobs[id] = j
		return nil
	})
	if err != nil {
		return JobView{}, err
	}
	s.cfg.Log.Log("Updated backup job %s", j.Name)
	return JobView{ID: id, Job: *j}, nil
}

// DeleteJob deletes a job. Its backups are kept unless purge is set, and
// kept backups are no longer thinned; see Retired.
func (s *Settings) DeleteJob(id string, purge bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	err := s.update(func(m *module) error {
		j := m.Jobs[id]
		if j == nil {
			return errorf(CodeNotFound, "Backup job %s not found.", id)
		}
		name = j.Name
		m.Retired[id] = &Retired{Name: j.Name, Target: j.Target, Deleted: s.nowMs(), Purge: purge}
		delete(m.Jobs, id)
		return nil
	})
	if err != nil {
		return err
	}
	if purge {
		s.cfg.Log.Log("Deleted backup job %s; its backups will be deleted", name)
	} else {
		s.cfg.Log.Log("Deleted backup job %s; its backups are kept", name)
	}
	return nil
}

// Jobs lists the jobs by name.
func (s *Settings) Jobs() ([]JobView, error) {
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(m.Jobs))
	for id, j := range m.Jobs {
		out = append(out, JobView{ID: id, Job: *j})
	}
	slices.SortFunc(out, func(a, b JobView) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Job returns one job.
func (s *Settings) Job(id string) (JobView, error) {
	m, err := s.read()
	if err != nil {
		return JobView{}, err
	}
	j := m.Jobs[id]
	if j == nil {
		return JobView{}, errorf(CodeNotFound, "Backup job %s not found.", id)
	}
	return JobView{ID: id, Job: *j}, nil
}

// Retired lists deleted jobs whose backups are still on a target.
func (s *Settings) Retired() ([]RetiredView, error) {
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]RetiredView, 0, len(m.Retired))
	for id, r := range m.Retired {
		out = append(out, RetiredView{ID: id, Retired: *r})
	}
	slices.SortFunc(out, func(a, b RetiredView) int {
		return cmp.Or(cmp.Compare(b.Deleted, a.Deleted), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// PurgeRetired asks for a deleted job's kept backups to be deleted.
func (s *Settings) PurgeRetired(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(m *module) error {
		r := m.Retired[id]
		if r == nil {
			return errorf(CodeNotFound, "Deleted backup job %s not found.", id)
		}
		r.Purge = true
		return nil
	})
}

// Purged is the manager's report that a deleted job's backups are gone.
func (s *Settings) Purged(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(m *module) error {
		if r := m.Retired[id]; r != nil && r.Purge {
			delete(m.Retired, id)
		}
		return nil
	})
}
