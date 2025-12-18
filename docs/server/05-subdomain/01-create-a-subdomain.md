## 🔗 Create a Subdomain
This command allows you to create a new subdomain. Odac will automatically configure it to point to a directory with the same name inside your main domain's root directory.

### Interactive Usage
```bash
odac subdomain create
```
After running the command, you will be prompted to enter the new subdomain name, including the main domain (e.g., `blog.example.com`).

### Single-Line Usage with Prefixes
```bash
# Specify subdomain directly
odac subdomain create -s blog.example.com

# Or use long form prefix
odac subdomain create --subdomain blog.example.com
```

### Available Prefixes
- `-s`, `--subdomain`: Full subdomain name (e.g., blog.example.com)

### Interactive Example
```bash
$ odac subdomain create
> Enter the subdomain name (subdomain.example.com): blog.example.com
```

### Single-Line Example
```bash
$ odac subdomain create -s blog.example.com
✓ Subdomain blog.example.com created successfully
```
