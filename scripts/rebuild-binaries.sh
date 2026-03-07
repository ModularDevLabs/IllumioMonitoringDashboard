#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export CGO_ENABLED=0
export GOCACHE="$ROOT_DIR/.gocache"
export GOMODCACHE="$ROOT_DIR/.gomodcache"
mkdir -p "$GOCACHE" "$GOMODCACHE"

printf 'Building linux amd64...\n'
GOOS=linux GOARCH=amd64 go build -o illumio-dashboard-linux-amd64 ./

printf 'Building windows amd64...\n'
GOOS=windows GOARCH=amd64 go build -o illumio-dashboard.exe ./

printf 'Building macOS amd64...\n'
GOOS=darwin GOARCH=amd64 go build -o illumio-dashboard-mac-intel ./

printf 'Building macOS arm64...\n'
GOOS=darwin GOARCH=arm64 go build -o illumio-dashboard-mac-arm ./

printf 'Done. Binaries refreshed in %s\n' "$ROOT_DIR"
