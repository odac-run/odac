package appmgr

import (
	"fmt"
	"time"

	"odac/internal/jscanon"
)

// BuildMissing is app.rollback's reason when the target build's image is
// not local; the rest of its closed set is shared with app.image.set
// (ImageInvalid, ImageBusy, ImageStartFailed, ImageUnhealthy,
// ImageRollbackFailed, ImageSnapshot).
const BuildMissing = "missing"

// RollbackPayload carries app.rollback's named arguments; RequireSnapshot
// is as in ImageSetPayload.
type RollbackPayload struct {
	Container       string
	BuildID         string
	RequireSnapshot bool
}

// RollbackResult is app.rollback's reply, flat on the wire like
// ImageSetResult: the dashboard reads reason and rolledBack beside success.
type RollbackResult struct {
	Success    bool
	Changed    bool
	BuildID    string
	Previous   string
	CommitSha  string
	ImageID    string
	Reason     string
	RolledBack bool
	Message    string
	Snapshot   string
}

// ReplyFields renders the reply in the contract's key order. A failure
// carries reason, rolledBack and message; a success names the builds only
// when something changed, and previous and commitSha only when known.
// Either ends with snapshot when one was taken.
func (r RollbackResult) ReplyFields() jscanon.Obj {
	if !r.Success {
		return withSnapshot(failureFields(r.Reason, r.RolledBack, r.Message), r.Snapshot)
	}
	obj := jscanon.Obj{{K: "success", V: true}, {K: "changed", V: r.Changed}}
	if !r.Changed {
		return obj
	}
	obj = append(obj, jscanon.Field{K: "buildId", V: r.BuildID})
	if r.Previous != "" {
		obj = append(obj, jscanon.Field{K: "previous", V: r.Previous})
	}
	if r.CommitSha != "" {
		obj = append(obj, jscanon.Field{K: "commitSha", V: r.CommitSha})
	}
	if r.ImageID != "" {
		obj = append(obj, jscanon.Field{K: "imageId", V: r.ImageID})
	}
	return withSnapshot(obj, r.Snapshot)
}

func rollbackFailure(reason string, rolledBack bool, format string, args ...any) RollbackResult {
	return RollbackResult{Reason: reason, RolledBack: rolledBack, Message: fmt.Sprintf(format, args...)}
}

