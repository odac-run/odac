## 🔑 Change Account Password
This command allows you to change the password for an existing email account.

### Interactive Usage
```bash
odac mail password
```
You will be prompted to enter the email address and the new password.

### Single-Line Usage with Prefixes
```bash
# Specify email and new password directly
odac mail password -e user@example.com -p newpassword

# Or use long form prefixes
odac mail password --email user@example.com --password newpassword
```

### Available Prefixes
- `-e`, `--email`: Email address to change password for
- `-p`, `--password`: New password for the account

**Note:** When using the `-p` prefix, you won't be prompted to confirm the password. In interactive mode, you'll need to enter the password twice for confirmation.
