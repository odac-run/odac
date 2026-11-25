package appmgr

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"odac/internal/applog"
	"odac/internal/docker"
)

// A git app keeps the images of its last successful builds under
// odac-app-<name>:<buildId>, beside the odac-app-<name> reference its
// container runs, so app.rollback can return to one without a rebuild.
//
// Only a build that went live is tagged: one that failed to compile, or
// whose container never came up, is a failed deployment and is never a
// rollback target. Retention runs after the new build is live and never
// takes the image the running container uses. odac-app-<name> always names
// the running build's image: a redeploy builds onto it and a rollback
// retags it, so Check's restarts and the next Blue-Green start what runs.

// buildTagRE is Docker's tag grammar. A build id outside it is never tagged
// and never accepted as a rollback target.
var buildTagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

const (
	// buildRepoPrefix is the repository of every image the builder makes.
	buildRepoPrefix = "odac-app-"
	// keptBuilds counts the running build, so two rollback targets remain.
	// applog keeps the same number of successful summaries, which is where
	// a rollback reads each build's commit and runtime.
	keptBuilds = 3
	// lowDiskKeptBuilds applies when the engine's disk is nearly full.
	lowDiskKeptBuilds = 1
	// lowDiskFraction is the free-space share under which only the running
	// build is kept.
	lowDiskFraction = 0.10
	// buildIDKey names the running build on the record: what a redeploy or
	// a rollback made live. It disambiguates builds that produced the same
	// image and never reaches app.list (currentBuild does).
	buildIDKey = "buildId"
)

// buildRepo is the repository of an app's built images.
func buildRepo(name string) string { return buildRepoPrefix + name }

// buildRef is one build's image reference.
func buildRef(name, buildID string) string { return buildRepo(name) + ":" + buildID }

// validBuildID reports whether id can be a build tag. "latest" is the
// running reference itself, never a build.
func validBuildID(id string) bool {
	return id != "latest" && buildTagRE.MatchString(id)
}

// localBuild is one build tag of an app and the image it names.
type localBuild struct {
	ID      string
	ImageID string
}

// buildTime is the millisecond timestamp generateRuntimeID embeds in a
// build id (build_<ms>_<hex>), 0 when the id has none.
func buildTime(id string) int64 {
	parts := strings.Split(id, "_")
	if len(parts) < 3 {
		return 0
	}
	ms, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
	if err != nil || ms < 0 {
		return 0
	}
	return ms
}

// sortBuilds orders builds newest first: by the time in the id, then by id.
// Image creation time is not used, since a fully cached rebuild reuses an
// older image and would sort a new build behind the ones it follows.
func sortBuilds(builds []localBuild) {
	sort.SliceStable(builds, func(i, j int) bool {
		ti, tj := buildTime(builds[i].ID), buildTime(builds[j].ID)
		if ti != tj {
			return ti > tj
		}
		return builds[i].ID > builds[j].ID
	})
}

// groupBuildTags turns an image tag listing into each app's builds, keyed
// by repository and sorted newest first. Anything that is not a build tag
// of a builder repository is ignored, and a tag listed twice counts once.
func groupBuildTags(tags []docker.ImageTag) map[string][]localBuild {
	out := map[string][]localBuild{}
	seen := map[string]bool{}
	for _, t := range tags {
		if !strings.HasPrefix(t.Repository, buildRepoPrefix) || !validBuildID(t.Tag) || t.ImageID == "" {
			continue
		}
		key := t.Repository + ":" + t.Tag
		if seen[key] {
			continue
		}
		seen[key] = true
		out[t.Repository] = append(out[t.Repository], localBuild{ID: t.Tag, ImageID: t.ImageID})
	}
	for repo := range out {
		sortBuilds(out[repo])
	}
	return out
}

// buildsToPrune picks the build tags retention removes from builds (newest
// first): keepID (the build just made live) and the newest others up to
// keep stay, and so does any tag naming runningImage, the image the live
// container uses, whatever its age.
func buildsToPrune(builds []localBuild, keep int, keepID, runningImage string) []string {
	kept := map[string]bool{}
	if keepID != "" {
		kept[keepID] = true
	}
	for _, b := range builds {
		if len(kept) >= keep {
			break
		}
		kept[b.ID] = true
	}
	var out []string
	for _, b := range builds {
		if kept[b.ID] || (runningImage != "" && b.ImageID == runningImage) {
			continue
		}
		out = append(out, b.ID)
	}
	return out
}

// currentBuild names the build the running container's image belongs to:
// the recorded build when its tag still names that image (two builds can
// produce the same image, and the record says which one was made live),
// else the newest tag naming it, else "" (an image from before tagging, a
// switch half done).
func currentBuild(builds []localBuild, recorded, runningImage string) string {
	if runningImage == "" {
		return ""
	}
	if recorded != "" {
		for _, b := range builds {
			if b.ID == recorded && b.ImageID == runningImage {
				return recorded
			}
		}
	}
	for _, b := range builds {
		if b.ImageID == runningImage {
			return b.ID
		}
	}
	return ""
}

