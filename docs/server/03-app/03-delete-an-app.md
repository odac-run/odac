## 🗑️ Delete an App
This command allows you to permanently remove an application configuration from your server.

### Usage
```bash
odac app delete
```

After running the command, you will be prompted to enter the app name or ID that you wish to delete.

### Single-Line Usage with Prefixes
```bash
# Specify name/ID directly
odac app delete -i <app-name>

# Or using the long-form prefix
odac app delete --id my-app
```

### Important Notes:
- Deleting an app will stop its process and remove its configuration.
- Associated domains might still exist, but they will no longer point to the deleted app.
