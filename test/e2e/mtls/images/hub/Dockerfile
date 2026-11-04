FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hub .

FROM alpine:3.22
LABEL org.odac.e2e="mtls-e2e"
COPY --from=build /out/hub /usr/local/bin/hub
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/hub"]
