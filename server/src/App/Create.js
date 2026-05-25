const {log, error} = Odac.core('Log', false).init('App.Create')
const fs = require('fs')
const path = require('path')
const nodeCrypto = require('crypto')

const LEGACY_ENV_DIRECTIVES = new Set(['generate'])

class Create {
  #api

  constructor(api) {
    this.#api = api
  }

  /**
   * Public entry point. Dispatches to the right strategy based on config shape.
   */
  async create(config) {
    // Support both string (legacy) and object config
    // String: create("mysql")
    // Object: create({type: "app", app: "postgres", name: "postgres-2--xyz"})
    // Object: create({type: "github", repo: "...", token: "...", name: "myapp"})

    if (typeof config === 'string') {
      if (/^(https?|git|ssh):\/\//.test(config) || /^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/.test(config)) {
        const name = this.#api.generateUniqueName(path.basename(config, '.git').replace(/[^a-zA-Z0-9-]/g, '-'))
        config = {type: 'git', url: config, name}
      } else {
        config = {type: 'app', app: config}
      }
    }

    log('Creating app: %j', config)

    // Validate config
    if (!config.type) {
      return Odac.server('Api').result(false, __('Missing config type'))
    }

    switch (config.type) {
      case 'app':
        return this.#fromRecipe(config)
      case 'git':
        return this.#fromGit(config)
      case 'github':
        return this.#fromGit(config) // legacy alias
      case 'template':
        return this.#fromTemplatePayload(config)
      default:
        return Odac.server('Api').result(false, __('Unknown config type: %s', config.type))
    }
  }

  async #fromRecipe(config) {
    const api = this.#api
    const {app: appType, name: customName} = config

    if (!appType) {
      log('createFromRecipe: Missing app type')
      return Odac.server('Api').result(false, __('Missing app type'))
    }

    log('createFromRecipe: Fetching recipe for %s', appType)

    let recipe
    try {
      recipe = await Odac.server('Hub').getApp(appType)
      log('createFromRecipe: Recipe received: %j', recipe)
    } catch (e) {
      error('createFromRecipe: Failed to fetch recipe: %s', e)
      return Odac.server('Api').result(false, __('Could not find recipe for %s: %s', appType, e))
    }

    // Template Detection: Multi-app stacks (e.g. WordPress + MariaDB) are delegated to the template handler
    if (recipe.apps && typeof recipe.apps === 'object' && Object.keys(recipe.apps).length > 0) {
      const baseName = customName || api.generateUniqueName(recipe.name)
      return this.#fromTemplate(baseName, recipe, config)
    }

    const name = customName || api.generateUniqueName(recipe.name)
    log('createFromRecipe: Using name: %s', name)

    if (api.get(name)) {
      log('createFromRecipe: App %s already exists', name)
      return Odac.server('Api').result(false, __('App %s already exists', name))
    }

    if (api.creating.has(name)) {
      log('createFromRecipe: App %s is already being created', name)
      return Odac.server('Api').result(false, __('App %s is already being created', name))
    }

    // Initialize Logger & Register SYNC to prevent race conditions with Hub requests
    const logger = api.getLoggerInstance(name)
    Odac.server('Container').registerBuildLogger(name, logger)

    api.creating.add(name)

    let logCtrl = null

