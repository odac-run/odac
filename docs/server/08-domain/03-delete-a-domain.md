# Delete a Domain

To remove a domain from your ODAC server, use the `domain delete` command.

### Usage

**Interactive:**
```bash
odac domain delete
```

**Single-line:**
```bash
odac domain delete <domain>
```
OR using prefixes:
```bash
odac domain delete -d <domain>
```

### Parameters

- `<domain>`: The domain name you want to remove.

### Example

```bash
odac domain delete mysite.com
```

### Important Considerations

- **DNS Records:** ODAC will attempt to automatically delete the DNS records associated with the domain.
- **SSL Certificates:** The SSL certificate files will remain on the server but will no longer be renewed for this domain.
- **Application Access:** The application will no longer be accessible via this domain after deletion.
