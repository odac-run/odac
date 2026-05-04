## 🔌 Connect a Device
This command allows you to map hardware devices (e.g., USB serial ports for Arduino) from your host machine directly into your application container.

### Interactive Usage
```bash
odac app device add
```
After running the command, you will be prompted for:
- **App ID or Name:** The application you want to connect the device to.
- **Host Device Path:** The path to the device on your server (e.g., `/dev/ttyACM0`).

### Single-Line Usage
```bash
# Connect /dev/ttyACM0 to 'my-arduino-app'
odac app device add my-arduino-app /dev/ttyACM0

# Or use flags
odac app device add -a my-arduino-app -d /dev/ttyACM0
```

### Disconnecting a Device
To remove a device mapping, use the `delete` sub-command:
```bash
odac app device delete my-arduino-app /dev/ttyACM0
```

### Available Prefixes
- `-a`, `--app`: The App ID or Name
- `-d`, `--device`: The host device path

> ⚠️ **Important:** After adding or removing a device, you must **restart** the application for the changes to take effect:
> ```bash
> odac app restart my-arduino-app
> ```

> 📝 **Note:** For security reasons, devices are mapped with `read-write-mknod` (rwm) permissions by default. Ensure your application has the necessary internal permissions (e.g., user groups) to access the device.