    try {
      const appDir = path.join(Odac.core('Config').config.app.path, name)
      if (!fs.existsSync(appDir)) fs.mkdirSync(appDir, {recursive: true})

      await logger.init()

      const buildId = api.generateRuntimeId('build')
      logCtrl = logger.createBuildStream(buildId, {
        image: recipe.image,
        strategy: 'recipe-app'
      })

      const app = {
        id: api.getNextId(),
        name,
        type: 'container',
        image: recipe.image,
        cmd: this.#normalizeCmd(recipe.cmd),
        ports: await api.preparePorts(recipe.ports),
        volumes: [...api.prepareVolumes(recipe.volumes, appDir), ...(await this.#writeConfigFiles(recipe.configs, appDir))],
        env: this.#mergeRecipeEnv(recipe, config.env, name),
        active: true,
        created: Date.now(),
        status: 'installing'
      }

      log('createFromRecipe: App config: %j', app)

      api.addApp(app)

      try {
        log('createFromRecipe: Starting app...')
        if (await api.run(app.id, logCtrl)) {
          log('createFromRecipe: App started successfully')
          Odac.server('Hub').trigger('app.list')
          if (logCtrl) await logCtrl.finalize(true)
          return Odac.server('Api').result(true, __('App %s created successfully.', name))
        }
        throw new Error('Failed to start app container. Check logs for details.')
      } catch (e) {
        error('createFromRecipe: Failed to start app: %s', e.message)
        api.filterApps(s => s.id !== app.id)
        if (logCtrl) await logCtrl.finalize(false)
        return Odac.server('Api').result(false, e.message)
      }
    } finally {
      api.creating.delete(name)
      Odac.server('Container').unregisterBuildLogger(name)
    }
  }

  /**
   * Handles direct template payloads from Hub (type: 'template').
   * Hub sends the full template data inline — no additional fetch required.
   */
  async #fromTemplatePayload(config) {
    const {apps, name} = config

    if (!apps || typeof apps !== 'object' || Object.keys(apps).length === 0) {
      return Odac.server('Api').result(false, __('Invalid template: no apps defined.'))
    }

    if (!name) {
      return Odac.server('Api').result(false, __('Missing template name.'))
    }

    const hasCloudContainers = Object.values(apps).every(app => app && typeof app === 'object' && app.container)
    const baseName = hasCloudContainers ? name : this.#api.generateUniqueName(name)
    return this.#fromTemplate(baseName, {name, apps}, config)
  }

  /**
   * Deploys a multi-app template stack (e.g. WordPress + MariaDB) as a single atomic operation.
   * Resolves inter-app dependencies via topological sort, generates secrets,
   * interpolates template variables, and links apps via env.linked for runtime resolution.
   * Rollback is automatic on partial failure — no orphan containers are left behind.
   */
  async #fromTemplate(baseName, recipe, config) {
    const api = this.#api
    log('createFromTemplate: Starting template deployment for %s (%s)', baseName, recipe.name)

    const templateApps = recipe.apps
    const appKeys = Object.keys(templateApps)

    if (appKeys.length === 0) {
      return Odac.server('Api').result(false, __('Template %s has no apps defined.', recipe.name))
    }

    // Phase 1: Resolve dependency order via topological sort (Kahn's O(V+E))
    let orderedKeys
    try {
      orderedKeys = this.#resolveTemplateDependencies(templateApps)
    } catch (e) {
      return Odac.server('Api').result(false, e.message)
    }

    log('createFromTemplate: Dependency order: %j', orderedKeys)

    // Phase 2: Resolve container names — use Cloud-provided names or generate locally
    const nameMap = {}
    for (const key of orderedKeys) {
      const cloudName = templateApps[key].container
      const containerName = cloudName || api.generateUniqueName(`${baseName}-${key}`)

      if (api.get(containerName)) {
        return Odac.server('Api').result(false, __('App %s already exists', containerName))
      }
      if (api.creating.has(containerName)) {
        return Odac.server('Api').result(false, __('App %s is already being created', containerName))
      }
      nameMap[key] = containerName
    }

    // Acquire creation locks atomically for all template members
    for (const key of orderedKeys) {
      api.creating.add(nameMap[key])
    }

