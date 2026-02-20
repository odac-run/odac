# Add a Domain

To add a new domain to an existing application, use the `domain add` command. This will register the domain, set up necessary DNS records, and provision an SSL certificate.

### Usage

**Interactive (Recommended):**
Simply run the command and follow the prompts:
```bash
odac domain add
```

**Single-line (for automation):**
```bash
odac domain add <domain> <appId>
```
OR using prefixes:
```bash
odac domain add -d <domain> -i <appId>
```

### Parameters

- `<domain>`: The domain name you want to add (e.g., `example.com`).
- `<appId>`: The ID or name of the application the domain should point to.

### Example

```bash
odac domain add mysite.com my-web-app
```

### What happens next?

1.  **Validation:** ODAC checks if the domain format is valid and if the application exists.
2.  **DNS Setup:** ODAC automatically creates the following DNS records for your domain:
    -   **A/AAAA records** pointing to your server's IP.
    -   **CNAME record** for `www` subdomain.
    -   **MX records** for mail handling.
    -   **SPF and DMARC records** for email security.
3.  **SSL Provisioning:** ODAC initiates a Let's Encrypt SSL certificate request for the domain and its `www` subdomain.

> 📝 **Note:** For local development, you can add `localhost` as a domain. DNS and SSL provisioning will be skipped for `localhost`.
