## 🔒 Renew an SSL Certificate
This command attempts to renew the SSL certificate for a given domain. This can be useful if a certificate is close to expiring and you want to force an early renewal.

### Interactive Usage
```bash
odac ssl renew
```
After running the command, you will be prompted to enter the domain name for which you want to renew the SSL certificate.

### Single-Line Usage with Prefixes
```bash
# Specify domain directly
odac ssl renew -d example.com

# Or use long form prefix
odac ssl renew --domain example.com
```

### Available Prefixes
- `-d`, `--domain`: Domain name to renew SSL certificate for

### Interactive Example
```bash
$ odac ssl renew
> Enter the domain name: example.com
```

### Single-Line Example
```bash
$ odac ssl renew -d example.com
✓ SSL certificate renewal initiated for example.com
```

Odac will then handle the process of validating your domain and renewing the certificate.
