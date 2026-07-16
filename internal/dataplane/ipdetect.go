package dataplane

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"odac/internal/logx"
)

// ipDetector ports DNS.js's IP detection (#collectLocalIPs, #getExternalIP,
// #lookupPTRRecords). Every input is injectable for tests; the defaults hit
// the real network exactly like Node: same service lists, same 5s timeouts,
// same User-Agent, same validation.
type ipDetector struct {
	v4Services []string
	v6Services []string
	httpGet    func(url string, ipv6 bool) (string, error)
	localAddrs func() []localAddr
	reverse    func(ctx context.Context, addr string) ([]string, error)
	ptrBudget  time.Duration
}

type localAddr struct {
	iface string
	addr  string
	v6    bool
}

func newIPDetector() *ipDetector {
	return &ipDetector{
		v4Services: []string{
			"https://curlmyip.org/",
			"https://ipv4.icanhazip.com/",
			"https://api.ipify.org/",
			"https://checkip.amazonaws.com/",
			"https://ipinfo.io/ip",
		},
		v6Services: []string{
			"https://ipv6.icanhazip.com/",
			"https://api64.ipify.org/",
			"https://v6.ident.me/",
		},
		httpGet:    realHTTPGet,
		localAddrs: realLocalAddrs,
		reverse:    net.DefaultResolver.LookupAddr,
		ptrBudget:  5 * time.Second,
	}
}

var (
	ipv4Re = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	ipv6Re = regexp.MustCompile(`^[0-9a-fA-F:]+$`)
)

// run executes one detection pass over the existing address set (entries
// accumulate across restarts with per-address dedupe, like Node's live
// this.ips arrays) and returns the updated set plus the primary IPv4.
func (det *ipDetector) run(log *logx.Logger, v4, v6 []IPEntry, primary string) ([]IPEntry, []IPEntry, string) {
	// Local interfaces.
	for _, la := range det.localAddrs() {
		if la.v6 {
			if strings.HasPrefix(strings.ToLower(la.addr), "fe80:") {
				continue
			}
			if !containsAddr(v6, la.addr) {
				pub := !isPrivateIPv6(la.addr)
				v6 = append(v6, IPEntry{Address: la.addr, Public: pub})
				log.Log(fmt.Sprintf("Local IPv6 detected on %s: %s [%s]", la.iface, la.addr, pubLabel(pub)))
			}
		} else if !containsAddr(v4, la.addr) {
			pub := !isPrivateIPv4(la.addr)
			v4 = append(v4, IPEntry{Address: la.addr, Public: pub})
			log.Log(fmt.Sprintf("Local IPv4 detected on %s: %s [%s]", la.iface, la.addr, pubLabel(pub)))
		}
	}

	// External IPv4: first responsive service wins, unshifted to the front.
	for _, service := range det.v4Services {
		log.Log(fmt.Sprintf("Attempting to get external IPv4 from %s", service))
		body, err := det.httpGet(service, false)
		if err != nil {
			log.Log(fmt.Sprintf("Failed to get IPv4 from %s:", service), err.Error())
			continue
		}
		ip := strings.TrimSpace(body)
		if ip != "" && ipv4Re.MatchString(ip) {
			log.Log("External IPv4 detected:", ip)
			if !containsAddr(v4, ip) {
				v4 = append([]IPEntry{{Address: ip, Public: true}}, v4...)
			}
			primary = ip
			break
		}
		log.Log(fmt.Sprintf("Invalid IPv4 format from %s:", service), ip)
	}

	// External IPv6.
	for _, service := range det.v6Services {
		log.Log(fmt.Sprintf("Attempting to get external IPv6 from %s", service))
		body, err := det.httpGet(service, true)
		if err != nil {
			log.Log(fmt.Sprintf("Failed to get IPv6 from %s:", service), err.Error())
			continue
		}
		ip := strings.TrimSpace(body)
		if ip != "" && ipv6Re.MatchString(ip) && strings.Contains(ip, ":") {
			log.Log("External IPv6 detected:", ip)
			if !containsAddr(v6, ip) {
				v6 = append([]IPEntry{{Address: ip, Public: true}}, v6...)
			}
			break
		}
		log.Log(fmt.Sprintf("Invalid IPv6 format from %s:", service), ip)
	}

	// Primary IPv4: first public, else first, else whatever it already was.
	if len(v4) == 0 {
		log.Log("Could not determine external IPv4, using default 127.0.0.1")
		log.Error("DNS", "All IPv4 detection methods failed, DNS A records will use 127.0.0.1")
	} else {
		primary = v4[0].Address
		for _, e := range v4 {
			if e.Public {
				primary = e.Address
				break
			}
		}
	}

	det.lookupPTRs(log, v4, v6)

	log.Log(fmt.Sprintf("Detected IPs - IPv4: [%s]", joinEntries(v4)))
	log.Log(fmt.Sprintf("Detected IPs - IPv6: [%s]", joinEntries(v6)))
	return v4, v6, primary
}

