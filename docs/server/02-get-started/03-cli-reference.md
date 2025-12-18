## 📖 CLI Reference

This comprehensive reference covers all Odac CLI commands and their usage patterns, including both interactive and single-line modes with prefix arguments.

### Command Structure

Odac CLI follows a hierarchical command structure:
```bash
candy [command] [subcommand] [prefixes] [arguments]
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

#### `candy auth`
Define your server to your Odac account.

**Interactive:**
```bash
candy auth
```

**Single-line:**
```bash
candy auth -k your-auth-key
candy auth --key your-auth-key
```

### Basic Server Commands

#### `candy` (no arguments)
Display server status, uptime, and statistics.

#### `candy restart`
Restart the Odac server.

#### `candy monit`
Monitor websites and services in real-time.

#### `candy debug`
View live server and application logs.

#### `candy help`
Display help information for all commands.

### Service Management

#### `candy run <file>`
Add a new service by specifying the entry file path.

**Example:**
```bash
candy run /path/to/your/app.js
candy run ./index.js
```

#### `candy service delete`
Delete a running service.

**Interactive:**
```bash
candy service delete
```

**Single-line:**
```bash
candy service delete -i service-name
candy service delete --id service-name
```

### Website Management

#### `candy web create`
Create a new website configuration.

**Interactive:**
```bash
candy web create
```

**Single-line:**
```bash
candy web create -d example.com
candy web create --domain example.com
```

#### `candy web delete`
Delete a website configuration.

**Interactive:**
```bash
candy web delete
```

**Single-line:**
```bash
candy web delete -d example.com
candy web delete --domain example.com
```

#### `candy web list`
List all configured websites.

```bash
candy web list
```

### Subdomain Management

#### `candy subdomain create`
Create a new subdomain.

**Interactive:**
```bash
candy subdomain create
```

**Single-line:**
```bash
candy subdomain create -s blog.example.com
candy subdomain create --subdomain blog.example.com
```

#### `candy subdomain delete`
Delete a subdomain.

**Interactive:**
```bash
candy subdomain delete
```

**Single-line:**
```bash
candy subdomain delete -s blog.example.com
candy subdomain delete --subdomain blog.example.com
```

#### `candy subdomain list`
List all subdomains for a domain.

**Interactive:**
```bash
candy subdomain list
```

**Single-line:**
```bash
candy subdomain list -d example.com
candy subdomain list --domain example.com
```

### SSL Certificate Management

#### `candy ssl renew`
Renew SSL certificate for a domain.

**Interactive:**
```bash
candy ssl renew
```

**Single-line:**
```bash
candy ssl renew -d example.com
candy ssl renew --domain example.com
```

### Mail Account Management

#### `candy mail create`
Create a new email account.

**Interactive:**
```bash
candy mail create
```

**Single-line:**
```bash
candy mail create -e user@example.com -p password123
candy mail create --email user@example.com --password password123
```

#### `candy mail delete`
Delete an email account.

**Interactive:**
```bash
candy mail delete
```

**Single-line:**
```bash
candy mail delete -e user@example.com
candy mail delete --email user@example.com
```

#### `candy mail list`
List all email accounts for a domain.

**Interactive:**
```bash
candy mail list
```

**Single-line:**
```bash
candy mail list -d example.com
candy mail list --domain example.com
```

#### `candy mail password`
Change password for an email account.

**Interactive:**
```bash
candy mail password
```

**Single-line:**
```bash
candy mail password -e user@example.com -p newpassword
candy mail password --email user@example.com --password newpassword
```

### Usage Tips

#### Automation and Scripting
Single-line commands with prefixes are perfect for automation:

```bash
#!/bin/bash
# Create multiple email accounts
candy mail create -e admin@example.com -p admin123
candy mail create -e support@example.com -p support123
candy mail create -e sales@example.com -p sales123

# Set up subdomains
candy subdomain create -s blog.example.com
candy subdomain create -s shop.example.com
candy subdomain create -s api.example.com
```

#### Mixed Usage
You can mix interactive and single-line modes as needed:

```bash
# Specify domain, but let the system prompt for other details
candy web create -d example.com
```

#### Password Security
When using password prefixes (`-p`, `--password`):
- Interactive mode requires password confirmation
- Single-line mode skips confirmation for automation
- Consider using environment variables for sensitive data in scripts

```bash
# Using environment variable
candy mail create -e user@example.com -p "$MAIL_PASSWORD"
```

### Error Handling

If a command fails or you provide invalid arguments, Odac will display helpful error messages and suggest corrections. Use `candy help [command]` to get specific help for any command.
