# syntax=docker/dockerfile:1
#
# An ODAC node for the mTLS end-to-end test: docker:dind with the agent built
# from source on top. The agent source arrives as the named build context
# `agent` (compose: build.additional_contexts.agent = ${AGENT_SRC}), so this
# directory never has to sit inside the agent tree. The build mirrors the
# agent's own Dockerfile (same Go image, same flags, same layout under /app).

FROM golang:1.26-alpine AS agent-build
WORKDIR /build
COPY --from=agent go.mod go.sum ./
RUN go mod download
COPY --from=agent cmd ./cmd
COPY --from=agent internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /build/bin/ ./cmd/...

FROM docker:29-dind
LABEL org.odac.e2e="mtls-e2e"
# Same runtime dependencies as the production image (docker-cli ships with dind).
RUN apk add --no-cache ca-certificates tzdata sqlite
WORKDIR /app
COPY --from=agent-build /build/bin ./bin
RUN ln -s /app/bin/odac /usr/local/bin/odac && mkdir -p /app/.odac
COPY entrypoint.sh /usr/local/bin/mtls-e2e-node
RUN chmod 0755 /usr/local/bin/mtls-e2e-node
ENV HOME=/app \
    TZ=UTC \
    DOCKER_TLS_CERTDIR=""
ENTRYPOINT ["/usr/local/bin/mtls-e2e-node"]
