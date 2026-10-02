#!/usr/bin/env bash
# 交叉编译 linux/amd64 静态二进制。Git Bash / WSL / Linux / macOS 均可执行。
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="${OUT:-dist/dadyumo-qqbot}"

mkdir -p dist
echo "编译 dadyumo-qqbot $VERSION -> $OUT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT" ./cmd/dadyumo-qqbot

chmod +x "$OUT"
ls -lh "$OUT"
