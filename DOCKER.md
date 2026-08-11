# Odac Docker Deployment Guide

## Quick Start

### Using Docker Compose (Recommended)

```bash
# Start Odac
docker-compose up -d

# View logs
docker-compose logs -f

# Stop Odac
docker-compose down

# Restart Odac
docker-compose restart
```

### Using Docker CLI

```bash
# Run Odac
docker run -d \
  --name odac \
  --restart unless-stopped \
  --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/odac/.odac:/app/.odac \
  -e ODAC_HOST_ROOT=/var/lib/odac \
  --cap-add NET_ADMIN \
  --cap-add NET_BIND_SERVICE \
  odacrun/odac:latest

# View logs
docker logs -f odac

# Execute CLI commands
docker exec -it odac odac status
docker exec -it odac odac monitor

# Stop and remove
docker stop odac
docker rm odac
```

## Building from Source

```bash
# Build image
docker build -t odacrun/odac:latest .

# Run locally built image
docker-compose up -d
```

## One-Line Install Script

```bash
curl -sL https://odac.run/install | bash
```

This script will:
1. Check if Docker is installed (install if missing)
2. Pull the latest Odac image
3. Start Odac with proper configuration
4. Display access information

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TZ` | `UTC` | Timezone |
| `ODAC_DATA_DIR` | `/var/lib/odac` | Host directory holding the persistent data (compose only) |
| `ODAC_HOST_ROOT` | — | Host path the container's `/app` maps to; required for git deploys (compose sets it from `ODAC_DATA_DIR`) |
| `ODAC_GPU_RUNTIME` | auto | Forces the reported GPU runtime (`nvidia`, `rocm`, `none`); only needed when the container cannot see the host's driver state |

## Volumes

| Mount | Purpose |
|-------|---------|
| `/app/.odac` | Configuration, databases, logs, app data — must be a **host bind** (named volumes break git deploys, see `ODAC_HOST_ROOT`) |
| `/var/run/docker.sock` | Docker daemon access (required) |

## Ports

Odac uses `host` network mode for direct port access:

| Port | Service |
|------|---------|
| 80 | HTTP |
| 443 | HTTPS |
| 25 | SMTP |
| 587 | SMTP Submission |
| 993 | IMAPS |
| 143 | IMAP |
| 53 | DNS (TCP/UDP) |

## Security Considerations

### Required Capabilities

- `NET_ADMIN`: For DNS server and network management
- `NET_BIND_SERVICE`: For binding to privileged ports (< 1024)

### Docker Socket Access

Odac requires access to `/var/run/docker.sock` to manage website containers. This is the same approach used by:
- Portainer
- Coolify
- CapRover
- Dokku

### Host Network Mode

Using `network_mode: host` provides:
- Direct port access without NAT overhead
- Better performance for DNS and mail services
- Simplified network configuration

## Troubleshooting

### Check Odac Status

```bash
docker exec odac odac status
```

The container's `HEALTHCHECK` runs `odac healthcheck` (exit 0 while the
server accepts API connections); `docker ps` shows the result as
`healthy`/`unhealthy`.

### View Logs

```bash
# All logs
docker logs odac

# Follow logs
docker logs -f odac

# Last 100 lines
docker logs --tail 100 odac
```

### Access Container Shell

```bash
docker exec -it odac sh
```

### Restart Services

```bash
# Restart Odac
docker restart odac

# Restart specific service
docker exec odac odac restart web
```

## Updating Odac

```bash
# Pull latest image
docker pull odacrun/odac:latest

# Restart with new image
docker-compose down
docker-compose up -d
```

## Backup and Restore

The persistent data lives in a plain host directory (default
`/var/lib/odac/.odac`), so backup and restore are ordinary tar commands.

### Backup

```bash
tar czf odac-backup.tar.gz -C /var/lib/odac .odac
```

### Restore

```bash
tar xzf odac-backup.tar.gz -C /var/lib/odac
```

## Uninstall

```bash
# Stop and remove container
docker-compose down

# Remove data directory (WARNING: This deletes all data!)
rm -rf /var/lib/odac

# Remove image
docker rmi odacrun/odac:latest
```

## Development

### Run in Development Mode

```bash
# Build dev image
docker build -t odacrun/odac:dev .

# Run with source mounted
docker run -it --rm \
  -v $(pwd):/app \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --network host \
  odacrun/odac:dev sh
```

## Technical Details

- **Base Image**: `alpine:3.22` (static Go binaries, no language runtime)
- **Go Version**: 1.25 (build stage only)
- **OS**: Alpine Linux (lightweight, secure)

### Run Tests

Tests run with the Go toolchain against the source tree (they are not
shipped in the image):

```bash
docker run --rm \
  -v $(pwd):/src -w /src \
  golang:1.25-alpine go test ./...
```
