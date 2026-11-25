package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/settings"
	"odac/internal/backup/thin"
	"odac/internal/system/diskbudget"
)

// Snapshots are short-lived backups in a repository of the manager's own,
// in ODAC's data directory, taken right before an operation that can go
// wrong (a deploy, a version change, a delete, an update, an in-place
// restore), on the
// operator's request and periodically. They run on the same engine and
// helpers as backups, but the system manages them: no job, no target, a
// key nobody sees, and a share of the disk that follows the swap rule
// (diskbudget) above the free space floor.
//
// A snapshot is whole or nothing: one that did not store every piece is
// deleted at once, so a restore point never mixes moments. It has a slot
// of its own beside the backup slot, so a deploy's snapshot never waits
// behind a remote backup; only maintenance of the snapshot repository and
// a restore from it hold it back.

// SnapshotTarget is the snapshot repository where the manager takes a
// target id: in restores, records and the dump reuse. Target ids are hex,
// so it cannot meet one.
const SnapshotTarget = "snapshots"

// KindSnapshot is a snapshot's record and what holds the snapshot
// repository while one is taken.
const KindSnapshot = "snapshot"

// Snapshot events: what took a snapshot. Each is the trigger its
// snapshots are tagged with. EventVersion is a version switch of an app
// (app.image.set, app.rollback), kept apart from EventDeploy because it is
// the point a restore after a failed upgrade returns to, and thinning keeps
// each event's series on its own.
const (
	EventDeploy     = "deploy"
	EventVersion    = "version"
	EventDelete     = "delete"
	EventUpdate     = "update"
	EventManual     = "manual"
	EventPeriodic   = "periodic"
	EventPreRestore = "pre-restore"
)

// StatusSkipped is a snapshot that was not taken, for the reason its
// record names.
const StatusSkipped = "skipped"

// Reasons a snapshot was not taken, or was given up.
const (
	ReasonDisabled    = "disabled"
	ReasonUnavailable = "unavailable"
	ReasonNoSpace     = "no_space"
	ReasonTimeout     = "timeout"
	ReasonCanceled    = "canceled"
	ReasonDeploy      = "deploy"
	ReasonNoData      = "no_data"
	ReasonFloor       = "floor"
)

// snapTiming holds the snapshot intervals; tests shrink them.
type snapTiming struct {
	// deployWait is how long a deploy waits for its snapshot once its
	// build is done; deleteWait and updateWait bound the snapshot before
	// a delete and an update.
	deployWait time.Duration
	deleteWait time.Duration
	updateWait time.Duration
	// abortGrace is how long a given-up snapshot has to stop its helpers.
	abortGrace time.Duration
	spaceCheck time.Duration
	// Periodic snapshots come about ten times the last one's duration
	// apart, within these bounds; the first one of an app comes a while
	// after it is first seen.
	periodicMin   time.Duration
	periodicMax   time.Duration
	periodicFirst time.Duration
	initRetry     time.Duration
}

func defaultSnapTiming() snapTiming {
	return snapTiming{
		deployWait:    3 * time.Minute,
		deleteWait:    5 * time.Minute,
		updateWait:    5 * time.Minute,
		abortGrace:    30 * time.Second,
		spaceCheck:    2 * time.Second,
		periodicMin:   time.Hour,
		periodicMax:   24 * time.Hour,
		periodicFirst: 15 * time.Minute,
		initRetry:     time.Minute,
	}
}

// Causes a snapshot is given up with.
var (
	errSnapTimeout  = errors.New("the snapshot did not finish in time")
	errSnapNoSpace  = errors.New("the disk is running out of free space")
	errSnapCanceled = errors.New("the snapshot is no longer needed")
)

func abortReason(cause error) string {
	switch {
	case errors.Is(cause, errSnapTimeout), errors.Is(cause, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(cause, errSnapNoSpace):
		return ReasonNoSpace
	}
	return ReasonCanceled
}

func snapshotDir(base string) string  { return filepath.Join(base, "backup", "snapshots") }
func snapshotRepo(base string) string { return filepath.Join(snapshotDir(base), proto.RepositoryDir) }
func snapshotKey(base string) string  { return filepath.Join(snapshotDir(base), "key") }

var snapKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func readSnapshotKey(base string) (string, error) {
	raw, err := os.ReadFile(snapshotKey(base))
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(raw))
	if !snapKeyPattern.MatchString(k) {
		return "", fmt.Errorf("backup: the snapshot key %s is damaged", snapshotKey(base))
	}
	return k, nil
}

