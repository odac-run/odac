// odac-backup runs one backup operation and exits. The server starts it per
// job inside a helper container made from ODAC's own image, so the backup
// engine (and its dependency tree) lives only in this binary and a crash,
// leak or stall in it stays inside that container. It never holds the
// Docker socket: the server owns every container and streams dump output
// to this process's stdin.
package main

import (
	"fmt"
	"io"
	"os"

	"odac/internal/sysinfo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, sysinfo.Version)
		return 0
	}
	fmt.Fprintln(stderr, "usage: odac-backup version")
	return 2
}
