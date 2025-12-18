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
- `-s`, `--subdomain`: Subdomain name
- `-i`, `--id`: Service ID or name
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
Monitor websites and services in real-time.

#### `odac debug`
View live server and application logs.

#### `odac help`
Display help information for all commands.

### Service Management

#### `odac run <file>`
Add a new service by specifying the entry file path.

**Example:**
```bash
odac run /path/to/your/app.js
odac run ./index.js
```

#### `odac service delete`
Delete a running service.

**Interactive:**
```bash
odac service delete
```

**Single-line:**
```bash
odac service delete -i service-name
odac service delete --id service-name
```

### Website Management

#### `odac web create`
Create a new website configuration.

**Interactive:**
```bash
odac web create
```

**Single-line:**
```bash
odac web create -d example.com
odac web create --domain example.com
```

#### `odac web delete`
Delete a website configuration.

**Interactive:**
```bash
odac web delete
```

**Single-line:**
```bash
odac web delete -d example.com
odac web delete --domain example.com
```

#### `odac web list`
List all configured websites.

```bash
odac web list
```

### Subdomain Management

#### `odac subdomain create`
Create a new subdomain.

**Interactive:**
```bash
odac subdomain create
```

**Single-line:**
```bash
odac subdomain create -s blog.example.com
odac subdomain create --subdomain blog.example.com
```

#### `odac subdomain delete`
Delete a subdomain.

**Interactive:**
```bash
odac subdomain delete
```

**Single-line:**
```bash
odac subdomain delete -s blog.example.com
odac subdomain delete --subdomain blog.example.com
```

#### `odac subdomain list`
List all subdomains for a domain.

**Interactive:**
```bash
odac subdomain list
```

**Single-line:**
```bash
odac subdomain list -d example.com
odac subdomain list --domain example.com
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
odac subdomain create -s blog.example.com
odac subdomain create -s shop.example.com
odac subdomain create -s api.example.com
```

#### Mixed Usage
You can mix interactive and single-line modes as needed:

```bash
# Specify domain, but let the system prompt for other details
odac web create -d example.com
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
