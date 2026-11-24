package appmgr

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"

	"odac/internal/jscanon"
)

// imageChangeKey is the persisted marker of an image change in flight. It is
// written, and flushed to disk, before the container is touched and removed
// in the same write that makes the record name the new image, so a record
// that still carries it after a restart names an interrupted change: Check
// then returns the app to the image its record names (recoverImageChange).
// It never reaches app.list.
const imageChangeKey = "imageChange"

// holdRepository names the tag that keeps an app's previous image alive while
// a change may still roll back to it. A re-pull of a rolling tag (postgres:17)
// leaves the previous image untagged, and any redeploy's PruneDanglingImages
// would then delete the one copy a rollback needs. One tag per app id, dropped
// once the change settles either way.
const holdRepository = "odac-previous"

// maxImageRef matches the dashboard's column; a longer reference cannot be
// one it sent.
const maxImageRef = 255

// The closed set of reasons an image change reports; the dashboard keys its
// handling on them. ImageSnapshot is a switch asked to require a snapshot
// (requireSnapshot) that could not get one: nothing was touched.
const (
	ImageInvalid        = "invalid"
	ImageBusy           = "busy"
	ImagePullFailed     = "pull_failed"
	ImageStartFailed    = "start_failed"
	ImageUnhealthy      = "unhealthy"
	ImageRollbackFailed = "rollback_failed"
	ImageSnapshot       = "snapshot"
)

// credentialInURL strips a userinfo part from any URL a daemon error quotes.
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)

// ImageSetPayload carries app.image.set's named arguments. RequireSnapshot
// refuses the change, before anything is touched, when no snapshot can be
// taken first: the dashboard asks for it on an automatic switch of an app
// with data, whose data is never restored by itself.
type ImageSetPayload struct {
	Container       string
	Image           string
	Pull            bool
	RequireSnapshot bool
}

// ImageSetResult is app.image.set's reply. It is flat on the wire
// (ReplyFields), not the {success, message, data} envelope, because the
// dashboard reads reason and rolledBack beside success. Snapshot is the run
// of the snapshot taken before the switch, the restore point of the data as
// it was on the previous image.
type ImageSetResult struct {
	Success     bool
	Changed     bool
	Previous    string
	Image       string
	ImageID     string
	ImageDigest string
	Reason      string
	RolledBack  bool
	Message     string
	Snapshot    string
}

// ReplyFields renders the reply in the contract's key order. Every failure
// carries reason, rolledBack and message; a success names the images only
// when something changed, and the digest only when it is known. Either ends
// with snapshot when one was taken.
func (r ImageSetResult) ReplyFields() jscanon.Obj {
	if !r.Success {
		return withSnapshot(failureFields(r.Reason, r.RolledBack, r.Message), r.Snapshot)
	}
	obj := jscanon.Obj{{K: "success", V: true}, {K: "changed", V: r.Changed}}
	if !r.Changed {
		return obj
	}
	obj = append(obj, jscanon.Field{K: "previous", V: r.Previous}, jscanon.Field{K: "image", V: r.Image})
	if r.ImageID != "" {
		obj = append(obj, jscanon.Field{K: "imageId", V: r.ImageID})
	}
	if r.ImageDigest != "" {
		obj = append(obj, jscanon.Field{K: "imageDigest", V: r.ImageDigest})
	}
	return withSnapshot(obj, r.Snapshot)
}

// failureFields is the failure shape both switches share.
func failureFields(reason string, rolledBack bool, message string) jscanon.Obj {
	return jscanon.Obj{
		{K: "success", V: false},
		{K: "reason", V: reason},
		{K: "rolledBack", V: rolledBack},
		{K: "message", V: message},
	}
}

// withSnapshot appends the snapshot run when there is one; absent is "none
// was taken", never an empty string.
func withSnapshot(obj jscanon.Obj, run string) jscanon.Obj {
	if run != "" {
		obj = append(obj, jscanon.Field{K: "snapshot", V: run})
	}
	return obj
}

