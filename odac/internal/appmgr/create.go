package appmgr

// Port of server/src/App/Create.js: app creation from Hub recipes, inline
// template payloads and git repositories.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"odac/internal/api"
	"odac/internal/ports"
)

var (
	templateVarRE = regexp.MustCompile(`\$\{([a-zA-Z0-9_]+)\.([a-zA-Z0-9_.]+)\}`)
	// createDispatchGitRE is the narrow protocol set the string-dispatch
	// uses (fromGit's own validation allows ftp/rsync too).
	createDispatchGitRE = regexp.MustCompile(`^(https?|git|ssh)://`)
	nonNameCharsRE      = regexp.MustCompile(`[^a-zA-Z0-9-]`)
)

// Create ports Create.create: dispatch on config shape. config is either a
// string (recipe name or git URL) or a decoded JSON object.
func (m *Manager) Create(config any) *api.Result {
	cfg, _ := config.(map[string]any)
	if str, ok := config.(string); ok {
		if createDispatchGitRE.MatchString(str) || scpLikeURL.MatchString(str) {
			base := strings.TrimSuffix(filepath.Base(str), ".git")
			var name string
			m.cfg.View(func() {
				name = m.generateUniqueNameLocked(nonNameCharsRE.ReplaceAllString(base, "-"))
			})
			cfg = map[string]any{"type": "git", "url": str, "name": name}
		} else {
			cfg = map[string]any{"type": "app", "app": str}
		}
	}

	if cfgJSON, err := json.Marshal(cfg); err == nil {
		m.clog.Log("Creating app: " + string(cfgJSON))
	}

	typ, _ := cfg["type"].(string)
	if typ == "" {
		return res(false, __("Missing config type"))
	}

	switch typ {
	case "app":
		return m.createFromRecipe(cfg)
	case "git", "github": // github is a legacy alias
		return m.createFromGit(cfg)
	case "template":
		return m.createFromTemplatePayload(cfg)
	default:
		return res(false, __("Unknown config type: %s", typ))
	}
}

// tryLockCreating claims a creation slot for name.
func (m *Manager) tryLockCreating(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.creating[name] {
		return false
	}
	m.creating[name] = true
	return true
}

func (m *Manager) unlockCreating(name string) {
	m.mu.Lock()
	delete(m.creating, name)
	m.mu.Unlock()
}

func (m *Manager) isCreating(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creating[name]
}

