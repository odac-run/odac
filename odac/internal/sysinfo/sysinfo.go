// Package sysinfo ports the half of server/src/System/Info.js the Hub
// consumes: getSystemInfo() (the `auth` request body and the `system.info`
// task payload), getLinuxDistro() and detectHostPlatform(). The other half
// (getStatus/getCpuUsage/getDiskUsage/getNetworkUsage) has NO live caller in
// Node — System.status() is never invoked — and is deliberately not ported
// (jest exercised it directly; the Go spec covers what ships).
//
// Deviations from Node (deliberate, all inventory-display fields):
//   - `node` reports the Go runtime version (runtime.Version()) instead of
//     process.version — it is honest inventory for the same dashboard slot.
//   - `version` is an embedded constant drift-guarded against package.json
//     (a binary has no __dirname to require it from), same pattern as the
//     2.4 locale catalogs.
//   - memory/load/uptime/cpu model come from /proc (Linux) or sysctl
//     (darwin) instead of libuv; non-Linux gaps degrade to zeros. The
//     production host is Linux (Docker image); darwin/windows builds serve
//     development.
package sysinfo

import (
	"os"
	"runtime"
	"strings"

	"odac/internal/jscanon"
)

// Version mirrors package.json's "version"; TestVersionMatchesPackageJSON
// fails on drift.
const Version = "1.10.1"

// Info assembles system inventory. containerEngine reports Docker
// availability (Node: Odac.server('Container').available).
type Info struct {
	containerEngine func() bool
}

// New builds an Info. engine may be nil (reports false).
func New(engine func() bool) *Info {
	return &Info{containerEngine: engine}
}

// Get ports getSystemInfo(): an insertion-ordered object matching Node's
// literal key order (arch … version, then the conditional distro).
func (i *Info) Get() jscanon.Obj {
	hostname, _ := os.Hostname()
	engine := false
	if i.containerEngine != nil {
		engine = i.containerEngine()
	}
	distro := LinuxDistro()
	platform := HostPlatform()
	totalKB, freeKB := memoryKB()
	l1, l2, l3 := loadAvg()
	release := kernelRelease()

	info := jscanon.Obj{
		{K: "arch", V: nodeArch(runtime.GOARCH)},
		{K: "container_engine", V: engine},
		{K: "cpu", V: jscanon.Obj{
			{K: "count", V: runtime.NumCPU()},
			{K: "model", V: cpuModel()},
		}},
		{K: "hostname", V: hostname},
		{K: "load", V: []any{l1, l2, l3}},
		{K: "memory", V: jscanon.Obj{
			{K: "total", V: totalKB},
			{K: "free", V: freeKB},
		}},
		{K: "node", V: runtime.Version()},
		{K: "platform", V: platform},
		{K: "release", V: release},
		{K: "uptime", V: uptimeSeconds()},
		{K: "version", V: Version},
	}

	if distro != nil {
		name, _ := distro["name"].(string)
		version, _ := distro["version"].(string)
		info = append(info, jscanon.Field{K: "distro", V: jscanon.Obj{
			{K: "name", V: distro["name"]},
			{K: "version", V: distro["version"]},
			{K: "id", V: distro["id"]},
		}})
		// Node overwrites release AFTER building the literal; key position
		// is unchanged, only the value.
		for idx := range info {
			if info[idx].K == "release" {
				info[idx].V = name + " " + version
			}
		}
	}
	return info
}

// HostPlatform ports detectHostPlatform(): the real host OS in Node's
// os.platform() vocabulary, seen through container walls (Docker Desktop's
// linuxkit VM, WSL).
func HostPlatform() string {
	if runtime.GOOS != "linux" {
		return nodeOS(runtime.GOOS)
	}

	release := strings.ToLower(kernelRelease())
	if strings.Contains(release, "microsoft") || strings.Contains(release, "wsl") {
		return "win32"
	}
	if !strings.Contains(release, "linuxkit") {
		return "linux"
	}

	// linuxkit kernel ⇒ Docker Desktop. Disambiguate Mac vs Windows via the
	// bind-mount filesystems Docker Desktop sets up for the host share.
	if mounts, err := os.ReadFile("/proc/mounts"); err == nil {
		s := string(mounts)
		if containsWord(s, "osxfs") || containsWord(s, "virtiofs") ||
			containsWord(s, "grpcfuse") || strings.Contains(s, "fuse.osxfs") {
			return "darwin"
		}
		if containsWord(s, "drvfs") || strings.Contains(s, "/run/desktop/mnt/host/") {
			return "win32"
		}
	}

	// DMI fingerprint as a last resort — requires privileged container.
	if vendor, err := os.ReadFile("/sys/class/dmi/id/sys_vendor"); err == nil {
		v := strings.ToLower(strings.TrimSpace(string(vendor)))
		if strings.Contains(v, "apple") {
			return "darwin"
		}
		if strings.Contains(v, "microsoft") {
			return "win32"
		}
	}
	return "linux"
}

// containsWord mirrors the \b…\b regexes in Node's mount-table checks.
func containsWord(s, word string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], word)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(word)
		beforeOK := start == 0 || !isWordChar(s[start-1])
		afterOK := end == len(s) || !isWordChar(s[end])
		if beforeOK && afterOK {
			return true
		}
		idx = start + 1
	}
}

func isWordChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// osReleaseCandidates is a test seam; production order per Node: an explicit
// bind mount, the pid-host path, then the container's own file.
var osReleaseCandidates = []string{"/host/etc/os-release", "/proc/1/root/etc/os-release", "/etc/os-release"}

// LinuxDistro ports getLinuxDistro(): nil on non-Linux hosts or when no
// candidate file yields NAME/ID.
func LinuxDistro() map[string]any {
	if HostPlatform() != "linux" {
		return nil
	}
	return parseOSRelease(osReleaseCandidates)
}

// parseOSRelease walks the candidate files and returns the first parseable
// {name, version, id}, or nil.
func parseOSRelease(candidates []string) map[string]any {
	for _, candidate := range candidates {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		fields := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found && key != "" && value != "" {
				fields[key] = strings.ReplaceAll(value, `"`, "")
			}
		}
		if fields["NAME"] == "" && fields["ID"] == "" {
			continue
		}
		name := fields["NAME"]
		if name == "" {
			name = fields["ID"]
		}
		version := fields["VERSION_ID"]
		if version == "" {
			version = fields["VERSION"]
		}
		if version == "" {
			version = "Unknown"
		}
		id := fields["ID"]
		if id == "" {
			id = "unknown"
		}
		return map[string]any{"name": name, "version": version, "id": id}
	}
	return nil
}

// nodeOS / nodeArch map Go vocabulary to Node's (same tables as
// internal/system; duplicated to keep this leaf package dependency-free).
func nodeOS(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos
}

func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return goarch
	}
}
