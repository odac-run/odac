## ⚙️ Start a New Service

To start a new service, use the `run` command followed by the path to your application's entry file.

### Usage
```bash
candy run <file>
```

### Arguments
- `<file>`: The path to the script or application entry point you want to run. This can be an absolute path or a relative path from your current directory.

### Examples

**Absolute path:**
```bash
candy run /path/to/your/app/index.js
```

**Relative path from current directory:**
```bash
candy run index.js
candy run ./src/server.js
candy run ../other-project/app.js
```

**Multiple services:**
```bash
# Start multiple services in sequence
candy run ./api/index.js
candy run ./worker/processor.js
candy run ./scheduler/cron.js
```

### Path Resolution
- **Absolute paths**: Start with `/` (Linux/macOS) or drive letter (Windows)
- **Relative paths**: Resolved from your current working directory
- **Automatic conversion**: Relative paths are automatically converted to absolute paths internally

### Service Management
Once started, Odac will:
- Monitor the service continuously
- Automatically restart it if it crashes
- Assign it a unique service ID for management
- Log all output for debugging

You can view service status using:
```bash
candy monit    # Real-time monitoring
candy          # Quick status overview
```

### Service Deletion
To remove a running service, use:
```bash
candy service delete -i <service-name-or-id>
```
