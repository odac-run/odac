package appmgr

import (
	"strings"

	"odac/internal/api"
)

// isNewEnvStructure ports the detection rule used everywhere:
// `envConfig.manual || Array.isArray(envConfig.linked)`. JS truthiness makes
// an empty manual map count as the new structure.
func isNewEnvStructure(envConfig map[string]any) bool {
	if envConfig == nil {
		return false
	}
	if jsTruthy(envConfig["manual"]) {
		return true
	}
	_, isArr := envConfig["linked"].([]any)
	return isArr
}

// getManualEnv ports #getManualEnv: the manual K/V pairs, handling both the
// legacy flat shape and the new {manual, linked} shape. The returned map is
// the live one (callers mutate it, Node-style).
func getManualEnv(envConfig map[string]any) map[string]any {
	if envConfig == nil {
		return map[string]any{}
	}
	if isNewEnvStructure(envConfig) {
		if manual, _ := envConfig["manual"].(map[string]any); manual != nil {
			return manual
		}
		return map[string]any{}
	}
	return envConfig
}

// sanitizeEnv ports #sanitizeEnv: mask values whose key matches the
// sensitive pattern (dev cd7f08b: cert|key|salt|secret|token, `pass` is NOT
// masked).
func sanitizeEnv(env map[string]any) map[string]any {
	sanitized := make(map[string]any, len(env))
	for key, value := range env {
		if isSensitiveKey(key) {
			sanitized[key] = "***"
		} else {
			sanitized[key] = value
		}
	}
	return sanitized
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, frag := range sensitiveKeys {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// resolveEnvLocked ports #resolveEnv: system defaults, then linked apps'
// manual envs, then the app's own manual envs (overriding). Caller holds
// cfg.View/Mutate (linked apps are resolved through the working set).
func (m *Manager) resolveEnvLocked(app map[string]any, includeSystem bool) map[string]any {
	finalEnv := map[string]any{}
	if includeSystem {
		finalEnv["HOST"] = "0.0.0.0"
		finalEnv["ODAC_APP"] = "true"
	}
	envConfig, _ := app["env"].(map[string]any)

	if isNewEnvStructure(envConfig) {
		if linked, _ := envConfig["linked"].([]any); linked != nil {
			for _, ln := range linked {
				linkName, _ := ln.(string)
				linkedApp := m.getLocked(linkName)
				if linkedApp == nil {
					continue
				}
				linkedEnvConfig, _ := linkedApp["env"].(map[string]any)
				for k, v := range getManualEnv(linkedEnvConfig) {
					finalEnv[k] = v
				}
			}
		}
	}

	for k, v := range getManualEnv(envConfig) {
		finalEnv[k] = v
	}
	return finalEnv
}

// envToStrings renders env values the way Node's template literals did when
// assembling Docker Env entries.
func envToStrings(env map[string]any) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = jsString(v)
	}
	return out
}

// GetEnv ports App.getEnv: structured env for the dashboard — manual and
// linked separated, sensitive values masked.
func (m *Manager) GetEnv(id any) *api.Result {
	var result *api.Result
	m.cfg.View(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}

		envConfig, _ := app["env"].(map[string]any)
		manual := sanitizeEnv(getManualEnv(envConfig))

		linked := []any{}
		if isNewEnvStructure(envConfig) {
			if linkedNames, _ := envConfig["linked"].([]any); linkedNames != nil {
				for _, ln := range linkedNames {
					name, _ := ln.(string)
					linkedApp := m.getLocked(name)
					if linkedApp == nil {
						continue
					}
					linkedEnvConfig, _ := linkedApp["env"].(map[string]any)
					linked = append(linked, map[string]any{
						"app": name,
						"env": sanitizeEnv(getManualEnv(linkedEnvConfig)),
					})
				}
			}
		}

		result = res(true, map[string]any{"manual": manual, "linked": linked})
	})
	return result
}

