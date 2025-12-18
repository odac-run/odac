## ⚡ CLI Quick Reference

A compact reference for all Odac CLI commands with their prefix arguments.

### Basic Commands
```bash
candy                    # Show server status
candy restart            # Restart server
candy monit              # Monitor services
candy debug              # View live logs
candy help               # Show help
```

### Authentication
```bash
candy auth [-k|--key] <key>
```

### Services
```bash
candy run <file>                           # Start new service
candy service delete [-i|--id] <service>  # Delete service
```

### Websites
```bash
candy web create [-d|--domain] <domain>   # Create website
candy web delete [-d|--domain] <domain>   # Delete website
candy web list                             # List websites
```

### Subdomains
```bash
candy subdomain create [-s|--subdomain] <subdomain>  # Create subdomain
candy subdomain delete [-s|--subdomain] <subdomain>  # Delete subdomain
candy subdomain list [-d|--domain] <domain>          # List subdomains
```

### SSL Certificates
```bash
candy ssl renew [-d|--domain] <domain>    # Renew SSL certificate
```

### Mail Accounts
```bash
candy mail create [-e|--email] <email> [-p|--password] <password>  # Create account
candy mail delete [-e|--email] <email>                             # Delete account
candy mail list [-d|--domain] <domain>                             # List accounts
candy mail password [-e|--email] <email> [-p|--password] <password> # Change password
```

### Common Prefixes
| Prefix | Long Form | Description |
|--------|-----------|-------------|
| `-d` | `--domain` | Domain name |
| `-e` | `--email` | Email address |
| `-p` | `--password` | Password |
| `-s` | `--subdomain` | Subdomain name |
| `-i` | `--id` | Service ID/name |
| `-k` | `--key` | Authentication key |

### Usage Patterns

**Interactive Mode:**
```bash
candy web create
# Prompts for domain name
```

**Single-Line Mode:**
```bash
candy web create -d example.com
# No prompts, immediate execution
```

**Mixed Mode:**
```bash
candy mail create -e user@example.com
# Prompts only for password
```

### Automation Examples
```bash
# Batch create email accounts
candy mail create -e admin@example.com -p admin123
candy mail create -e support@example.com -p support456

# Set up multiple subdomains
candy subdomain create -s blog.example.com
candy subdomain create -s api.example.com
candy subdomain create -s shop.example.com

# Renew multiple SSL certificates
candy ssl renew -d example.com
candy ssl renew -d api.example.com
```

### Tips
- Use single-line mode for scripts and automation
- Use interactive mode for one-off operations
- Combine both modes as needed
- All commands support `--help` for detailed information