// createFromRecipe ports #fromRecipe: single-app Hub recipes ("mysql").
func (m *Manager) createFromRecipe(cfg map[string]any) *api.Result {
	appType, _ := cfg["app"].(string)
	customName, _ := cfg["name"].(string)

	if appType == "" {
		m.clog.Log("createFromRecipe: Missing app type")
		return res(false, __("Missing app type"))
	}

	m.clog.Log("createFromRecipe: Fetching recipe for %s", appType)

	if m.deps.Hub == nil {
		return res(false, __("Could not find recipe for %s: %s", appType, "Hub is not available"))
	}
	recipe, err := m.deps.Hub.GetApp(appType)
	if err != nil {
		m.clog.Error("createFromRecipe: Failed to fetch recipe: %s", err.Error())
		return res(false, __("Could not find recipe for %s: %s", appType, err.Error()))
	}

	// Template detection: multi-app stacks are delegated to the template
	// handler.
	recipeName, _ := recipe["name"].(string)
	if apps, _ := recipe["apps"].(map[string]any); len(apps) > 0 {
		baseName := customName
		if baseName == "" {
			m.cfg.View(func() { baseName = m.generateUniqueNameLocked(recipeName) })
		}
		return m.createFromTemplate(baseName, recipeName, apps, cfg)
	}

	name := customName
	exists := false
	m.cfg.View(func() {
		if name == "" {
			name = m.generateUniqueNameLocked(recipeName)
		}
		exists = m.getLocked(name) != nil
	})
	m.clog.Log("createFromRecipe: Using name: %s", name)

	if exists {
		m.clog.Log("createFromRecipe: App %s already exists", name)
		return res(false, __("App %s already exists", name))
	}
	if !m.tryLockCreating(name) {
		m.clog.Log("createFromRecipe: App %s is already being created", name)
		return res(false, __("App %s is already being created", name))
	}
	defer m.unlockCreating(name)

	// Register the logger SYNCHRONOUSLY to prevent races with Hub requests.
	logger := m.getLoggerInstance(name)
	m.deps.Docker.RegisterBuildLogger(name, logger)
	defer m.deps.Docker.UnregisterBuildLogger(name)

	appDir := filepath.Join(m.appsPath(), name)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return res(false, err.Error())
	}

	if err := logger.Init(); err != nil {
		return res(false, err.Error())
	}

	image, _ := recipe["image"].(string)
	logCtrl, err := logger.NewBuildStream(generateRuntimeID("build"), map[string]any{
		"image":    image,
		"strategy": "recipe-app",
	})
	if err != nil {
		return res(false, err.Error())
	}

	recipePorts := toMapSlice(recipe["ports"])
	recipeVolumes := toMapSlice(recipe["volumes"])
	userEnv, _ := cfg["env"].(map[string]any)

	configVolumes, err := m.writeConfigFiles(recipe["configs"], appDir)
	if err != nil {
		return res(false, err.Error())
	}

	var appID any
	m.cfg.Mutate(func() {
		app := map[string]any{
			"id":      m.getNextIDLocked(),
			"name":    name,
			"type":    "container",
			"image":   image,
			"cmd":     normalizeCmd(recipe["cmd"]),
			"ports":   toAnySlice(m.preparePorts(recipePorts)),
			"volumes": append(m.prepareVolumes(recipeVolumes, appDir), configVolumes...),
			"env":     m.mergeRecipeEnv(recipe, userEnv, name),
			"active":  true,
			"created": nowMs(),
			"status":  "installing",
		}
		appID = app["id"]
		m.apps = append(m.apps, app)
		m.saveAppsLocked()
	})

	m.clog.Log("createFromRecipe: Starting app...")
	if m.run(appID, logCtrl) {
		m.clog.Log("createFromRecipe: App started successfully")
		m.hubTrigger("app.list")
		logCtrl.Finalize(true)
		return res(true, __("App %s created successfully.", name))
	}

	failMsg := "Failed to start app container. Check logs for details."
	m.clog.Error("createFromRecipe: Failed to start app: %s", failMsg)
	m.cfg.Mutate(func() {
		filtered := m.apps[:0]
		for _, a := range m.apps {
			if a["id"] == appID {
				continue
			}
			filtered = append(filtered, a)
		}
		m.apps = filtered
		m.saveAppsLocked()
	})
	logCtrl.Finalize(false)
	return res(false, failMsg)
}

// createFromTemplatePayload ports #fromTemplatePayload: full template data
// inline from the Hub (type: 'template').
func (m *Manager) createFromTemplatePayload(cfg map[string]any) *api.Result {
	apps, _ := cfg["apps"].(map[string]any)
	name, _ := cfg["name"].(string)

	if len(apps) == 0 {
		return res(false, __("Invalid template: no apps defined."))
	}
	if name == "" {
		return res(false, __("Missing template name."))
	}

	hasCloudContainers := true
	for _, a := range apps {
		app, _ := a.(map[string]any)
		if app == nil || !jsTruthy(app["container"]) {
			hasCloudContainers = false
			break
		}
	}
	baseName := name
	if !hasCloudContainers {
		m.cfg.View(func() { baseName = m.generateUniqueNameLocked(name) })
	}
	return m.createFromTemplate(baseName, name, apps, cfg)
}

