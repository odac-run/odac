// Package appstatus centralizes the app lifecycle status vocabulary shared by
// the server's app list and the CLI renderers. Both sides need to know which
// states mean "an operation currently owns this app"; keeping the set in one
// place stops the two from drifting apart.
package appstatus

// The transient states an app carries while an operation (create, build,
// deploy, start) owns it. In those windows the container does not exist yet,
// so its absence must not be reported as a resting "stopped".
const (
	Installing = "installing"
	Building   = "building"
	Starting   = "starting"
	Switching  = "switching"
	Updating   = "updating"
)

var transient = map[string]bool{
	Installing: true,
	Building:   true,
	Starting:   true,
	Switching:  true,
	Updating:   true,
}

// IsTransient reports whether status means an operation is in flight, so
// callers keep the lifecycle label instead of falling back to the live
// container state.
func IsTransient(status string) bool { return transient[status] }
