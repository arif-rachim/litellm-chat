#!/usr/bin/env bash
# Build the distributable binaries: one self-contained file per platform.
set -euo pipefail
cd "$(dirname "$0")"

VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y.%m.%d)}
OUT=dist
rm -rf "$OUT"; mkdir -p "$OUT"

for target in linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os=${target%/*} arch=${target#*/}
  ext=""; [ "$os" = windows ] && ext=.exe
  echo "→ $target"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/lchat-$os-$arch$ext" .
done

cd "$OUT"
sha256sum lchat-* > SHA256SUMS
gzip -k -9 lchat-linux-amd64 lchat-linux-arm64
ls -lh
echo
echo "Distribusi: salin satu file yang cocok ke mesin tujuan, misalnya"
echo "  install -Dm755 lchat-linux-amd64 ~/.local/bin/lchat"
echo "  (Windows: lchat-windows-amd64.exe, ganti nama jadi lchat.exe di folder yang ada di PATH)"
