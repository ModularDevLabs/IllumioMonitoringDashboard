#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export CGO_ENABLED=0
export GOCACHE="$ROOT_DIR/.gocache"
export GOMODCACHE="$ROOT_DIR/.gomodcache"
mkdir -p "$GOCACHE" "$GOMODCACHE"

branch_name="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo detached-head)"
branch_slug="$(printf '%s' "$branch_name" | tr '/[:space:]' '__')"
OUT_DIR="${REBUILD_OUTPUT_DIR:-$ROOT_DIR/build/$branch_slug}"
mkdir -p "$OUT_DIR"

version_label="$(git tag --points-at HEAD | head -n1 || true)"
if [[ -z "$version_label" ]]; then
  version_label="$(git tag --list "*-rc*" --sort=-creatordate | head -n1 || true)"
fi
if [[ -z "$version_label" ]]; then
  version_label="$(git tag --list --sort=-creatordate | head -n1 || true)"
fi
if [[ -z "$version_label" ]]; then
  version_label="$(git rev-parse --short HEAD 2>/dev/null || echo unversioned)"
fi
LDFLAGS="-X main.buildVersion=$version_label"

printf 'Building linux amd64...\n'
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT_DIR/illumio-dashboard-linux-amd64" ./

printf 'Building windows amd64...\n'
GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT_DIR/illumio-dashboard.exe" ./

printf 'Building macOS amd64...\n'
GOOS=darwin GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT_DIR/illumio-dashboard-mac-intel" ./

printf 'Building macOS arm64...\n'
GOOS=darwin GOARCH=arm64 go build -ldflags "$LDFLAGS" -o "$OUT_DIR/illumio-dashboard-mac-arm" ./

printf 'Done. Binaries refreshed in %s\n' "$OUT_DIR"
printf 'Embedded version label: %s\n' "$version_label"
printf 'Tip: set REBUILD_OUTPUT_DIR=%s to write into repo root for release packaging.\n' "$ROOT_DIR"