// ensureSnapshotKey creates the snapshot repository's key once, through a
// hard link so that two processes never write different ones, and never
// rewrites it: a damaged key is an error, since a new one would not open
// the repository either.
func ensureSnapshotKey(base string) error {
	if _, err := readSnapshotKey(base); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := snapshotDir(base)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(hex.EncodeToString(b) + "\n"); err != nil {
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
	if err := os.Link(tmp.Name(), snapshotKey(base)); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// snapshotStoreExists reports a snapshot repository made by an earlier
// start, which maintenance looks after even while snapshots are off.
func (m *Manager) snapshotStoreExists() bool {
	_, err := os.Stat(snapshotKey(m.cfg.BaseDir))
	return err == nil && m.cfg.ServerID != ""
}

// snapshotAccess opens the snapshot repository. It claims no owner: no
// other machine reaches ODAC's data directory.
func (m *Manager) snapshotAccess() (settings.Access, error) {
	if m.cfg.ServerID == "" {
		return settings.Access{}, errors.New("backup: snapshots need this server's id")
	}
	key, err := readSnapshotKey(m.cfg.BaseDir)
	if err != nil {
		return settings.Access{}, err
	}
	repo := snapshotRepo(m.cfg.BaseDir)
	if err := os.MkdirAll(repo, 0o700); err != nil {
		return settings.Access{}, err
	}
	return settings.Access{Repo: proto.Repo{
		Password: key, ServerID: m.cfg.ServerID,
		Storage: proto.Storage{Filesystem: &proto.FilesystemStorage{Path: repo}},
	}}, nil
}

// ensureStore opens the snapshot repository once per start, creating it
// and its key the first time. A repository its key does not open can
// never be opened again (the key is never rewritten, so only a lost key
// gets there), and snapshots are disposable: it is started over.
func (m *Manager) ensureStore(ctx context.Context) error {
	m.snapInitMu.Lock()
	defer m.snapInitMu.Unlock()
	m.mu.Lock()
	ready := m.snapReady
	m.mu.Unlock()
	if ready {
		return nil
	}
	if err := ensureSnapshotKey(m.cfg.BaseDir); err != nil {
		return err
	}
	init := func() error {
		a, err := m.snapshotAccess()
		if err != nil {
			return err
		}
		_, err = m.Init(ctx, a)
		return err
	}
	err := init()
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.CodeWrongPassword {
		m.log.Warn("The snapshot repository does not open with its key; starting it over")
		if rerr := os.RemoveAll(snapshotRepo(m.cfg.BaseDir)); rerr != nil {
			return rerr
		}
		err = init()
	}
	if err != nil {
		m.warn(ReasonUnavailable, tr("The snapshot repository cannot be opened: %s", err.Error()))
		return err
	}
	m.clearWarning(ReasonUnavailable)
	m.mu.Lock()
	m.snapReady = true
	m.mu.Unlock()
	return nil
}

// Disk measures the disk the snapshot repository lives on, which is
// ODAC's data disk.
type Disk interface {
	// Space reports the bytes free and the size of the file system
	// holding dir.
	Space(dir string) (free, total int64, ok bool)
	// Size is what the files under dir take.
	Size(dir string) int64
}

type hostDisk struct{}

func (hostDisk) Space(dir string) (int64, int64, bool) { return diskSpace(dir) }

func (hostDisk) Size(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// snapSpace is the disk as the snapshot budget sees it: the file
// system's free space and size, what the repository holds and how much
// of it is garbage on its way back.
type snapSpace struct {
	Free, Total, Used, Garbage int64
	ok                         bool
}

func (m *Manager) snapSpace() snapSpace {
	free, total, ok := m.cfg.Disk.Space(m.cfg.BaseDir)
	s := snapSpace{Free: free, Total: total, ok: ok && total > 0}
	s.Used = m.cfg.Disk.Size(snapshotRepo(m.cfg.BaseDir))
	m.state.view(func(d *stateData) {
		if t := d.Targets[SnapshotTarget]; t != nil {
			s.Garbage = t.Garbage
		}
	})
	s.Garbage = min(max(s.Garbage, 0), s.Used)
	return s
}

// snapshotMargin is the room a snapshot being written keeps above the
// floor: a percent of the disk, at most 1 GiB.
func snapshotMargin(total int64) int64 { return min(1<<30, total/100) }

// snapshotLimit is the most the snapshot repository may hold: the swap
// rule's share of the free space plus the repository's own size, and
// never so much that the disk falls below its floor. A disk already below
// the floor gives snapshots nothing. -1 is a disk that cannot be
// measured, where only the rules of retention apply.
func snapshotLimit(s snapSpace, pct int) int64 {
	if !s.ok {
		return -1
	}
	floor := thin.Floor(s.Total)
	if s.Free < floor {
		return 0
	}
	return max(0, min(diskbudget.Limit(s.Free, s.Used, int64(pct)), s.Used+s.Free-floor))
}

// snapshotExcess is what the snapshots must give up for extra more bytes
// to fit their limit. Garbage counts as given up already: under
// SafetyFull it takes several maintenance runs to come back, and deciding
// on the measured size alone would delete again for it.
func snapshotExcess(s snapSpace, pct int, extra int64) int64 {
	limit := snapshotLimit(s, pct)
	if limit < 0 {
		return 0
	}
	return max(0, s.Used-s.Garbage+extra-limit)
}

// snapshotFits reports a disk with room for extra more bytes above the
// floor and the margin a snapshot keeps while it writes. Deleting does
// not help here: it gives space back only after maintenance.
func snapshotFits(s snapSpace, extra int64) bool {
	return !s.ok || s.Free-extra >= thin.Floor(s.Total)+2*snapshotMargin(s.Total)
}

// watchSpace gives the snapshot being written up when the disk comes
// close to its floor, or when the snapshot has taken more than its room
// (measured as the free space it used, which is conservative). It returns
// when stop is closed.
func (m *Manager) watchSpace(stop <-chan struct{}, abort context.CancelCauseFunc, start snapSpace, room int64) {
	if !start.ok {
		return
	}
	line := thin.Floor(start.Total) + snapshotMargin(start.Total)
	t := time.NewTicker(m.t.snap.spaceCheck)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			free, _, ok := m.cfg.Disk.Space(m.cfg.BaseDir)
			if !ok {
				continue
			}
			if free < line || room >= 0 && start.Free-free > room+snapshotMargin(start.Total) {
				abort(errSnapNoSpace)
				return
			}
		}
	}
}

// SnapshotRunning is the snapshot in flight. Times are Unix milliseconds.
type SnapshotRunning struct {
	Run   string `json:"run"`
	App   string `json:"app"`
	Event string `json:"event"`
	Start int64  `json:"start"`
}

// acquireSnapshot waits for the snapshot slot and the snapshot
// repository, which maintenance or a restore from it may hold; own is a
// caller that holds the repository already (a restore from it taking its
// safety snapshot). The returned function gives them back.
func (m *Manager) acquireSnapshot(ctx context.Context, own bool, running SnapshotRunning) (func(), error) {
	for {
		m.mu.Lock()
		if m.released {
			m.mu.Unlock()
			return nil, errReleased
		}
		if !m.snapSlot && (own || m.busy[SnapshotTarget] == "") {
			m.snapSlot = true
			if !own {
				m.busy[SnapshotTarget] = KindSnapshot
			}
			m.snapRunning = &running
			m.mu.Unlock()
			return func() {
				if !own {
					m.free(SnapshotTarget, KindSnapshot)
					return
				}
				m.mu.Lock()
				m.snapSlot, m.snapRunning = false, nil
				m.signalFreedLocked()
				m.mu.Unlock()
				m.poke()
			}, nil
		}
		freed := m.freed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-freed:
		case <-time.After(m.t.poll):
		}
	}
}

