// odac-backup runs one backup operation and exits. The server starts it per
// job inside a helper container made from ODAC's own image, so the backup
// engine (and its dependency tree) lives only in this binary and a crash,
// leak or stall in it stays inside that container. It never holds the
// Docker socket: the server owns every container and streams dump output
// to this process's stdin. The conversation itself is internal/backup/proto.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"odac/internal/sysinfo"
)

func main() {
	// docker stop sends SIGTERM; the run ends as canceled and closes the
	// repository instead of dying mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "version":
			fmt.Fprintln(stdout, sysinfo.Version)
			return 0
		case "run":
			return serve(ctx, stdin, stdout)
		}
	}
	fmt.Fprintln(stderr, "usage: odac-backup version | run")
	return 2
}
