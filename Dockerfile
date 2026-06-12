# Build stage: static binary, no cgo (the proxy has no cgo deps; only the
# optional tray app does, and it is not part of the image).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cnc-proxy ./cmd/proxy

# Runtime stage. Alpine rather than scratch so the healthcheck has wget and
# operators can exec a shell for debugging.
FROM alpine:3.21
RUN adduser -D -H -u 65532 cnc && mkdir -p /data && chown cnc /data
COPY --from=build /out/cnc-proxy /usr/local/bin/cnc-proxy
USER cnc
VOLUME /data
# 2222: relay for the controller; 8420: HTTP API + web UI; 8421: WebDAV.
EXPOSE 2222 8420 8421
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8420/healthz || exit 1
ENTRYPOINT ["cnc-proxy", "-data-dir", "/data"]
