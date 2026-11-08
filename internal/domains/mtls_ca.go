package domains

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"

	"odac/internal/api"
)

// A CA no record trusts opens nothing, but its key can still sign, and trusting
// the CA again would bring back everything it signed. So a CA leaves with its
// key: automatically when Set drops it from the last trust list, or through
// RemoveCA. Neither removes one whose certificates are still valid unless the
// operator forces it, because a key cannot be brought back.

var caIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// referencedCAsLocked returns every CA a record or host override lists, whatever
// its mode: a record turned off keeps its list, so turning it on again reuses the
// CA. Caller holds the config lock.
func referencedCAsLocked(domains map[string]any) map[string]bool {
	out := map[string]bool{}
	for name := range domains {
		cfg := recordConfig(domains, name)
		if cfg == nil {
			continue
		}
		for _, id := range stringList(cfg["ca"]) {
			out[id] = true
		}
		hosts, _ := cfg["hosts"].(map[string]any)
		for _, h := range hosts {
			override, _ := h.(map[string]any)
			for _, id := range stringList(override["ca"]) {
				out[id] = true
			}
		}
	}
	return out
}

func (m *MTLS) referencedCAs() map[string]bool {
	var out map[string]bool
	m.cfg.View(func() { out = referencedCAsLocked(m.domainsLocked()) })
	return out
}

// retireDroppedLocked removes the CAs that were trusted somewhere before a change
// and nowhere after it. Only those: a CA imported and not bound yet was never in
// before, so a change elsewhere cannot take it. One with valid certificates is
// kept, so a mistaken --ca can be undone. It returns what the reply should say.
// Caller holds m.mu.
func (m *MTLS) retireDroppedLocked(before map[string]bool) []string {
	after := m.referencedCAs()
	var dropped []string
	m.cfg.View(func() {
		cas := m.casLocked(false)
		for id := range before {
			if _, exists := cas[id]; exists && !after[id] {
				dropped = append(dropped, id)
			}
		}
	})
	if len(dropped) == 0 {
		return nil
	}
	sort.Strings(dropped)
	store, err := m.certs()
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return []string{__("Kept client CA %s, which no domain trusts any more: the certificate store is unavailable.", strings.Join(dropped, ", "))}
	}
	now := float64(m.now().UnixMilli())
	var notes []string
	for _, id := range dropped {
		live, err := store.liveCount(id, now)
		if err == nil && live == 0 {
			err = m.retireCALocked(store, id, nil)
			if err == nil {
				notes = append(notes, __("Removed client CA %s, which no domain trusts any more.", id))
				continue
			}
		}
		if err != nil {
			m.log.Error("Client certificate store failed: %s", err.Error())
			notes = append(notes, __("Kept client CA %s, which no domain trusts any more: the certificate store is unavailable.", id))
			continue
		}
		notes = append(notes, __("Kept client CA %s, which no domain trusts any more, because %s valid certificate(s) were signed by it. Trust it again to bring them back, or remove it with: odac domain mtls ca remove --ca %s --force", id, live, id))
	}
	return notes
}

// retireCALocked removes a CA, the certificates it signed (revocations stay, see
// retireCA), its key and its per-host issuers, and drops it from the trust lists
// of dormant, records turned off that still name it. The certificates go first,
// so a failure leaves the CA in place rather than records under a missing CA.
// Caller holds m.mu.
func (m *MTLS) retireCALocked(store *certStore, id string, dormant []string) error {
	if err := store.retireCA(id); err != nil {
		return err
	}
	m.cfg.Mutate(func() {
		domains := m.domainsLocked()
		for _, name := range dormant {
			record, _ := domains[name].(map[string]any)
			cfg, _ := record["mtls"].(map[string]any)
			if cfg == nil {
				continue
			}
			kept := []any{}
			for _, ca := range stringList(cfg["ca"]) {
				if ca != id {
					kept = append(kept, ca)
				}
			}
			if len(kept) == 0 {
				delete(record, "mtls")
			} else {
				cfg["ca"] = kept
			}
		}
		if len(dormant) > 0 {
			m.cfg.Touch("domains")
		}
		delete(m.casLocked(false), id)
		m.cfg.Touch("mtls")
	})
	m.saveLocked()
	if err := os.Remove(m.caKeyPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Error("Failed to remove client CA key %s: %s", id, err.Error())
	}
	m.sweepIssuersLocked(store)
	m.log.Log("Removed client CA %s", id)
	return nil
}