    try {
      // Phase 3: Pre-generate all environment variables
      const envMap = {}
      for (const key of orderedKeys) {
        envMap[key] = this.#prepareEnv(templateApps[key].env || {}, nameMap[key])
      }

      // Phase 4: Build interpolation context and resolve ${...} template variables
      const context = {}
      for (const key of orderedKeys) {
        context[key] = {env: envMap[key], name: nameMap[key]}
      }
      for (const key of orderedKeys) {
        envMap[key] = this.#interpolateTemplateVars(envMap[key], context)
      }

      // Apply user-provided env overrides (if any)
      const userEnv = config.env || {}
      for (const key of orderedKeys) {
        if (userEnv[key] && typeof userEnv[key] === 'object') {
          Object.assign(envMap[key], userEnv[key])
        }
      }

      // Phase 5: Create and start each app in dependency order
      const createdApps = []
      const loggers = new Map()

      for (const key of orderedKeys) {
        const appDef = templateApps[key]
        const containerName = nameMap[key]
        const appDir = path.join(Odac.core('Config').config.app.path, containerName)

        await fs.promises.mkdir(appDir, {recursive: true})

        const logger = api.getLoggerInstance(containerName)
        Odac.server('Container').registerBuildLogger(containerName, logger)
        await logger.init()

        const buildId = api.generateRuntimeId('build')
        const logCtrl = logger.createBuildStream(buildId, {
          image: appDef.image,
          strategy: 'template-app',
          template: {group: baseName, role: key}
        })
        loggers.set(containerName, {logCtrl, logger})

        const linkedNames = (appDef.linked || []).map(depKey => nameMap[depKey]).filter(Boolean)

        const app = {
          id: api.getNextId(),
          name: containerName,
          type: 'container',
          image: appDef.image,
          cmd: this.#normalizeCmd(appDef.cmd),
          ports: await api.preparePorts(appDef.ports),
          volumes: [...api.prepareVolumes(appDef.volumes, appDir), ...(await this.#writeConfigFiles(appDef.configs, appDir))],
          env: {
            manual: envMap[key],
            linked: linkedNames
          },
          template: {
            group: baseName,
            name: recipe.name,
            role: key
          },
          active: true,
          created: Date.now(),
          status: 'installing'
        }

        api.addApp(app)

        log('createFromTemplate: Starting %s [%s] (%s)...', containerName, key, appDef.image)

        try {
          if (!(await api.run(app.id, logCtrl))) {
            throw new Error('Container run returned false')
          }
          await logCtrl.finalize(true)
        } catch (runErr) {
          if (logCtrl) await logCtrl.finalize(false)
          throw new Error(__('Failed to start %s (%s): %s', containerName, key, runErr.message))
        }

        createdApps.push(app)
        log('createFromTemplate: %s started successfully', containerName)
      }

      Odac.server('Hub').trigger('app.list')

      const names = createdApps.map(a => a.name).join(', ')
      return Odac.server('Api').result(true, __('Template %s deployed successfully: %s', recipe.name, names))
    } catch (e) {
      error('createFromTemplate: Deployment failed, initiating rollback: %s', e.message)

      // Rollback: Stop and remove all partially created apps
      const rollbackApps = api.getApps().filter(a => a.template?.group === baseName && a.template?.name === recipe.name)
      for (const app of rollbackApps) {
        try {
          await api.stop(app.id)
          await Odac.server('Container').remove(app.name)
        } catch {
          /* ignore rollback errors */
        }
      }

      const rollbackIds = new Set(rollbackApps.map(a => a.id))
      api.filterApps(a => !rollbackIds.has(a.id))

      return Odac.server('Api').result(false, e.message)
    } finally {
      for (const key of orderedKeys) {
        api.creating.delete(nameMap[key])
        Odac.server('Container').unregisterBuildLogger(nameMap[key])
      }
    }
  }

  async #fromGit(config) {
    const api = this.#api
    const {url, token, branch, name, linked, dev = false, env = {}, port = 3000} = config

    log('createFromGit: Starting git deployment')
    log('createFromGit: URL: %s, Branch: %s, Name: %s', url, branch, name)

    if (!url) {
      return Odac.server('Api').result(false, __('Missing git URL'))
    }

    // Security: Validate Git URL to prevent Command Injection
    if (/[;&|`$(){}<>\n\r]/.test(url)) {
      return Odac.server('Api').result(false, __('Invalid Git URL: Contains illegal characters.'))
    }

    if (!url.match(/^(https?|git|ssh|ftps?|rsync):\/\//) && !url.match(/^[a-zA-Z0-9_\-.]+@[a-zA-Z0-9.\-_]+:/)) {
      return Odac.server('Api').result(false, __('Invalid Git URL: Unsupported protocol.'))
    }
    if (!name) {
      return Odac.server('Api').result(false, __('Missing app name'))
    }

    if (api.get(name)) {
      return Odac.server('Api').result(false, __('App %s already exists', name))
    }

    if (api.creating.has(name)) {
      return Odac.server('Api').result(false, __('App %s is already being created', name))
    }

    const logger = api.getLoggerInstance(name)
    Odac.server('Container').registerBuildLogger(name, logger)

    api.creating.add(name)

    try {
      // Validate the app name to prevent path traversal.
      if (path.basename(name) !== name) {
        return Odac.server('Api').result(false, __('Invalid app name.'))
      }

      const appDir = path.join(Odac.core('Config').config.app.path, name)
      log('createFromGit: App directory: %s', appDir)

      if (fs.existsSync(appDir)) {
        log('createFromGit: Removing existing directory')
        fs.rmSync(appDir, {recursive: true, force: true})
      }
      fs.mkdirSync(appDir, {recursive: true})

      await logger.init()

      const imageName = `odac-app-${name}`
      let logCtrl = null

      try {
        const buildId = api.generateRuntimeId('build')
        logCtrl = logger.createBuildStream(buildId, {
          image: imageName,
          strategy: 'git-app'
        })

        log('createFromGit: Cloning repository...')
        if (logCtrl) logCtrl.startPhase('git_clone')
        await Odac.server('Container').cloneRepo(url, branch, appDir, token, logCtrl)
        if (logCtrl) logCtrl.endPhase('git_clone', true)
        log('createFromGit: Clone successful')

        log('createFromGit: Building image...')
        await Odac.server('Container').build(appDir, imageName, name, {
          stream: logCtrl.stream,
          start: logCtrl.startPhase,
          end: logCtrl.endPhase,
          finalize: () => {},
          subscribe: logCtrl.subscribe
        })
        log('createFromGit: Build successful')

        // Auto-detect port from Image EXPOSE if not manually specified
        let detectedPort = port
        if (!config.port) {
          try {
            const exposed = await Odac.server('Container').getImageExposedPorts(imageName)
            if (exposed && exposed.length > 0) {
              detectedPort = exposed[0]
              log('createFromGit: Auto-detected port from image: %d', detectedPort)
            }
          } catch (e) {
            log('createFromGit: Failed to detect port from image: %s', e.message)
          }
        }

        const gitMetadata = api.getGitMetadata(url)
        const app = {
          id: api.getNextId(),
          name,
          type: 'git',
          git: {
            repo: gitMetadata.repo,
            branch: branch || 'main',
            provider: gitMetadata.provider
          },
          url,
          branch,
          image: imageName,
          env: {
            manual: env.manual || Array.isArray(env.linked) ? env.manual || {} : env,
            linked: env.manual || Array.isArray(env.linked) ? env.linked || [] : linked || []
          },
          ports: [{container: parseInt(detectedPort)}],
          dev,
          active: true,
          created: Date.now(),
          status: 'starting'
        }

        api.addApp(app)

        log('createFromGit: Starting container...')
        if (logCtrl) logCtrl.startPhase('start_new_container')
        await api.runGitApp(app)
        if (logCtrl) logCtrl.endPhase('start_new_container', true)

        api.set(app.id, {status: 'running', started: Date.now()})
        api.scanAndSaveHttpStatus(app).catch(e => error('HTTP scan failed for %s: %s', app.name, e.message))
        log('createFromGit: App started successfully')

        Odac.server('Hub').trigger('app.list')

        if (logCtrl) await logCtrl.finalize(true)
        return Odac.server('Api').result(true, __('App %s deployed successfully.', name))
      } catch (e) {
        error('createFromGit: Failed: %s', e.message)
        if (logCtrl) await logCtrl.finalize(false)
        if (fs.existsSync(appDir)) {
          fs.rmSync(appDir, {recursive: true, force: true})
        }
        return Odac.server('Api').result(false, e.message)
      }
    } finally {
      api.creating.delete(name)
      Odac.server('Container').unregisterBuildLogger(name)
    }
  }

  // ---- Helpers (template & env preparation) ----

  /**
   * Performs topological sort on template app definitions based on linked dependencies.
   * Uses Kahn's algorithm for O(V+E) performance. Detects circular dependencies.
   */
  #resolveTemplateDependencies(templateApps) {
    const keys = Object.keys(templateApps)
    const adjacency = {}
    const inDegree = {}

    for (const key of keys) {
      adjacency[key] = []
      inDegree[key] = 0
    }

    for (const key of keys) {
      const deps = templateApps[key].linked || []
      for (const dep of deps) {
        if (!adjacency[dep]) {
          throw new Error(__('Template dependency "%s" (required by "%s") is not defined.', dep, key))
        }
        adjacency[dep].push(key)
        inDegree[key]++
      }
    }

    const queue = keys.filter(k => inDegree[k] === 0)
    const sorted = []

    while (queue.length > 0) {
      const node = queue.shift()
      sorted.push(node)

      for (const neighbor of adjacency[node]) {
        inDegree[neighbor]--
        if (inDegree[neighbor] === 0) {
          queue.push(neighbor)
        }
      }
    }

    if (sorted.length !== keys.length) {
      throw new Error(__('Circular dependency detected in template.'))
    }

    return sorted
  }

  /**
   * Resolves template variable references in environment values.
   * Supports ${appKey.name} for container name and ${appKey.env.VAR} for env values.
   */
  #interpolateTemplateVars(env, context) {
    const resolved = {}
    const varPattern = /\$\{([a-zA-Z0-9_]+)\.([a-zA-Z0-9_.]+)\}/g

    for (const [key, value] of Object.entries(env)) {
      if (typeof value !== 'string') {
        resolved[key] = value
        continue
      }

      resolved[key] = value.replace(varPattern, (match, appKey, propPath) => {
        const appCtx = context[appKey]
        if (!appCtx) return match

        const parts = propPath.split('.')
        let current = appCtx
        for (const part of parts) {
          if (current && typeof current === 'object' && part in current) {
            current = current[part]
          } else {
            return match
          }
        }

        return typeof current === 'string' ? current : String(current)
      })
    }

    return resolved
  }

  #mergeRecipeEnv(recipe, userEnv = {}, containerName = '') {
    const defaultEnv = this.#prepareEnv(recipe.env, containerName)
    const defaultLinked = recipe.linked || []

    const userIsStructured = userEnv.manual || Array.isArray(userEnv.linked)
    const userManual = this.#getManualEnv(userEnv)
    const userLinked = userIsStructured ? userEnv.linked || [] : []

    const manual = {...defaultEnv, ...userManual}

    const linkedSet = new Set([...defaultLinked, ...userLinked])
    const linked = [...linkedSet]

    return {manual, linked}
  }

  #prepareEnv(recipeEnv, containerName = '') {
    if (!recipeEnv) return {}

    const ctx = {containerName}
    const env = {}
    for (const [key, value] of Object.entries(recipeEnv)) {
      env[key] = typeof value === 'object' && value !== null ? this.#resolveEnvDirective(value, ctx) : value
    }

    return env
  }

  #resolveEnvDirective(directive, ctx) {
    const type = directive.type || Object.keys(directive).find(k => LEGACY_ENV_DIRECTIVES.has(k))

    switch (type) {
      case 'container':
        return ctx.containerName
      case 'generate':
        return this.#generatePassword(directive.length || 16)
      default:
        return directive
    }
  }

  #getManualEnv(envConfig) {
    if (!envConfig) return {}
    const isNewStructure = envConfig.manual || Array.isArray(envConfig.linked)
    return isNewStructure ? envConfig.manual || {} : envConfig
  }

  #generatePassword(length = 16) {
    return nodeCrypto
      .randomBytes(Math.ceil(length / 2))
      .toString('hex')
      .slice(0, length)
  }

  /**
   * Normalizes a command input into an array suitable for Docker Cmd.
   */
  #normalizeCmd(cmd) {
    if (!cmd) return null
    if (Array.isArray(cmd)) return cmd.length > 0 ? cmd : null
    if (typeof cmd === 'string') {
      const parts = cmd.trim().split(/\s+/)
      return parts.length > 0 ? parts : null
    }
    return null
  }

  /**
   * Writes recipe-defined config files to the app directory and returns
   * corresponding volume mappings for container mount.
   */
  async #writeConfigFiles(configs, appDir) {
    if (!Array.isArray(configs) || configs.length === 0) return []

    const volumes = []
    const configBase = path.join(appDir, 'configs')

    for (const cfg of configs) {
      if (!cfg.path || cfg.content === undefined || cfg.content === null) continue

      const normalized = path.normalize(cfg.path)
      if (normalized.includes('..') || path.isAbsolute(normalized)) {
        error('writeConfigFiles: Skipping unsafe config path: %s', cfg.path)
        continue
      }

      const hostFile = path.join(configBase, normalized)

      if (!path.resolve(hostFile).startsWith(path.resolve(configBase))) {
        error('writeConfigFiles: Resolved path escapes sandbox: %s → %s', cfg.path, hostFile)
        continue
      }
      await fs.promises.mkdir(path.dirname(hostFile), {recursive: true})

      const data = typeof cfg.content === 'string' ? cfg.content : JSON.stringify(cfg.content, null, 2)
      await fs.promises.writeFile(hostFile, data, 'utf8')

      volumes.push({host: hostFile, container: cfg.path})
      log('writeConfigFiles: Wrote %s (%d bytes)', cfg.path, Buffer.byteLength(data))
    }

    return volumes
  }
}

module.exports = Create
