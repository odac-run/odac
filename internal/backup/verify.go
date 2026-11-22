package backup

import (
	"context"
	"errors"
	"strconv"
	"time"

	"odac/internal/backup/proto"
)

// Share of the files a verification reads in full; every object is looked
// up whatever the share. A filesystem target (a disk, SMB or NFS share,
// and the snapshot repository) costs only time to read, while object
// storage and WebDAV bill or carry every byte, so they are sampled less.
// Successive samples cover the repository over the weeks.
const (
	verifyLocal  = 5.0
	verifyRemote = 1.0
	// verifyFull follows a verification that found damage: the warning
	// only clears once one that reads everything finds none.
	verifyFull = 100.0
	// verifyErrorsKept bounds the damage reports a record carries.
	verifyErrorsKept = 10
)

var errNoVerify = errors.New("backup: odac-backup reported no verification")

// ReasonCorrupt is the snapshot warning of a verification that found
// damage in the snapshot repository.
const ReasonCorrupt = "corrupt"

// verifyDue is when a target is verified next: on its schedule, or right
// after its first maintenance, so a target that does not keep what it is
// given is known about early.
func verifyDue(ts *targetState) time.Time {
	switch {
	case ts == nil:
		return time.Time{}
	case ts.NextVerify > 0:
		return fromMs(ts.NextVerify)
	case ts.LastMaintain > 0:
		return fromMs(ts.LastMaintain)
	}
	return time.Time{}
}

// verify checks a repository in the maintenance slot. What it will read is
// written down before the helper starts, so the server that adopts the
// helper in an update records it the same way.
func (m *Manager) verify(ctx context.Context, target string) {
	start := m.now()
	full := false
	m.state.view(func(d *stateData) {
		if t := d.Targets[target]; t != nil {
			full = t.Corrupt != nil
		}
	})
	pct := 0.0
	res, err := m.callFor(ctx, target, false, m.t.verifyLimit, func(req *proto.Request) {
		pct = verifyLocal
		if req.Repo.Storage.Filesystem == nil {
			pct = verifyRemote
		}
		if full {
			pct = verifyFull
		}
		req.Op, req.Verify = proto.OpVerify, &proto.Verify{Percent: pct}
		if serr := m.state.update(func(d *stateData) { d.target(target).Verifying = &VerifyReport{Percent: pct} }); serr != nil {
			m.log.Error("Failed to save the backup state: %s", serr.Error())
		}
	}, proto.Handler{})
	if errReleasedOr(err) || ctx.Err() != nil {
		return
	}
	m.afterVerify(target, &RunRecord{Kind: KindVerify, Target: target, Start: ms(start)}, pct, res, err)
}

// adoptedVerify records a verification the previous server started.
func (m *Manager) adoptedVerify(target string, res *proto.Result, err error) {
	pct := 0.0
	m.state.view(func(d *stateData) {
		if t := d.Targets[target]; t != nil && t.Verifying != nil {
			pct = t.Verifying.Percent
		}
	})
	m.afterVerify(target, &RunRecord{Kind: KindVerify, Target: target, Start: ms(m.now()), Adopted: true}, pct, res, err)
}

// afterVerify records a verification and schedules the next. One that
// could not run is tried again after the short maintenance interval; one
// that ran is due again a week later. Damage becomes the target's
// corruption warning, which only a verification that read every file
// clears.
func (m *Manager) afterVerify(target string, rec *RunRecord, pct float64, res *proto.Result, err error) {
	now := m.now()
	rec.End, rec.Status = ms(now), StatusOK
	var corrupt *Corruption
	switch {
	case err != nil:
		rec.Status, rec.Error = StatusFailed, err.Error()
	case res == nil || res.Verify == nil:
		rec.Status, rec.Error = StatusFailed, tr("odac-backup reported no verification.")
		err = errNoVerify
	default:
		v := res.Verify
		rep := &VerifyReport{Percent: pct, Snapshots: v.Snapshots, Objects: v.Objects, FilesRead: v.FilesRead, BytesRead: v.BytesRead, Damaged: len(v.Errors)}
		rep.Errors = v.Errors[:min(len(v.Errors), verifyErrorsKept)]
		rec.Verify = rep
		if rep.Damaged > 0 {
			rec.Status = StatusFailed
			rec.Error = tr("Verification found %s damaged objects: restoring the backups that use them can fail. Keep a second target, and see the backup documentation on damage.", strconv.Itoa(rep.Damaged))
			corrupt = &Corruption{At: ms(now), Damaged: rep.Damaged, Errors: rep.Errors}
		}
	}
	cleared := false
	serr := m.state.update(func(d *stateData) {
		t := d.target(target)
		t.LastVerify, t.Verifying = ms(now), nil
		wait := m.t.verifyEvery
		if err != nil {
			wait = m.t.maintainSoon
		}
		t.NextVerify = ms(now.Add(wait))
		switch {
		case corrupt != nil:
			t.Corrupt = corrupt
		case err == nil && pct >= verifyFull && t.Corrupt != nil:
			t.Corrupt, cleared = nil, true
		}
		if target != SnapshotTarget {
			d.History = append(d.History, *rec)
		}
	})
	if serr != nil {
		m.log.Error("Failed to save the backup state: %s", serr.Error())
	}
	if target != SnapshotTarget {
		m.report("", rec)
		return
	}
	switch {
	case corrupt != nil:
		m.warn(ReasonCorrupt, rec.Error)
	case cleared:
		m.clearWarning(ReasonCorrupt)
	}
	m.recordSnapshot(SnapshotRecord{Kind: KindVerify, Status: rec.Status, Start: rec.Start, End: rec.End, Error: rec.Error, Adopted: rec.Adopted, Verify: rec.Verify})
}
