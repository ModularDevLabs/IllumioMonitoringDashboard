#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 <release-tag>"
  echo "Example: $0 v1.1.2"
  exit 1
fi

TAG="$1"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

./scripts/rebuild-binaries.sh

gh release upload "$TAG" \
  illumio-dashboard-linux-amd64 \
  illumio-dashboard.exe \
  illumio-dashboard-mac-intel \
  illumio-dashboard-mac-arm \
  --clobber

echo "Uploaded release assets for $TAG"
