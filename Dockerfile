# Stage 0: Build the Go binaries
FROM golang:1.25-alpine AS go-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Static binaries; -ldflags="-s -w" strips debug symbols to reduce size.
# Builds all six: odac, odac-server, odac-watchdog, odac-proxy, odac-dns, odac-mail.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /build/bin/ ./cmd/...

# Stage 1: Production Image
FROM alpine:3.22

LABEL maintainer="emre.red <mail@emre.red>"
LABEL description="Odac Server - Next-Gen hosting platform with DNS, SSL, Mail & Monitoring"

# Runtime dependencies:
#   docker-cli / docker-cli-compose — container management (odac monitor shells
#     out to `docker`; everything else goes through the socket API)
#   sqlite — CLI for inspecting the mail database (the server itself uses a
#     pure-Go driver and does not need it)
#   ca-certificates — outbound TLS (hub, ACME, image pulls)
#   tzdata — TZ env support for Go binaries (Node bundled its own tz database)
RUN apk add --no-cache \
    docker-cli \
    docker-cli-compose \
    sqlite \
    ca-certificates \
    tzdata

WORKDIR /app

COPY --from=go-builder /build/bin ./bin

# The CLI on PATH (replaces the Node image's `npm link`)
RUN ln -s /app/bin/odac /usr/local/bin/odac

# Create necessary directories
RUN mkdir -p /app/.odac

# Expose ports (documentation only, will use host network)
EXPOSE 80 443/tcp 443/udp 25 587 993 143 53/udp 53/tcp

# Health check: exits 0 while the server accepts API connections
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
  CMD ["/app/bin/odac", "healthcheck"]

# Config lives in $HOME/.odac
ENV HOME=/app

# Volumes for persistence
VOLUME ["/app/.odac"]

# Start Odac daemon (Go watchdog supervising the Go server)
CMD ["./bin/odac-watchdog"]