// RemoveCA deletes a CA and its key. It is the operator's alone and only the
// local API registers it, like ImportCA. A CA still trusted on a host with a
// policy is refused, since that host would lose every certificate at once; a
// record turned off that still names it only loses the name. A CA with valid
// certificates needs opts.force, because nothing brings its key back.
func (m *MTLS) RemoveCA(caller api.Caller, idArg, optsArg any) api.Result {
	if caller.App != "" || !caller.Root {
		return permissionDenied()
	}
	id, _ := idArg.(string)
	id = strings.ToLower(strings.TrimSpace(id))
	if !caIDRe.MatchString(id) {
		return api.Res(false, __("Unknown CA."))
	}
	force := optionsOf(optsArg)["force"] == true

	m.mu.Lock()
	result, removed, recordsChanged := m.removeCALocked(id, force)
	m.mu.Unlock()

	if removed && m.proxy != nil {
		if recordsChanged {
			m.proxy.SyncConfig()
		} else {
			m.proxy.SyncClientCerts()
		}
	}
	return result
}

func (m *MTLS) removeCALocked(id string, force bool) (api.Result, bool, bool) {
	var exists bool
	var users, dormant []string
	m.cfg.View(func() {
		_, exists = m.casLocked(false)[id]
		domains := m.domainsLocked()
		for name := range domains {
			cfg := recordConfig(domains, name)
			if cfg == nil {
				continue
			}
			if contains(stringList(cfg["ca"]), id) {
				// With no mode and no overrides the list is all that is left.
				if cfg["mode"] == nil && cfg["hosts"] == nil {
					dormant = append(dormant, name)
				} else {
					users = append(users, name)
				}
			}
			hosts, _ := cfg["hosts"].(map[string]any)
			for host, h := range hosts {
				override, _ := h.(map[string]any)
				if contains(stringList(override["ca"]), id) {
					users = append(users, host)
				}
			}
		}
	})
	if !exists {
		return api.Res(false, __("Unknown CA.")), false, false
	}
	if len(users) > 0 {
		sort.Strings(users)
		return api.Res(false, __("CA %s is still trusted on %s. Trust another CA there first: odac domain mtls set -d <host> --mode <mode> --ca <id>", id, strings.Join(users, ", "))), false, false
	}

	store, err := m.certs()
	var live int
	if err == nil {
		live, err = store.liveCount(id, float64(m.now().UnixMilli()))
	}
	if err != nil {
		m.log.Error("Client certificate store failed: %s", err.Error())
		return api.Res(false, __("The certificate store is unavailable.")), false, false
	}
	if live > 0 && !force {
		return api.Res(false, __("CA %s signed %s valid certificate(s), which stop working for good once it is removed. Add --force to remove it anyway.", id, live)), false, false
	}
	if err := m.retireCALocked(store, id, dormant); err != nil {
		return m.storeFailure(err), false, false
	}
	message := __("Removed client CA %s.", id)
	if live > 0 {
		message += " " + __("%s certificate(s) it signed no longer work.", live)
	}
	return api.Res(true, message, map[string]any{"id": id, "certificates": live}), true, len(dormant) > 0
}

// appendNotes adds sentences to a reply's message.
func appendNotes(result api.Result, notes []string) api.Result {
	if len(notes) == 0 {
		return result
	}
	message, _ := result.Message.(string)
	result.Message = strings.TrimSpace(message + " " + strings.Join(notes, " "))
	return result
}
