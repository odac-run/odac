package settings

import (
	"context"
	"net"
	"strings"
	"time"
)

const lookupTimeout = 3 * time.Second

// HostLocator decides "local" from this machine's own addresses: loopback,
// an address of one of its interfaces (ODAC runs in the host's network
// namespace, so these are the host's), or a container on one of its
// Docker bridges, such as a MinIO app on this server. A name is resolved
// once, with a short limit; one that does not resolve reads as remote,
// since the label is a warning and saving the target is not refused for
// it.
type HostLocator struct {
	// Resolver defaults to net.DefaultResolver.
	Resolver *net.Resolver
}

// IsLocal reports whether host is this machine.
func (l HostLocator) IsLocal(ctx context.Context, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		r := l.Resolver
		if r == nil {
			r = net.DefaultResolver
		}
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		addrs, err := r.LookupIPAddr(ctx, host)
		if err != nil {
			return false
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	own, bridges := localNets()
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return true
		}
		for _, o := range own {
			if o.Equal(ip) {
				return true
			}
		}
		for _, b := range bridges {
			if b.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// localNets returns this machine's addresses and the subnets of its
// Docker bridges, whose every address is a container here.
func localNets() (own []net.IP, bridges []*net.IPNet) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		bridge := ifc.Name == "docker0" || strings.HasPrefix(ifc.Name, "br-")
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			own = append(own, n.IP)
			if bridge {
				bridges = append(bridges, n)
			}
		}
	}
	return own, bridges
}