// snapRequest is one snapshot to take.
type snapRequest struct {
	// series is the app the snapshot is filed under, whose deploy,
	// delete or schedule took it; apps are the apps it takes, the series'
	// group (see snapshotGroup).
	series string
	event  string
	apps   []string
	// server takes ODAC's own state.
	server bool
	// exempt and reuse are as in runSpec (reuse is !noReuse).
	exempt bool
	reuse  bool
	// own: the caller holds the snapshot repository (see acquireSnapshot).
	own bool
	run string
	// abort gives the snapshot up; its cause is the record's reason.
	abort context.Context
	// every bounds the run's time the way a job's interval does.
	every int
}

// snapshotsOff reports why no snapshot can be taken now, if one cannot.
func (m *Manager) snapshotsOff() (string, string) {
	if m.cfg.ServerID == "" {
		return ReasonUnavailable, tr("Snapshots are not available on this server.")
	}
	set := m.settings()
	if set == nil {
		return ReasonUnavailable, tr("Snapshots are not available on this server.")
	}
	s, err := set.Snapshots()
	if err != nil {
		return ReasonUnavailable, err.Error()
	}
	if !s.Enabled {
		return ReasonDisabled, tr("Snapshots are turned off.")
	}
	return "", ""
}

// snapshotPct is the share of the disk snapshots may take.
func (m *Manager) snapshotPct() int {
	if set := m.settings(); set != nil {
		if s, err := set.Snapshots(); err == nil && s.MaxDiskPct > 0 {
			return s.MaxDiskPct
		}
	}
	return settings.DefaultMaxDiskPct
}

