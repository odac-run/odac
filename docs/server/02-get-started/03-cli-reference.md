## 📖 CLI Reference

This comprehensive reference covers all Odac CLI commands and their usage patterns, including both interactive and single-line modes with prefix arguments.

### Command Structure

Odac CLI follows a hierarchical command structure:
```bash
odac [command] [subcommand] [prefixes] [arguments]
```

### Prefix Arguments

Most commands support prefix arguments that allow you to provide values directly in the command line, avoiding interactive prompts. This is especially useful for automation, scripting, and quick operations.

#### Common Prefixes
- `-d`, `--domain`: Domain name
- `-e`, `--email`: Email address  
- `-p`, `--password`: Password
- `-i`, `--id`: Project ID or name
- `-k`, `--key`: Authentication key

### Authentication Commands

#### `odac auth`
Define your server to your Odac account.

**Interactive:**
```bash
odac auth
```

**Single-line:**
```bash
odac auth -k your-auth-key
odac auth --key your-auth-key
```

### Basic Server Commands

#### `odac` (no arguments)
Display server status, uptime, and statistics.

#### `odac restart`
Restart the Odac server.

#### `odac monit`
Monitor applications in real-time.

#### `odac debug`
View live server and application logs.

#### `odac help`
Display help information for all commands.
### Application Management

#### `odac app create`
Create a new application configuration.

**Interactive:**
```bash
odac app create
```

**Single-line:**
```bash
odac app create -n my-app -u https://github.com/user/repo.git
odac app create --name my-app --url https://github.com/user/repo.git
```

#### `odac app api`
Let an application call ODAC's own API over the unix socket mounted into its container. See [API Access](../03-app/08-api-access.md).

**Interactive:**
```bash
odac app api my-app
```

**Single-line:**
```bash
odac app api my-app --allow app.list,mail.send   # Only these actions
odac app api my-app --all                        # Every action (asks to confirm)
odac app api my-app --off                        # Revoke access
```

Granting requires a restart; a revoke takes effect immediately. The access is local to this server: it grants nothing in ODAC Cloud and no reach over your other servers.

#### `odac app delete`
Delete an application configuration.

**Interactive:**
```bash
odac app delete
```

**Single-line:**
```bash
odac app delete -i my-app
odac app delete --id my-app
```

#### `odac app device add`
Connect a hardware device to an application container.

**Interactive:**
```bash
odac app device add
```

**Single-line:**
```bash
odac app device add -a my-app -d /dev/ttyACM0
odac app device add --app my-app --device /dev/ttyACM0
```

#### `odac app device delete`
Disconnect a hardware device from an application container.

**Interactive:**
```bash
odac app device delete
```

**Single-line:**
```bash
odac app device delete -a my-app -d /dev/ttyACM0
odac app device delete --app my-app --device /dev/ttyACM0
```

#### `odac app gpu`
Reserve host GPUs for an application. See [GPU Reservation](../03-app/09-gpu-reservation.md).

```bash
odac app gpu my-app                     # Reserve the detected GPU, all devices
odac app gpu my-app --nvidia            # Name the runtime (--amd, --intel)
odac app gpu my-app --nvidia --count 2  # Reserve two devices
odac app gpu my-app --optional          # Use a GPU when the host has one, else CPU
odac app gpu my-app --off               # Release the reservation
```

A restart is required for the change to take effect.

#### `odac app isolate`
Cut off an application's outbound network access. See [Network Isolation](../03-app/07-network-isolation.md).

```bash
odac app isolate my-app         # No outbound network access
odac app isolate my-app --off   # Restore it
```

A restart is required for the change to take effect.

#### `odac app list`
List all configured applications.

```bash
odac app list
```

#### `odac app network`
Set an application's container network mode. See [Network Mode](../03-app/06-network-mode.md).

```bash
odac app network my-app --host     # Share the host network namespace
odac app network my-app --bridge   # ODAC's shared bridge network (default)
```

