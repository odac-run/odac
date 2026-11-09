package docker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/network"
)

// PrivateNetwork is where an app with an empty network list lands: a bridge
// with inter-container traffic switched off. The proxy reaches it from the
// host namespace and the app keeps its egress, but no other container can
// reach it, which is what an operator removing every network asks for. It is
// ODAC's placement, not a choice: no list can name it and no report shows it.
const PrivateNetwork = "odac-network-private"

// iccOption is the bridge driver option behind PrivateNetwork. Like
// `internal` it belongs to the network and cannot change after creation.
const iccOption = "com.docker.network.bridge.enable_icc"

var networkNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// reservedNetworks cannot appear in an app's list. "host" there would share
// the host's stack past every guard on networkMode (domains, net.* sysctls),
// "none" leaves the container without an address the proxy can reach, and
// PrivateNetwork is only ever ODAC's fallback for an empty list.
var reservedNetworks = map[string]bool{"host": true, "none": true, "default": true, PrivateNetwork: true}

// NormalizeNetworks validates an app's network list and returns it
// deduplicated and sorted, never nil. It is the single check for both doors,
// the Hub payload and a hand-edited record at container create, so the
// persisted list and the create call always agree. An empty list is legal
// and means PrivateNetwork.
func NormalizeNetworks(names []string) ([]string, error) {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !networkNamePattern.MatchString(n) || reservedNetworks[strings.ToLower(n)] {
			return nil, fmt.Errorf("invalid network name: %q", n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// IsDefaultNetworks reports whether a normalized list is what an app gets
// with no list at all, which is when appmgr leaves the record untouched.
func IsDefaultNetworks(names []string) bool {
	return len(names) == 1 && names[0] == DefaultNetwork
}

// EnsureNetworks creates every missing network in the list, so a stopped
// app's new list is refused when it is set rather than on each start.
func (c *Client) EnsureNetworks(names []string) error {
	if !c.available {
		return errors.New("Docker is not available")
	}
	ctx := context.Background()
	for _, n := range withPrivateFallback(names) {
		if err := c.ensureNetwork(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func withPrivateFallback(names []string) []string {
	if len(names) == 0 {
		return []string{PrivateNetwork}
	}
	return names
}

// networkSpec is how each network is created. ODAC's own two carry the
// property their guarantee rests on; every other name is a plain bridge.
func networkSpec(name string) network.CreateOptions {
	opts := network.CreateOptions{Driver: "bridge"}
	switch name {
	case IsolatedNetwork:
		// FORWARD rules drop every packet routed between the subnet and any
		// other interface: the enforcement behind RunOptions.Isolated.
		opts.Internal = true
	case PrivateNetwork:
		opts.Options = map[string]string{iccOption: "false"}
	}
	return opts
}

// ensureNetwork makes sure the named network exists. Failures are logged
// and returned; container create callers may ignore them, since the daemon
// refuses a missing network there anyway. A network that already exists is
// taken as-is because its options cannot be changed, so one of ODAC's own
// names created by hand without its property is reported, not trusted.
func (c *Client) ensureNetwork(ctx context.Context, name string) error {
	networks, err := c.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		c.log.Error("Failed to ensure network %s: %s", name, err.Error())
		return err
	}
	want := networkSpec(name)
	for _, n := range networks {
		if n.Name != name {
			continue
		}
		if want.Internal && !n.Internal {
			c.log.Error("Network %s exists but is NOT internal: apps on it can still reach the internet. Remove it and let ODAC recreate it.", name)
		}
		if want.Options[iccOption] == "false" && n.Options[iccOption] != "false" {
			c.log.Error("Network %s exists but allows inter-container traffic: apps on it can reach each other. Remove it and let ODAC recreate it.", name)
		}
		return nil
	}
	c.log.Log("Creating network %s (internal=%v)...", name, want.Internal)
	if _, err := c.api.NetworkCreate(ctx, name, want); err != nil {
		c.log.Error("Failed to ensure network %s: %s", name, err.Error())
		return err
	}
	return nil
}

// appNetworks resolves RunOptions.Networks for a bridge container: nil is an
// app that never set a list, empty is one that removed every network. A list
// that fails validation can only come from a hand-edited record, so it
// degrades to DefaultNetwork instead of failing every start.
func (c *Client) appNetworks(name string, requested []string) []string {
	if requested == nil {
		return []string{DefaultNetwork}
	}
	nets, err := NormalizeNetworks(requested)
	if err != nil {
		c.log.Error("App %s has an unusable network list (%s), starting it on %s.", name, err.Error(), DefaultNetwork)
		return []string{DefaultNetwork}
	}
	return withPrivateFallback(nets)
}

func (c *Client) ignoreNetworks(name string, requested []string, reason string) {
	if len(requested) > 0 {
		c.log.Log("App %s: ignoring its network list [%s] because of %s.", name, strings.Join(requested, ", "), reason)
	}
}
