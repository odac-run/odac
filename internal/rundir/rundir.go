// Package rundir names the directories that hold ODAC's unix sockets.
//
// App is bind-mounted read-only at /odac into every container with an API
// grant, and a read-only mount does not stop connect() on a socket inside it.
// App therefore holds api.sock and nothing else. Every control socket and PID
// file lives in Control, a sibling that is never mounted. A private
// subdirectory of App would not do: root in a container keeps
// CAP_DAC_OVERRIDE and walks straight into a 0700 directory.
package rundir

import (
	"os"
	"path/filepath"
	"runtime"
)

// BackendMount is where a container sees its own backend socket directory, and
// BackendSocket the socket the app creates there. ODAC_PROXY_SOCKET carries the
// joined path to the app; the env var is the contract, these are not.
const (
	BackendMount  = "/odac-proxy"
	BackendSocket = "http.sock"
)

// App returns <base>/run, the directory mounted into apps. Only api.sock may
// live here.
func App(base string) string {
	return filepath.Join(base, "run")
}

// Control returns <base>/control, which holds the data-plane control sockets,
// their PID files and the update handover socket.
func Control(base string) string {
	return filepath.Join(base, "control")
}

// Prepare creates dir with mode 0700, or tightens an existing one to it.
func Prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Backends returns <base>/sockets, which holds one directory per app container
// for the unix socket the proxy dials. Each directory is mounted into its own
// container only. It returns "" off Linux, where the proxy cannot dial a
// socket pinned, and when ODAC_BACKEND_SOCKETS is "off", which keeps every app
// on TCP.
func Backends(base string) string {
	if runtime.GOOS != "linux" || os.Getenv("ODAC_BACKEND_SOCKETS") == "off" {
		return ""
	}
	return filepath.Join(base, "sockets")
}
