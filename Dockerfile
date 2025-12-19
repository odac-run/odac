FROM node:22-alpine

LABEL maintainer="emre.red <mail@emre.red>"
LABEL description="Odac Server - Next-Gen hosting platform with DNS, SSL, Mail & Monitoring"

# Install system dependencies
RUN apk add --no-cache \
    docker-cli \
    docker-compose \
    python3 \
    make \
    g++ \
    sqlite \
    bash \
    curl

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install production dependencies
RUN npm ci --only=production

# Copy application files
COPY . .

# Create necessary directories
RUN mkdir -p /app/storage /app/sites

# Expose ports (documentation only, will use host network)
EXPOSE 80 443 25 587 993 143 53/udp 53/tcp

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
  CMD node -e "require('./core/Odac.js'); process.exit(Odac.core('Config').get('server.status') === 'online' ? 0 : 1)" || exit 1

# Set environment
ENV NODE_ENV=production

# Volumes for persistence
VOLUME ["/app/storage", "/app/sites"]

# Start Odac daemon
CMD ["node", "watchdog/index.js"]
