## ⚡ CLI Quick Reference

A compact reference for all Odac CLI commands with their prefix arguments.

### Basic Commands
```bash
odac                    # Show server status
odac restart            # Restart server
odac monit              # Monitor applications
odac debug              # View live logs
odac help               # Show help
```

### Authentication
```bash
odac auth [-k|--key] <key>
```


### Applications
```bash
odac app api [-i|--id] <app> [--allow <actions>|--all|--off] # Grant API access
odac app create [-n|--name] <name> [-u|--url] <gitUrl>  # Create app
odac app delete [-i|--id] <app>                          # Delete app
odac app device add [-a|--app] <app> [-d|--device] <path> # Connect device
odac app device delete [-a|--app] <app> [-d|--device] <path> # Disconnect device
odac app gpu [-i|--id] <app> [--nvidia|--amd|--intel] [--count <n>] [--optional] [--off] # Reserve GPUs
odac app isolate [-i|--id] <app> [--off]                 # Block outbound access
odac app list                                            # List apps
odac app network [-i|--id] <app> [--host|--bridge]       # Set network mode
odac app privileged [-i|--id] <app> [--root|--full|--off] # Grant elevated access
odac app restart [-i|--id] <app>                         # Restart app
```

### Domains
```bash
odac domain add [-d|--domain] <domain> [-a|--app] <appId>  # Add domain
odac domain delete [-d|--domain] <domain>                    # Delete domain
odac domain list [-a|--app] <appId>                          # List domains
```



### SSL Certificates
```bash
odac ssl renew [-d|--domain] <domain>    # Renew SSL certificate
```

### Mail Accounts
```bash
odac mail create [-e|--email] <email> [-p|--password] <password>  # Create account
odac mail delete [-e|--email] <email>                             # Delete account
odac mail list [-d|--domain] <domain>                             # List accounts
odac mail password [-e|--email] <email> [-p|--password] <password> # Change password
```

### Backups
```bash
odac backup target add --name <n> (--disk <path>|--smb //h/s|--nfs h:/e|--s3 <bucket> --endpoint <host>|--webdav <url>) # Add target, shows its key
odac backup target list|update|delete|adopt|servers [-t|--target] <target> # Manage targets
odac backup key [-t|--target] <target> [--out <file>]           # Show a target's key again
odac backup job create --name <n> --target <t> (--apps a,b|--all) [--server] [--every 6h] [--density low|normal|high] [--max-age <days>] [--budget 50g]
odac backup job list|update|delete|run|retired|purge [-j|--job] <job> # Manage jobs
odac backup status                                              # Schedule, targets, verification
odac backup history [-j|--job] <job> [--limit <n>]             # Finished runs
odac backup coverage                                           # What is unprotected or damaged
odac backup points [-t|--target] <t> [--app <app>] [--server <id>] # Restore points
odac backup restore apps [-t] <t> --run <run> [--apps a,b] [--in-place|--new-app] [--name old=new] # Restore apps
odac backup restore list|cancel|state                          # Restores, ODAC's own state
```

### Snapshots
```bash
odac snapshot list [--app <app>]                 # Restore points
odac snapshot status                             # Disk share and warnings
odac snapshot history [--app <app>]              # Snapshots taken and given up
odac snapshot take --app <app>                   # Take one now
odac snapshot restore [--run <run>] [--in-place|--new-app] # Restore (in place by default)
odac snapshot delete --run <run>                 # Delete one
odac snapshot settings [--on|--off] [--max-disk <percent>] # Settings
```

### Common Prefixes
| Prefix | Long Form | Description |
|--------|-----------|-------------|
| `-d` | `--domain` | Domain name |
| `-e` | `--email` | Email address |
| `-p` | `--password` | Password |

| `-i` | `--id` | Project ID/name |
| `-k` | `--key` | Authentication key |

### Usage Patterns

**Interactive Mode:**
```bash
odac app create
# Prompts for app details
```

**Single-Line Mode:**
```bash
odac app create -n my-app -u https://github.com/user/repo.git
# No prompts, immediate execution
```

**Mixed Mode:**
```bash
odac app create -n my-app
# Prompts only for URL/repo
```

### Automation Examples
```bash
# Batch create email accounts
odac mail create -e admin@example.com -p admin123
odac mail create -e support@example.com -p support456

# Set up multiple subdomains
odac domain add -d blog.example.com -i my-app
odac domain add -d api.example.com -i my-app
odac domain add -d shop.example.com -i my-app

# Renew multiple SSL certificates
odac ssl renew -d example.com
odac ssl renew -d api.example.com
```

### Tips
- Use single-line mode for scripts and automation
- Use interactive mode for one-off operations
- Combine both modes as needed
- All commands support `--help` for detailed information