// Package ports is the Go port of server/src/Ports.js (dev 5a23c38/db7cdb2):
// the shared semantics of app port mappings, used by the app manager (Docker
// publishing) and the proxy payload builder (backend routing).
//
// A port entry is `{host, container, public?, auto?, proto?}` decoded from config
// JSON, so entries are map[string]any and numbers are float64. `container` is
// always the port the process listens on inside the container; `host` decides
// how it is exposed:
//
//   - <number> -> published on the host through Docker PortBindings.
//   - 'proxy'  -> not published; ODAC's reverse proxy routes to the container
//     port over the internal Docker network.
//
// 'proxy' is the explicit spelling of what used to be an absent `host`; a
// missing host still counts as proxy so configs written before the sentinel
// keep resolving. 'auto' is an input-only host value, resolved to a free host
// port before the entry is persisted.
//
// `public` only applies to published entries and decides the bind address:
// absent/false binds 127.0.0.1, true binds every interface. Publishing is
// opt-in per entry because a published port never traverses the host
// firewall's INPUT chain: Docker's DNAT rewrites the destination in
// nat/PREROUTING, so a public entry is reachable from the internet even when
// ufw says otherwise — it must never become the default for an entry that did
// not ask for it.
//
// `auto` (the boolean field, distinct from the 'auto' host input) marks a
// container port ODAC guessed rather than one the user or a recipe declared.
// Only a guess may be corrected by runtime auto-discovery.
//
// `proto` selects the transport, 'tcp' (the default) or 'udp'. It is absent
// on every entry that speaks TCP, and stays absent: the dashboard decides
// whether an app's ports changed by comparing their serialization, so
// stamping a cosmetic proto:'tcp' would mark every app on every server as
// changed once and rewrite the column for nothing.
package ports

import (
	"math"
	"strings"
)

// Proxy is the sentinel `host` value marking an entry as reverse-proxy routed.
const Proxy = "proxy"

// Loopback is the bind address for a published-but-not-public entry.
const Loopback = "127.0.0.1"

// AllInterfaces is the bind address for a public entry. Empty means "every
// interface" to Docker, which resolves it to 0.0.0.0 plus [::] when the host
// has IPv6. Naming the families explicitly instead would fail container
// create on IPv6-less hosts.
const AllInterfaces = ""

// hostFalsy mirrors JS `!entry.host` on a decoded-JSON value: nil (absent),
// false, "", 0 and NaN are falsy.
func hostFalsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case float64:
		return x == 0 || math.IsNaN(x)
	case int:
		return x == 0
	}
	return false
}

// IsProxy ports Ports.isProxy: true when the entry is routed by the ODAC
// proxy rather than published. A missing `host` counts as proxy so configs
// written before the sentinel existed keep resolving correctly.
func IsProxy(entry map[string]any) bool {
	return entry != nil && (hostFalsy(entry["host"]) || entry["host"] == Proxy)
}

// IsPublished ports Ports.isPublished: true when the entry must be handed to
// Docker as a host port binding.
func IsPublished(entry map[string]any) bool {
	return entry != nil && !IsProxy(entry)
}

// IsPublic ports Ports.isPublic: true when the entry is published on every
// interface rather than loopback. Proxy-routed entries are never public
// (they have no host binding at all), and only the exact boolean true opts in.
func IsPublic(entry map[string]any) bool {
	return IsPublished(entry) && entry["public"] == true
}

// BindIP ports Ports.bindIp: the HostIp to hand Docker for a published entry.
func BindIP(entry map[string]any) string {
	if IsPublic(entry) {
		return AllInterfaces
	}
	return Loopback
}

// IsAuto ports Ports.isAuto: true when ODAC guessed this entry's container
// port instead of being told it. Only a guess may be rewritten by runtime
// auto-discovery.
func IsAuto(entry map[string]any) bool {
	return entry != nil && entry["auto"] == true
}

// Transport protocols an entry may name. These are wire values: the Cloud
// sends them in app.create / app.port.set and reads them back in app.list.
const (
	// TCP is the default and is never persisted, see ParseProto.
	TCP = "tcp"
	// UDP publishes the entry as a datagram port. A UDP port is what makes
	// a VPN (WireGuard, OpenVPN) deployable: published as TCP it would
	// answer nothing at all.
	UDP = "udp"
)

