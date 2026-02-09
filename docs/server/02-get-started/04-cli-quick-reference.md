## ⚡ CLI Quick Reference

A compact reference for all Odac CLI commands with their prefix arguments.

### Basic Commands
```bash
odac                    # Show server status
odac restart            # Restart server
odac monit              # Monitor services
odac debug              # View live logs
odac help               # Show help
```

### Authentication
```bash
odac auth [-k|--key] <key>
```

### Services
```bash
odac run <file>                           # Start new service
odac service delete [-i|--id] <service>  # Delete service
```

### Websites
```bash
odac web create [-d|--domain] <domain>   # Create website
odac web delete [-d|--domain] <domain>   # Delete website
odac web list                             # List websites
```

### Domains
```bash
odac domain add [-d|--domain] <domain> [-i|--id] <appId>  # Add domain
odac domain delete [-d|--domain] <domain>                  # Delete domain
odac domain list [-i|--id] <appId>                        # List domains
```

### Subdomains
```bash
odac subdomain create [-s|--subdomain] <subdomain>  # Create subdomain
odac subdomain delete [-s|--subdomain] <subdomain>  # Delete subdomain
odac subdomain list [-d|--domain] <domain>          # List subdomains
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
odac web create
# Prompts for domain name
```

**Single-Line Mode:**
```bash
odac web create -d example.com
# No prompts, immediate execution
```

**Mixed Mode:**
```bash
odac mail create -e user@example.com
# Prompts only for password
```

### Automation Examples
```bash
# Batch create email accounts
odac mail create -e admin@example.com -p admin123
odac mail create -e support@example.com -p support456

# Set up multiple subdomains
odac subdomain create -s blog.example.com
odac subdomain create -s api.example.com
odac subdomain create -s shop.example.com

# Renew multiple SSL certificates
odac ssl renew -d example.com
odac ssl renew -d api.example.com
```

### Tips
- Use single-line mode for scripts and automation
- Use interactive mode for one-off operations
- Combine both modes as needed
- All commands support `--help` for detailed information