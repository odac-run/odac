# Stage 0: Build Go Proxy
FROM golang:1.24-alpine AS go-builder
WORKDIR /build
# Copy Go source
COPY server/proxy ./server/proxy
# Build static binary
# -ldflags="-s -w" reduces binary size by stripping debug symbols
RUN cd server/proxy && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /build/odac-proxy

# Stage 1: Build Node.js Native Dependencies
FROM node:22-alpine AS node-builder

LABEL maintainer="emre.red <mail@emre.red>"
LABEL description="Odac Server - Next-Gen hosting platform with DNS, SSL, Mail & Monitoring"

# Install build dependencies
RUN apk add --no-cache \
    python3 \
    make \
    g++

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install all dependencies (including devDependencies for native builds)
RUN npm ci

# Copy application files (Optional: if you need to build frontend assets later, copy . here)
# COPY . .

# Stage 2: Production Image
FROM node:22-alpine

LABEL maintainer="emre.red <mail@emre.red>"
LABEL description="Odac Server - Next-Gen hosting platform with DNS, SSL, Mail & Monitoring"

# Install runtime dependencies
RUN apk add --no-cache \
    docker-cli \
    docker-compose \
    sqlite \
    bash \
    curl \
    ca-certificates

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install production dependencies only
RUN npm ci --omit=dev

# Copy Node.js modules from builder
COPY --from=node-builder /app/node_modules ./node_modules

# Copy Go Proxy binary from go-builder
COPY --from=go-builder /build/odac-proxy ./bin/odac-proxy

# Copy application source code
COPY . .
# Ensure binary is executable
RUN chmod +x ./bin/odac-proxy

# Create necessary directories
RUN mkdir -p /app/storage /app/sites

# Link odac CLI globally
RUN npm link

# Expose ports (documentation only, will use host network)
EXPOSE 80 443 25 587 993 143 53/udp 53/tcp

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
  CMD node -e "require('./core/Odac.js'); process.exit(Odac.core('Config').get('server.status') === 'online' ? 0 : 1)" || exit 1

# Set environment
ENV NODE_ENV=production
ENV HOME=/app/storage
ENV ODAC_WEB_PATH=/app/sites

# Volumes for persistence
VOLUME ["/app/storage", "/app/sites"]

# Start Odac daemon
CMD ["node", "watchdog/index.js"]
