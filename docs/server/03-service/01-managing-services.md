## ⚙️ Services

In Odac, **Services** are containers that run your scripts (Node.js, Python, etc.) or third-party applications (MySQL, Redis, etc.). All services run in isolated Docker containers.

### Running a Script
To run a local script, use the `service run` command:

```bash
odac service run <file>
```
Supported extensions: `.js`, `.py`, `.php`, `.rb`, `.sh`

**Examples:**
```bash
odac service run index.js
odac service run worker.py
odac service run ./scripts/cleanup.sh
```

### Installing an Application
To install a third-party application like a database, use the `service install` command:

```bash
odac service install <type>
```

**Supported Apps:**
- `mysql`: MySQL Database
- `postgres`: PostgreSQL Database
- `redis`: Redis Cache

**Example:**
```bash
odac service install mysql
```
This will automatically:
1. Pull the official Docker image.
2. Find an available port.
3. Generate secure credentials.
4. Create necessary volumes for data persistence.
5. Start the container.

### Listing Services
To see all running services (scripts and apps), use:
```bash
odac service list
```

### Deleting Services
To stop and remove a service:
```bash
odac service delete <name-or-id>
```

**Example:**
```bash
odac service delete index
odac service delete mysql-1
```