// createFromTemplate ports #fromTemplate: deploy a multi-app stack as one
// atomic operation — topological start order, generated secrets, ${...}
// interpolation, env.linked wiring, automatic rollback on partial failure.
func (m *Manager) createFromTemplate(baseName, recipeName string, templateApps map[string]any, cfg map[string]any) *api.Result {
	m.clog.Log("createFromTemplate: Starting template deployment for %s (%s)", baseName, recipeName)

	if len(templateApps) == 0 {
		return res(false, __("Template %s has no apps defined.", recipeName))
	}

	// Phase 1: dependency order (Kahn's topological sort).
	orderedKeys, err := resolveTemplateDependencies(templateApps)
	if err != nil {
		return res(false, err.Error())
	}
	if keysJSON, jerr := json.Marshal(orderedKeys); jerr == nil {
		m.clog.Log("createFromTemplate: Dependency order: " + string(keysJSON))
	}

	// Phase 2: container names — Cloud-provided or locally generated.
	nameMap := map[string]string{}
	var conflict *api.Result
	m.cfg.View(func() {
		for _, key := range orderedKeys {
			appDef, _ := templateApps[key].(map[string]any)
			containerName, _ := appDef["container"].(string)
			if containerName == "" {
				containerName = m.generateUniqueNameLocked(baseName + "-" + key)
			}
			if m.getLocked(containerName) != nil {
				conflict = res(false, __("App %s already exists", containerName))
				return
			}
			if m.isCreating(containerName) {
				conflict = res(false, __("App %s is already being created", containerName))
				return
			}
			nameMap[key] = containerName
		}
	})
	if conflict != nil {
		return conflict
	}

	// Acquire creation locks atomically for all template members.
	for _, key := range orderedKeys {
		m.tryLockCreating(nameMap[key])
	}
	defer func() {
		for _, key := range orderedKeys {
			m.unlockCreating(nameMap[key])
			m.deps.Docker.UnregisterBuildLogger(nameMap[key])
		}
	}()

	deploy := func() error {
		// Phase 3: pre-generate all environment variables.
		envMap := map[string]map[string]any{}
		for _, key := range orderedKeys {
			appDef, _ := templateApps[key].(map[string]any)
			appEnv, _ := appDef["env"].(map[string]any)
			envMap[key] = prepareEnv(appEnv, nameMap[key])
		}

		// Phase 4: interpolation context + ${...} resolution.
		context := map[string]any{}
		for _, key := range orderedKeys {
			context[key] = map[string]any{"env": envMap[key], "name": nameMap[key]}
		}
		for _, key := range orderedKeys {
			envMap[key] = interpolateTemplateVars(envMap[key], context)
		}

		// User-provided env overrides (resolve directives on them too, so an
		// unresolved {generate: true} cannot overwrite a generated value).
		userEnv, _ := cfg["env"].(map[string]any)
		for _, key := range orderedKeys {
			if override, _ := userEnv[key].(map[string]any); override != nil {
				for k, v := range prepareEnv(override, nameMap[key]) {
					envMap[key][k] = v
				}
			}
		}

		// Phase 5: create and start each app in dependency order.
		var createdNames []string
		for _, key := range orderedKeys {
			appDef, _ := templateApps[key].(map[string]any)
			containerName := nameMap[key]
			appDir := filepath.Join(m.appsPath(), containerName)

			if err := os.MkdirAll(appDir, 0o755); err != nil {
				return err
			}

			logger := m.getLoggerInstance(containerName)
			m.deps.Docker.RegisterBuildLogger(containerName, logger)
			if err := logger.Init(); err != nil {
				return err
			}

			image, _ := appDef["image"].(string)
			logCtrl, err := logger.NewBuildStream(generateRuntimeID("build"), map[string]any{
				"image":    image,
				"strategy": "template-app",
				"template": map[string]any{"group": baseName, "role": key},
			})
			if err != nil {
				return err
			}

			linkedNames := []any{}
			if linked, _ := appDef["linked"].([]any); linked != nil {
				for _, dep := range linked {
					depKey, _ := dep.(string)
					if mapped := nameMap[depKey]; mapped != "" {
						linkedNames = append(linkedNames, mapped)
					}
				}
			}

			configVolumes, err := m.writeConfigFiles(appDef["configs"], appDir)
			if err != nil {
				return err
			}

			var appID any
			m.cfg.Mutate(func() {
				app := map[string]any{
					"id":      m.getNextIDLocked(),
					"name":    containerName,
					"type":    "container",
					"image":   image,
					"cmd":     normalizeCmd(appDef["cmd"]),
					"ports":   toAnySlice(m.preparePorts(toMapSlice(appDef["ports"]))),
					"volumes": append(m.prepareVolumes(toMapSlice(appDef["volumes"]), appDir), configVolumes...),
					"env":     map[string]any{"manual": envMap[key], "linked": linkedNames},
					"template": map[string]any{
						"group": baseName,
						"name":  recipeName,
						"role":  key,
					},
					"active":  true,
					"created": nowMs(),
					"status":  "installing",
				}
				appID = app["id"]
				m.apps = append(m.apps, app)
				m.saveAppsLocked()
			})

			m.clog.Log("createFromTemplate: Starting %s [%s] (%s)...", containerName, key, image)

			if !m.run(appID, logCtrl) {
				logCtrl.Finalize(false)
				return errors.New(__("Failed to start %s (%s): %s", containerName, key, "Container run returned false"))
			}
			logCtrl.Finalize(true)

			createdNames = append(createdNames, containerName)
			m.clog.Log("createFromTemplate: %s started successfully", containerName)
		}

		m.hubTrigger("app.list")
		return nil
	}

	if err := deploy(); err != nil {
		m.clog.Error("createFromTemplate: Deployment failed, initiating rollback: %s", err.Error())

		// Rollback: stop and remove all partially created template members.
		var rollback []map[string]any
		m.cfg.View(func() {
			for _, app := range m.apps {
				tpl, _ := app["template"].(map[string]any)
				if tpl != nil && tpl["group"] == baseName && tpl["name"] == recipeName {
					rollback = append(rollback, map[string]any{"id": app["id"], "name": app["name"]})
				}
			}
		})
		for _, app := range rollback {
			m.Stop(app["id"])
			name, _ := app["name"].(string)
			m.deps.Docker.Remove(name)
		}
		m.cfg.Mutate(func() {
			filtered := m.apps[:0]
			for _, app := range m.apps {
				tpl, _ := app["template"].(map[string]any)
				if tpl != nil && tpl["group"] == baseName && tpl["name"] == recipeName {
					continue
				}
				filtered = append(filtered, app)
			}
			m.apps = filtered
			m.saveAppsLocked()
		})

		return res(false, err.Error())
	}

	// Recompute the member list for the success message (deploy() started
	// every ordered key or errored out above).
	names := make([]string, len(orderedKeys))
	for i, key := range orderedKeys {
		names[i] = nameMap[key]
	}
	return res(true, __("Template %s deployed successfully: %s", recipeName, strings.Join(names, ", ")))
}

