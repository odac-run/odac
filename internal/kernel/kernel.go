// Package kernel is the single source of truth for the kernel-level knobs an
// app may ask for: extra Linux capabilities (`caps`) and namespaced sysctls
// (`sysctls`). Both arrive on the app.create payload — at the root for a
// single app, per sub-app under `apps.<key>` for a template — and are
// persisted verbatim on the app record.
//
// It is deliberately dependency-free: appmgr validates and persists a
// request with it, docker translates it into container HostConfig, and the
// network-mode guard asks it which sysctls belong to the network namespace.
// One vocabulary, three consumers, no drift.
//
// Both fields are allowlisted, and the allowlist is the whole point. A
// capability is host privilege handed to a container: NET_ADMIN lets an app
// build tunnels and rewrite its namespace's routing (what WireGuard needs),
// while SYS_ADMIN or SYS_MODULE would hand it the host itself. A sysctl
// outside Docker's namespaced set is a host-wide kernel setting the daemon
// refuses to set per container. Anything not named here is refused at the
// door rather than passed through and diagnosed by the daemon: the app
// manager respawns a dead daemon-refused container on every check tick, so a
// refusal that reaches Docker becomes a restart loop instead of an error.
//
// This mirrors the server-side allowlist rather than replacing it. The
// authority is the Cloud; this is the second layer, and the two must carry
// the same formula.
package kernel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Capability names ODAC will add to a container. Each is namespaced (it
// applies to the container, not the host) and each has a workload behind it:
//
//   - NET_ADMIN: interface, routing and netfilter control inside the
//     container's own network namespace. WireGuard, OpenVPN, Tailscale.
//   - NET_RAW: raw and packet sockets, i.e. ping and traceroute.
//   - NET_BIND_SERVICE: bind a port below 1024 as a non-root user.
//   - IPC_LOCK: mlock memory so secrets never reach swap (Vault, Redis).
//   - SYS_NICE: change scheduling priority of the app's own threads.
//
// Deliberately absent: SYS_ADMIN, SYS_MODULE, SYS_RAWIO, SYS_PTRACE,
// SYS_TIME, DAC_READ_SEARCH and friends. Each of those crosses the container
// boundary — loading a kernel module or reading arbitrary host memory is
// root on the host, whatever the container thinks it is.
const (
	CapNetAdmin       = "NET_ADMIN"
	CapNetRaw         = "NET_RAW"
	CapNetBindService = "NET_BIND_SERVICE"
	CapIPCLock        = "IPC_LOCK"
	CapSysNice        = "SYS_NICE"
)

// allowedCaps is the closed set. Adding to it is a security decision, not a
// convenience one: mirror the server-side list, never widen past it.
var allowedCaps = map[string]bool{
	CapNetAdmin:       true,
	CapNetRaw:         true,
	CapNetBindService: true,
	CapIPCLock:        true,
	CapSysNice:        true,
}

// NetSysctlPrefix marks the sysctls that live in the network namespace.
// They are the ones a host-networked container may not set: the namespace
// they would configure is the host's own, so the daemon refuses the create
// outright rather than let a container retune the host's TCP stack.
const NetSysctlPrefix = "net."

// allowedSysctlPrefixes mirrors Docker's namespaced sysctl set: everything
// under net.*, the System V IPC knobs, and the POSIX message queue ones.
// A sysctl outside this set is host-wide, and the daemon rejects it per
// container — allowing it here would only move the failure to container
// create, which respawns.
var allowedSysctlPrefixes = []string{
	NetSysctlPrefix,
	"kernel.msg",
	"kernel.sem",
	"kernel.shm",
	"fs.mqueue.",
}

// Limits bound an untrusted payload. Neither field has a legitimate use at
// these sizes; the caps set is five entries long and no app tunes dozens of
// sysctls.
const (
	maxCaps        = 16
	maxSysctls     = 32
	maxSysctlValue = 128
)

// Spec is a validated kernel request attached to an app. A nil Spec means
// "nothing requested", which is what almost every app is: it must translate
// to the exact same container config as before this field existed.
type Spec struct {
	// Caps are canonical capability names without the CAP_ prefix, sorted
	// and deduplicated so a re-parse of a persisted record is byte-stable.
	Caps []string
	// Sysctls are validated name/value pairs, values rendered as the strings
	// the daemon takes.
	Sysctls map[string]string
}

// Parse validates the `caps` and `sysctls` members of an app definition (an
// app.create payload, a recipe, a template sub-app, or a persisted app
// record). A definition declaring neither yields (nil, nil): the app is
// untouched by this feature and its container config must stay identical.
//
// Anything present but malformed IS an error. Dropping a capability an app
// was created with silently produces a container that starts and then fails
// at its first socket operation, which is far harder to diagnose than a
// refused create.
func Parse(def any) (*Spec, error) {
	raw, ok := def.(map[string]any)
	if !ok || raw == nil {
		return nil, nil
	}
	caps, err := parseCaps(raw["caps"])
	if err != nil {
		return nil, err
	}
	sysctls, err := parseSysctls(raw["sysctls"])
	if err != nil {
		return nil, err
	}
	if len(caps) == 0 && len(sysctls) == 0 {
		return nil, nil
	}
	return &Spec{Caps: caps, Sysctls: sysctls}, nil
}

// parseCaps validates the `caps` member: an array of capability names, with
// or without the CAP_ prefix and in any case.
func parseCaps(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("caps must be an array of capability names")
	}
	if len(list) > maxCaps {
		return nil, fmt.Errorf("too many caps: %d requested, %d is the maximum", len(list), maxCaps)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("caps must be an array of capability names")
		}
		name = strings.ToUpper(strings.TrimSpace(name))
		name = strings.TrimPrefix(name, "CAP_")
		if name == "" {
			return nil, fmt.Errorf("caps must be an array of capability names")
		}
		if !allowedCaps[name] {
			return nil, fmt.Errorf("capability %s is not allowed, pick from: %s", name, strings.Join(AllowedCaps(), ", "))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Strings(out)
	return out, nil
}

// parseSysctls validates the `sysctls` member: an object of name/value pairs
// whose values may cross a JSON boundary as strings, numbers or booleans.
func parseSysctls(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("sysctls must be an object of name/value pairs")
	}
	if len(raw) > maxSysctls {
		return nil, fmt.Errorf("too many sysctls: %d requested, %d is the maximum", len(raw), maxSysctls)
	}
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		key := strings.TrimSpace(name)
		if !validSysctlName(key) {
			return nil, fmt.Errorf("invalid sysctl name: %s", name)
		}
		if !sysctlAllowed(key) {
			return nil, fmt.Errorf("sysctl %s is not allowed, only these are namespaced per container: %s", key, strings.Join(allowedSysctlPrefixes, ", "))
		}
		str, ok := scalarString(value)
		if !ok {
			return nil, fmt.Errorf("invalid value for sysctl %s: expected a string, number or boolean", key)
		}
		if str == "" || len(str) > maxSysctlValue || hasControlChar(str) {
			return nil, fmt.Errorf("invalid value for sysctl %s", key)
		}
		out[key] = str
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// AllowedCaps lists the capability allowlist, sorted, for error messages and
// documentation.
func AllowedCaps() []string {
	out := make([]string, 0, len(allowedCaps))
	for name := range allowedCaps {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// IsNetSysctl reports whether a sysctl configures the network namespace.
func IsNetSysctl(name string) bool {
	return strings.HasPrefix(name, NetSysctlPrefix)
}

// NetSysctls lists the spec's network-namespace sysctls, sorted. Empty for a
// nil spec, so callers can ask without a nil check.
func (s *Spec) NetSysctls() []string {
	if s == nil {
		return nil
	}
	var out []string
	for name := range s.Sysctls {
		if IsNetSysctl(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// WithoutNetSysctls returns the spec with its network-namespace sysctls
// removed, or nil when nothing is left. Host networking is the caller: the
// container joins the host's network namespace, so those settings would
// retune the host and the daemon refuses the create. The IPC and mqueue
// sysctls stay, they are unaffected by the network namespace.
func (s *Spec) WithoutNetSysctls() *Spec {
	if s == nil {
		return nil
	}
	kept := map[string]string{}
	for name, value := range s.Sysctls {
		if !IsNetSysctl(name) {
			kept[name] = value
		}
	}
	if len(s.Caps) == 0 && len(kept) == 0 {
		return nil
	}
	return &Spec{Caps: s.Caps, Sysctls: kept}
}

// Apply stamps the spec onto an app record, and clears the fields when there
// is nothing to stamp. Absent means "no request": an app that never asked
// for a capability must not grow an empty `caps` array, because the Cloud
// compares app.list rows by serialization and a cosmetic field rewrites the
// column for every app on every server.
func (s *Spec) Apply(app map[string]any) {
	if s == nil || len(s.Caps) == 0 {
		delete(app, "caps")
	} else {
		caps := make([]any, len(s.Caps))
		for i, c := range s.Caps {
			caps[i] = c
		}
		app["caps"] = caps
	}
	if s == nil || len(s.Sysctls) == 0 {
		delete(app, "sysctls")
		return
	}
	sysctls := make(map[string]any, len(s.Sysctls))
	for k, v := range s.Sysctls {
		sysctls[k] = v
	}
	app["sysctls"] = sysctls
}

// validSysctlName bounds a sysctl name to the character set the kernel uses
// for its /proc/sys paths. It also blocks the traversal shapes (an empty
// segment, a leading or trailing dot) that a path-joining daemon could read
// as something other than a sysctl.
func validSysctlName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, segment := range strings.Split(name, ".") {
		if segment == "" {
			return false
		}
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			default:
				return false
			}
		}
	}
	return true
}

// sysctlAllowed reports whether a validated name is in Docker's namespaced
// set.
func sysctlAllowed(name string) bool {
	for _, prefix := range allowedSysctlPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// scalarString renders a decoded-JSON scalar the way the daemon wants it:
// sysctl values are strings on the wire, but a dashboard that sends
// `{'net.ipv4.ip_forward': 1}` means the same thing as '1'. An integral
// float64 must not become "1e+00" or "1.000000".
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), true
	case bool:
		if x {
			return "1", true
		}
		return "0", true
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	}
	return "", false
}

// hasControlChar reports whether s carries a byte the daemon's JSON body has
// no business holding.
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
