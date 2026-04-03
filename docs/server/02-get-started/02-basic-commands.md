## 💻 Basic Commands

These are the most common commands for interacting with the Odac server.

### Check Status
To see the current status of the Odac server, including uptime and the number of active applications, simply run the `odac` command with no arguments:
```bash
odac
```

### Restart the Server
If you need to apply new configurations or restart the Odac system, you can use the `restart` command:
```bash
odac restart
```

### Monitor Applications
To get a real-time, interactive view of your running applications, use the `monit` command:
```bash
odac monit
```

### View Live Logs
For debugging purposes, you can view a live stream of all server and application logs with the `debug` command:
```bash
odac debug
```

### Get Help
To see a list of all available commands, use the `help` command:
```bash
odac help
```

### Using Prefix Arguments
Many Odac commands support prefix arguments that allow you to provide values directly in the command line, avoiding interactive prompts. This is especially useful for automation and scripting.

**Common Prefixes:**
- `-d`, `--domain`: Specify domain name
- `-e`, `--email`: Specify email address  
- `-p`, `--password`: Specify password
- `-s`, `--subdomain`: Specify subdomain
- `-i`, `--id`: Specify project ID
- `-k`, `--key`: Specify authentication key

**Example:**
```bash
# Interactive mode (prompts for input)
odac app create

# Single-line mode with prefix
odac app create -n example-app -u https://github.com/user/repo.git
```

---

**Next Steps:** For more advanced topics, such as managing applications, SSL certificates, and mail accounts, please refer to the upcoming documentation files.
