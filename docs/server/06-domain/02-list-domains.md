# List Domains

To view all registered domains on your ODAC server, use the `domain list` command.

### Usage

**Interactive:**
```bash
odac domain list
```

**Single-line (filter by app):**
```bash
odac domain list <appId>
```
OR using prefixes:
```bash
odac domain list -i <appId>
```

### Parameters

- `[appId]` (Optional): Filter the list to show only domains associated with a specific application.

### Example

List all domains:
```bash
odac domain list
```

List domains for a specific app:
```bash
odac domain list my-web-app
```

### Output

The command will display a table containing:
- **Domain:** The registered domain name.
- **App:** The target application ID.
- **Created:** The date when the domain was added.
- **SSL:** Current SSL certificate status and expiry date.
