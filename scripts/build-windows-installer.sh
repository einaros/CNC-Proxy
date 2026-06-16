#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${IMAGE:-cnc-proxy-windows-builder}"
OUT_DIR="${OUT_DIR:-$ROOT/dist/windows}"
export DOCKER_CONFIG="${DOCKER_CONFIG:-/private/tmp/cnc-docker-config}"

mkdir -p "$OUT_DIR"
mkdir -p "$DOCKER_CONFIG"

docker build -f "$ROOT/build/windows/Dockerfile" -t "$IMAGE" "$ROOT"

container="$(docker create "$IMAGE")"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "$OUT_DIR"/*
docker cp "$container:/out/." "$OUT_DIR/"

cat <<EOF
Windows artifacts written to:
  $OUT_DIR

Installer:
  $OUT_DIR/cnc-proxy-installer.exe

Run on the target PC:
  cnc-proxy-installer.exe -remote

Use the printed manager token for remote deployments:
  deploy -target http://<target-ip>:8430 -token <token> -source .
EOF
