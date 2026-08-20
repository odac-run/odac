// Package netmode is the single source of truth for an app's container
// network mode: the wire values the Cloud and CLI send, and the parser that
// turns an untrusted payload value into a canonical mode.
//
// It is deliberately dependency-free because the mode is read at three
// unrelated layers: appmgr persists it, docker translates it into container
// HostConfig, and the proxy data plane picks a backend address from it. One
// vocabulary, three consumers, no drift.
package netmode

import (
	"fmt"
	"strings"
)

// Modes. These are wire values — the Cloud sends them in app.network.mode and
// reads them back in app.list, so they may not be renamed casually.
const (
	// Bridge is the default: the container joins ODAC's shared user-defined
	// bridge and is reachable by container IP.
	Bridge = "bridge"
	// Host shares the host's network namespace. The container has no IP of
	// its own and no port isolation — it binds host ports directly.
	Host = "host"
)

// Deliberately NOT a mode: egress isolation. An app with no outbound access
// is still a bridge app — same container IP, same port namespace, same proxy
// routing — it just sits on a Docker `internal` bridge instead of the shared
// one. Modelling it as a third mode would conflate two orthogonal axes (which
// namespace vs whether egress is allowed) and cost every consumer the
// "isolated implies bridge-routed" rule. It travels as its own boolean; see
// docker.RunOptions.Isolated.

// LoopbackAddr is where a host-mode app answers. ODAC itself runs with
// network_mode: host (see docker-compose.yml), so the orchestrator and the
// proxy sit in the same namespace the app binds into — loopback reaches it,
// and unlike a bridge IP it never changes across restarts.
const LoopbackAddr = "127.0.0.1"

// Parse normalizes an untrusted payload value into a canonical mode. An
// absent, empty or nil value means Bridge, so existing apps keep their
// behaviour without a config migration. "default" is accepted as a Bridge
// alias because that is Docker's own name for it.
func Parse(v any) (string, error) {
	if v == nil {
		return Bridge, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("network mode must be a string, got %T", v)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", Bridge, "default":
		return Bridge, nil
	case Host:
		return Host, nil
	default:
		return "", fmt.Errorf("invalid network mode %q, expected %q or %q", s, Bridge, Host)
	}
}

// IsHost reports whether a persisted value selects host networking. It
// swallows the parse error on purpose: callers on the run/route hot paths
// need a decision, not a diagnostic, and an unparseable value is treated as
// the safe default (the shared bridge).
func IsHost(v any) bool {
	mode, err := Parse(v)
	return err == nil && mode == Host
}