A restart is required for the change to take effect.

#### `odac app privileged`
Grant elevated access to an application (CLI-only, at your own risk). See [Privileged Access](../03-app/05-privileged-access.md).

```bash
odac app privileged my-app          # Run as root (default)
odac app privileged my-app --full   # Full Docker Privileged mode + root
odac app privileged my-app --off    # Revoke elevated access
```

#### `odac app restart`
Restart an application container.

**Interactive:**
```bash
odac app restart
```

**Single-line:**
```bash
odac app restart -i my-app
odac app restart --id my-app
```

### Domain Management

#### `odac domain add`
Add a new domain and link it to an application. This automatically sets up DNS and SSL.

**Interactive:**
```bash
odac domain add
```

**Single-line:**
```bash
odac domain add -d example.com -a my-app
odac domain add --domain example.com --app my-app
```

> Apps using host networking are refused: host mode rules out zero-downtime deploys, so a routed domain would mean a live site that only redeploys with downtime. See [Network Mode](../03-app/06-network-mode.md).

#### `odac domain delete`
Delete a domain configuration and its DNS records.

**Interactive:**
```bash
odac domain delete
```

**Single-line:**
```bash
odac domain delete -d example.com
odac domain delete --domain example.com
```

#### `odac domain list`
List all domains or filter by application.

**Interactive:**
```bash
odac domain list
```

**Single-line:**
```bash
odac domain list -a my-app
odac domain list --app my-app
```



### SSL Certificate Management

#### `odac ssl renew`
Renew SSL certificate for a domain.

**Interactive:**
```bash
odac ssl renew
```

**Single-line:**
```bash
odac ssl renew -d example.com
odac ssl renew --domain example.com
```

### Mail Account Management

#### `odac mail create`
Create a new email account.

**Interactive:**
```bash
odac mail create
```

**Single-line:**
```bash
odac mail create -e user@example.com -p password123
odac mail create --email user@example.com --password password123
```

#### `odac mail delete`
Delete an email account.

**Interactive:**
```bash
odac mail delete
```

**Single-line:**
```bash
odac mail delete -e user@example.com
odac mail delete --email user@example.com
```

#### `odac mail list`
List all email accounts for a domain.

**Interactive:**
```bash
odac mail list
```

**Single-line:**
```bash
odac mail list -d example.com
odac mail list --domain example.com
```

#### `odac mail password`
Change password for an email account.

**Interactive:**
```bash
odac mail password
```

**Single-line:**
```bash
odac mail password -e user@example.com -p newpassword
odac mail password --email user@example.com --password newpassword
```

### Backup Management

Backups go to targets you add and run on the schedule of jobs you create. A target or job is named by its id or its name with `-t`/`--target` and `-j`/`--job`; when there is only one, it is chosen for you. See [Backups and Snapshots](../08-backup/01-overview.md).

#### `odac backup target add`
Add a target: `--disk <host path>`, `--smb //<host>/<share>`, `--nfs <host>:<export>`, `--s3 <bucket> --endpoint <host>` or `--webdav <url>`. Shows the target's key once and saves the target only after you type `yes`.

```bash
odac backup target add --name usb --disk /mnt/usb-backup
odac backup target add --name nas --smb //nas.lan/backups --path odac --username odac
odac backup target add --name offsite --s3 my-bucket --endpoint s3.eu-central-1.amazonaws.com --access-key AKIA... --recovery-file ./offsite.json
```