// takeSnapshot takes one snapshot and records it, whatever came of it,
// except when the server is being replaced while it runs: the helper then
// belongs to the next server, which records it. It waits for the snapshot
// slot while ctx and the request's abort last.
func (m *Manager) takeSnapshot(ctx context.Context, sr snapRequest) (rec SnapshotRecord) {
	start := m.now()
	if sr.run == "" {
		sr.run = newRunID(start)
	}
	if sr.every <= 0 {
		sr.every = 60
	}
	rec = SnapshotRecord{Kind: KindSnapshot, Run: sr.run, App: sr.series, Event: sr.event, Start: ms(start)}
	base := sr.abort
	if base == nil {
		base = context.Background()
	}
	abortCtx, abort := context.WithCancelCause(base)
	defer abort(nil)
	opCtx, cancelOp := context.WithCancel(ctx)
	defer cancelOp()
	defer context.AfterFunc(abortCtx, cancelOp)()

	recorded := true
	defer func() {
		if recorded {
			rec.End = ms(m.now())
			m.recordSnapshot(rec)
		}
	}()
	given := func() bool {
		if abortCtx.Err() == nil {
			return false
		}
		cause := context.Cause(abortCtx)
		rec.Status, rec.Reason, rec.Error = StatusFailed, abortReason(cause), cause.Error()
		if rec.Reason != ReasonNoSpace {
			rec.Status = StatusSkipped
		}
		return true
	}
	skip := func(reason, msg string) { rec.Status, rec.Reason, rec.Error = StatusSkipped, reason, msg }

	if reason, msg := m.snapshotsOff(); reason != "" {
		skip(reason, msg)
		return rec
	}
	if err := m.ensureStore(opCtx); err != nil {
		if !given() {
			skip(ReasonUnavailable, err.Error())
		}
		return rec
	}
	release, err := m.acquireSnapshot(opCtx, sr.own, SnapshotRunning{Run: sr.run, App: sr.series, Event: sr.event, Start: ms(start)})
	if err != nil {
		if errors.Is(err, errReleased) || m.isReleased() {
			recorded = false
		} else if !given() {
			skip(ReasonCanceled, err.Error())
		}
		return rec
	}
	defer release()

	all := m.cfg.Apps.BackupApps()
	apps := make([]*appRun, 0, len(sr.apps))
	for _, name := range sr.apps {
		a := &appRun{res: AppResult{App: name}}
		apps = append(apps, a)
		plan, d, err := m.plan(opCtx, name, sr.run)
		switch {
		case given():
			return rec
		case d != nil:
			a.deferred = d
		case err != nil:
			a.res.Status, a.res.Error = StatusFailed, err.Error()
		case plan == nil:
			a.res.Status = StatusOK
		default:
			a.plan = plan
			a.res.Warnings = plan.Warnings
		}
	}
	var serverDirs []string
	if sr.server {
		rec.Server = &AppResult{App: serverApp}
		for _, d := range m.cfg.ServerDirs {
			if isDir(d) {
				serverDirs = append(serverDirs, d)
			}
		}
	}
	pieces := m.pieces(sr.series, apps, all, serverDirs)
	for _, a := range apps {
		if a.deferred != nil {
			rec.Status, rec.Reason, rec.Error = StatusDeferred, ReasonDeploy, a.res.App+": "+a.deferred.reason
			if !a.deferred.deploy {
				rec.Reason = ReasonUnavailable
			}
			rec.Apps = appResults(apps)
			return rec
		}
	}
	if len(pieces) == 0 {
		skip(ReasonNoData, tr("Nothing of these apps is stored on this server's disk."))
		rec.Apps = appResults(apps)
		return rec
	}

	// The room for it, thinning first when its likely size does not fit.
	pct := m.snapshotPct()
	space := m.snapSpace()
	var est int64
	m.state.view(func(d *stateData) { est = d.Snap.Growth[sr.series] })
	if !snapshotFits(space, est) {
		m.warn(ReasonNoSpace, tr("A snapshot of %s was not taken: the disk is too close to its free space floor.", sr.series))
		skip(ReasonNoSpace, tr("The disk is too close to its free space floor."))
		return rec
	}
	if snapshotExcess(space, pct, est) > 0 {
		tres := m.tidySnapshots(opCtx, nil, est)
		rec.Deleted += tres.deleted
		space = m.snapSpace()
		if snapshotExcess(space, pct, est) > 0 {
			m.warn(ReasonNoSpace, tr("A snapshot of %s was not taken: the snapshots' share of the disk is full.", sr.series))
			skip(ReasonNoSpace, tr("The snapshots' share of the disk is full."))
			return rec
		}
	}
	room := int64(-1)
	if limit := snapshotLimit(space, pct); limit >= 0 {
		room = limit - (space.Used - space.Garbage)
	}

	cleanup := m.addDefinitions(sr.run, sr.series, pieces)
	defer func() {
		if recorded {
			cleanup()
		}
	}()
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		m.watchSpace(stop, abort, space, room)
	}()
	defer func() {
		close(stop)
		<-watched
	}()

	rs := runSpec{
		job:     settings.JobView{ID: sr.series, Job: settings.Job{Name: sr.series, Target: SnapshotTarget, Every: sr.every}},
		run:     sr.run,
		trigger: sr.event,
		abort:   abortCtx,
		exempt:  sr.exempt,
		noReuse: !sr.reuse,
	}
	deadline := start.Add(m.runLimit(sr.every))
	var snaps []proto.Snapshot
	failed := false
	for i, p := range pieces {
		labels := map[string]string{
			labelKind: KindSnapshot, labelJob: sr.series, labelTarget: SnapshotTarget,
			labelTrigger: sr.event, labelApps: strings.Join(p.apps, ","),
		}
		if i == len(pieces)-1 && !failed {
			labels[labelComplete] = "true"
		}
		if abortCtx.Err() != nil {
			failed = true
			break
		}
		pr := m.runPiece(opCtx, rs, p, deadline, labels)
		snaps = append(snaps, pr.snaps...)
		if pr.handedOver || errors.Is(pr.err, errReleased) || m.isReleased() {
			recorded = false
			return rec
		}
		if pr.err != nil || pr.deferred != nil {
			failed = true
		}
		for _, name := range p.apps {
			a := find(apps, name)
			if a == nil {
				continue
			}
			switch {
			case pr.deferred != nil:
				a.deferred = pr.deferred
			case pr.err != nil:
				a.res.Status, a.res.Error = StatusFailed, describe(opCtx, pr.err)
			default:
				a.res.Status = StatusOK
				if pr.reused {
					a.res.Method = MethodReused
				}
			}
		}
		if p.plan == nil && rec.Server != nil {
			rec.Server.Status = StatusOK
			if pr.err != nil {
				rec.Server.Status, rec.Server.Error = StatusFailed, describe(opCtx, pr.err)
			}
		}
		if failed {
			break
		}
	}
	rec.Apps = appResults(apps)
	rec.Snapshots = len(snaps)
	for _, s := range snaps {
		rec.Bytes += s.Bytes
	}
	if rec.Server != nil && rec.Server.Status == "" {
		rec.Server.Status = StatusOK
		if failed {
			rec.Server.Status = StatusFailed
		}
	}

	switch {
	case given():
	case !failed:
		rec.Status = StatusOK
	default:
		rec.Status = StatusFailed
		for _, a := range apps {
			if a.deferred != nil && a.deferred.deploy {
				rec.Status, rec.Reason, rec.Error = StatusDeferred, ReasonDeploy, a.res.App+": "+a.deferred.reason
			}
		}
		if rec.Error == "" {
			rec.Error = firstSnapError(rec)
		}
	}
	if rec.Status != StatusOK {
		// Whole or nothing: what an unfinished snapshot stored goes, and
		// its half-written parts with the next maintenance.
		if len(snaps) > 0 {
			m.deleteRuns(ctx, SnapshotTarget, groupRuns(snaps), map[string]bool{sr.run: true})
			rec.Snapshots, rec.Bytes = 0, 0
		}
		if rec.Reason == ReasonNoSpace {
			m.warn(ReasonNoSpace, tr("A snapshot of %s was given up: the disk came close to its free space floor.", sr.series))
		}
		m.maintainSoon(true)
		return rec
	}
	rec.Added = max(0, m.cfg.Disk.Size(snapshotRepo(m.cfg.BaseDir))-space.Used)
	m.state.update(func(d *stateData) { d.Snap.Growth[sr.series] = rec.Added })
	m.clearWarning(ReasonNoSpace)
	m.maintainSoon(false)
	rec.Deleted += m.tidySnapshots(ctx, nil, 0).deleted
	return rec
}

