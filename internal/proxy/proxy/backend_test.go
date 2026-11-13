package proxy

import (
	"testing"

	"odac/internal/proxy/config"
)

func TestSocketUnderAcceptsOnlyContainerDirAndName(t *testing.T) {
	root := "/data/sockets"
	for path, want := range map[string]bool{
		"/data/sockets/app-blue/http.sock":        true,
		"/data/sockets/app-blue/./http.sock":      true,
		"/data/sockets/http.sock":                 false,
		"/data/sockets/app/nested/http.sock":      false,
		"/data/sockets/app/../../run/docker.sock": false,
		"/var/run/docker.sock":                    false,
		"/data/socketsX/app/http.sock":            false,
		"app/http.sock":                           false,
		"":                                        false,
	} {
		if got := socketUnder(root, path); got != want {
			t.Errorf("socketUnder(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSocketBackendsIgnoreSocketsWithoutRoot(t *testing.T) {
	var s socketBackends
	s.update(map[string]config.Website{"a.test": {ContainerIP: "10.0.0.2", Port: 3000, Socket: "/data/sockets/a/http.sock"}})
	if s.lookup("10.0.0.2:3000") != nil {
		t.Fatal("a socket backend was registered with no socket root")
	}
}
