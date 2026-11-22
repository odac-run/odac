// Package service is the operator's door to backups: it turns the API's
// and the Hub's requests into calls on the backup settings and manager and
// their answers into api.Result. Both doors use the same methods, with the
// same object payload, so a dashboard and the CLI cannot drift apart.
//
// Every action here is closed to app tokens (api.AppMayCall): a target is
// host root (it bind-mounts a host path), a restore overwrites another
// app's data, and a key opens every backup. The key and the staging of
// ODAC's own state also check the caller, so a call site that forgot to
// fill it reaches neither.
//
// Replies follow the dashboard's serialization-diff rules: lists are
// sorted and never null, and members that have nothing to say are absent.
// No list carries a secret; a target's key leaves only through Key, and
// once through PrepareTarget, which is how the operator first sees it.
package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"odac/internal/api"
	"odac/internal/backup"
	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
	"odac/internal/lang"
)

// Manager is the slice of *backup.Manager the service drives.
type Manager interface {
	Status() ([]backup.JobStatus, []backup.TargetStatus)
	History(job string, limit int) []backup.RunRecord
	RunNow(job string) (backup.JobStatus, error)
	Wake()
	Servers(ctx context.Context, target string) ([]proto.ServerDir, error)
	RestorePoints(ctx context.Context, target, server, key, app string) ([]backup.RestorePoint, error)
	PreviewRestore(ctx context.Context, req backup.RestoreRequest) (*backup.RestoreResult, error)
	StartRestore(ctx context.Context, req backup.RestoreRequest) (*backup.RestoreResult, error)
	CancelRestore(id string) bool
	Restores() []backup.RestoreResult
	StageServerRestore(ctx context.Context, req backup.ServerRestoreRequest) (*backup.ServerRestore, error)
}

// Settings is the slice of *settings.Settings the service drives.
type Settings interface {
	Targets() ([]settings.TargetView, error)
	Target(id string) (settings.TargetView, error)
	PrepareTarget(ctx context.Context, spec settings.TargetSpec) (settings.Pending, error)
	ConfirmTarget(ctx context.Context, token string, adopt bool) (settings.TargetView, error)
	CancelTarget(token string)
	UpdateTarget(ctx context.Context, id string, spec settings.TargetSpec) (settings.TargetView, error)
	DeleteTarget(id string) error
	Adopt(ctx context.Context, id string) (settings.TargetView, error)
	Key(id string) (settings.Recovery, error)
	Jobs() ([]settings.JobView, error)
	Job(id string) (settings.JobView, error)
	CreateJob(spec settings.JobSpec) (settings.JobView, error)
	UpdateJob(id string, spec settings.JobSpec) (settings.JobView, error)
	DeleteJob(id string, purge bool) error
	Retired() ([]settings.RetiredView, error)
	PurgeRetired(id string) error
	Coverage() (settings.Coverage, error)
}

// Config wires a Service.
type Config struct {
	Manager  Manager
	Settings Settings
	// ServerID is this server's id, which marks its own directory among a
	// target's server directories.
	ServerID string
	// BaseDir is ODAC's base directory, where a staged restore of ODAC's
	// own state waits for the next start.
	BaseDir string
}

// Service answers the backup actions.
type Service struct {
	cfg Config
	// limits bound each kind of call; tests shrink them.
	limits limits
}

type limits struct {
	locate  time.Duration
	open    time.Duration
	list    time.Duration
	restore time.Duration
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Manager == nil || cfg.Settings == nil {
		return nil, errors.New("service: a manager and settings are required")
	}
	return &Service{cfg: cfg, limits: limits{
		// A new target's location is a name lookup.
		locate: 30 * time.Second,
		// Opening a repository runs a helper; a remote one over a slow
		// link takes its time.
		open: 5 * time.Minute,
		// Listing walks every snapshot in the repository.
		list: 10 * time.Minute,
		// Staging ODAC's own state restores its directories.
		restore: time.Hour,
	}}, nil
}

// Error codes the service adds to the settings' and odac-backup's own.
const (
	// CodeFailed is a failure that is not a refusal: a run that broke, a
	// state file that could not be written.
	CodeFailed = "failed"
	// CodeTimeout is a call that did not finish within its limit.
	CodeTimeout = "timeout"
	// CodeDenied is a caller who may not see what was asked for.
	CodeDenied = "permission_denied"
)

