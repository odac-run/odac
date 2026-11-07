# syntax=docker/dockerfile:1
#
# odac-tunnel for the edge scenarios, built from the named context `tunnel`
# (run.sh: --build-context tunnel=${TUNNEL_SRC}). It runs as an app on the edge
# nodes, the way the edge runs it, so this image only has to carry the binary.

FROM golang:1.26-alpine AS tunnel-build
WORKDIR /src
COPY --from=tunnel go.mod go.sum ./
RUN go mod download
COPY --from=tunnel cmd ./cmd
COPY --from=tunnel internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/odac-tunnel ./cmd/odac-tunnel

FROM alpine:3.22
LABEL org.odac.e2e="mtls-e2e"
RUN apk add --no-cache ca-certificates
COPY --from=tunnel-build /out/odac-tunnel /usr/local/bin/odac-tunnel
# The token store is ./storage (bbolt).
WORKDIR /srv
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/odac-tunnel"]
