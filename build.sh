#!/usr/bin/env bash
set -euo pipefail

echo "======================================"
echo " Building Resource-Optimized v2node"
echo "======================================"

export CGO_ENABLED=0
export GOEXPERIMENT=jsonv2

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo "custom")}"
LDFLAGS="-s -w -X 'github.com/wyx2685/v2node/cmd.Version=${VERSION}'"

OUTPUT_NAME="${OUTPUT_NAME:-v2node}"

go build -ldflags="${LDFLAGS}" -trimpath -v -o "${OUTPUT_NAME}" .

echo "Build successful: ${OUTPUT_NAME}"
ls -lh "${OUTPUT_NAME}"