// ParseProto coerces an entry's `proto` value, which may have crossed a JSON
// or form boundary. Absent, null and empty mean TCP, so every entry written
// before this field existed keeps resolving to exactly what it did. Returns
// ok=false for anything else: a mistyped protocol silently downgraded to TCP
// gives a VPN app a port that accepts connections and speaks nothing, which
// is worse than a refused edit.
func ParseProto(value any) (proto string, ok bool) {
	switch x := value.(type) {
	case nil:
		return TCP, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "":
			return TCP, true
		case TCP:
			return TCP, true
		case UDP:
			return UDP, true
		}
	}
	return "", false
}

// Proto is the canonical protocol of a persisted entry. Like the other
// readers here it never fails: only a hand-edited config can hold a value
// ParseProto rejects, and the run and route paths need a decision, so an
// unreadable value degrades to the default rather than blocking a start.
func Proto(entry map[string]any) string {
	if entry == nil {
		return TCP
	}
	if proto, ok := ParseProto(entry["proto"]); ok {
		return proto
	}
	return TCP
}

// IsUDP reports whether the entry is published as a datagram port.
func IsUDP(entry map[string]any) bool {
	return Proto(entry) == UDP
}

// Primary ports Ports.primary: the entry the reverse proxy routes, and the
// app's main container port — the first proxy-routed entry, else the first
// TCP entry, else the first entry (an app may publish a port and also sit
// behind the proxy, and the dashboard does not guarantee an order between
// the two). Nil when there are no entries.
//
// The TCP step exists because everything downstream of Primary speaks TCP:
// the proxy dials this address for HTTP, and the port poller probes it. An
// app whose first entry is its UDP tunnel (WireGuard) would otherwise hand
// the proxy a port that cannot answer a single request. Sets that are all
// TCP resolve exactly as they did before the step existed.
func Primary(portList []any) map[string]any {
	for _, p := range portList {
		if pm, _ := p.(map[string]any); IsProxy(pm) {
			return pm
		}
	}
	for _, p := range portList {
		if pm, _ := p.(map[string]any); pm != nil && !IsUDP(pm) {
			return pm
		}
	}
	if len(portList) == 0 {
		return nil
	}
	pm, _ := portList[0].(map[string]any)
	return pm
}

// Discovered ports Ports.discovered: builds the entry for a container port
// ODAC inferred (image EXPOSE, or the 3000 default). Centralized so a guess
// can never be persisted without its `auto` marker, which is what keeps
// auto-discovery off declared ports. The container port is stored as float64
// so the entry round-trips through config JSON like a Node-written one.
func Discovered(containerPort int) map[string]any {
	return map[string]any{"host": Proxy, "container": float64(containerPort), "auto": true}
}

// ParsePublic ports Ports.parsePublic: coerces a `public` flag that may have
// crossed a JSON or form boundary (the dashboard may serialize the checkbox
// as a string, so "false" must not read as truthy). Returns the flag plus
// ok=false where Node returns null: the value is not a boolean at all, and a
// public entry reaches the internet so the caller must fail loudly rather
// than fall back to either interpretation.
func ParsePublic(value any) (isPublic, ok bool) {
	switch x := value.(type) {
	case nil:
		return false, true
	case bool:
		return x, true
	case string:
		switch x {
		case "":
			return false, true
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// Normalize ports Ports.normalize: rewrites legacy entries in place to the
// canonical shape. Entries persisted before the sentinel omit `host`
// entirely; they get the sentinel plus the `auto` guess marker — no shipped
// surface let a proxy container port be chosen by hand, so every legacy one
// was inferred by ODAC, and marking them keeps auto-discovery correcting them
// exactly as it did before the marker existed. Only entries loaded from disk
// pass through here, so a port the user sets from now on is never marked.
func Normalize(portList []any) []any {
	for _, p := range portList {
		if pm, _ := p.(map[string]any); pm != nil && hostFalsy(pm["host"]) {
			pm["host"] = Proxy
			pm["auto"] = true
		}
	}
	return portList
}
