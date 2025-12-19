## ⚙️ Start a New Service

To start a new service, use the `run` command followed by the path to your application's entry file.

### Usage
```bash
odac run <file>
```

### Arguments
- `<file>`: The path to the script or application entry point you want to run. This can be an absolute path or a relative path from your current directory.

### Examples

**Absolute path:**
```bash
odac run /path/to/your/app/index.js
```

**Relative path from current directory:**
```bash
odac run index.js
odac run ./src/server.js
odac run ../other-project/app.js
```

**Multiple services:**
```bash
# Start multiple services in sequence
odac run ./api/index.js
odac run ./worker/processor.js
odac run ./scheduler/cron.js
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
odac monit    # Real-time monitoring
odac          # Quick status overview
```

### Service Deletion
To remove a running service, use:
```bash
odac service delete -i <service-name-or-id>
```
