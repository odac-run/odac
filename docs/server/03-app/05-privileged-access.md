## 🔐 Privileged Access

By default, ODAC runs application containers **unprivileged** for security. Some workloads need elevated permissions — to talk to hardware, open raw network sockets, mount filesystems, or use capabilities the default sandbox blocks. This command grants that access on a per-app basis.

> ⚠️ **At your own risk.** This is an operator-only escape hatch. It can **only** be enabled from the CLI (never from the dashboard), and you take full responsibility for the security implications.

### Two Levels

| Level | Flag | What it does | When to use |
|-------|------|--------------|-------------|
| **Root** | `--root` (default) | Runs the container as the `root` user. Docker Privileged stays **off**. | The app needs root inside the container — e.g. binding privileged ports, writing to root-owned mounts, or accessing a device mapped via [`app device add`](04-connect-device.md) whose node permissions would otherwise require a manual `chmod`. Root bypasses those permission/group checks. |
| **Full** | `--full` | Docker **Privileged** mode **+** root. Grants full kernel/device access, all Linux capabilities, and exposes the entire host `/dev`. | The app needs capabilities the default sandbox blocks — raw packet capture / network sniffing (`CAP_NET_RAW`, `CAP_NET_ADMIN`), mounting filesystems, manipulating cgroups/namespaces, raw or hot-plugged USB (`/dev/bus/usb/...`), or anything where a single mapped device isn't enough. In this mode `app device add` becomes redundant — all host devices are already visible. |

### Common Use Cases

- **Hardware / serial devices** — Arduino, sensors, or other USB-connected gear (often `--root` + [`app device add`](04-connect-device.md) is enough).
- **Network tooling** — packet sniffers, VPN clients, raw sockets, or anything needing `CAP_NET_RAW` / `CAP_NET_ADMIN` (`--full`).
- **Storage** — apps that mount or manage filesystems, loop devices, or block storage (`--full`).
- **System-level agents** — monitoring or tooling that inspects host devices, kernel interfaces, or cgroups (`--full`).

### Usage

```bash
# Run an app as root (e.g. to bind privileged ports or reach a mapped device)
odac app privileged my-app
odac app privileged my-app --root

# Full Docker privileged mode — broad host access (network sniffing,
# disk/IO monitoring, mounting filesystems). Use sparingly.
odac app privileged my-app --full

# Revoke all elevated access
odac app privileged my-app --off
```

You will be asked to confirm with `yes` before any elevation is applied.

### Available Prefixes
- `-i`, `--id`: The App ID or Name
- `--root`: Run the container as root (default if no level flag is given)
- `--full`: Enable full Docker Privileged mode + root
- `--off`: Remove all elevated access

> ⚠️ **Important:** Changes take effect on the next start. **Restart** the application after changing its privilege level:
> ```bash
> odac app restart my-app
> ```

### Examples

**Serial device (e.g. Arduino) — `--root` + a mapped device:**
```bash
odac app device add my-app /dev/ttyACM0   # map the device
odac app privileged my-app                # run as root so it can access it without chmod
odac app restart my-app
```

**Network sniffer / disk-IO monitor — `--full` for raw host access:**
```bash
odac app privileged my-app --full   # all capabilities + full /dev (CAP_NET_RAW, block devices, etc.)
odac app restart my-app
```

> 📝 **Note:** Prefer the narrowest option that works. Start with `--root` (plus [`app device add`](04-connect-device.md) if you only need one device); only reach for `--full` when the app genuinely needs host-wide capabilities.