// buildIDs renders builds as app.list's localBuilds: never nil, because an
// empty list is a report ("none") and an absent key is not.
func buildIDs(builds []localBuild) []string {
	out := make([]string, 0, len(builds))
	for _, b := range builds {
		out = append(out, b.ID)
	}
	return out
}

// applyBuilds adds localBuilds and currentBuild to a git app's app.list row
// and drops the record's own buildId, which is bookkeeping. A row of any
// other type never carries them, and a hand-edited record cannot inject
// them.
func applyBuilds(cp map[string]any, builds map[string][]localBuild, known bool, name, runningImage string) {
	recorded, _ := cp[buildIDKey].(string)
	delete(cp, buildIDKey)
	delete(cp, "localBuilds")
	delete(cp, "currentBuild")
	if cp["type"] != "git" || !known {
		return
	}
	list := builds[buildRepo(name)]
	cp["localBuilds"] = buildIDs(list)
	if cur := currentBuild(list, recorded, runningImage); cur != "" {
		cp["currentBuild"] = cur
	}
}

// appBuilds lists one app's build tags, newest first.
func (m *Manager) appBuilds(name string) ([]localBuild, error) {
	tags, err := m.deps.Docker.ImageTags(buildRepo(name))
	if err != nil {
		return nil, err
	}
	return groupBuildTags(tags)[buildRepo(name)], nil
}

// tagBuild names a build that just went live, imageName being the
// reference the build wrote. It reports whether the tag exists, so the
// record names only a build a rollback can reach. A record whose image is
// not the app's own builder repository (hand-edited) is never tagged.
func (m *Manager) tagBuild(name, imageName, buildID string) bool {
	if imageName != buildRepo(name) || !validBuildID(buildID) {
		return false
	}
	if err := m.deps.Docker.TagImage(imageName, buildRef(name, buildID)); err != nil {
		m.log.Error("Could not keep build %s of %s for rollback: %s", buildID, name, err.Error())
		return false
	}
	return true
}

// retainBuilds applies retention after keepID went live. fresh is a new
// app: any build tag already under its name belongs to a deleted app of the
// same name and must not become this one's rollback target, so only keepID
// stays. A listing that fails leaves every tag in place.
func (m *Manager) retainBuilds(name, keepID string, fresh bool) {
	builds, err := m.appBuilds(name)
	if err != nil {
		m.log.Error("Could not list the kept builds of %s: %s", name, err.Error())
		return
	}
	keep := keptBuilds
	if fresh {
		keep = 1
	} else if m.engineDiskLow() {
		keep = lowDiskKeptBuilds
		m.log.Log("Less than %d%% of the Docker disk is free: keeping only the running build of %s", int(lowDiskFraction*100), name)
	}
	running := ""
	if st, err := m.deps.Docker.InspectState(name); err == nil {
		running = st.ImageID
	} else {
		// Without the running image nothing proves a tag is unused, and the
		// engine still refuses to drop the last name of an image in use.
		m.log.Error("Could not read the running image of %s before pruning builds: %s", name, err.Error())
	}
	for _, id := range buildsToPrune(builds, keep, keepID, running) {
		if err := m.deps.Docker.UntagImage(buildRef(name, id)); err != nil {
			m.log.Error("Could not drop build %s of %s: %s", id, name, shortError(err))
		}
	}
}

// engineDiskLow reports whether the disk holding Docker's data root has
// less than lowDiskFraction free. Unknown is not low: retention then keeps
// the normal count, and the engine's own errors report a full disk.
func (m *Manager) engineDiskLow() bool {
	free, total, ok := m.deps.Docker.DataRootSpace()
	return ok && total > 0 && float64(free) < float64(total)*lowDiskFraction
}

// removeBuildTags drops every build tag of a deleted app.
func (m *Manager) removeBuildTags(name string) {
	builds, err := m.appBuilds(name)
	if err != nil {
		m.log.Error("Could not list the kept builds of %s: %s", name, err.Error())
		return
	}
	for _, b := range builds {
		m.deps.Docker.RemoveImage(buildRef(name, b.ID))
	}
}

// buildIdentity is what a build's summary says about what it built.
type buildIdentity struct {
	commitSha string
	runtime   map[string]any
}

// readBuildIdentity reads a build's commit and runtime from its applog
// summary, each "" / nil when unknown or malformed.
func (m *Manager) readBuildIdentity(name, buildID string) buildIdentity {
	var out buildIdentity
	info := applog.New(m.logsRoot, name).Build(buildID)
	if info == nil {
		return out
	}
	if sha, _ := info.Metadata["commitSha"].(string); fullCommitRE.MatchString(sha) {
		out.commitSha = sha
	}
	if rt := docker.RuntimeRow(info.Metadata["runtime"]); rt != nil {
		out.runtime = rt
	}
	return out
}

// recordBuildCommit stores the commit a build deployed in its summary, so a
// later rollback to it can restore the record's commitSha.
func recordBuildCommit(logCtrl *applog.BuildControl, commit string) {
	if logCtrl != nil && commit != "" {
		logCtrl.SetMetadata("commitSha", commit)
	}
}