// createFromGit ports #fromGit.
func (m *Manager) createFromGit(cfg map[string]any) *api.Result {
	url, _ := cfg["url"].(string)
	token, _ := cfg["token"].(string)
	branch, _ := cfg["branch"].(string)
	name, _ := cfg["name"].(string)
	dev := cfg["dev"] == true

	m.clog.Log("createFromGit: Starting git deployment")
	m.clog.Log("createFromGit: URL: %s, Branch: %s, Name: %s", url, branch, name)

	if url == "" {
		return res(false, __("Missing git URL"))
	}
	// Security: validate the URL to prevent command injection.
	if hasIllegalURLChars(url) {
		return res(false, __("Invalid Git URL: Contains illegal characters."))
	}
	if !validGitURL(url, true) {
		return res(false, __("Invalid Git URL: Unsupported protocol."))
	}
	if !validBranch(branch) {
		return res(false, __("Invalid branch name format."))
	}
	if name == "" {
		return res(false, __("Missing app name"))
	}

	exists := false
	m.cfg.View(func() { exists = m.getLocked(name) != nil })
	if exists {
		return res(false, __("App %s already exists", name))
	}
	if !m.tryLockCreating(name) {
		return res(false, __("App %s is already being created", name))
	}
	defer m.unlockCreating(name)

	logger := m.getLoggerInstance(name)
	m.deps.Docker.RegisterBuildLogger(name, logger)
	defer m.deps.Docker.UnregisterBuildLogger(name)

	// Validate the app name to prevent path traversal.
	if filepath.Base(name) != name {
		return res(false, __("Invalid app name."))
	}

	appDir := filepath.Join(m.appsPath(), name)
	m.clog.Log("createFromGit: App directory: %s", appDir)

	if _, err := os.Stat(appDir); err == nil {
		m.clog.Log("createFromGit: Removing existing directory")
		if err := os.RemoveAll(appDir); err != nil {
			return res(false, err.Error())
		}
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return res(false, err.Error())
	}

	if err := logger.Init(); err != nil {
		return res(false, err.Error())
	}

	imageName := "odac-app-" + name
	logCtrl, err := logger.NewBuildStream(generateRuntimeID("build"), map[string]any{
		"image":    imageName,
		"strategy": "git-app",
	})
	if err != nil {
		return res(false, err.Error())
	}

	fail := func(err error) *api.Result {
		m.clog.Error("createFromGit: Failed: %s", err.Error())
		logCtrl.Finalize(false)
		if _, statErr := os.Stat(appDir); statErr == nil {
			os.RemoveAll(appDir)
		}
		return res(false, err.Error())
	}

	m.clog.Log("createFromGit: Cloning repository...")
	logCtrl.StartPhase("git_clone")
	if err := m.deps.Docker.CloneRepo(url, branch, appDir, token, logCtrl); err != nil {
		return fail(err)
	}
	logCtrl.EndPhase("git_clone", true)
	m.clog.Log("createFromGit: Clone successful")

	m.clog.Log("createFromGit: Building image...")
	if err := m.deps.Docker.Build(appDir, imageName, name, logCtrl); err != nil {
		return fail(err)
	}
	m.clog.Log("createFromGit: Build successful")

	// Auto-detect the port from image EXPOSE unless manually specified.
	detectedPort := 3000
	if p, present := cfg["port"]; present {
		if n, ok := jsNumber(p); ok {
			detectedPort = int(n)
		}
	} else if exposed := m.deps.Docker.GetImageExposedPorts(imageName); len(exposed) > 0 {
		detectedPort = exposed[0]
		m.clog.Log("createFromGit: Auto-detected port from image: %s", itoa(detectedPort))
	}

	repo, provider := gitMetadata(url)
	gitBranch := branch
	if gitBranch == "" {
		gitBranch = "main"
	}

	// env may be the structured {manual, linked} shape or a legacy flat map;
	// a top-level `linked` list applies only to the legacy shape.
	env, _ := cfg["env"].(map[string]any)
	linked, _ := cfg["linked"].([]any)
	var envManual any = map[string]any{}
	var envLinked any = []any{}
	if isNewEnvStructure(env) {
		if manual, _ := env["manual"].(map[string]any); manual != nil {
			envManual = manual
		}
		if l, _ := env["linked"].([]any); l != nil {
			envLinked = l
		}
	} else {
		if env != nil {
			envManual = env
		}
		if linked != nil {
			envLinked = linked
		}
	}

	var appID any
	m.cfg.Mutate(func() {
		app := map[string]any{
			"id":   m.getNextIDLocked(),
			"name": name,
			"type": "git",
			"git": map[string]any{
				"repo":     repo,
				"branch":   gitBranch,
				"provider": provider,
			},
			"url":     url,
			"image":   imageName,
			"env":     map[string]any{"manual": envManual, "linked": envLinked},
			"ports":   []any{map[string]any{"host": ports.Proxy, "container": float64(detectedPort)}},
			"dev":     dev,
			"active":  true,
			"created": nowMs(),
			"status":  "starting",
		}
		if branch != "" {
			app["branch"] = branch
		}
		appID = app["id"]
		m.apps = append(m.apps, app)
		m.saveAppsLocked()
	})

	m.clog.Log("createFromGit: Starting container...")
	logCtrl.StartPhase("start_new_container")
	if err := m.runGitApp(appID, ""); err != nil {
		return fail(err)
	}
	logCtrl.EndPhase("start_new_container", true)

	m.set(appID, map[string]any{"status": "running", "started": nowMs()})
	m.spawn(func() {
		if scanErr := m.scanAndSaveHTTPStatus(appID); scanErr != nil {
			m.clog.Error("HTTP scan failed for %s: %s", name, scanErr.Error())
		}
	})
	m.clog.Log("createFromGit: App started successfully")

	m.hubTrigger("app.list")

	logCtrl.Finalize(true)
	return res(true, __("App %s deployed successfully.", name))
}