// Rollback is app.rollback: return a git app to one of its kept builds
// without rebuilding it.
//
// The switch is the image change's (imageSwitch): the previous image is
// held, the marker is on disk before anything stops, and the record names
// the target build only once it proved itself. The difference is the
// reference: a git app's record always names odac-app-<name>, so the target
// build's image is put under that name first, and every failure points it
// back at the previous image. That keeps the invariant that odac-app-<name>
// is what runs, so a restart, Check's respawn and the next Blue-Green start
// the right build, and recoverImageChange finishes an interrupted rollback
// unchanged.
//
// The snapshot comes first, while the current build serves: before the
// target is tagged and before a green container starts, since the target's
// code may migrate the data as it starts.
//
// An app that redeploys Blue-Green rolls back Blue-Green: traffic moves only
// once the green container passed readiness, so a failure leaves the old
// container serving and is reported as rolledBack (the app is on the
// previous build, it never left it). Any other app is recreated and gated
// like an image change.
//
// Nothing here writes a build log or a tag, or reorders retention: the
// target stays where it was in localBuilds.
func (m *Manager) Rollback(p RollbackPayload) (out RollbackResult) {
	var name, typ, current, recorded string
	var idNum float64
	var active, dev bool
	var prevStatus any
	found := false
	m.cfg.View(func() {
		if app := m.getLocked(p.Container); app != nil && p.Container != "" {
			found = true
			name, _ = app["name"].(string)
			typ, _ = app["type"].(string)
			current, _ = app["image"].(string)
			recorded, _ = app[buildIDKey].(string)
			idNum, _ = app["id"].(float64)
			active = app["active"] == true
			dev = app["dev"] == true
			prevStatus = app["status"]
		}
	})
	switch {
	case !found:
		return rollbackFailure(ImageInvalid, false, "App %s not found.", p.Container)
	case typ != "git":
		return rollbackFailure(ImageInvalid, false, "Only git apps roll back to a previous build. A catalog app changes its image tag instead.")
	case dev:
		return rollbackFailure(ImageInvalid, false, "App %s runs in development mode from its source directory and has no build to roll back to.", name)
	case !validBuildID(p.BuildID):
		return rollbackFailure(ImageInvalid, false, "Invalid build id.")
	case current != buildRepo(name):
		return rollbackFailure(ImageInvalid, false, "App %s does not run on its own build image, so it has no kept builds.", name)
	}
	// Check the processing lock before active: HoldApp persists active=false
	// while a restore holds the app, and a rollback must answer busy rather
	// than invalid.
	if m.isCreating(name) || m.isProcessing(idNum) {
		return rollbackFailure(ImageBusy, false, "App %s is already being processed.", name)
	}
	if !active {
		return rollbackFailure(ImageInvalid, false, "App %s is stopped. Start it before rolling it back.", name)
	}
	if m.isCreating(name) || !m.tryLockProcessing(idNum) {
		return rollbackFailure(ImageBusy, false, "App %s is already being processed.", name)
	}
	defer m.unlockProcessing(idNum)
	m.cfg.View(func() {
		if app := m.getLocked(idNum); app != nil {
			active = app["active"] == true
			prevStatus = app["status"]
		} else {
			active = false
		}
	})
	if !active {
		return rollbackFailure(ImageInvalid, false, "App %s is stopped. Start it before rolling it back.", name)
	}

	// Nothing was tried: the Cloud retries a busy answer.
	if !m.deps.Docker.Available() {
		return rollbackFailure(ImageBusy, false, "Docker is not available.")
	}
	target := buildRef(name, p.BuildID)
	meta, err := m.deps.Docker.ImageMeta(target)
	if err != nil || meta.ID == "" {
		return rollbackFailure(BuildMissing, false, "Build %s of %s is not kept on this server anymore.", p.BuildID, name)
	}
	st, err := m.deps.Docker.InspectState(name)
	if err != nil {
		return rollbackFailure(ImageBusy, false, "Docker did not answer for %s: %s", name, shortError(err))
	}
	// What a failure returns to: the running container's image, else what
	// the record's reference names (the image Check would start).
	prevID := ""
	if st.Exists {
		prevID = st.ImageID
	}
	if prevID == "" {
		if cur, err := m.deps.Docker.ImageMeta(current); err == nil {
			prevID = cur.ID
		}
	}
	builds, _ := m.appBuilds(name)
	previous := currentBuild(builds, recorded, prevID)
	identity := m.readBuildIdentity(name, p.BuildID)

	// The target already runs. Two builds can produce one image; the record
	// then follows the build that was asked for, without touching anything.
	if prevID == meta.ID {
		if recorded != p.BuildID {
			m.cfg.Mutate(func() {
				if app := m.getLocked(idNum); app != nil {
					applyBuildIdentityLocked(app, p.BuildID, identity)
					m.saveAppsLocked()
				}
			})
		}
		m.log.Log("Build %s of %s already runs", p.BuildID, name)
		return RollbackResult{Success: true}
	}

	back := "the previous build"
	if previous != "" {
		back = "build " + previous
	}
	deadline := time.Now().Add(m.imageSetBudget)
	m.log.Log("Rolling %s back to build %s (from %s)", name, p.BuildID, back)
	m.set(idNum, map[string]any{"status": "updating"})
	restoreStatus := func() { m.set(idNum, map[string]any{"status": prevStatus}) }

	snapRun, refusal := m.switchSnapshot(name, deadline, meta.Healthcheck, p.RequireSnapshot)
	if refusal != "" {
		restoreStatus()
		return rollbackFailure(ImageSnapshot, false, "%s", refusal)
	}
	// Every answer from here on, failures included, names the snapshot.
	defer func() { out.Snapshot = snapRun }()

	zdd := m.zddEligible(name, idNum)
	marker := map[string]any{"image": target, "build": p.BuildID}
	greenName := ""
	if zdd {
		greenName = name + "-green-" + generateRuntimeID("")
		marker["green"] = greenName
	}
	sw := m.beginSwitch(name, idNum, current, prevID, marker, switchText{
		op: "Rollback", to: "build " + p.BuildID, back: back, during: "the rollback",
	})

	// The record's reference moves first; every path below either commits
	// it or points it back.
	if err := m.deps.Docker.TagImage(target, current); err != nil {
		sw.abort()
		restoreStatus()
		return rollbackFailure(ImageStartFailed, true, "Could not select build %s: %s. %s stays live.", p.BuildID, shortError(err), capitalize(back))
	}

	if zdd {
		if result, failed := m.rollbackBlueGreen(sw, idNum, name, greenName, p.BuildID, back, restoreStatus); failed {
			return result
		}
	} else {
		m.endLogStream(name)
		m.deps.Docker.Stop(name)
		if err := m.runHeldOn(idNum, nil, ""); err != nil {
			return fromSwitch(sw.rollback(ImageStartFailed, fmt.Sprintf("Build %s did not start: %s.", p.BuildID, shortError(err))))
		}
		if cause := m.imageGate(name, idNum, meta.Healthcheck, deadline); cause != "" {
			return fromSwitch(sw.rollback(ImageUnhealthy, cause))
		}
	}

	sw.commit(func(app map[string]any) { applyBuildIdentityLocked(app, p.BuildID, identity) })
	if !zdd {
		// The content changed under the same address; Blue-Green purges on
		// its own switch.
		m.proxyPurge(idNum)
	}

	done := RollbackResult{Success: true, Changed: true, BuildID: p.BuildID, Previous: previous, CommitSha: identity.commitSha, ImageID: meta.ID}
	if st, err := m.deps.Docker.InspectState(name); err == nil && st.ImageID != "" {
		done.ImageID = st.ImageID
	}
	m.log.Log("Rolled %s back to build %s", name, p.BuildID)
	return done
}

