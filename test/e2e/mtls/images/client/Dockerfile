# The visitor, and the one-shot `pki` step. Keys and CSRs are made here and the
# private keys never leave this container (its /work is a tmpfs). curl does the
# WebSocket scenario too (wss:// support is built in since 8.11).
FROM alpine:3.22
LABEL org.odac.e2e="mtls-e2e"
RUN apk add --no-cache curl openssl jq ca-certificates coreutils
COPY pki.sh /usr/local/bin/mtls-e2e-pki
RUN chmod 0755 /usr/local/bin/mtls-e2e-pki
WORKDIR /work
CMD ["sleep", "infinity"]