// maintainSoon schedules the snapshot repository's maintenance: the
// first one after its first snapshot, and at once after a snapshot that
// left half-written parts behind.
func (m *Manager) maintainSoon(now bool) {
	m.state.update(func(d *stateData) {
		t := d.target(SnapshotTarget)
		switch {
		case now:
			t.NextMaintain = ms(m.now())
		case t.NextMaintain == 0:
			t.NextMaintain = ms(m.now().Add(m.t.firstMaintain))
		}
	})
}

func appResults(apps []*appRun) []AppResult {
	out := make([]AppResult, 0, len(apps))
	for _, a := range apps {
		r := a.res
		if a.deferred != nil {
			r.Status, r.Error = StatusDeferred, a.deferred.reason
		} else if r.Status == "" {
			r.Status = StatusFailed
			r.Error = tr("not taken: another part of the snapshot failed")
		}
		out = append(out, r)
	}
	return out
}

func firstSnapError(rec SnapshotRecord) string {
	for _, a := range rec.Apps {
		if a.Status == StatusFailed && a.Error != "" {
			return a.App + ": " + a.Error
		}
	}
	if rec.Server != nil && rec.Server.Error != "" {
		return "server: " + rec.Server.Error
	}
	return tr("the snapshot failed")
}

// snapshotGroup is what a snapshot filed under app takes: the app and
// every app it links through env.linked, directly or through another app,
// that holds data. A migration changes the linked database more often
// than the app, so the group comes back together.
func snapshotGroup(app string, all []settings.App) []string {
	byName := map[string]settings.App{}
	for _, a := range all {
		byName[a.Name] = a
	}
	if _, ok := byName[app]; !ok {
		return nil
	}
	var out []string
	for name := range closure(app, byName) {
		if byName[name].Stateful {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// closure is app and everything it links, transitively.
func closure(app string, byName map[string]settings.App) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		if _, ok := byName[n]; !ok {
			return
		}
		seen[n] = true
		for _, l := range byName[n].Linked {
			walk(l)
		}
	}
	walk(app)
	return seen
}

// periodicRoots are the apps that get periodic snapshots: every app with
// data, except one that another app with data takes in its group anyway
// (its database, most often). In a cycle of links the first name in
// order takes the others.
func periodicRoots(all []settings.App) []string {
	byName := map[string]settings.App{}
	for _, a := range all {
		byName[a.Name] = a
	}
	groups := map[string]map[string]bool{}
	for _, a := range all {
		if a.Stateful {
			groups[a.Name] = closure(a.Name, byName)
		}
	}
	var out []string
	for name := range groups {
		covered := false
		for other, g := range groups {
			if other != name && g[name] && !(groups[name][other] && other > name) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// track registers work of the manager outside its own goroutines, which
// Stop then waits for; false when the manager is not running.
func (m *Manager) track() (context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel == nil || m.released {
		return nil, false
	}
	m.workers.Add(1)
	return m.ctx, true
}

// PendingSnapshot is the snapshot an operation that replaces an app's
// container (a deploy, a version switch) started before the new container
// runs. Its methods are safe on nil, which is an operation that takes none.
type PendingSnapshot struct {
	m     *Manager
	event string
	done  chan struct{}
	abort context.CancelCauseFunc
	rec   SnapshotRecord
}

// BeforeDeploy starts the snapshot before app's container is replaced and
// returns at once: event is EventDeploy for a deploy (started beside its
// build) or EventVersion for a version switch. It takes the app and the
// apps it links, and nothing when none of them holds data, snapshots are
// off or the event is not one of those two; the line returned then says
// why, for a caller that must not go on without a snapshot. The operation
// neither holds it back nor cancels its dump: it is part of the operation.
func (m *Manager) BeforeDeploy(app, event string) (*PendingSnapshot, string) {
	if event != EventDeploy && event != EventVersion {
		return nil, noSnapshotLine(event, tr("the operation takes no snapshot"))
	}
	if reason, _ := m.snapshotsOff(); reason != "" {
		return nil, noSnapshotLine(event, snapshotNote(SnapshotRecord{Reason: reason}))
	}
	group := snapshotGroup(app, m.cfg.Apps.BackupApps())
	if len(group) == 0 {
		return nil, noSnapshotLine(event, tr("%s and the apps it links hold no data", app))
	}
	ctx, ok := m.track()
	if !ok {
		return nil, noSnapshotLine(event, tr("backups are not running"))
	}
	abortCtx, abort := context.WithCancelCause(context.Background())
	p := &PendingSnapshot{m: m, event: event, done: make(chan struct{}), abort: abort}
	go func() {
		defer m.workers.Done()
		defer close(p.done)
		p.rec = m.takeSnapshot(ctx, snapRequest{series: app, event: event, apps: group, exempt: true, abort: abortCtx, every: 15})
	}()
	return p, ""
}

// Wait waits for the snapshot once the operation is ready to start its new
// container, and gives the snapshot up after a bounded time: its dump must
// not go on beside the migration the new container starts. The bound is
// deployWait plus abortGrace for the helpers to stop, and with a positive
// limit never more than limit in all, because a version switch has a time
// budget of its own and a health gate after the wait. It returns the run of
// the snapshot taken ("" for none) and a line for the log. A deploy goes on
// either way; a switch that requires a snapshot does not.
func (p *PendingSnapshot) Wait(limit time.Duration) (string, string) {
	if p == nil {
		return "", ""
	}
	wait, grace := p.m.t.snap.deployWait, p.m.t.snap.abortGrace
	if limit > 0 && wait+grace > limit {
		grace = min(grace, limit/2)
		wait = limit - grace
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
		p.abort(errSnapTimeout)
		g := time.NewTimer(grace)
		defer g.Stop()
		select {
		case <-p.done:
		case <-g.C:
			return "", noSnapshotLine(p.event, snapshotNote(SnapshotRecord{Reason: ReasonTimeout}))
		}
	}
	rec := p.rec
	if rec.Status == StatusOK {
		if p.event == EventVersion {
			return rec.Run, tr("Snapshot %s taken before the version change.", rec.Run)
		}
		return rec.Run, tr("Snapshot %s taken before the deploy.", rec.Run)
	}
	return "", noSnapshotLine(p.event, snapshotNote(rec))
}

// noSnapshotLine is the log line of an operation that has no snapshot.
func noSnapshotLine(event, why string) string {
	if event == EventVersion {
		return tr("No snapshot before the version change: %s", why)
	}
	return tr("Deploying without a snapshot: %s", why)
}

// Cancel gives up the snapshot of a deploy that ended before it needed
// it; a snapshot that finished stays.
func (p *PendingSnapshot) Cancel() {
	if p != nil {
		p.abort(errSnapCanceled)
	}
}

func snapshotNote(rec SnapshotRecord) string {
	switch rec.Reason {
	case ReasonTimeout:
		return tr("it did not finish in time")
	case ReasonNoSpace:
		return tr("there is not enough free disk space")
	case ReasonDisabled:
		return tr("snapshots are turned off")
	case ReasonUnavailable:
		if rec.Error == "" {
			return tr("snapshots are not available on this server")
		}
	}
	if rec.Error != "" {
		return rec.Error
	}
	return rec.Status
}

// BeforeDelete takes a snapshot of app before it is deleted, for a
// bounded time, and returns a line for the log. The snapshot holds the
// app's definition beside its data, so the app can be brought back as a
// new app while its snapshots are kept (thin.SnapshotRules.Gone). It never
// stops the delete.
func (m *Manager) BeforeDelete(app string) string {
	if reason, _ := m.snapshotsOff(); reason != "" {
		return ""
	}
	all := m.cfg.Apps.BackupApps()
	stateful := false
	for _, a := range all {
		stateful = stateful || a.Name == app && a.Stateful
	}
	if !stateful {
		return ""
	}
	ctx, ok := m.track()
	if !ok {
		return ""
	}
	defer m.workers.Done()
	abortCtx, abort := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(m.t.snap.deleteWait, func() { abort(errSnapTimeout) })
	defer timer.Stop()
	rec := m.takeSnapshot(ctx, snapRequest{series: app, event: EventDelete, apps: []string{app}, exempt: true, abort: abortCtx, every: 15})
	m.markGone(app, m.now())
	if rec.Status == StatusOK {
		days := strconv.Itoa(int(thin.DefaultSnapshotRules.Gone / (24 * time.Hour)))
		return tr("Snapshot %s taken before deleting %s; it can be restored as a new app for %s days.", rec.Run, app, days)
	}
	return tr("Deleting %s without a snapshot: %s", app, snapshotNote(rec))
}

// BeforeUpdate takes a snapshot of ODAC's own state before an update, for
// a bounded time, and returns a line for the log. It never stops the
// update.
func (m *Manager) BeforeUpdate() string {
	if reason, _ := m.snapshotsOff(); reason != "" {
		return ""
	}
	ctx, ok := m.track()
	if !ok {
		return ""
	}
	defer m.workers.Done()
	abortCtx, abort := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(m.t.snap.updateWait, func() { abort(errSnapTimeout) })
	defer timer.Stop()
	rec := m.takeSnapshot(ctx, snapRequest{series: serverApp, event: EventUpdate, server: true, exempt: true, abort: abortCtx, every: 15})
	if rec.Status == StatusOK {
		return tr("Snapshot %s of ODAC's own state taken before the update.", rec.Run)
	}
	return tr("Updating without a snapshot of ODAC's own state: %s", snapshotNote(rec))
}

// TakeSnapshot takes a snapshot of app and the apps it links, now, in the
// background; the reply names the run, the outcome is in the history.
func (m *Manager) TakeSnapshot(app string) (SnapshotRunning, error) {
	if reason, msg := m.snapshotsOff(); reason != "" {
		return SnapshotRunning{}, refusal(settings.CodeInvalid, "%s", msg)
	}
	all := m.cfg.Apps.BackupApps()
	if !slices.ContainsFunc(all, func(a settings.App) bool { return a.Name == app }) {
		return SnapshotRunning{}, refusal(settings.CodeNotFound, "App %s not found.", app)
	}
	group := snapshotGroup(app, all)
	if len(group) == 0 {
		return SnapshotRunning{}, refusal(settings.CodeInvalid, "App %s and the apps it links hold no data to snapshot.", app)
	}
	ctx, ok := m.track()
	if !ok {
		return SnapshotRunning{}, errors.New("backup: backups are not running")
	}
	now := m.now()
	run := newRunID(now)
	go func() {
		defer m.workers.Done()
		m.takeSnapshot(ctx, snapRequest{series: app, event: EventManual, apps: group, run: run})
	}()
	return SnapshotRunning{Run: run, App: app, Event: EventManual, Start: ms(now)}, nil
}

// safetyBackup takes a snapshot of the apps an in-place restore is about
// to change, as they are now, before anything changes: the way back from
// a restore that was a mistake. It goes to the snapshot repository,
// whatever the restore reads from, and is kept like any event snapshot.
func (m *Manager) safetyBackup(ctx context.Context, p *restorePlan) (string, error) {
	var names []string
	for _, ap := range p.apps {
		names = append(names, ap.name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "", errors.New("backup: nothing to back up")
	}
	series := names[0]
	for _, a := range p.req.Apps {
		if slices.Contains(names, a) {
			series = a
			break
		}
	}
	rec := m.takeSnapshot(ctx, snapRequest{series: series, event: EventPreRestore, apps: names, own: p.req.Target == SnapshotTarget, every: 60})
	if rec.Status != StatusOK {
		msg := rec.Error
		if msg == "" {
			msg = rec.Status
		}
		return "", errors.New(msg)
	}
	m.log.Log("Took snapshot %s of %s before restoring them", rec.Run, strings.Join(p.names(), ", "))
	return rec.Run, nil
}

// markGone records when a series' app was deleted, which starts the
// window its snapshots are kept for.
func (m *Manager) markGone(app string, at time.Time) {
	m.state.update(func(d *stateData) {
		if d.Snap.Gone[app] == 0 {
			d.Snap.Gone[app] = ms(at)
		}
	})
}
