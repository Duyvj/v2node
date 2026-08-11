#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly VERSION="v0.4.4-ram3"
readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly OUT_DIR="${ROOT}/artifacts"
readonly GO_VERSION_REQUIRED="go1.26.1"

export GOEXPERIMENT=jsonv2
export CGO_ENABLED=0
export GOTOOLCHAIN=local
export GOFLAGS=-mod=readonly
export TZ=UTC
export LC_ALL=C

command -v go >/dev/null 2>&1 || { echo 'go is required' >&2; exit 1; }
[[ "$(go version | awk '{print $3}')" == "$GO_VERSION_REQUIRED" ]] || {
  echo "expected ${GO_VERSION_REQUIRED}, got $(go version)" >&2
  exit 1
}
[[ -f "${ROOT}/go.mod" ]] || { echo 'source tree missing' >&2; exit 1; }

cd "$ROOT"
go mod download
go mod verify
go test ./...
go vet ./...
XRAY_PACKAGES=(
  ./app/proxyman/inbound
  ./app/reverse
  ./common/net/cnc
  ./common/signal/pubsub
  ./common/task
  ./proxy/anytls
  ./proxy/shadowsocks
  ./proxy/vless/outbound
  ./transport/internet/browser_dialer
  ./transport/internet/grpc
  ./transport/internet/grpc/encoding
  ./transport/internet/hysteria
  ./transport/internet/splithttp
  ./transport/internet/tuic
)
(cd third_party/xray-core && go mod verify)
(cd third_party/xray-core && go test "${XRAY_PACKAGES[@]}")
(cd third_party/xray-core && go vet \
  ./app/proxyman/inbound \
  ./app/reverse \
  ./common/net/cnc \
  ./common/signal/pubsub \
  ./common/task \
  ./proxy/anytls \
  ./proxy/shadowsocks \
  ./transport/internet/browser_dialer \
  ./transport/internet/grpc \
  ./transport/internet/grpc/encoding \
  ./transport/internet/hysteria \
  ./transport/internet/tuic)
# The pinned upstream protobuf code triggers known copylock warnings unchanged
# from pristine. Disable only that analyzer so every other splithttp vet check runs.
(cd third_party/xray-core && go vet -copylocks=false ./transport/internet/splithttp)
# The pinned VLESS outbound contains two pre-existing unsafe.Pointer warnings.
# Keep every other vet analyzer enabled for its lifecycle regression package.
(cd third_party/xray-core && go vet -unsafeptr=false ./proxy/vless/outbound)

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR/stage"
for target in 'amd64:64' 'arm64:arm64-v8a'; do
  GOARCH="${target%%:*}"
  ASSET_ARCH="${target##*:}"
  STAGE="${OUT_DIR}/stage/${ASSET_ARCH}"
  mkdir -p "$STAGE"
  GOOS=linux GOARCH="$GOARCH" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X github.com/wyx2685/v2node/cmd.version=${VERSION}" \
    -o "$STAGE/v2node" ./
  cp "$ROOT/LICENSE" "$STAGE/"
  printf '%s\n' "$VERSION" > "$STAGE/VERSION"
  cp "$ROOT/BUILDINFO" "$STAGE/BUILDINFO"
  chmod 0755 "$STAGE/v2node"
  GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" go run ./build/packagezip \
    -source "$STAGE" \
    -output "$OUT_DIR/v2node-linux-${ASSET_ARCH}.zip"
done

(cd "$OUT_DIR" && sha256sum --text v2node-linux-*.zip > SHA256SUMS)
cat "$OUT_DIR/SHA256SUMS"