func imageFailure(reason string, rolledBack bool, format string, args ...any) ImageSetResult {
	return ImageSetResult{Reason: reason, RolledBack: rolledBack, Message: fmt.Sprintf(format, args...)}
}

// imageChangeRefusal checks a requested reference against the app's own, on
// the agent's side too: the hub is not the only gate. Only the tag may move;
// an app whose image is built here has no registry copy to move to.
func imageChangeRefusal(typ, current, target string) string {
	if typ != "container" {
		return "Only catalog apps can change their image. A git app is rebuilt from its source."
	}
	if target == "" || len(target) > maxImageRef {
		return "Invalid image reference."
	}
	next, err := reference.ParseNormalizedNamed(target)
	if err != nil {
		return "Invalid image reference."
	}
	if _, digested := next.(reference.Digested); digested {
		return "The image must be a repository:tag reference."
	}
	if _, tagged := next.(reference.Tagged); !tagged {
		return "The image must be a repository:tag reference."
	}
	cur, err := reference.ParseNormalizedNamed(current)
	if err != nil {
		return "The app's current image reference cannot be read."
	}
	if strings.HasPrefix(reference.FamiliarName(cur), "odac-app-") {
		return "This app's image is built on the server and has no registry copy to switch to."
	}
	if cur.Name() != next.Name() {
		return fmt.Sprintf("Only the tag may change: %s is not %s.", reference.FamiliarName(next), reference.FamiliarName(cur))
	}
	return ""
}

// sameReference reports whether two references name the same repository and
// tag once normalized (postgres == docker.io/library/postgres:latest).
func sameReference(a, b string) bool {
	na, errA := reference.ParseNormalizedNamed(a)
	nb, errB := reference.ParseNormalizedNamed(b)
	if errA != nil || errB != nil {
		return false
	}
	return reference.TagNameOnly(na).String() == reference.TagNameOnly(nb).String()
}

func holdTag(id float64) string {
	return holdRepository + ":app-" + strconv.FormatInt(int64(id), 10)
}

// shortError keeps a daemon error displayable: one line, no credentials,
// bounded.
func shortError(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimPrefix(msg, "Error response from daemon: ")
	msg = credentialInURL.ReplaceAllString(msg, "$1")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "..."
	}
	return msg
}

