const fs = require('fs')
const os = require('os')
const path = require('path')

const {log, error} = Candy.core('Log', false).init('Config')

class Config {
  #dir
  #file
  #configDir
  #loaded = false
  #saving = false
  #changed = false
  #moduleChanged = {}
  #isModular = false
  config = {
    server: {
      pid: null,
      started: null,
      watchdog: null
    }
  }

  // Module mapping configuration - defines which config keys belong to which module files
  #moduleMap = {
    server: ['server'],
    web: ['websites', 'web'],
    service: ['services'],
    ssl: ['ssl'],
    mail: ['mail'],
    dns: ['dns'],
    api: ['api'],
    firewall: ['firewall'],
    hub: ['hub']
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
        } else if (key === 'services') {
          config[key] = []
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

  // Detect configuration format: 'modular', 'single', or 'new'
  #detectConfigFormat() {
    const modularExists = fs.existsSync(this.#configDir)
    const singleExists = fs.existsSync(this.#file)

    if (modularExists) {
      return 'modular'
    } else if (singleExists) {
      return 'single'
    } else {
      return 'new' // Fresh installation
    }
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
      this.#loaded = true

      log(`[Config] Loaded ${loadedModules} module(s) successfully`)
      if (failedModules.length > 0) {
        log(`[Config] Failed to load ${failedModules.length} module(s): ${failedModules.join(', ')}`)
      }
    } catch (err) {
      error(`[Config] Critical error during modular load: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] Attempting to fall back to single-file mode')

      // Try to fall back to single-file if it exists
      if (fs.existsSync(this.#file)) {
        log('[Config] Single-file config found, attempting to load')
        this.#isModular = false
        this.#load()
      } else {
        error('[Config] No fallback available, using default configuration')
        this.config = {
          server: {
            pid: null,
            started: null,
            watchdog: null
          }
        }
        this.#loaded = true
      }
    }
  }

  // Migrate from single-file to modular format
  #migrate() {
    log('[Config] Starting migration from single-file to modular configuration...')

    try {
      // 1. Create config directory if it doesn't exist
      if (!fs.existsSync(this.#configDir)) {
        try {
          fs.mkdirSync(this.#configDir, {recursive: true})
          log(`[Config] Created config directory: ${this.#configDir}`)
        } catch (mkdirErr) {
          error(`[Config] Failed to create config directory: ${mkdirErr.message}`)
          error(`[Config] Error code: ${mkdirErr.code}`)
          if (mkdirErr.code === 'EACCES' || mkdirErr.code === 'EPERM') {
            error('[Config] Permission denied - check file system permissions')
          }
          throw mkdirErr
        }
      }

      // 2. Create backup of original config.json as config.json.pre-modular
      const preModularBackup = this.#file + '.pre-modular'
      if (fs.existsSync(this.#file) && !fs.existsSync(preModularBackup)) {
        try {
          fs.copyFileSync(this.#file, preModularBackup)
          log(`[Config] Created pre-migration backup: ${preModularBackup}`)
        } catch (backupErr) {
          error(`[Config] Failed to create pre-migration backup: ${backupErr.message}`)
          error(`[Config] Error code: ${backupErr.code}`)
          // Continue anyway - migration can proceed without backup
          log('[Config] Continuing migration without backup')
        }
      }

      // 3. Store original config for verification (only keys that actually exist)
      const originalConfig = JSON.parse(JSON.stringify(this.config))
      log(`[Config] Original config keys: ${Object.keys(originalConfig).join(', ')}`)

      // 4. Split config object by module mapping and write each module
      let successfulWrites = 0
      let failedWrites = []

      for (const [moduleName, keys] of Object.entries(this.#moduleMap)) {
        const moduleData = {}
        let hasData = false

        // Extract relevant config keys for this module
        for (const key of keys) {
          if (this.config[key] !== undefined) {
            moduleData[key] = this.config[key]
            hasData = true
          }
        }

        // Write module file if it has any keys (even if empty objects/arrays)
        if (hasData) {
          const moduleFile = path.join(this.#configDir, moduleName + '.json')

          try {
            // Write each module to its respective file using atomic write
            this.#atomicWrite(moduleFile, moduleData)
            log(`[Config] Created module file: ${moduleName}.json`)
            successfulWrites++
          } catch (err) {
            error(`[Config] Failed to write module ${moduleName}.json: ${err.message}`)
            error(`[Config] Error code: ${err.code}`)
            failedWrites.push(moduleName)

            if (err.code === 'EACCES' || err.code === 'EPERM') {
              error(`[Config] Permission denied for ${moduleFile}`)
            } else if (err.code === 'ENOSPC') {
              error('[Config] No space left on device')
            }

            throw new Error(`Migration failed while writing ${moduleName}.json: ${err.message}`)
          }
        }
      }

      log(`[Config] Successfully wrote ${successfulWrites} module file(s)`)

      // 5. Verify migration
      const verificationResult = this.#verifyMigration(originalConfig)

      if (verificationResult.success) {
        log('[Config] Migration completed successfully')
        log('[Config] Data integrity verified')
        this.#isModular = true
        return true
      } else {
        error(`[Config] Migration verification failed: ${verificationResult.error}`)
        throw new Error(`Migration verification failed: ${verificationResult.error}`)
      }
    } catch (err) {
      error(`[Config] Migration failed: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] System will remain in single-file mode')
      // Rollback will be handled by the caller
      return false
    }
  }

  // Verify migration by loading modular config and comparing with original
  #verifyMigration(originalConfig) {
    try {
      log('Verifying migration data integrity...')

      // Load modular config back into memory
      const verifyConfig = {
        server: {
          pid: null,
          started: null,
          watchdog: null
        }
      }

      // Load all module files
      for (const [moduleName, keys] of Object.entries(this.#moduleMap)) {
        const moduleData = this.#loadModuleFile(moduleName)

        if (moduleData && typeof moduleData === 'object') {
          for (const key of keys) {
            if (moduleData[key] !== undefined) {
              verifyConfig[key] = moduleData[key]
            }
          }
        }
      }

      // Only verify keys that exist in original config
      // Missing keys in both configs are acceptable (e.g., DNS not configured yet)
      for (const key of Object.keys(originalConfig)) {
        if (!(key in verifyConfig)) {
          return {
            success: false,
            error: `Data mismatch: ${key} exists in original but missing after migration`
          }
        }

        const comparison = this.#deepCompare(originalConfig[key], verifyConfig[key], key)
        if (!comparison.equal) {
          return {
            success: false,
            error: `Data mismatch in ${key}: ${comparison.differences.join(', ')}`
          }
        }
      }

      log('Migration verification passed - data integrity confirmed')
      return {success: true}
    } catch (err) {
      return {
        success: false,
        error: `Verification error: ${err.message}`
      }
    }
  }

  // Deep compare two objects and return differences
  #deepCompare(obj1, obj2, path = '') {
    const differences = []

    // Check if both are objects
    if (typeof obj1 !== typeof obj2) {
      differences.push(`${path}: type mismatch (${typeof obj1} vs ${typeof obj2})`)
      return {equal: false, differences}
    }

    // Handle null values
    if (obj1 === null || obj2 === null) {
      if (obj1 !== obj2) {
        differences.push(`${path}: null mismatch`)
        return {equal: false, differences}
      }
      return {equal: true, differences}
    }

    // Handle arrays
    if (Array.isArray(obj1) && Array.isArray(obj2)) {
      if (obj1.length !== obj2.length) {
        differences.push(`${path}: array length mismatch (${obj1.length} vs ${obj2.length})`)
        return {equal: false, differences}
      }

      for (let i = 0; i < obj1.length; i++) {
        const result = this.#deepCompare(obj1[i], obj2[i], `${path}[${i}]`)
        if (!result.equal) {
          differences.push(...result.differences)
        }
      }

      return {equal: differences.length === 0, differences}
    }

    // Handle objects
    if (typeof obj1 === 'object' && typeof obj2 === 'object') {
      const keys1 = Object.keys(obj1)
      const keys2 = Object.keys(obj2)

      // Check for missing keys
      for (const key of keys1) {
        if (!(key in obj2)) {
          differences.push(`${path}.${key}: missing in second object`)
        }
      }

      for (const key of keys2) {
        if (!(key in obj1)) {
          differences.push(`${path}.${key}: missing in first object`)
        }
      }

      // Compare common keys
      for (const key of keys1) {
        if (key in obj2) {
          const newPath = path ? `${path}.${key}` : key
          const result = this.#deepCompare(obj1[key], obj2[key], newPath)
          if (!result.equal) {
            differences.push(...result.differences)
          }
        }
      }

      return {equal: differences.length === 0, differences}
    }

    // Handle primitive values
    if (obj1 !== obj2) {
      differences.push(`${path}: value mismatch (${obj1} vs ${obj2})`)
      return {equal: false, differences}
    }

    return {equal: true, differences}
  }

  // Rollback to single-file mode if migration fails
  #rollbackMigration() {
    log('[Config] Rolling back to single-file mode...')

    try {
      // Remove modular config directory if it exists
      if (fs.existsSync(this.#configDir)) {
        try {
          fs.rmSync(this.#configDir, {recursive: true, force: true})
          log('[Config] Removed modular config directory')
        } catch (err) {
          error(`[Config] Failed to remove config directory: ${err.message}`)
        }
      }

      // Restore from pre-modular backup if it exists
      const preModularBackup = this.#file + '.pre-modular'
      if (fs.existsSync(preModularBackup)) {
        fs.copyFileSync(preModularBackup, this.#file)
        log('[Config] Restored original config.json from pre-modular backup')
      }

      this.#isModular = false
      log('[Config] Rollback completed - system is in single-file mode')
    } catch (err) {
      error(`[Config] Rollback failed: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)
      error('[Config] Manual intervention may be required')
      error(`[Config] Config directory: ${this.#configDir}`)
      error(`[Config] Config file: ${this.#file}`)
    }
  }

  // Fallback to single-file mode when modular operations fail
  #fallbackToSingleFile() {
    log('[Config] Initiating fallback to single-file mode...')

    try {
      // Switch to single-file mode
      this.#isModular = false

      // Attempt to save current config to single file
      try {
        this.#saveSingleFile()
        log('[Config] Successfully saved config to single file')
        log('[Config] System is now operating in single-file mode')
      } catch (saveErr) {
        error(`[Config] Failed to save to single file: ${saveErr.message}`)
        error(`[Config] Error code: ${saveErr.code}`)

        // Provide specific guidance based on error type
        if (saveErr.code === 'EACCES' || saveErr.code === 'EPERM') {
          error('[Config] Permission denied - check file system permissions')
          error(`[Config] File path: ${this.#file}`)
        } else if (saveErr.code === 'ENOSPC') {
          error('[Config] No space left on device')
        } else if (saveErr.code === 'EROFS') {
          error('[Config] File system is read-only')
        }

        error('[Config] WARNING: Unable to save configuration')
        error('[Config] Configuration changes may be lost')
      }
    } catch (err) {
      error(`[Config] Fallback to single-file mode failed: ${err.message}`)
      error('[Config] System may be in an inconsistent state')
      error('[Config] Please check file system permissions and disk space')
    }
  }

  // Save modular configuration - only writes changed modules
  #saveModular() {
    if (!this.#configDir) {
      error('[Config] Error: Config directory not initialized')
      error('[Config] Falling back to single-file mode')
      this.#fallbackToSingleFile()
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
        if (err.code === 'EACCES' || err.code === 'EPERM') {
          error('[Config] Permission denied - check file system permissions')
        }
        error('[Config] Falling back to single-file mode')
        this.#fallbackToSingleFile()
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

        // Provide specific error guidance
        if (err.code === 'EACCES' || err.code === 'EPERM') {
          error(`[Config] Permission denied for ${moduleFile}`)
          error('[Config] Check file system permissions for the config directory')
        } else if (err.code === 'ENOSPC') {
          error('[Config] No space left on device')
        } else if (err.code === 'EROFS') {
          error('[Config] File system is read-only')
        }

        failedModules.push(moduleName)
        // Don't clear the changed flag so we can retry on next save
      }
    }

    // If all module saves failed and we had changes to save, fall back to single-file
    if (failedModules.length > 0 && successfulSaves === 0) {
      error(`[Config] All modular saves failed (${failedModules.length} modules)`)
      error('[Config] Falling back to single-file mode to prevent data loss')
      this.#fallbackToSingleFile()
    } else if (failedModules.length > 0) {
      log(`[Config] Partial save failure: ${failedModules.length} module(s) failed, ${successfulSaves} succeeded`)
      log(`[Config] Failed modules: ${failedModules.join(', ')}`)
    }
  }

  init() {
    try {
      this.#dir = path.join(os.homedir(), '.candypack')
      this.#file = path.join(this.#dir, 'config.json')
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

      // Detect configuration format and load accordingly
      const format = this.#detectConfigFormat()
      log(`[Config] Detected configuration format: ${format}`)

      switch (format) {
        case 'modular':
          // Load from modular config directory
          log('[Config] Loading modular configuration...')
          this.#isModular = true
          this.#loadModular()
          break

        case 'single': {
          // Load single-file config and migrate to modular
          log('[Config] Detected single-file configuration, loading and migrating...')
          try {
            this.#load()
          } catch (loadErr) {
            error(`[Config] Failed to load single-file config: ${loadErr.message}`)
            error('[Config] Initializing with default configuration')
            this.config = {
              server: {
                pid: null,
                started: null,
                watchdog: null
              }
            }
            this.#loaded = true
          }

          // Attempt migration to modular format
          const migrationSuccess = this.#migrate()

          if (migrationSuccess) {
            this.#isModular = true
            log('[Config] Successfully migrated to modular configuration')
          } else {
            // Migration failed - rollback and stay in single-file mode
            error('[Config] Migration failed, rolling back to single-file mode')
            this.#rollbackMigration()
            this.#isModular = false
          }
          break
        }

        case 'new':
          // Fresh installation - create modular structure from start
          log('[Config] New installation detected, creating modular configuration structure...')
          this.#isModular = true

          // Create config directory
          if (!fs.existsSync(this.#configDir)) {
            try {
              fs.mkdirSync(this.#configDir, {recursive: true})
              log(`[Config] Created config directory: ${this.#configDir}`)
            } catch (mkdirErr) {
              error(`[Config] Failed to create config directory: ${mkdirErr.message}`)
              error(`[Config] Error code: ${mkdirErr.code}`)
              if (mkdirErr.code === 'EACCES' || mkdirErr.code === 'EPERM') {
                error('[Config] Permission denied - check file system permissions')
              }
              log('[Config] Falling back to single-file mode')
              this.#isModular = false
            }
          }

          // Initialize with default config structure
          this.config = {
            server: {
              pid: null,
              started: null,
              watchdog: null
            }
          }
          this.#loaded = true
          break
      }

      // Ensure config structure exists after loading (server object, etc.)
      // This guarantees the system never enters a broken state
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

      // Set up auto-save interval with modular support
      // Handle process.mainModule safely
      if (process.mainModule && process.mainModule.path && !process.mainModule.path.includes('node_modules/candypack/bin')) {
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

      this.#loaded = true
      this.#isModular = false
      error('[Config] System initialized with minimal configuration')
      error('[Config] Please check file system permissions and disk space')
    }
  }

  #load() {
    if (this.#saving && this.#loaded) return
    if (!fs.existsSync(this.#file)) {
      this.#loaded = true
      return
    }
    let data = fs.readFileSync(this.#file, 'utf8')
    if (!data) {
      log('Error reading config file:', this.#file)
      this.#loaded = true
      this.#save()
      return
    }
    try {
      if (data.length > 2) {
        data = JSON.parse(data)
        this.#loaded = true
      }
    } catch {
      log('Error parsing config file:', this.#file)
    }
    if (!this.#loaded) {
      if (data.length > 2) {
        const backup = path.join(this.#dir, 'config-corrupted.json')
        if (fs.existsSync(this.#file)) fs.copyFileSync(this.#file, backup)
      }
      const bakDir = path.join(this.#dir, '.bak')
      const backupFile = path.join(bakDir, 'config.json.bak')
      // Try new backup location first, then fall back to old location
      const backupPath = fs.existsSync(backupFile) ? backupFile : this.#file + '.bak'
      if (fs.existsSync(backupPath)) {
        data = fs.readFileSync(backupPath, 'utf8')
        if (!data) {
          error('Error reading backup file:', backupPath)
          this.#save(true)
          return
        }
        try {
          data = JSON.parse(data)
          fs.promises.writeFile(this.#file, JSON.stringify(data, null, 4), 'utf8')
        } catch (e) {
          error(e)
          this.#save(true)
          return
        }
        if (data && typeof data === 'object') {
          this.config = data
        }
        return
      } else {
        this.config = {}
        this.#save(true)
        return
      }
    } else {
      if (data && typeof data === 'object') {
        this.config = data
      }
      return
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
      this.#loaded = false

      // Detect current format before reloading
      const format = this.#detectConfigFormat()

      if (format === 'modular' || this.#isModular) {
        log('[Config] Reloading from modular format')
        this.#loadModular()
      } else {
        log('[Config] Reloading from single-file format')
        this.#load()
      }

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

      this.#loaded = true
    }
  }

  #save() {
    // Maintain existing save debouncing and change detection
    if (this.#saving || !this.#changed) return
    this.#changed = false
    this.#saving = true

    try {
      // Check if system is in modular mode
      if (this.#isModular) {
        // Call modular save method if in modular mode
        this.#saveModular()
      } else {
        // Fall back to single-file save for backward compatibility
        this.#saveSingleFile()
      }
    } catch (err) {
      error(`[Config] Save operation failed: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)

      // Provide specific error guidance
      if (err.code === 'EACCES' || err.code === 'EPERM') {
        error('[Config] Permission denied - check file system permissions')
      } else if (err.code === 'ENOSPC') {
        error('[Config] No space left on device')
      } else if (err.code === 'EROFS') {
        error('[Config] File system is read-only')
      }

      // Mark as changed again so we can retry on next interval
      this.#changed = true
      log('[Config] Configuration changes will be retried on next save interval')
    } finally {
      this.#saving = false
    }
  }

  // Single-file save method for backward compatibility
  #saveSingleFile() {
    try {
      let json = JSON.stringify(this.config, null, 4)
      if (json.length < 3) json = '{}'

      // Write main config file
      fs.writeFileSync(this.#file, json, 'utf8')

      // Write backup file to .bak directory after a delay
      setTimeout(() => {
        try {
          const bakDir = path.join(this.#dir, '.bak')
          // Ensure .bak directory exists
          if (!fs.existsSync(bakDir)) {
            fs.mkdirSync(bakDir, {recursive: true})
          }
          fs.writeFileSync(path.join(bakDir, 'config.json.bak'), json, 'utf8')
        } catch (backupErr) {
          error(`[Config] Failed to write backup file: ${backupErr.message}`)
          error(`[Config] Error code: ${backupErr.code}`)
          // Don't throw - backup failure shouldn't stop the main save
        }
      }, 5000)
    } catch (err) {
      error(`[Config] Failed to save single-file config: ${err.message}`)
      error(`[Config] Error code: ${err.code}`)

      if (err.code === 'EACCES' || err.code === 'EPERM') {
        error(`[Config] Permission denied for ${this.#file}`)
        error('[Config] Check file system permissions')
      } else if (err.code === 'ENOSPC') {
        error('[Config] No space left on device')
      } else if (err.code === 'EROFS') {
        error('[Config] File system is read-only')
      }

      throw err
    }
  }
}

module.exports = Config
