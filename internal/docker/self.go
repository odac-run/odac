package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	shortIDPattern   = regexp.MustCompile(`^[0-9a-f]{12}$`)
	mountinfoPattern = regexp.MustCompile(`/docker/containers/([0-9a-f]{64})`)
	cgroupPattern    = regexp.MustCompile(`[-/]docker[-/]([0-9a-f]{64})`)
)

// SelfCandidates lists, best first, the names or ids that may identify the
// container this process runs in: the hostname when it looks like a short
// container id (a bridge container's default; a host-network one has the
// host's name), then the id Docker leaves in the mount table and the
// cgroup path. It only reads, so callers inspect each candidate in turn.
// The updater and the backup helper share it because both must name the
// running ODAC container, and a probe fixed in one place must not stay
// broken in the other.
func SelfCandidates(hostname string, readFile func(string) ([]byte, error)) []string {
	var out []string
	if shortIDPattern.MatchString(hostname) {
		out = append(out, hostname)
	}
	for _, p := range []struct {
		file    string
		pattern *regexp.Regexp
	}{
		{"/proc/self/mountinfo", mountinfoPattern},
		{"/proc/self/cgroup", cgroupPattern},
	} {
		content, err := readFile(p.file)
		if err != nil {
			continue
		}
		if m := p.pattern.FindSubmatch(content); m != nil {
			out = append(out, string(m[1]))
		}
	}
	return out
}

// SelfImage returns the image id of the container this process runs in,
// for helpers that must run the same build as the server: the image id,
// not its tag, because the tag moves to the next release during an
// update while this process still speaks the old protocol.
func (c *Client) SelfImage() (string, error) {
	c.mu.Lock()
	cached := c.selfImage
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if !c.available {
		return "", errors.New("Docker is not available")
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	for _, cand := range SelfCandidates(host, os.ReadFile) {
		info, err := c.api.ContainerInspect(ctx, cand)
		if err != nil || info.Image == "" {
			continue
		}
		c.mu.Lock()
		c.selfImage = info.Image
		c.mu.Unlock()
		return info.Image, nil
	}
	return "", fmt.Errorf("cannot identify ODAC's own container (hostname %q)", strings.TrimSpace(host))
}