// SetImage is app.image.set: move a catalog app to another tag of its image,
// keeping the app on the previous one unless the new one proves healthy.
//
// Order is what keeps the app up: the pull runs while the old container
// serves, then the version snapshot (switchSnapshot) while it still serves,
// the container is recreated only once the image is local, and the record
// names the new image only after the health gate passed. Anything
// that fails after the recreate rolls back to the previous image, which is
// still local (held under holdTag even when a re-pull untagged it).
//
// Crash safety: the imageChangeKey marker is flushed to disk before the
// container is stopped, while the record still names the previous image. A
// restart in the window between the recreate and the end of the gate finds
// the marker, and recoverImageChange treats the interruption as a failed
// gate: the previous image is tagged back, a container on any other image is
// removed, and the next pulse recreates the app from its record. The record
// is never ahead of a gate that passed.
func (m *Manager) SetImage(p ImageSetPayload) ImageSetResult {
	var name, typ, current string
	var idNum float64
	var active bool
	var prevStatus any
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(p.Container); app != nil && p.Container != "" {
			found = true
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
			current, _ = app["image"].(string)
			idNum, _ = app["id"].(float64)
			active = app["active"] == true
			prevStatus = app["status"]
		}
	})
	if !found {
		return imageFailure(ImageInvalid, false, "App %s not found.", p.Container)
	}
	if refusal := imageChangeRefusal(typ, current, p.Image); refusal != "" {
		return imageFailure(ImageInvalid, false, "%s", refusal)
	}
	// A stopped app has nothing to gate on, and starting it is not what
	// was asked for.
	if !active {
		return imageFailure(ImageInvalid, false, "App %s is stopped. Start it before changing its image.", name)
	}
	if m.isCreating(name) || !m.tryLockProcessing(idNum) {
		return imageFailure(ImageBusy, false, "App %s is already being processed.", name)
	}
	defer m.unlockProcessing(idNum)

	target := p.Image
	deadline := time.Now().Add(m.imageSetBudget)
	m.log.Log("Changing the image of %s from %s to %s (pull: %t)", name, current, target, p.Pull)
	m.set(idNum, map[string]any{"status": "updating"})
	restoreStatus := func() { m.set(idNum, map[string]any{"status": prevStatus}) }

	if !m.deps.Docker.Available() {
		restoreStatus()
		return imageFailure(ImagePullFailed, false, "Docker is not available.")
	}

	// Step 1: the pull, with the old container untouched.
	before := m.deps.Docker.GetStatus(name)
	pullCtx, cancel := context.WithDeadline(context.Background(), minTime(deadline, time.Now().Add(m.imagePullTimeout)))
	err := m.deps.Docker.PullImage(pullCtx, target, p.Pull)
	// Read before cancel: the daemon's error for an aborted pull is a
	// transport error, rarely the context's own.
	timedOut := errors.Is(pullCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		restoreStatus()
		if timedOut {
			return imageFailure(ImagePullFailed, false, "Pulling %s did not finish in time.", target)
		}
		return imageFailure(ImagePullFailed, false, "Could not pull %s: %s", target, shortError(err))
	}
	meta, err := m.deps.Docker.ImageMeta(target)
	if err != nil || meta.ID == "" {
		restoreStatus()
		return imageFailure(ImagePullFailed, false, "Image %s is not available locally after the pull.", target)
	}
	if m.appDeleted(idNum) {
		return imageFailure(ImageStartFailed, false, "App was deleted during the image change.")
	}

	// Step 2: nothing to do when the same tag still resolves to what runs.
	if sameReference(current, target) && before.ImageID == meta.ID {
		restoreStatus()
		m.log.Log("Image of %s is unchanged (%s)", name, meta.ID)
		return ImageSetResult{Success: true}
	}

	// Step 3: the snapshot, while the old container still serves: after the
	// pull, so it is as close to the switch as it can be, and done before
	// the stop, because the new image's migration must not run beside its
	// dump. A change that requires one and gets none ends here.
	snapRun, refusal := m.switchSnapshot(name, deadline, meta.Healthcheck, p.RequireSnapshot)
	if refusal != "" {
		// A re-pull of the record's own tag moved it; nothing else changed.
		if sameReference(current, target) && before.ImageID != "" && before.ImageID != meta.ID {
			if err := m.deps.Docker.TagImage(before.ImageID, current); err != nil {
				m.log.Error("Could not restore the tag %s for %s: %s", current, name, err.Error())
			}
		}
		restoreStatus()
		return imageFailure(ImageSnapshot, false, "%s", refusal)
	}

	// Step 4: keep the previous image reachable, then record the change on
	// disk before anything stops.
	sw := m.beginSwitch(name, idNum, current, before.ImageID, map[string]any{"image": target}, switchText{
		op: "Image change", to: target, back: current, during: "the image change",
	})
	rollback := func(reason, cause string) ImageSetResult {
		f := sw.rollback(reason, cause)
		out := imageFailure(f.reason, f.rolledBack, "%s", f.message)
		out.Snapshot = snapRun
		return out
	}

	// Step 5: recreate on the new image, same config. The record still
	// names the previous image, so only this start sees the new one.
	m.endLogStream(name)
	m.deps.Docker.Stop(name)
	if err := m.runHeldOn(idNum, nil, target); err != nil {
		return rollback(ImageStartFailed, fmt.Sprintf("%s did not start: %s.", target, shortError(err)))
	}

	// Step 6: the health gate.
	if cause := m.imageGate(name, idNum, meta.Healthcheck, deadline); cause != "" {
		return rollback(ImageUnhealthy, cause)
	}

	// Step 7: the record follows only now, and the marker goes with it.
	sw.commit(func(app map[string]any) { app["image"] = target })

	out := ImageSetResult{Success: true, Changed: true, Previous: current, Image: target, ImageID: meta.ID, Snapshot: snapRun}
	if st, err := m.deps.Docker.InspectState(name); err == nil && st.ImageID != "" {
		out.ImageID = st.ImageID
	}
	out.ImageDigest = m.deps.Docker.ImageDigest(out.ImageID, target)
	m.log.Log("Image of %s changed from %s to %s", name, current, target)
	return out
}

