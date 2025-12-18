## 🗑️ Delete a Service

This command removes a running service from Odac's monitoring and stops its execution.

### Interactive Usage
```bash
candy service delete
```
You will be prompted to enter the service ID or name that you want to delete.

### Single-Line Usage with Prefixes
```bash
# Specify service ID or name directly
candy service delete -i my-service
candy service delete -i /path/to/app/index.js

# Or use long form prefix
candy service delete --id my-service
```

### Available Prefixes
- `-i`, `--id`: Service ID or name to delete

### Finding Service Information
To find the service ID or name, use:
```bash
candy monit    # Interactive monitoring view
candy          # Quick status with service count
```

### Interactive Example
```bash
$ candy service delete
> Enter the Service ID or Name: my-api-service
✓ Service 'my-api-service' deleted successfully
```

### Single-Line Example
```bash
$ candy service delete -i my-api-service
✓ Service 'my-api-service' deleted successfully
```

### Important Notes
- Deleting a service stops its execution immediately
- The service will no longer be monitored or automatically restarted
- This does not delete the source code files, only removes the service from Odac
- You can restart the same service later using `candy run <file>`