// rollbackBlueGreen runs the green container on the target build and moves
// traffic only once it is ready. failed reports a finished failure; on
// false the switch is live and the caller commits it.
func (m *Manager) rollbackBlueGreen(sw *imageSwitch, id float64, name, greenName, buildID, back string, restoreStatus func()) (RollbackResult, bool) {
	startFailed := false
	err := m.performBlueGreenDeploy(id, greenName, deployOptions{
		operation: "Rollback",
		runGreenContainer: func() error {
			err := m.runGitApp(id, greenName)
			startFailed = err != nil
			return err
		},
	})
	removeGreen := func() {
		if status := m.deps.Docker.GetStatus(greenName); status.Running {
			m.deps.Docker.Stop(greenName)
			m.deps.Docker.Remove(greenName)
		}
		m.cleanupGreenArtifacts(greenName)
	}
	if m.appDeleted(id) {
		removeGreen()
		sw.settle()
		return rollbackFailure(ImageStartFailed, false, "The app was deleted during the rollback."), true
	}
	if err == nil {
		return RollbackResult{}, false
	}
	m.log.Error("Rollback of %s to build %s failed: %s %s stays live.", name, buildID, err.Error(), capitalize(back))
	removeGreen()
	// The old container never stopped: only the reference goes back.
	sw.abort()
	restoreStatus()
	reason := ImageUnhealthy
	if startFailed {
		reason = ImageStartFailed
	}
	return rollbackFailure(reason, true, "%s %s stays live.", shortError(err), capitalize(back)), true
}

// applyBuildIdentityLocked makes the record describe the build now live:
// its id, and the commit and runtime its summary recorded. Unknown members
// are removed rather than kept from another build. The runtime override is
// the operator's standing choice and is not touched. Caller holds
// cfg.Mutate.
func applyBuildIdentityLocked(app map[string]any, buildID string, id buildIdentity) {
	app[buildIDKey] = buildID
	if id.commitSha != "" {
		app["commitSha"] = id.commitSha
	} else {
		delete(app, "commitSha")
	}
	if id.runtime != nil {
		app["runtime"] = id.runtime
	} else {
		delete(app, "runtime")
	}
}

func fromSwitch(f switchFailure) RollbackResult {
	return rollbackFailure(f.reason, f.rolledBack, "%s", f.message)
}

// capitalize upper-cases a message fragment's first ASCII letter.
func capitalize(s string) string {
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}