// ---- helpers (template & env preparation) ----

// resolveTemplateDependencies ports #resolveTemplateDependencies: Kahn's
// topological sort over `linked` edges, detecting undefined and circular
// dependencies. Keys with equal in-degree keep... Node's Object.keys
// insertion order, which a Go map cannot reproduce — sorted key order is
// used instead (deterministic; only the start order of independent apps
// differs, never dependency correctness).
func resolveTemplateDependencies(templateApps map[string]any) ([]string, error) {
	keys := make([]string, 0, len(templateApps))
	for k := range templateApps {
		keys = append(keys, k)
	}
	sortStrings(keys)

	adjacency := map[string][]string{}
	inDegree := map[string]int{}
	for _, key := range keys {
		adjacency[key] = nil
		inDegree[key] = 0
	}

	for _, key := range keys {
		appDef, _ := templateApps[key].(map[string]any)
		deps, _ := appDef["linked"].([]any)
		for _, d := range deps {
			dep, _ := d.(string)
			if _, ok := inDegree[dep]; !ok {
				return nil, errors.New(__("Template dependency \"%s\" (required by \"%s\") is not defined.", dep, key))
			}
			adjacency[dep] = append(adjacency[dep], key)
			inDegree[key]++
		}
	}

	queue := make([]string, 0, len(keys))
	for _, k := range keys {
		if inDegree[k] == 0 {
			queue = append(queue, k)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)
		for _, neighbor := range adjacency[node] {
			inDegree[neighbor]--
			if inDegree[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	if len(sorted) != len(keys) {
		return nil, errors.New(__("Circular dependency detected in template."))
	}
	return sorted, nil
}

// interpolateTemplateVars ports #interpolateTemplateVars: resolve
// ${appKey.name} and ${appKey.env.VAR} references in env values.
func interpolateTemplateVars(env map[string]any, context map[string]any) map[string]any {
	resolved := make(map[string]any, len(env))
	for key, value := range env {
		str, isStr := value.(string)
		if !isStr {
			resolved[key] = value
			continue
		}
		resolved[key] = templateVarRE.ReplaceAllStringFunc(str, func(match string) string {
			sub := templateVarRE.FindStringSubmatch(match)
			appCtx, ok := context[sub[1]].(map[string]any)
			if !ok {
				return match
			}
			var current any = appCtx
			for _, part := range strings.Split(sub[2], ".") {
				cm, isMap := current.(map[string]any)
				if !isMap {
					return match
				}
				next, present := cm[part]
				if !present {
					return match
				}
				current = next
			}
			return jsString(current)
		})
	}
	return resolved
}

// mergeRecipeEnv ports #mergeRecipeEnv: recipe defaults + user overrides
// (directives resolved on both sides), linked lists unioned.
func (m *Manager) mergeRecipeEnv(recipe map[string]any, userEnv map[string]any, containerName string) map[string]any {
	recipeEnv, _ := recipe["env"].(map[string]any)
	defaultEnv := prepareEnv(recipeEnv, containerName)
	defaultLinked, _ := recipe["linked"].([]any)

	userIsStructured := isNewEnvStructure(userEnv)
	userManual := getManualEnv(userEnv)
	var userLinked []any
	if userIsStructured {
		userLinked, _ = userEnv["linked"].([]any)
	}

	manual := map[string]any{}
	for k, v := range defaultEnv {
		manual[k] = v
	}
	for k, v := range prepareEnv(userManual, containerName) {
		manual[k] = v
	}

	linked := []any{}
	seen := map[any]bool{}
	for _, l := range append(append([]any{}, defaultLinked...), userLinked...) {
		if !seen[l] {
			seen[l] = true
			linked = append(linked, l)
		}
	}

	return map[string]any{"manual": manual, "linked": linked}
}

// prepareEnv ports #prepareEnv: resolve directive objects, pass scalars
// through.
func prepareEnv(recipeEnv map[string]any, containerName string) map[string]any {
	env := map[string]any{}
	for key, value := range recipeEnv {
		if directive, _ := value.(map[string]any); directive != nil {
			env[key] = resolveEnvDirective(directive, containerName)
		} else {
			env[key] = value
		}
	}
	return env
}

// resolveEnvDirective ports #resolveEnvDirective: {type: 'container'} ->
// container name, {type/legacy 'generate'} -> random secret; anything else
// passes through unresolved.
func resolveEnvDirective(directive map[string]any, containerName string) any {
	typ, _ := directive["type"].(string)
	if typ == "" {
		if _, ok := directive["generate"]; ok { // legacy spelling
			typ = "generate"
		}
	}

	switch typ {
	case "container":
		return containerName
	case "generate":
		length := 16
		if n, ok := jsNumber(directive["length"]); ok && n > 0 {
			length = int(n)
		}
		return generatePassword(length)
	default:
		return directive
	}
}

// normalizeCmd ports #normalizeCmd: string -> whitespace split, empty ->
// null. Null IS persisted (config-schema.md).
func normalizeCmd(cmd any) any {
	switch x := cmd.(type) {
	case nil:
		return nil
	case []any:
		if len(x) == 0 {
			return nil
		}
		return x
	case string:
		if x == "" {
			return nil
		}
		fields := strings.Fields(x)
		if len(fields) == 0 {
			return nil
		}
		out := make([]any, len(fields))
		for i, f := range fields {
			out[i] = f
		}
		return out
	}
	return nil
}

// writeConfigFiles ports #writeConfigFiles: write recipe-defined config
// files under <appDir>/configs and return their volume mappings.
func (m *Manager) writeConfigFiles(configs any, appDir string) ([]any, error) {
	list, _ := configs.([]any)
	if len(list) == 0 {
		return []any{}, nil
	}

	volumes := []any{}
	configBase := filepath.Join(appDir, "configs")

	for _, raw := range list {
		cfg, _ := raw.(map[string]any)
		if cfg == nil {
			continue
		}
		cfgPath, _ := cfg["path"].(string)
		content, hasContent := cfg["content"]
		if cfgPath == "" || !hasContent || content == nil {
			continue
		}

		normalized := filepath.Clean(cfgPath)
		if strings.Contains(normalized, "..") || filepath.IsAbs(normalized) {
			m.clog.Error("writeConfigFiles: Skipping unsafe config path: %s", cfgPath)
			continue
		}

		hostFile := filepath.Join(configBase, normalized)
		if !strings.HasPrefix(filepath.Clean(hostFile), configBase+string(filepath.Separator)) {
			m.clog.Error("writeConfigFiles: Resolved path escapes sandbox: %s", cfgPath)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(hostFile), 0o755); err != nil {
			return volumes, err
		}

		data, isStr := content.(string)
		if !isStr {
			marshaled, err := json.MarshalIndent(content, "", "  ")
			if err != nil {
				return volumes, err
			}
			data = string(marshaled)
		}
		if err := os.WriteFile(hostFile, []byte(data), 0o644); err != nil {
			return volumes, err
		}

		volumes = append(volumes, map[string]any{"host": hostFile, "container": cfgPath})
		m.clog.Log("writeConfigFiles: Wrote %s ("+itoa(len(data))+" bytes)", cfgPath)
	}

	return volumes, nil
}

// toMapSlice converts a decoded-JSON array to []map[string]any, dropping
// non-map entries.
func toMapSlice(v any) []map[string]any {
	list, _ := v.([]any)
	if list == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if entry, _ := item.(map[string]any); entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

func toAnySlice(list []map[string]any) []any {
	out := make([]any, len(list))
	for i, e := range list {
		out[i] = e
	}
	return out
}