// lookupPTRs resolves every address's PTR in parallel, capped at ptrBudget
// overall (Node: Promise.race(allSettled, 5s timer)). Results that miss the
// budget are dropped; the buffered channel lets late goroutines finish
// without touching the entries the caller already reads.
func (det *ipDetector) lookupPTRs(log *logx.Logger, sets ...[]IPEntry) {
	type ptrRes struct {
		set, idx int
		ptr      string
	}
	total := 0
	for _, set := range sets {
		total += len(set)
	}
	if total == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), det.ptrBudget)
	defer cancel()
	ch := make(chan ptrRes, total)
	for si, set := range sets {
		for i := range set {
			go func(si, i int, addr string) {
				ptr := ""
				if names, err := det.reverse(ctx, addr); err == nil && len(names) > 0 {
					// LookupAddr returns FQDNs with a trailing dot; Node's
					// dns.promises.reverse does not — normalize for parity.
					ptr = strings.TrimSuffix(names[0], ".")
				}
				ch <- ptrRes{si, i, ptr}
			}(si, i, set[i].Address)
		}
	}

	deadline := time.After(det.ptrBudget)
	for received := 0; received < total; received++ {
		select {
		case r := <-ch:
			sets[r.set][r.idx].PTR = r.ptr
			if r.ptr != "" {
				log.Log(fmt.Sprintf("PTR record for %s: %s", sets[r.set][r.idx].Address, r.ptr))
			}
		case <-deadline:
			return
		}
	}
}

func containsAddr(set []IPEntry, addr string) bool {
	for _, e := range set {
		if e.Address == addr {
			return true
		}
	}
	return false
}

func pubLabel(public bool) string {
	if public {
		return "public"
	}
	return "private"
}

func joinEntries(set []IPEntry) string {
	parts := make([]string, len(set))
	for i, e := range set {
		s := e.Address + " [" + pubLabel(e.Public) + "]"
		if e.PTR != "" {
			s += " (" + e.PTR + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}

// Private IPv4 ranges (RFC 1918, RFC 6598 CGNAT, loopback, link-local) —
// exact port of DNS.js #privateIPv4Ranges.
var privateIPv4Ranges = [][2]uint32{
	{0x0a000000, 0x0affffff}, // 10.0.0.0/8
	{0x64400000, 0x647fffff}, // 100.64.0.0/10 (CGNAT)
	{0x7f000000, 0x7fffffff}, // 127.0.0.0/8 (loopback)
	{0xa9fe0000, 0xa9feffff}, // 169.254.0.0/16 (link-local)
	{0xac100000, 0xac1fffff}, // 172.16.0.0/12
	{0xc0a80000, 0xc0a8ffff}, // 192.168.0.0/16
}

func isPrivateIPv4(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}
	var n uint32
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 {
			return false // Node: NaN propagates and every range check fails
		}
		n = n<<8 | uint32(v)
	}
	for _, r := range privateIPv4Ranges {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

func isPrivateIPv6(ip string) bool {
	n := strings.ToLower(ip)
	return strings.HasPrefix(n, "fe80:") || strings.HasPrefix(n, "fc") ||
		strings.HasPrefix(n, "fd") || n == "::1"
}

func realHTTPGet(url string, ipv6 bool) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	if ipv6 { // Node: axios family: 6
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp6", addr)
			},
		}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Odac-DNS/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// realLocalAddrs lists non-loopback interface addresses (Node skips
// iface.internal, which marks loopback addresses).
func realLocalAddrs() []localAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []localAddr
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				out = append(out, localAddr{iface: ifc.Name, addr: v4.String()})
			} else {
				out = append(out, localAddr{iface: ifc.Name, addr: ipnet.IP.String(), v6: true})
			}
		}
	}
	return out
}
