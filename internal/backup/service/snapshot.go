package service

import (
	"math"
	"strconv"

	"odac/internal/api"
	"odac/internal/backup"
	"odac/internal/backup/settings"
	"odac/internal/lang"
)

// The snapshot.* actions. Snapshots are the system's own: the operator
// may turn them off, change their share of the disk, take one, delete one
// and restore one, and nothing else. Like backup.*, every action is
// closed to app tokens.

// SnapshotSettings returns the snapshot settings: {enabled, maxDiskPct}.
func (s *Service) SnapshotSettings() api.Result {
	v, err := s.cfg.Settings.Snapshots()
	if err != nil {
		return fail(err)
	}
	return ok(nil, v)
}

// UpdateSnapshotSettings changes them: {enabled?, maxDiskPct?}. Turning
// snapshots off takes no new ones and keeps those there are, which
// snapshot.delete removes.
func (s *Service) UpdateSnapshotSettings(payload any) api.Result {
	p, err := decode[settings.SnapshotsPatch](payload)
	if err != nil {
		return badPayload(err)
	}
	v, err := s.cfg.Settings.SetSnapshots(p)
	if err != nil {
		return fail(err)
	}
	s.cfg.Manager.Wake()
	if !v.Enabled {
		return ok(lang.T("Snapshots are off. The ones already taken stay restorable until they are deleted."), v)
	}
	return ok(lang.T("Snapshots are on and may take %s percent of the disk.", strconv.Itoa(v.MaxDiskPct)), v)
}

// SnapshotStatus reports where snapshots stand.
func (s *Service) SnapshotStatus() api.Result {
	return ok(nil, s.cfg.Manager.SnapshotStatus())
}

// SnapshotHistory lists the snapshots taken and given up, and the
// maintenance of their repository, newest first: {app, limit}.
func (s *Service) SnapshotHistory(payload any) api.Result {
	in, err := decode[struct {
		App   string  `json:"app"`
		Limit float64 `json:"limit"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	limit := DefaultHistory
	if in.Limit > 0 {
		limit = int(math.Min(in.Limit, MaxHistory))
	}
	list := s.cfg.Manager.SnapshotHistory(in.App, limit)
	if list == nil {
		list = []backup.SnapshotRecord{}
	}
	return ok(nil, list)
}

// SnapshotPoints lists the snapshots a restore can return to, newest
// first, of one app when the payload names it: {app}.
func (s *Service) SnapshotPoints(payload any) api.Result {
	in, err := decode[struct {
		App string `json:"app"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	list := s.cfg.Manager.SnapshotPoints(in.App)
	if list == nil {
		list = []backup.SnapshotPoint{}
	}
	return ok(nil, list)
}

// TakeSnapshot takes a snapshot of an app and the apps it links, now:
// {app}. It runs in the background; its outcome is in the history.
func (s *Service) TakeSnapshot(payload any) api.Result {
	in, err := decode[struct {
		App string `json:"app"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	r, err := s.cfg.Manager.TakeSnapshot(in.App)
	if err != nil {
		return fail(err)
	}
	return ok(lang.T("Snapshot %s of %s started.", r.Run, r.App), r)
}

// DeleteSnapshot deletes one snapshot: {run}. Its space comes back with
// the repository's maintenance.
func (s *Service) DeleteSnapshot(payload any) api.Result {
	in, err := decode[struct {
		Run string `json:"run"`
	}](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	if err := s.cfg.Manager.DeleteSnapshot(ctx, in.Run); err != nil {
		return fail(err)
	}
	return api.Res(true, lang.T("Snapshot %s deleted.", in.Run))
}

// PreviewSnapshotRestore works a snapshot restore out without changing
// anything: {run, apps, mode, names, skipSafety}. The mode is in-place
// unless asked otherwise, or new-app when an app of the snapshot no
// longer exists (a deleted app brought back).
func (s *Service) PreviewSnapshotRestore(payload any) api.Result {
	req, err := decode[backup.SnapshotRestoreRequest](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	r, err := s.cfg.Manager.PreviewSnapshotRestore(ctx, req)
	if err != nil {
		return fail(err)
	}
	return ok(nil, r)
}

// StartSnapshotRestore starts a snapshot restore in the background and
// answers with its plan; its progress is in backup.restore.list, under the
// target "snapshots". The payload is as for PreviewSnapshotRestore.
func (s *Service) StartSnapshotRestore(payload any) api.Result {
	req, err := decode[backup.SnapshotRestoreRequest](payload)
	if err != nil {
		return badPayload(err)
	}
	ctx, cancel := s.ctx(s.limits.list)
	defer cancel()
	r, err := s.cfg.Manager.StartSnapshotRestore(ctx, req)
	if err != nil {
		return fail(err)
	}
	msg := lang.T("Restore %s started.", r.ID)
	if r.Mode == backup.RestoreInPlace {
		msg = lang.T("Restore %s started. Its apps are stopped until it ends.", r.ID)
	}
	return ok(msg, r)
}
