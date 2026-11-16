package appmgr

import (
	"regexp"
	"strings"

	"odac/internal/applog"
	"odac/internal/docker"
)

// fullCommitRE is the only commitSha shape app.list reports. The redeploy
// payload accepts abbreviated SHAs, but an abbreviation is the caller's
// shorthand, not a record of what was built.
var fullCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// recordBuildRuntime stores the runtime a build used in that build's
// summary, which is what app.list's build.runtime reads. It is recorded on a
// failed build too: the summary describes the attempt, not the running app.
func recordBuildRuntime(logCtrl *applog.BuildControl, rt docker.Runtime) {
	if logCtrl == nil {
		return
	}
	if row := rt.Map(); row != nil {
		logCtrl.SetMetadata("runtime", row)
	}
}

// deployedCommit picks the commit a deploy built: the HEAD git resolved in
// the sandbox, else the requested SHA when it was a full one, else "" (the
// deploy happened, but nothing can name its commit).
func deployedCommit(head, requested string) string {
	if fullCommitRE.MatchString(head) {
		return head
	}
	if requested = strings.ToLower(requested); fullCommitRE.MatchString(requested) {
		return requested
	}
	return ""
}

// applyIdentity adds what app.list reports about an app's image and source
// to a row copied from its record. Every member is present only when it is
// known, since the Cloud diffs rows by serialization: imageId and
// imageDigest come from the container, runtime and commitSha from the record
// (re-validated, because the record can be edited by hand).
func (m *Manager) applyIdentity(cp map[string]any, status docker.Status) {
	if status.ImageID != "" {
		cp["imageId"] = status.ImageID
		ref, _ := cp["image"].(string)
		if digest := m.deps.Docker.ImageDigest(status.ImageID, ref); digest != "" {
			cp["imageDigest"] = digest
		}
	}

	if rt := docker.RuntimeRow(cp["runtime"]); rt != nil && cp["type"] == "git" {
		cp["runtime"] = rt
	} else {
		delete(cp, "runtime")
	}

	sha, _ := cp["commitSha"].(string)
	if sha = strings.ToLower(sha); fullCommitRE.MatchString(sha) {
		cp["commitSha"] = sha
	} else {
		delete(cp, "commitSha")
	}
}