// switchText names the two sides of a switch in its log lines and replies.
type switchText struct {
	op     string // log subject: "Image change", "Rollback"
	to     string // what the app moves to
	back   string // what a failure returns it to
	during string // "the image change", for the deleted-meanwhile message
}

// imageSwitch is the part app.image.set and app.rollback share: a container
// recreated on another image while the previous image stays reachable and
// the record keeps naming what ran before, until commit.
//
// beginSwitch holds the previous image under holdTag (a re-pull or a retag
// can leave it untagged, and any redeploy's PruneDanglingImages would delete
// the one copy a rollback needs) and flushes the imageChangeKey marker to
// disk before the caller stops anything, so recoverImageChange can finish an
// interrupted switch the way a failed gate would. current is the record's
// own reference; a failure points it back at prevID.
type imageSwitch struct {
	m       *Manager
	name    string
	id      float64
	current string
	prevID  string
	hold    string
	text    switchText
}

// switchFailure is a failed switch's outcome, which each caller maps onto
// its own reply type.
type switchFailure struct {
	reason     string
	rolledBack bool
	message    string
}

// beginSwitch records the switch before the container is touched. marker
// carries the caller's own members; previous, previousId and hold are
// added here.
func (m *Manager) beginSwitch(name string, id float64, current, prevID string, marker map[string]any, text switchText) *imageSwitch {
	s := &imageSwitch{m: m, name: name, id: id, current: current, prevID: prevID, text: text}
	if prevID != "" {
		s.hold = holdTag(id)
		if err := m.deps.Docker.TagImage(prevID, s.hold); err != nil {
			m.log.Error("Could not hold the previous image of %s: %s", name, err.Error())
			s.hold = ""
		}
	}
	marker["previous"] = current
	if prevID != "" {
		marker["previousId"] = prevID
	}
	if s.hold != "" {
		marker["hold"] = s.hold
	}
	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			app[imageChangeKey] = marker
			m.saveAppsLocked()
		}
	})
	m.flushConfig()
	return s
}

// settle ends a switch that did not commit: the marker goes and so does the
// hold tag. Only the hold tag goes: an image that still has its own tag
// stays local for the next rollback, as the contract promises.
func (s *imageSwitch) settle() {
	s.m.cfg.Mutate(func() {
		if app := s.m.getLocked(s.id); app != nil {
			delete(app, imageChangeKey)
			s.m.saveAppsLocked()
		}
	})
	s.m.flushConfig()
	if s.hold != "" {
		s.m.deps.Docker.RemoveImage(s.hold)
	}
}

// restoreTag points the record's own reference back at what ran, which a
// re-pull of that tag (or a rollback's retag) moved.
func (s *imageSwitch) restoreTag() {
	if s.prevID == "" {
		return
	}
	source := s.prevID
	if s.hold != "" {
		source = s.hold
	}
	if err := s.m.deps.Docker.TagImage(source, s.current); err != nil {
		s.m.log.Error("Could not restore the tag %s for %s: %s", s.current, s.name, err.Error())
	}
}

// abort ends a switch whose container was never stopped: the reference
// goes back and the marker with it, nothing is recreated.
func (s *imageSwitch) abort() {
	s.restoreTag()
	s.settle()
}