// ok answers with data encoded once, as JSON: the Hub signs what it
// sends, and a struct is not something it can canonicalize.
func ok(message any, data any) api.Result {
	raw, err := json.Marshal(data)
	if err != nil {
		return fail(err)
	}
	return api.Res(true, message, json.RawMessage(raw))
}

// fail turns an error into a refusal with a stable code: the settings'
// codes, odac-backup's protocol codes, or CodeFailed.
func fail(err error) api.Result {
	var se *settings.Error
	var pe *proto.Error
	switch {
	case errors.As(err, &se):
		return api.Res(false, se.Message, map[string]any{"code": se.Code})
	case errors.Is(err, context.DeadlineExceeded):
		return api.Res(false, lang.T("The backup operation did not finish in time."), map[string]any{"code": CodeTimeout})
	case errors.As(err, &pe):
		return api.Res(false, pe.Message, map[string]any{"code": pe.Code})
	}
	return api.Res(false, err.Error(), map[string]any{"code": CodeFailed})
}

func invalid(format string, a ...any) api.Result {
	return api.Res(false, lang.T(format, a...), map[string]any{"code": settings.CodeInvalid})
}

func denied() api.Result {
	return api.Res(false, lang.T("Only the server's operator may do this."), map[string]any{"code": CodeDenied})
}

// object reads a payload as a JSON object; nil reads as empty.
func object(payload any) (map[string]any, bool) {
	if payload == nil {
		return map[string]any{}, true
	}
	m, isMap := payload.(map[string]any)
	return m, isMap
}

// decode reads a payload into T by its JSON names. Unknown members are
// ignored, so a newer dashboard can send what this server does not know
// yet.
func decode[T any](payload any) (T, error) {
	var out T
	if payload == nil {
		return out, nil
	}
	if _, isMap := payload.(map[string]any); !isMap {
		return out, errors.New("not an object")
	}
	raw, err := json.Marshal(payload)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func badPayload(err error) api.Result {
	return invalid("Invalid request: %s", err.Error())
}

// idOf reads the payload's id member.
func idOf(payload any) string {
	m, _ := object(payload)
	s, _ := m["id"].(string)
	return s
}

func (s *Service) ctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// Targets lists the targets, by name.
func (s *Service) Targets() api.Result {
	list, err := s.cfg.Settings.Targets()
	if err != nil {
		return fail(err)
	}
	return ok(nil, list)
}

// PrepareTarget checks a new target and returns its key, shown once, with
// the token ConfirmTarget saves it with. The payload is a target spec:
// name, one of disk, smb, nfs, s3 and webdav, and an optional key of the
// operator's own.
func (s *Service) PrepareTarget(payload any) api.Result {
	spec, err := decode[settings.TargetSpec](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.locate)
	defer cancel()
	p, err := s.cfg.Settings.PrepareTarget(ctx, spec)
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("Save this key before you confirm: without it and without this server, the backups cannot be opened."), p)
}

