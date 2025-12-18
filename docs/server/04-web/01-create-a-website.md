## 🌐 Create a Website
This command helps you set up a new website on your server. Odac will ask for the domain name and the path to the website's files.

### Interactive Usage
```bash
odac web create
```
After running the command, you will be prompted to enter the following information:
- **Domain Name:** The primary domain for your website (e.g., `example.com`).
- **Path:** The absolute path to your website's root directory (e.g., `/var/www/example.com`). If you leave this blank, Odac will suggest a default path based on the domain name.

### Single-Line Usage with Prefixes
```bash
# Specify domain directly
odac web create -d example.com

# Or use long form prefix
odac web create --domain example.com
```

### Available Prefixes
- `-d`, `--domain`: Domain name for the new website

### Interactive Example
```bash
$ odac web create
> Enter the domain name: example.com
> Enter the path to the website (/home/user/example.com/):
```

### Single-Line Example
```bash
$ odac web create -d example.com
✓ Website created successfully for example.com
```