// rollback returns a stopped or failing switch to what ran before. The
// rollback itself is not gated: the previous container starting is enough.
func (s *imageSwitch) rollback(reason, cause string) switchFailure {
	m := s.m
	m.log.Error("%s of %s to %s failed: %s Rolling back to %s.", s.text.op, s.name, s.text.to, cause, s.text.back)
	if m.appDeleted(s.id) {
		s.settle()
		return switchFailure{reason, false, fmt.Sprintf("%s The app was deleted during %s.", cause, s.text.during)}
	}
	m.endLogStream(s.name)
	m.deps.Docker.Stop(s.name)
	s.restoreTag()
	runErr := m.runHeldOn(s.id, nil, "")
	s.settle()
	if runErr != nil {
		return switchFailure{ImageRollbackFailed, false, fmt.Sprintf("%s Rolling back to %s also failed: %s. The app is not running.", cause, s.text.back, shortError(runErr))}
	}
	return switchFailure{reason, true, fmt.Sprintf("%s Rolled back to %s.", cause, s.text.back)}
}

// commit makes the record follow a switch that passed: apply writes the
// caller's fields in the same write that drops the marker, so the record is
// never ahead of a passed gate and never behind it with a marker left over.
func (s *imageSwitch) commit(apply func(app map[string]any)) {
	s.m.cfg.Mutate(func() {
		if app := s.m.getLocked(s.id); app != nil {
			apply(app)
			delete(app, imageChangeKey)
			s.m.saveAppsLocked()
		}
	})
	s.m.flushConfig()
	if s.hold != "" {
		s.m.deps.Docker.RemoveImage(s.hold)
	}
}

// imageGate waits for a recreated container to prove itself: running without
// a break for imageStableFor (a restart shows up as a raised RestartCount, a
// fresh container starting at 0), never OOM-killed, and, when the image has
// a HEALTHCHECK, healthy within imageHealthTimeout. "unhealthy" fails at
// once. It returns "" on a pass, else the cause. A failed probe is not an
// answer and is retried until the deadline, which keeps a rollback's share of
// the budget.
func (m *Manager) imageGate(name string, id float64, healthcheck bool, deadline time.Time) string {
	start := time.Now()
	gateEnd := deadline.Add(-rollbackReserve(m.imageSetBudget))
	for {
		if m.appDeleted(id) {
			return "The app was deleted during the image change."
		}
		st, err := m.deps.Docker.InspectState(name)
		if err == nil {
			switch {
			case !st.Exists:
				return "The new container disappeared."
			case st.OOMKilled:
				return "The new container ran out of memory."
			case st.Restarts > 0:
				return fmt.Sprintf("The new container restarted %d time(s).", st.Restarts)
			case !st.Running:
				return "The new container exited."
			case st.Health == "unhealthy":
				return "The new container's healthcheck reported unhealthy."
			}
			elapsed := time.Since(start)
			healthy := !healthcheck || st.Health == "healthy"
			if !healthy && elapsed >= m.imageHealthTimeout {
				return fmt.Sprintf("The new container did not become healthy within %s.", m.imageHealthTimeout)
			}
			if healthy && elapsed >= m.imageStableFor {
				return ""
			}
		}
		if !time.Now().Before(gateEnd) {
			return "The new container did not settle within the time budget."
		}
		m.sleep(m.imageGateInterval)
	}
}

// rollbackReserve is the part of the budget the gate leaves for a rollback:
// stopping the new container and starting the previous one.
func rollbackReserve(budget time.Duration) time.Duration {
	return budget / 12
}

// startReserve is the part of the budget kept for stopping the old
// container and starting the new one, before the gate's clock runs.
func startReserve(budget time.Duration) time.Duration {
	return budget / 24
}

// snapshotWait is how long a switch may wait for its snapshot: what is left
// of its budget once the new container's start, the whole health gate and a
// rollback have their share. A snapshot that ate into the gate's time would
// turn a healthy image into a rollback for want of time.
func (m *Manager) snapshotWait(deadline time.Time, healthcheck bool) time.Duration {
	gate := m.imageStableFor
	if healthcheck {
		gate = max(gate, m.imageHealthTimeout)
	}
	return time.Until(deadline) - rollbackReserve(m.imageSetBudget) - startReserve(m.imageSetBudget) - gate
}

