const fs = require('fs')
const os = require('os')
const path = require('path')

const {log, error} = Odac.core('Log', false).init('Config')

class Config {
  #dir
  #configDir
  #saving = false
  #changed = false
  #moduleChanged = {}
  config = {
    server: {
      pid: null,
      started: null,
      watchdog: null
    }
  }

  // Module mapping configuration - defines which config keys belong to which module files
  #moduleMap = {
    api: ['api'],
    app: ['apps', 'app'],
    dns: ['dns'],
    domain: ['domains'],
    firewall: ['firewall'],
    hub: ['hub'],
    mail: ['mail'],
    server: ['server'],
    service: ['services'],
    ssl: ['ssl'],
    web: ['websites', 'web']
  }

  // Initialize default configuration for module keys
  #initializeDefaultModuleConfig(config, keys) {
    for (const key of keys) {
      if (config[key] === undefined) {
        // Initialize with appropriate default values
        if (key === 'server') {
          config[key] = {
            pid: null,
            started: null,
            watchdog: null
          }
        } else if (key === 'websites') {
          config[key] = {}
        } else if (key === 'apps' || key === 'domains') {
          config[key] = []
        } else if (key === 'app') {
          config[key] = {}
        } else if (key === 'firewall') {
          config[key] = {
            enabled: true,
            blacklist: [],
            whitelist: [],
            rateLimit: {
              enabled: true,
              windowMs: 60000,
              max: 300
            }
          }
        } else {
          config[key] = {}
        }
      }
    }
  }

  force() {
    this.#changed = true
    // Mark all modules as changed to force save
    for (const moduleName of Object.keys(this.#moduleMap)) {
      this.#moduleChanged[moduleName] = true
    }
    this.#save()
  }

  // Get module name for a config key
  #getModuleForKey(key) {
    for (const [module, keys] of Object.entries(this.#moduleMap)) {
      if (keys.includes(key)) return module
    }
    return null
  }

  // Load individual module file from config directory with corruption recovery
  #loadModuleFile(moduleName) {
    const moduleFile = path.join(this.#configDir, moduleName + '.json')
    const bakDir = path.join(this.#dir, '.bak')
    const backupFile = path.join(bakDir, moduleName + '.json.bak')
    const corruptedFile = moduleFile + '.corrupted'

    // Return null if file doesn't exist
    if (!fs.existsSync(moduleFile)) {
      return null
    }

    // Try to load the main file
    try {
      const data = fs.readFileSync(moduleFile, 'utf8')
      if (!data || data.length < 2) {
        error(`Module file ${moduleName}.json is empty`)
        // Try backup if main file is empty
        return this.#loadModuleFromBackup(moduleName, moduleFile, backupFile, corruptedFile)
      }
      return JSON.parse(data)
    } catch (err) {
      // JSON parse error or read error detected
      error(`Error loading module file ${moduleName}.json:`, err.message)

      // Try to recover from backup
      return this.#loadModuleFromBackup(moduleName, moduleFile, backupFile, corruptedFile)
    }
  }

  // Atomic write helper method - writes data safely with backup
  #atomicWrite(filePath, data) {
    const tempFile = filePath + '.tmp'
    const bakDir = path.join(this.#dir, '.bak')
    const fileName = path.basename(filePath)
    const backupFile = path.join(bakDir, fileName + '.bak')

    try {
      // 1. Write to temporary file first
      const jsonData = JSON.stringify(data, null, 4)
      fs.writeFileSync(tempFile, jsonData, 'utf8')

      // 2. Copy existing file to .bak directory before overwriting (if it exists)
      if (fs.existsSync(filePath)) {
        try {
          // Ensure .bak directory exists
          if (!fs.existsSync(bakDir)) {
            fs.mkdirSync(bakDir, {recursive: true})
          }
          fs.copyFileSync(filePath, backupFile)
        } catch (backupErr) {
          error(`[Config] Warning: Failed to create backup for ${filePath}: ${backupErr.message}`)
          // Continue anyway - better to save without backup than not save at all
        }
      }

      // 3. Atomic rename to replace main file
      fs.renameSync(tempFile, filePath)

      return true
    } catch (err) {
      error(`[Config] Atomic write failed for ${filePath}: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)

      // Clean up temporary file on error
      if (fs.existsSync(tempFile)) {
        try {
          fs.unlinkSync(tempFile)
        } catch (cleanupErr) {
          error(`[Config] Failed to clean up temp file ${tempFile}: ${cleanupErr.message}`)
        }
      }
      throw err
    }
  }

  // Attempt to load module from backup file
  #loadModuleFromBackup(moduleName, moduleFile, backupFile, corruptedFile) {
    // Check if backup file exists
    if (!fs.existsSync(backupFile)) {
      error(`No backup file found for ${moduleName}.json, initializing with defaults`)
      return null
    }

    try {
      // Try to load from backup
      const backupData = fs.readFileSync(backupFile, 'utf8')
      if (!backupData || backupData.length < 2) {
        error(`Backup file ${moduleName}.json.bak is empty, initializing with defaults`)
        return null
      }

      const parsedData = JSON.parse(backupData)

      // Backup is valid - create .corrupted backup of broken file
      try {
        if (fs.existsSync(moduleFile)) {
          fs.copyFileSync(moduleFile, corruptedFile)
          log(`Created corrupted backup: ${moduleName}.json.corrupted`)
        }
      } catch (copyErr) {
        error(`Failed to create corrupted backup for ${moduleName}.json:`, copyErr.message)
      }

      // Restore from backup to main file
      try {
        fs.writeFileSync(moduleFile, backupData, 'utf8')
        log(`Restored ${moduleName}.json from backup`)
      } catch (writeErr) {
        error(`Failed to restore ${moduleName}.json from backup:`, writeErr.message)
      }

      return parsedData
    } catch (err) {
      // Both main and backup are corrupted
      error(`Both ${moduleName}.json and backup are corrupted:`, err.message)
      error(`Initializing ${moduleName} with default values`)

      // Create .corrupted backup of both files if they exist
      try {
        if (fs.existsSync(moduleFile)) {
          fs.copyFileSync(moduleFile, corruptedFile)
        }
        if (fs.existsSync(backupFile)) {
          fs.copyFileSync(backupFile, backupFile + '.corrupted')
        }
      } catch (copyErr) {
        error(`Failed to create corrupted backups:`, copyErr.message)
      }

      return null
    }
  }

  // Merge all module configs into single in-memory object
  #loadModular() {
    log('[Config] Loading modular configuration...')

    try {
      // Start with default config structure
      const mergedConfig = {
        server: {
          pid: null,
          started: null,
          watchdog: null
        }
      }

      let loadedModules = 0
      let failedModules = []

      // Iterate through all modules and merge their data
      for (const [moduleName, keys] of Object.entries(this.#moduleMap)) {
        try {
          const moduleData = this.#loadModuleFile(moduleName)

          if (moduleData && typeof moduleData === 'object') {
            // Merge each key from the module into the main config
            for (const key of keys) {
              if (moduleData[key] !== undefined) {
                mergedConfig[key] = moduleData[key]
              }
            }
            loadedModules++
          } else {
            // Module file is missing or corrupted - initialize with defaults
            log(`[Config] Module ${moduleName} not loaded, using defaults`)
            this.#initializeDefaultModuleConfig(mergedConfig, keys)
          }
        } catch (err) {
          error(`[Config] Error loading module ${moduleName}: ${err.message}`)
          failedModules.push(moduleName)

          // Initialize with defaults even on error
          this.#initializeDefaultModuleConfig(mergedConfig, keys)
        }
      }

      this.config = mergedConfig

      log(`[Config] Loaded ${loadedModules} module(s) successfully`)
      if (failedModules.length > 0) {
        log(`[Config] Failed to load ${failedModules.length} module(s): ${failedModules.join(', ')}`)
      }
    } catch (err) {
      error(`[Config] Critical error during modular load: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] Using default configuration')

      this.config = {
        server: {
          pid: null,
          started: null,
          watchdog: null
        }
      }
    }
  }

  // Save modular configuration - only writes changed modules
  #saveModular() {
    if (!this.#configDir) {
      error('[Config] Error: Config directory not initialized')
      return
    }

    // Ensure config directory exists
    if (!fs.existsSync(this.#configDir)) {
      try {
        fs.mkdirSync(this.#configDir, {recursive: true})
        log(`[Config] Created config directory: ${this.#configDir}`)
      } catch (err) {
        error(`[Config] Failed to create config directory: ${err.message}`)
        error(`[Config] Error code: ${err.code}`)
        return
      }
    }

    let failedModules = []
    let successfulSaves = 0

    // Iterate through module mapping and save changed modules
    for (const [moduleName, keys] of Object.entries(this.#moduleMap)) {
      // Only write modules that have changed
      if (!this.#moduleChanged[moduleName]) {
        continue
      }

      const moduleFile = path.join(this.#configDir, moduleName + '.json')
      const moduleData = {}

      // Extract relevant config keys for this module
      for (const key of keys) {
        if (this.config[key] !== undefined) {
          moduleData[key] = this.config[key]
        }
      }

      // Use atomic write for each module file
      try {
        this.#atomicWrite(moduleFile, moduleData)
        // Clear the changed flag after successful write
        this.#moduleChanged[moduleName] = false
        successfulSaves++
      } catch (err) {
        // Handle individual module save failures without stopping other saves
        error(`[Config] Failed to save module ${moduleName}: ${err.message}`)
        error(`[Config] Error code: ${err.code}`)

        failedModules.push(moduleName)
        // Don't clear the changed flag so we can retry on next save
      }
    }

    if (failedModules.length > 0) {
      log(`[Config] Partial save failure: ${failedModules.length} module(s) failed, ${successfulSaves} succeeded`)
      log(`[Config] Failed modules: ${failedModules.join(', ')}`)
    }
  }

  init() {
    try {
      this.#dir = path.join(os.homedir(), '.odac')
      this.#configDir = path.join(this.#dir, 'config')

      // Ensure base directory exists
      if (!fs.existsSync(this.#dir)) {
        try {
          fs.mkdirSync(this.#dir)
          log(`[Config] Created base directory: ${this.#dir}`)
        } catch (mkdirErr) {
          error(`[Config] Failed to create base directory: ${mkdirErr.message}`)
          error(`[Config] Error code: ${mkdirErr.code}`)
          if (mkdirErr.code === 'EACCES' || mkdirErr.code === 'EPERM') {
            error('[Config] Permission denied - check file system permissions')
          }
          throw mkdirErr
        }
      }

      // Ensure config directory exists for new installs
      if (!fs.existsSync(this.#configDir)) {
        try {
          fs.mkdirSync(this.#configDir, {recursive: true})
          log(`[Config] Created config directory: ${this.#configDir}`)
        } catch (mkdirErr) {
          error(`[Config] Failed to create config directory: ${mkdirErr.message}`)
          throw mkdirErr
        }
      }

      log('[Config] Loading modular configuration...')
      this.#loadModular()

      // Ensure config structure exists after loading
      if (!this.config || typeof this.config !== 'object') {
        log('[Config] Config object invalid, initializing with defaults')
        this.config = {}
      }
      if (!this.config.server || typeof this.config.server !== 'object') {
        log('[Config] Server config missing, initializing with defaults')
        this.config.server = {
          pid: null,
          started: null,
          watchdog: null
        }
      }

      // Set up auto-save interval
      if (process.mainModule && process.mainModule.path && !process.mainModule.path.includes('node_modules/odac/bin')) {
        setInterval(() => this.#save(), 500).unref()
        this.config = this.#proxy(this.config)
      }

      // Update OS and arch information
      if (
        !this.config.server.os ||
        this.config.server.os != os.platform() ||
        !this.config.server.arch ||
        this.config.server.arch != os.arch()
      ) {
        this.config.server.os = os.platform()
        this.config.server.arch = os.arch()
      }

      log('[Config] Initialization completed successfully')
    } catch (err) {
      error(`[Config] Critical initialization error: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] Stack trace:', err.stack)

      // Ensure we have a valid config object even in worst case
      if (!this.config || typeof this.config !== 'object') {
        error('[Config] Creating emergency default configuration')
        this.config = {
          server: {
            pid: null,
            started: null,
            watchdog: null,
            os: os.platform(),
            arch: os.arch()
          }
        }
      }

      error('[Config] System initialized with minimal configuration')
      error('[Config] Please check file system permissions and disk space')
    }
  }

  #proxy(target, parentKey = null) {
    if (typeof target !== 'object' || target === null) return target

    const handler = {
      get: (obj, prop) => {
        const value = obj[prop]
        if (typeof value === 'object' && value !== null) {
          // Pass the top-level key down for tracking nested changes
          const topKey = parentKey || prop
          return this.#proxy(value, topKey)
        }
        return value
      },
      set: (obj, prop, value) => {
        // Mark config as changed
        this.#changed = true

        // Track which module this change belongs to
        // Use parentKey if we're in a nested object, otherwise use the current prop
        const topKey = parentKey || prop
        const moduleName = this.#getModuleForKey(topKey)
        if (moduleName) {
          this.#moduleChanged[moduleName] = true
        }

        // Set the value, wrapping objects/arrays in proxy for nested tracking
        if (typeof value === 'object' && value !== null) {
          obj[prop] = this.#proxy(value, topKey)
        } else {
          obj[prop] = value
        }

        return true
      },
      deleteProperty: (obj, prop) => {
        // Mark config as changed
        this.#changed = true

        // Track which module this change belongs to
        const topKey = parentKey || prop
        const moduleName = this.#getModuleForKey(topKey)
        if (moduleName) {
          this.#moduleChanged[moduleName] = true
        }

        delete obj[prop]
        return true
      }
    }

    return new Proxy(target, handler)
  }

  reload() {
    log('[Config] Reloading configuration...')

    try {
      log('[Config] Reloading modular configuration')
      this.#loadModular()

      // Reset module change tracking flags after reload
      this.#moduleChanged = {}

      log('[Config] Configuration reloaded successfully')
    } catch (err) {
      error(`[Config] Failed to reload configuration: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] Keeping existing configuration in memory')

      // Ensure we still have a valid config
      if (!this.config || typeof this.config !== 'object') {
        error('[Config] Configuration corrupted, initializing with defaults')
        this.config = {
          server: {
            pid: null,
            started: null,
            watchdog: null,
            os: os.platform(),
            arch: os.arch()
          }
        }
      }
    }
  }

  #save() {
    // Maintain existing save debouncing and change detection
    if (this.#saving || !this.#changed) return
    this.#changed = false
    this.#saving = true

    try {
      this.#saveModular()
    } catch (err) {
      error(`[Config] Save operation failed: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)

      // Mark as changed again so we can retry on next interval
      this.#changed = true
      log('[Config] Configuration changes will be retried on next save interval')
    } finally {
      this.#saving = false
    }
  }
}

module.exports = Config
