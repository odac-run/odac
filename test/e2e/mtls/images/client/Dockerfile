# The visitor. Keys and CSRs are made here and the private keys never leave
# this container (its /work is a tmpfs). curl does the WebSocket scenario too
# (wss:// support is built in since 8.11); websocat is installed when the
# distribution carries it, for interactive debugging.
FROM alpine:3.22
LABEL org.odac.e2e="mtls-e2e"
RUN apk add --no-cache curl openssl jq ca-certificates coreutils \
 && (apk add --no-cache websocat >/dev/null 2>&1 || echo "websocat not packaged, skipping")
WORKDIR /work
CMD ["sleep", "infinity"]