Options: `--path`, `--username`, `--password`, `--domain`, `--version` (SMB, NFS), `--region`, `--prefix`, `--access-key`, `--secret-key`, `--no-tls` (S3), `--key` (your own key, at least 16 characters), `--adopt` (take over this server's old directory), `--recovery-file <file>`, `--yes`.

#### `odac backup target list` / `update` / `delete` / `adopt` / `servers`
```bash
odac backup target list
odac backup target update -t nas --name nas-2 --password
odac backup target delete -t usb            # its backups stay on the storage
odac backup target adopt -t nas             # write again after an owner conflict
odac backup target servers -t offsite       # server directories on the target
```

#### `odac backup key`
Show a target's key again, with what a new server needs to open its backups.

```bash
odac backup key -t offsite
odac backup key -t offsite --out ./offsite-recovery.json
```

#### `odac backup job create` / `update` / `delete` / `run` / `list`
```bash
odac backup job create --name nightly --target offsite --all --server --every 6h
odac backup job create --name db --target usb --apps postgres,api --every 1h --density high --max-age 30 --budget 50g
odac backup job update -j nightly --every 12h --no-server
odac backup job run -j nightly
odac backup job list
odac backup job delete -j db                # keeps its backups
odac backup job delete -j db --purge        # deletes them too
```

#### `odac backup job retired` / `purge`
List deleted jobs whose backups are kept, and delete those backups.

```bash
odac backup job retired
odac backup job purge -j db
```

#### `odac backup status` / `history` / `coverage`
```bash
odac backup status                      # jobs' next runs; targets' size, maintenance, verification and fair share
odac backup history -j nightly --limit 50
odac backup coverage                    # unprotected apps, damage, conflicts, orphaned servers
```

#### `odac backup points`
List the runs a restore can return to.

```bash
odac backup points -t offsite --app blog
odac backup points -t offsite --server <other server id> --key <key>
```

#### `odac backup restore apps`
Restore apps from a run, as new apps (default) or `--in-place`. Shows the plan, and for an in-place restore the data that would be lost, before asking.

```bash
odac backup restore apps -t offsite --run <run> --apps blog
odac backup restore apps -t offsite --run <run> --apps blog --in-place
odac backup restore apps -t offsite --run <run> --apps blog --name blog=blog-copy
odac backup restore apps -t offsite --server <old server id> --run <run> --apps blog
```

Options: `--skip-safety` (no safety snapshot first), `--yes`, `--no-wait`.

#### `odac backup restore list` / `cancel` / `state`
```bash
odac backup restore list
odac backup restore cancel --id <id>
odac backup restore state -t offsite       # ODAC's own state onto a fresh server
odac backup restore state --cancel
```

### Snapshot Management

Snapshots are taken by ODAC itself before deploys, deletes and updates, and periodically. See [Snapshots](../08-backup/05-snapshots.md).

```bash
odac snapshot list --app blog
odac snapshot status
odac snapshot history --app blog --limit 20
odac snapshot take --app blog
odac snapshot restore --run <run>                  # in place by default
odac snapshot restore --run <run> --new-app --name blog=blog-copy
odac snapshot delete --run <run>
odac snapshot settings --max-disk 15
odac snapshot settings --off
```

### Usage Tips

#### Automation and Scripting
Single-line commands with prefixes are perfect for automation:

```bash
#!/bin/bash
# Create multiple email accounts
odac mail create -e admin@example.com -p admin123
odac mail create -e support@example.com -p support123
odac mail create -e sales@example.com -p sales123

# Set up subdomains
odac domain add -d blog.example.com -i my-app
odac domain add -d shop.example.com -i my-app
odac domain add -d api.example.com -i my-app
```

#### Mixed Usage
You can mix interactive and single-line modes as needed:

```bash
# Specify name, but let the system prompt for other details
odac app create -n my-app
```

#### Password Security
When using password prefixes (`-p`, `--password`):
- Interactive mode requires password confirmation
- Single-line mode skips confirmation for automation
- Consider using environment variables for sensitive data in scripts

```bash
# Using environment variable
odac mail create -e user@example.com -p "$MAIL_PASSWORD"
```

### Error Handling

If a command fails or you provide invalid arguments, Odac will display helpful error messages and suggest corrections. Use `odac help [command]` to get specific help for any command.