// ConfirmTarget saves a prepared target: {token, adopt}. adopt takes over
// a directory this server wrote on the target before.
func (s *Service) ConfirmTarget(payload any) api.Result {
	in, err := decode[struct {
		Token string `json:"token"`
		Adopt bool   `json:"adopt"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.open)
	defer cancel()
	v, err := s.cfg.Settings.ConfirmTarget(ctx, in.Token, in.Adopt)
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("Backup target %s added.", v.Name), v)
}

// CancelTarget drops a prepared target: {token}.
func (s *Service) CancelTarget(payload any) api.Result {
	m, _ := object(payload)
	token, _ := m["token"].(string)
	s.cfg.Settings.CancelTarget(token)
	return api.Res(true, lang.T("The target was not added."))
}

// targetSpecOnly are the members of a TargetView that are not part of a
// spec: what the server decides or records.
var targetSpecOnly = []string{"id", "kind", "keySource", "location", "sameDisk", "created", "conflict"}

// UpdateTarget renames a target or replaces its credentials: {id, ...}.
// Members left out keep their value, the kind's object member by member,
// so a rename is {id, name} and a new S3 secret {id, s3: {secretAccessKey}}.
func (s *Service) UpdateTarget(payload any) api.Result {
	patch, isMap := object(payload)
	if !isMap {
		return badPayload(errors.New("not an object"))
	}
	id := idOf(payload)
	cur, err := s.cfg.Settings.Target(id)
	if err != nil {
		return fail(err)
	}
	base, err := toMap(cur)
	if err != nil {
		return fail(err)
	}
	for _, k := range targetSpecOnly {
		delete(base, k)
	}
	for k, v := range patch {
		if k == "id" {
			continue
		}
		if inner, isMap := v.(map[string]any); isMap {
			if prev, isMap := base[k].(map[string]any); isMap {
				for ik, iv := range inner {
					prev[ik] = iv
				}
				continue
			}
		}
		base[k] = v
	}
	spec, err := decode[settings.TargetSpec](base)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.open)
	defer cancel()
	v, err := s.cfg.Settings.UpdateTarget(ctx, id, spec)
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("Backup target %s updated.", v.Name), v)
}

// DeleteTarget removes a target: {id}. Its backups stay on the storage.
func (s *Service) DeleteTarget(payload any) api.Result {
	if err := s.cfg.Settings.DeleteTarget(idOf(payload)); err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	return api.Res(true, lang.T("Backup target deleted. Its backups stay on the storage, and adding it again with its key brings them back."))
}

// AdoptTarget takes this server's directory on a target over and lets
// writes go on after an owner conflict: {id}.
func (s *Service) AdoptTarget(payload any) api.Result {
	ctx, cancel := s.ctx(s.limits.open)
	defer cancel()
	v, err := s.cfg.Settings.Adopt(ctx, idOf(payload))
	if err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	return ok(lang.T("This server writes to backup target %s again.", v.Name), v)
}

// ServerDir is one server's directory on a target. Times are Unix
// milliseconds.
type ServerDir struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Self marks this server's own directory.
	Self       bool   `json:"self,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	Updated    int64  `json:"updated,omitempty"`
	Repository bool   `json:"repository,omitempty"`
	Error      string `json:"error,omitempty"`
}

// TargetServers lists the server directories on a target, the choices of
// a restore from another server: {id}.
func (s *Service) TargetServers(payload any) api.Result {
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	dirs, err := s.cfg.Manager.Servers(ctx, idOf(payload))
	if err != nil {
		return fail(err)
	}
	out := make([]ServerDir, 0, len(dirs))
	for _, d := range dirs {
		v := ServerDir{ID: d.ID, Name: d.Name, Self: d.ID == s.cfg.ServerID, Seq: d.Seq, Repository: d.Repository, Error: d.Error}
		if !d.Updated.IsZero() {
			v.Updated = d.Updated.UnixMilli()
		}
		out = append(out, v)
	}
	sortServers(out)
	return ok(nil, out)
}

// Key returns a target's key with what recovery needs: {id}. Only the
// operator reaches it; an app token never reaches the action at all.
func (s *Service) Key(caller api.Caller, payload any) api.Result {
	if !caller.Root {
		return denied()
	}
	r, err := s.cfg.Settings.Key(idOf(payload))
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("Keep this key somewhere other than this server. If it is lost together with the server, the backups cannot be opened by anyone."), r)
}

// Jobs lists the jobs, by name.
func (s *Service) Jobs() api.Result {
	list, err := s.cfg.Settings.Jobs()
	if err != nil {
		return fail(err)
	}
	return ok(nil, list)
}

// densities are the presets a dashboard offers instead of a number.
var densities = map[string]float64{"low": thin.Low, "normal": thin.Normal, "high": thin.High}

// jobPayload reads a job spec, accepting a density preset by name.
func jobPayload(m map[string]any) (settings.JobSpec, error) {
	if d, isString := m["density"].(string); isString {
		k, known := densities[d]
		if !known {
			return settings.JobSpec{}, fmt.Errorf("density is low, normal, high or a number")
		}
		c := make(map[string]any, len(m))
		for key, v := range m {
			c[key] = v
		}
		c["density"] = k
		m = c
	}
	return decode[settings.JobSpec](m)
}

// CreateJob saves a new job: a job spec (name, target, apps or allApps,
// server, every in minutes, density, maxAgeDays, budget in bytes). Its
// first run starts at once.
func (s *Service) CreateJob(payload any) api.Result {
	m, isMap := object(payload)
	if !isMap {
		return badPayload(errors.New("not an object"))
	}
	spec, err := jobPayload(m)
	if err != nil {
		return badPayload(err)
	}
	v, err := s.cfg.Settings.CreateJob(spec)
	if err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	return ok(lang.T("Backup job %s created; its first run starts now.", v.Name), v)
}

// jobSpecOnly are the members of a JobView that are not part of a spec.
var jobSpecOnly = []string{"id", "created"}

// UpdateJob changes a job: {id, ...}. Members left out keep their value;
// apps and allApps replace each other.
func (s *Service) UpdateJob(payload any) api.Result {
	patch, isMap := object(payload)
	if !isMap {
		return badPayload(errors.New("not an object"))
	}
	id := idOf(payload)
	cur, err := s.cfg.Settings.Job(id)
	if err != nil {
		return fail(err)
	}
	base, err := toMap(cur)
	if err != nil {
		return fail(err)
	}
	for _, k := range jobSpecOnly {
		delete(base, k)
	}
	if _, set := patch["apps"]; set {
		delete(base, "allApps")
	}
	if all, _ := patch["allApps"].(bool); all {
		delete(base, "apps")
	}
	for k, v := range patch {
		if k != "id" {
			base[k] = v
		}
	}
	spec, err := jobPayload(base)
	if err != nil {
		return badPayload(err)
	}
	v, err := s.cfg.Settings.UpdateJob(id, spec)
	if err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	return ok(lang.T("Backup job %s updated.", v.Name), v)
}

// DeleteJob deletes a job: {id, purge}. Its backups are kept unless purge
// is set; kept backups are no longer thinned and stay restorable.
func (s *Service) DeleteJob(payload any) api.Result {
	in, err := decode[struct {
		ID    string `json:"id"`
		Purge bool   `json:"purge"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	if err := s.cfg.Settings.DeleteJob(in.ID, in.Purge); err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	if in.Purge {
		return api.Res(true, lang.T("Backup job deleted; its backups are deleted with the next maintenance of its target."))
	}
	return api.Res(true, lang.T("Backup job deleted; its backups are kept and stay restorable."))
}

// RunJob asks for a run of a job now: {id}. It waits for its turn like any
// run; the schedule keeps its own clock.
func (s *Service) RunJob(payload any) api.Result {
	st, err := s.cfg.Manager.RunNow(idOf(payload))
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("The run is queued and starts as soon as no other backup or restore holds the server."), st)
}

// Retired lists deleted jobs whose backups are kept, newest first.
func (s *Service) Retired() api.Result {
	list, err := s.cfg.Settings.Retired()
	if err != nil {
		return fail(err)
	}
	return ok(nil, list)
}

// PurgeRetired deletes a deleted job's kept backups: {id}.
func (s *Service) PurgeRetired(payload any) api.Result {
	if err := s.cfg.Settings.PurgeRetired(idOf(payload)); err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	return api.Res(true, lang.T("The backups are deleted with the next maintenance of their target."))
}

// Coverage reports what the jobs leave unprotected.
func (s *Service) Coverage() api.Result {
	c, err := s.cfg.Settings.Coverage()
	if err != nil {
		return fail(err)
	}
	return ok(nil, c)
}

// Status is where every job and target stands.
type Status struct {
	Jobs    []backup.JobStatus    `json:"jobs"`
	Targets []backup.TargetStatus `json:"targets"`
}

// Status reports the schedule and the targets' last measurements.
func (s *Service) Status() api.Result {
	jobs, targets := s.cfg.Manager.Status()
	if jobs == nil {
		jobs = []backup.JobStatus{}
	}
	if targets == nil {
		targets = []backup.TargetStatus{}
	}
	return ok(nil, Status{Jobs: jobs, Targets: targets})
}

// History bounds: the default is what a dashboard shows, the maximum what
// the manager keeps.
const (
	DefaultHistory = 50
	MaxHistory     = 200
)

// History lists finished runs and maintenance, newest first: {job, limit}.
func (s *Service) History(payload any) api.Result {
	in, err := decode[struct {
		Job   string  `json:"job"`
		Limit float64 `json:"limit"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	limit := DefaultHistory
	if in.Limit > 0 {
		limit = int(math.Min(in.Limit, MaxHistory))
	}
	list := s.cfg.Manager.History(in.Job, limit)
	if list == nil {
		list = []backup.RunRecord{}
	}
	return ok(nil, list)
}

// pointRequest names a server's directory on a target. Key opens it when
// its key is not the target's; it is never echoed back.
type pointRequest struct {
	Target string `json:"target"`
	Server string `json:"server"`
	Key    string `json:"key"`
	App    string `json:"app"`
}

// Points lists the runs a restore can return to, newest first: {target,
// server, key, app}.
func (s *Service) Points(payload any) api.Result {
	in, err := decode[pointRequest](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	list, err := s.cfg.Manager.RestorePoints(ctx, in.Target, in.Server, in.Key, in.App)
	if err != nil {
		return fail(err)
	}
	if list == nil {
		list = []backup.RestorePoint{}
	}
	return ok(nil, list)
}

// restoreRequest is a RestoreRequest as a payload: its key is read here,
// since the request type keeps it out of every encoding.
type restoreRequest struct {
	backup.RestoreRequest
	Key string `json:"key"`
}

// request reads a restore and fills its default mode: a backup comes back
// as a new app, which leaves the live one alone. In place is the
// operator's to ask for, because it gives up what was written since.
func request(payload any) (backup.RestoreRequest, error) {
	in, err := decode[restoreRequest](payload)
	if err != nil {
		return backup.RestoreRequest{}, err
	}
	req := in.RestoreRequest
	req.Key = in.Key
	if req.Mode == "" {
		req.Mode = backup.RestoreNewApp
	}
	return req, nil
}

// PreviewRestore works a restore out without changing anything: which apps
// come back, how, and in place how much of their history they give up.
// The payload is a restore request: target, server, key, run, apps, mode
// (new-app by default, or in-place), names and skipSafety.
func (s *Service) PreviewRestore(payload any) api.Result {
	req, err := request(payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	r, err := s.cfg.Manager.PreviewRestore(ctx, req)
	if err != nil {
		return fail(err)
	}
	return ok(nil, r)
}

// StartRestore starts a restore in the background and answers with its
// plan; its progress is in Restores. The payload is as for PreviewRestore.
func (s *Service) StartRestore(payload any) api.Result {
	req, err := request(payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	r, err := s.cfg.Manager.StartRestore(ctx, req)
	if err != nil {
		return fail(err)
	}
	msg := lang.T("Restore %s started.", r.ID)
	if r.Mode == backup.RestoreInPlace {
		msg = lang.T("Restore %s started. Its apps are stopped until it ends.", r.ID)
	}
	return ok(msg, r)
}

// CancelRestore stops the restore in flight, which rolls it back: {id}.
func (s *Service) CancelRestore(payload any) api.Result {
	if !s.cfg.Manager.CancelRestore(idOf(payload)) {
		return api.Res(false, lang.T("No restore with this id is in progress."), map[string]any{"code": settings.CodeNotFound})
	}
	return api.Res(true, lang.T("The restore is stopped and rolled back."))
}

// Restores lists the restore in flight, then the finished ones, newest
// first.
func (s *Service) Restores() api.Result {
	list := s.cfg.Manager.Restores()
	if list == nil {
		list = []backup.RestoreResult{}
	}
	return ok(nil, list)
}

// StageServerRestore restores ODAC's own state from a run beside the live
// one, for the next start of the server to apply: {target, server, key,
// run}. It is the first step of moving onto a freshly installed server,
// which is why it refuses a server that has apps, and it is the
// operator's alone.
func (s *Service) StageServerRestore(caller api.Caller, payload any) api.Result {
	if !caller.Root {
		return denied()
	}
	in, err := decode[pointRequest](payload)
	if err != nil {
		return badPayload(err)
	}
	m, _ := object(payload)
	run, _ := m["run"].(string)
	ctx, cancel := s.ctx(s.limits.restore)
	defer cancel()
	sr, err := s.cfg.Manager.StageServerRestore(ctx, backup.ServerRestoreRequest{Target: in.Target, Server: in.Server, Key: in.Key, Run: run})
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("ODAC's own state from run %s is staged. Restart ODAC to apply it; every restored app stays stopped until its data is restored.", sr.Run), sr)
}

// PendingServerRestore returns the staged restore of ODAC's own state, or
// null when nothing is staged.
func (s *Service) PendingServerRestore(caller api.Caller) api.Result {
	if !caller.Root {
		return denied()
	}
	sr, err := backup.PendingServerRestore(s.cfg.BaseDir)
	if err != nil {
		return fail(err)
	}
	if sr == nil {
		return api.Res(true, lang.T("No restore of ODAC's own state is staged."), nil)
	}
	return ok(nil, sr)
}

// CancelServerRestore drops a staged restore of ODAC's own state that the
// server has not begun to apply.
func (s *Service) CancelServerRestore(caller api.Caller) api.Result {
	if !caller.Root {
		return denied()
	}
	if err := backup.CancelServerRestore(s.cfg.BaseDir); err != nil {
		return fail(err)
	}
	return api.Res(true, lang.T("No restore of ODAC's own state is staged any more."))
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(raw, &m)
	return m, err
}

// sortServers puts this server first, then the others by name and id.
func sortServers(list []ServerDir) {
	slices.SortFunc(list, func(a, b ServerDir) int {
		if a.Self != b.Self {
			if a.Self {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
}
