## 🚀 Create an App
This command helps you set up a new application on your server. Odac will ask for the app type, name, and either a path or git URL depending on the type.

### Interactive Usage
```bash
odac app create
```
After running the command, you will be prompted to enter the following information:
- **App Type/Repo:** The type of the app or a Git URL (e.g., `git`, `github`, or just a URL).
- **Name:** The internal name for your app (e.g., `my-cool-app`).
- **URL/Path:** If you chose `git`, you'll be asked for the Git URL.

### Single-Line Usage with Prefixes
```bash
# Specify name and git URL directly
odac app create -n my-app -u https://github.com/user/repo.git

# Or use long form prefix
odac app create --name my-app --url https://github.com/user/repo.git
```

### Available Prefixes
- `-n`, `--name`: Internal name for the new app
- `-u`, `--url`: Git repository URL
- `-b`, `--branch`: Specific branch to clone
- `-t`, `--type`: App type (e.g., `git`, `github`, `app`)
- `-D`, `--dev`: Run in development mode

### Interactive Example
```bash
$ odac app create
> Enter the app type or repo: git
> Enter Git URL: https://github.com/user/my-node-app.git
> Enter the app name (my-node-app):
```

### Single-Line Example
```bash
$ odac app create -n my-app -u https://github.com/user/repo.git
✓ Application created successfully: my-app
```

> 📝 **Note:** After creating an app, you can link a domain to it using the [Add a Domain](../06-domain/01-add-a-domain.md) command.

> 📝 **Note:** An app that needs more of the kernel than the container defaults give it (a VPN, say) declares that on the create payload: see [Capabilities & Sysctls](10-kernel-capabilities.md) for `caps`, `sysctls` and UDP ports. An app that moves data between its processes through shared memory (Frigate, say) sizes its `/dev/shm` the same way: see [Shared Memory](11-shared-memory.md) for `shmSize`.