// DeleteEnv ports App.deleteEnv: batch-remove keys from the manual envs.
func (m *Manager) DeleteEnv(id any, keys []string) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}
		if len(keys) == 0 {
			result = res(false, __("Invalid keys payload. Expected a non-empty array."))
			return
		}

		envConfig, _ := app["env"].(map[string]any)
		isNew := isNewEnvStructure(envConfig)
		manual := getManualEnv(envConfig)

		removed := 0
		for _, key := range keys {
			if _, ok := manual[key]; ok {
				delete(manual, key)
				removed++
			}
		}

		if isNew {
			envConfig["manual"] = manual
		} else {
			app["env"] = map[string]any{"manual": manual, "linked": []any{}}
		}

		m.saveAppsLocked()
		result = res(true, __("Removed %d key(s) from %s. Restart required to apply.", removed, app["name"]))
	})
	return result
}

// LinkEnv ports App.linkEnv: link another app's manual envs to this app.
func (m *Manager) LinkEnv(id any, target string) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}
		if target == "" {
			result = res(false, __("Invalid target. Expected an app name."))
			return
		}
		if app["name"] == target {
			result = res(false, __("Cannot link an app to itself."))
			return
		}
		if m.getLocked(target) == nil {
			result = res(false, __("Target app %s not found.", target))
			return
		}

		envConfig, _ := app["env"].(map[string]any)
		if isNewEnvStructure(envConfig) {
			linked, _ := envConfig["linked"].([]any)
			found := false
			for _, ln := range linked {
				if ln == target {
					found = true
					break
				}
			}
			if !found {
				linked = append(linked, target)
			}
			envConfig["linked"] = linked
		} else {
			var manual any = map[string]any{}
			if envConfig != nil {
				manual = envConfig
			}
			app["env"] = map[string]any{"manual": manual, "linked": []any{target}}
		}

		m.saveAppsLocked()
		result = res(true, __("Linked %s to %s. Restart required to apply.", target, app["name"]))
	})
	return result
}

// SetEnv ports App.setEnv: merge K/V pairs into the manual envs (migrating
// the legacy flat shape in place).
func (m *Manager) SetEnv(id any, env map[string]any) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}
		if env == nil {
			result = res(false, __("Invalid env payload. Expected an object."))
			return
		}

		envConfig, _ := app["env"].(map[string]any)
		if isNewEnvStructure(envConfig) {
			manual, _ := envConfig["manual"].(map[string]any)
			merged := map[string]any{}
			for k, v := range manual {
				merged[k] = v
			}
			for k, v := range env {
				merged[k] = v
			}
			envConfig["manual"] = merged
		} else {
			// Migrate legacy flat env to structured format.
			merged := map[string]any{}
			for k, v := range envConfig {
				merged[k] = v
			}
			for k, v := range env {
				merged[k] = v
			}
			app["env"] = map[string]any{"manual": merged, "linked": []any{}}
		}

		m.saveAppsLocked()
		result = res(true, __("Environment updated for %s. Restart required to apply.", app["name"]))
	})
	return result
}

// UnlinkEnv ports App.unlinkEnv: remove an app from the linked list.
func (m *Manager) UnlinkEnv(id any, target string) *api.Result {
	var result *api.Result
	m.cfg.Mutate(func() {
		app := m.getLocked(id)
		if app == nil {
			result = res(false, __("App %s not found.", jsString(id)))
			return
		}
		if target == "" {
			result = res(false, __("Invalid target. Expected an app name."))
			return
		}

		envConfig, _ := app["env"].(map[string]any)
		var linked []any
		if envConfig != nil {
			linked, _ = envConfig["linked"].([]any)
		}

		found := false
		for _, ln := range linked {
			if ln == target {
				found = true
				break
			}
		}
		if !found {
			result = res(false, __("App %s is not linked to %s.", target, app["name"]))
			return
		}

		filtered := make([]any, 0, len(linked)-1)
		for _, ln := range linked {
			if ln != target {
				filtered = append(filtered, ln)
			}
		}
		envConfig["linked"] = filtered

		m.saveAppsLocked()
		result = res(true, __("Unlinked %s from %s. Restart required to apply.", target, app["name"]))
	})
	return result
}