// switchSnapshot takes the snapshot a version switch (app.image.set,
// app.rollback) starts with, while the old container serves, and returns its
// run, "" when none was taken. A switch goes on without one, unless require
// is set: refusal is then the reason, and the caller touches nothing. The
// data is never restored by itself; the run is what an operator restores.
func (m *Manager) switchSnapshot(name string, deadline time.Time, healthcheck, require bool) (run, refusal string) {
	refuse := func(why string) string {
		return sentence(why) + " The version was not changed, because the change requires a snapshot."
	}
	s := m.snapshotter()
	if s == nil {
		if require {
			return "", refuse("Snapshots are not available on this server.")
		}
		return "", ""
	}
	snap, why := s.BeforeDeploy(name, SnapshotVersion)
	if snap == nil {
		if require {
			return "", refuse(why)
		}
		return "", ""
	}
	defer snap.Cancel()
	limit := m.snapshotWait(deadline, healthcheck)
	if limit <= 0 {
		note := "No snapshot before the version change: no time is left in the switch's budget."
		if require {
			return "", refuse(note)
		}
		m.log.Warn("%s: %s", name, note)
		return "", ""
	}
	run, note := snap.Wait(limit)
	if run == "" {
		m.log.Warn("%s: %s", name, note)
		if require {
			return "", refuse(note)
		}
		return "", ""
	}
	m.log.Log("%s: %s", name, note)
	return run, ""
}

// sentence ends s with a full stop unless it already ends a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "No snapshot could be taken."
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// flushConfig writes the record now instead of on the next autosave: the
// marker protects against a crash only once it is on disk.
func (m *Manager) flushConfig() {
	if err := m.cfg.SaveDirty(); err != nil {
		m.log.Error("Could not save the app list: %s", err.Error())
	}
}

// recoverImageChange finishes an image change a restart interrupted, the way
// a failed gate would: the record still names the previous image, so the
// previous image is tagged back under that name, a container on any other
// image is removed, and Check recreates the app from its record on the next
// pulse. A container already on the previous image is left alone. A daemon
// that does not answer keeps the marker for the next pulse. Caller holds the
// processing lock.
func (m *Manager) recoverImageChange(id float64) {
	var name, current string
	var marker map[string]any
	m.cfg.View(func() {
		if app := m.getLocked(id); app != nil {
			name, _ = app["name"].(string)
			current, _ = app["image"].(string)
			marker, _ = app[imageChangeKey].(map[string]any)
		}
	})
	if marker == nil {
		return
	}
	prevID, _ := marker["previousId"].(string)
	hold, _ := marker["hold"].(string)
	if hold != "" && hold != holdTag(id) {
		// A hand-edited marker names no tag of ours; never remove it.
		hold = ""
	}

	st, err := m.deps.Docker.InspectState(name)
	if err != nil {
		m.log.Error("Cannot recover the interrupted image change of %s yet: %s", name, err.Error())
		return
	}
	m.log.Log("Image change of %s was interrupted. Returning it to %s.", name, current)
	if prevID != "" && current != "" {
		source := prevID
		if hold != "" {
			source = hold
		}
		if err := m.deps.Docker.TagImage(source, current); err != nil {
			m.log.Error("Could not restore the tag %s for %s: %s", current, name, err.Error())
		}
	}
	if st.Exists && (prevID == "" || st.ImageID != prevID) {
		m.endLogStream(name)
		m.deps.Docker.Stop(name)
		m.deps.Docker.Remove(name)
	}
	// A Blue-Green rollback names its green container: it runs the build
	// the record does not name, whether or not traffic reached it.
	green, _ := marker["green"].(string)
	if !strings.HasPrefix(green, name+"-green-") || !greenSuffix.MatchString(green) {
		green = ""
	}
	if green != "" {
		m.endLogStream(green)
		m.deps.Docker.Stop(green)
		m.deps.Docker.Remove(green)
		m.cleanupGreenArtifacts(green)
	}
	m.cfg.Mutate(func() {
		if app := m.getLocked(id); app != nil {
			delete(app, imageChangeKey)
			if green != "" && app["activeContainerId"] == green {
				delete(app, "activeContainerId")
			}
			m.saveAppsLocked()
		}
	})
	m.flushConfig()
	if hold != "" {
		m.deps.Docker.RemoveImage(hold)
	}
}
