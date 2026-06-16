FROM golang:1.26-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    gcc-mingw-w64-x86-64 \
    zip \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV GOOS=windows
ENV GOARCH=amd64
ENV CGO_ENABLED=1
ENV CC=x86_64-w64-mingw32-gcc
ENV CXX=x86_64-w64-mingw32-g++

RUN mkdir -p /out/payload

# The tray app uses systray/cgo, so it is built with MinGW and the Windows GUI
# subsystem. The proxy is also GUI-subsystem so the tray supervisor can run it
# without opening a console window; stdout/stderr still go to the supervisor log.
RUN go build -mod=mod -trimpath -tags tray -ldflags="-s -w -H=windowsgui" -o /out/payload/cnc-tray.exe ./cmd/tray
RUN go build -mod=mod -trimpath -ldflags="-s -w -H=windowsgui" -o /out/payload/cnc-proxy.exe ./cmd/proxy

ENV CGO_ENABLED=0
RUN go build -mod=mod -trimpath -ldflags="-s -w" -o /out/payload/deploy.exe ./cmd/deploy
RUN go build -mod=mod -trimpath -ldflags="-s -w" -o /out/cnc-proxy-installer-stub.exe ./cmd/wininstaller

RUN (cd /out/payload && zip -9 /out/cnc-proxy-windows-payload.zip cnc-tray.exe cnc-proxy.exe deploy.exe)

ENV GOOS=linux
RUN GOARCH="$(go env GOHOSTARCH)" go run ./cmd/winpack \
  -stub /out/cnc-proxy-installer-stub.exe \
  -payload /out/cnc-proxy-windows-payload.zip \
  -out /out/cnc-proxy-installer.exe
