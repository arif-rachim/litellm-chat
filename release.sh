#!/usr/bin/env bash
# Build the distributable binaries: one self-contained file per architecture.
set -euo pipefail
cd "$(dirname "$0")"

VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y.%m.%d)}
OUT=dist
rm -rf "$OUT"; mkdir -p "$OUT"

for arch in amd64 arm64; do
  echo "→ linux/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/lchat-linux-$arch" .
done

cd "$OUT"
sha256sum lchat-linux-* > SHA256SUMS
gzip -k -9 lchat-linux-amd64 lchat-linux-arm64
ls -lh
echo
echo "Distribusi: salin satu file yang cocok ke mesin tujuan, misalnya"
echo "  install -Dm755 lchat-linux-amd64 ~/.local/bin/lchat"
