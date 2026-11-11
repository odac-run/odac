# The app under test. Built on the host engine, then `docker load`ed into the
# node's dind engine, because the node network has no route to a registry.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/origin .

FROM alpine:3.22
LABEL org.odac.e2e="mtls-e2e"
COPY --from=build /out/origin /usr/local/bin/origin
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/origin"]
