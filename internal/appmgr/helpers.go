package appmgr

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"odac/internal/ports"
)

func itoa(n int) string { return strconv.Itoa(n) }

// jsString mirrors `${v}` / String(v) on decoded-JSON values (same table as
// dataplane's helper): numbers render without a trailing ".0", arrays join
// with ",", objects become "[object Object]".
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				continue
			}
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return fmt.Sprintf("%v", v)
}

// jsTruthy mirrors JS truthiness on decoded-JSON values. Note that an empty
// map/array IS truthy (JS objects always are) — the env new-structure
// detection (`envConfig.manual || Array.isArray(envConfig.linked)`) depends
// on `manual: {}` counting as present.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 // NaN cannot arrive via JSON
	case int:
		return x != 0
	}
	return true // maps, slices, everything else
}

// jsNumber mirrors JS Number(v) for the values API payloads carry. ok=false
// is NaN.
func jsNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true // Number(null) === 0; undefined is screened by callers
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, true
		}
		n, err := strconv.ParseFloat(s, 64)
		return n, err == nil
	}
	return 0, false
}

// isInt reports whether f is a JS-safe integer (Number.isInteger).
func isInt(f float64) bool { return f == float64(int64(f)) }

// generateRuntimeID ports #generateRuntimeId: `[prefix_]<ms>_<8-hex>`.
func generateRuntimeID(prefix string) string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	suffix := strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + hex.EncodeToString(buf)
	if prefix != "" {
		return prefix + "_" + suffix
	}
	return suffix
}

// generatePassword ports Create's #generatePassword: hex of ceil(n/2) random
// bytes, sliced to n chars.
func generatePassword(length int) string {
	if length <= 0 {
		length = 16
	}
	buf := make([]byte, (length+1)/2)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:length]
}

// gitMetadata ports #getGitMetadata: provider + user/repo extraction with
// strict hostname matching (subdomain-safe, path-spoof-safe).
func gitMetadata(rawURL string) (repo, provider string) {
	provider = "git"
	if rawURL == "" {
		return "", provider
	}

	hostname := ""
	if strings.Contains(rawURL, "://") {
		if u, err := url.Parse(rawURL); err == nil {
			hostname = strings.ToLower(u.Hostname())
			pathname := strings.TrimSuffix(u.Path, "/")
			parts := strings.Split(pathname, "/")
			if len(parts) >= 2 {
				repoPart := parts[len(parts)-1]
				userPart := parts[len(parts)-2]
				repo = userPart + "/" + repoPart
			}
		} else {
			repo = rawURL
		}
	} else if strings.Contains(rawURL, "@") {
		parts := strings.SplitN(rawURL, "@", 2)
		hostPart := strings.SplitN(parts[1], ":", 2)
		hostname = strings.ToLower(hostPart[0])
		if len(hostPart) > 1 {
			repo = hostPart[1]
		}
	}

	if strings.HasSuffix(repo, ".git") {
		repo = repo[:len(repo)-4]
	} else if repo == "" && hostname == "" {
		repo = rawURL // fallback for local paths or malformed input
	}

	for domain, name := range map[string]string{
		"bitbucket.org": "bitbucket",
		"github.com":    "github",
		"gitlab.com":    "gitlab",
	} {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			provider = name
			break
		}
	}
	return repo, provider
}

// hasIllegalURLChars ports the shell-metacharacter screen shared by
// createFromGit and redeploy.
func hasIllegalURLChars(s string) bool {
	return strings.ContainsAny(s, ";&|`$(){}<>\n\r")
}

// validGitURL ports the protocol allow-list (https?|git|ssh|ftps?|rsync)://
// or scp-like user@host: syntax.
func validGitURL(rawURL string, allowFtpRsync bool) bool {
	schemes := []string{"http://", "https://", "git://", "ssh://"}
	if allowFtpRsync {
		schemes = append(schemes, "ftp://", "ftps://", "rsync://")
	}
	for _, s := range schemes {
		if strings.HasPrefix(rawURL, s) {
			return true
		}
	}
	return scpLikeURL.MatchString(rawURL)
}

// validBranch rejects git argument injection (--upload-pack) and shell
// metacharacters. An empty branch is valid (clone default).
func validBranch(branch string) bool {
	return branch == "" || (!strings.HasPrefix(branch, "-") && !hasIllegalURLChars(branch))
}

// findPortCollision ports #findPortCollision: reject sets Docker or the
// Proxy would resolve ambiguously. Returns a localized message or "".
func findPortCollision(portList []map[string]any) string {
	seenHost := map[float64]bool{}
	proxyCount := 0

	for _, entry := range portList {
		if entry["host"] == ports.Proxy {
			if proxyCount++; proxyCount > 1 {
				return __("Only one port may be routed by the proxy. Publish the others on a host port.")
			}
			continue
		}
		// 'auto' resolves to a distinct free port per entry.
		if entry["host"] == "auto" {
			continue
		}
		hostPort, _ := jsNumber(entry["host"])
		if seenHost[hostPort] {
			return __("Duplicate host port: %s. Each host port may be bound once.", jsString(entry["host"]))
		}
		seenHost[hostPort] = true
	}
	return ""
}

// preparePorts ports #preparePorts: canonicalize entries before persisting —
// resolve 'auto' host ports, default an omitted host to the proxy sentinel,
// coerce ports to numbers, stamp `public` only when true.
func (m *Manager) preparePorts(recipePorts []map[string]any) []map[string]any {
	if recipePorts == nil {
		return []map[string]any{}
	}

	// Auto-assigned ports are not bound yet, so isPortInUse cannot see them.
	assigned := map[float64]bool{}
	prepared := make([]map[string]any, 0, len(recipePorts))

	for _, port := range recipePorts {
		var host any = ports.Proxy
		if h, present := port["host"]; present && h != nil {
			host = h
		}
		if host == "auto" {
			host = float64(m.findAvailablePort(30000, assigned))
		} else if host != ports.Proxy {
			host, _ = jsNumber(host)
		}

		if hostNum, ok := host.(float64); ok {
			assigned[hostNum] = true
		}

		containerNum, _ := jsNumber(port["container"])
		entry := map[string]any{"host": host, "container": containerNum}
		// Only stamp the flag when it is on: an absent `public` already
		// means loopback, so writing `false` would churn every config.
		if isPublic, ok := ports.ParsePublic(port["public"]); host != ports.Proxy && ok && isPublic {
			entry["public"] = true
		}
		prepared = append(prepared, entry)
	}
	return prepared
}

// findAvailablePort ports #findAvailablePort.
func (m *Manager) findAvailablePort(start int, assigned map[float64]bool) int {
	port := start
	for assigned[float64(port)] || isPortInUse(port) {
		port++
	}
	return port
}

// isPortInUse ports #isPortInUse: a bind probe on 127.0.0.1.
func isPortInUse(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}
