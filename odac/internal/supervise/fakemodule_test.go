package supervise

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/logx"
)

// TestMain doubles as a fake data-plane module: tests copy this test binary
// into a bin dir as "odac-proxy" and set GO_TEST_FAKE_MODULE=1 (inherited by
// children), so spawned copies bind the control socket like the real binaries
// and block forever. The executable name tells the roles apart — the copy is
// "odac-proxy", the test process is "supervise.test".
func TestMain(m *testing.M) {
	if os.Getenv("GO_TEST_FAKE_MODULE") == "1" && strings.HasPrefix(filepath.Base(os.Args[0]), "odac-") {
		fakeModuleMain()
		return
	}
	logx.Stdout = io.Discard
	logx.Stderr = io.Discard
	os.Exit(m.Run())
}

func fakeModuleMain() {
	sock := ""
	for _, k := range []string{"ODAC_SOCKET_PATH", "ODAC_DNS_SOCKET_PATH", "ODAC_MAIL_SOCKET_PATH"} {
		if v := os.Getenv(k); v != "" {
			sock = v
			break
		}
	}
	if sock == "" || os.Getenv("GO_FAKE_NO_SOCKET") == "1" {
		select {}
	}
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		c.Close()
	}
}
