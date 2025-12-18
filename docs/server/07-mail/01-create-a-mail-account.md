## ✉️ Create a Mail Account
This command allows you to create a new email account.

### Interactive Usage
```bash
odac mail create
```
You will be prompted to enter the new email address and a password for the account.

### Single-Line Usage with Prefixes
```bash
# Specify email and password directly
odac mail create -e user@example.com -p mypassword

# Or use long form prefixes
odac mail create --email user@example.com --password mypassword
```

### Available Prefixes
- `-e`, `--email`: Email address for the new account
- `-p`, `--password`: Password for the new account

**Note:** When using the `-p` prefix, you won't be prompted to confirm the password. In interactive mode, you'll need to enter the password twice for confirmation.
