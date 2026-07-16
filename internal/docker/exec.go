package docker

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecInContainer ports Container.execInContainer: runs `sh -c command`
// inside the running container and returns its stdout.
//
// SECURITY: command MUST be a hardcoded literal string — never pass
// user-controlled input. All current callers use static strings (the
// port-scanning helpers). A future caller with dynamic args must switch to
// an array Cmd to bypass the shell entirely.
func (c *Client) ExecInContainer(name, command string) (string, error) {
	if !c.available {
		return "", fmt.Errorf("Docker not available")
	}
	ctx := context.Background()

	wrap := func(err error) error {
		return fmt.Errorf("Failed to exec command in %s: %s", name, err.Error())
	}

	if !c.IsRunning(name) {
		return "", wrap(fmt.Errorf("Container %s is not running", name))
	}

	exec, err := c.api.ContainerExecCreate(ctx, name, container.ExecOptions{
		Cmd:          []string{"sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", wrap(err)
	}

	resp, err := c.api.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", wrap(err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader); err != nil {
		return "", wrap(err)
	}

	info, err := c.api.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return "", wrap(err)
	}
	if info.ExitCode != 0 {
		detail := stderr.String()
		if detail == "" {
			detail = stdout.String()
		}
		return "", wrap(fmt.Errorf("Command failed (Exit Code %d): %s", info.ExitCode, detail))
	}
	return stdout.String(), nil
}

// GetListeningPorts ports Container.getListeningPorts: reads the
// container's /proc/net/tcp and /proc/net/tcp6 tables and returns the
// non-loopback LISTEN ports (much faster and more reliable than scanning;
// works on every Linux container). Empty on any failure.
func (c *Client) GetListeningPorts(name string) []int {
	if !c.available {
		return nil
	}
	seen := map[int]bool{}
	var order []int

	for _, proc := range []string{"cat /proc/net/tcp", "cat /proc/net/tcp6"} {
		out, err := c.ExecInContainer(name, proc)
		if err != nil {
			continue
		}
		for _, p := range parseProcNetTCP(out) {
			if !seen[p] {
				seen[p] = true
				order = append(order, p)
			}
		}
	}
	return order
}

// parseProcNetTCP extracts LISTEN (state 0A) local ports from a
// /proc/net/tcp(6) dump, skipping loopback binds and ports outside
// (0, 60000).
func parseProcNetTCP(output string) []int {
	var out []int
	lines := strings.Split(output, "\n")
	first := true
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if first { // header row
			first = false
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		localAddress, state := parts[1], parts[3]
		if state != "0A" {
			continue
		}
		ipHex, portHex, found := strings.Cut(localAddress, ":")
		if !found || portHex == "" {
			continue
		}
		port64, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil {
			continue
		}
		port := int(port64)

		// 0100007F = 127.0.0.1 (IPv4 loopback, little endian);
		// ...01000000 = ::1 (IPv6 loopback).
		isLoopback := ipHex == "0100007F" || ipHex == "00000000000000000000000001000000"
		if !isLoopback && port > 0 && port < 60000 {
			out = append(out, port)
		}
	}
	return dedupe(out)
}

func dedupe(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
