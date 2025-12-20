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
  -v odac-storage:/app/storage \
  -v odac-sites:/app/sites \
  --cap-add NET_ADMIN \
  --cap-add NET_BIND_SERVICE \
  odacrun/odac:latest

# View logs
docker logs -f odac

# Execute CLI commands
docker exec -it odac node bin/odac status
docker exec -it odac node bin/odac monitor

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
| `NODE_ENV` | `production` | Node.js environment |
| `TZ` | `UTC` | Timezone |

## Volumes

| Volume | Purpose |
|--------|---------|
| `/app/storage` | Configuration, databases, logs |
| `/app/sites` | Website files and data |
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
docker exec odac node bin/odac status
```

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
docker exec odac node bin/odac restart web
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

### Backup

```bash
# Backup volumes
docker run --rm \
  -v odac-storage:/data \
  -v $(pwd):/backup \
  alpine tar czf /backup/odac-storage-backup.tar.gz -C /data .

docker run --rm \
  -v odac-sites:/data \
  -v $(pwd):/backup \
  alpine tar czf /backup/odac-sites-backup.tar.gz -C /data .
```

### Restore

```bash
# Restore volumes
docker run --rm \
  -v odac-storage:/data \
  -v $(pwd):/backup \
  alpine sh -c "cd /data && tar xzf /backup/odac-storage-backup.tar.gz"

docker run --rm \
  -v odac-sites:/data \
  -v $(pwd):/backup \
  alpine sh -c "cd /data && tar xzf /backup/odac-sites-backup.tar.gz"
```

## Uninstall

```bash
# Stop and remove container
docker-compose down

# Remove volumes (WARNING: This deletes all data!)
docker volume rm odac-storage odac-sites

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

- **Base Image**: `node:22-alpine` (minimal footprint)
- **Node.js Version**: 22.x LTS
- **OS**: Alpine Linux (lightweight, secure)

### Run Tests

```bash
docker run --rm \
  -v $(pwd):/app \
  odacrun/odac:dev npm test
```
