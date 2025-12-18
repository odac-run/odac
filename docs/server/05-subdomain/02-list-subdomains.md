## 📋 List Subdomains
To see a list of all subdomains configured for a specific domain, use the `list` command.

### Interactive Usage
```bash
odac subdomain list
```
This command will prompt you to enter the main domain name for which you want to list the subdomains.

### Single-Line Usage with Prefixes
```bash
# Specify domain directly
odac subdomain list -d example.com

# Or use long form prefix
odac subdomain list --domain example.com
```

### Available Prefixes
- `-d`, `--domain`: Domain name to list subdomains for

### Interactive Example
```bash
$ odac subdomain list
> Enter the domain name: example.com
```

### Single-Line Example
```bash
$ odac subdomain list -d example.com
```

This will display a list of all subdomains associated with `example.com`.
