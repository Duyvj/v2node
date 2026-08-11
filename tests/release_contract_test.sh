#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly ARTIFACTS="${1:-$ROOT/artifacts}"
readonly VERSION='v0.4.4-ram5'

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

command -v unzip >/dev/null 2>&1 || fail 'unzip is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" ||
  fail 'installer copies differ'

if grep -Eq 'PLACEHOLDER|TO_BE_FILLED|raw\.githubusercontent\.com/wyx2685/v2node|raw\.githubusercontent\.com/Duyvj/v2node/main/' \
    "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" "$ROOT/script/v2node.sh"; then
  fail 'placeholder, mutable main URL or upstream installer URL remains'
fi

constant() {
  local name="$1"
  sed -n "s/^readonly $name='\\([^']*\\)'$/\\1/p" "$ROOT/deploy/install.sh"
}

sha64="$(constant DEFAULT_SHA256_64)"
sha_arm64="$(constant DEFAULT_SHA256_ARM64)"
binary_sha64="$(constant BINARY_SHA256_64)"
binary_sha_arm64="$(constant BINARY_SHA256_ARM64)"
geoip_sha="$(constant GEOIP_SHA256)"
geosite_sha="$(constant GEOSITE_SHA256)"
config_sha="$(constant CONFIG_SHA256)"
menu_sha="$(constant MENU_SHA256)"

for value in "$sha64" "$sha_arm64" "$binary_sha64" "$binary_sha_arm64" \
  "$geoip_sha" "$geosite_sha" "$config_sha" "$menu_sha"; do
  [[ "$value" =~ ^[0-9a-f]{64}$ ]] || fail 'embedded SHA-256 is invalid'
done

[[ "$(sha256sum "$ROOT/assets/geoip.dat" | awk '{print $1}')" == "$geoip_sha" ]] ||
  fail 'embedded geoip hash differs from tagged source asset'
[[ "$(sha256sum "$ROOT/assets/geosite.dat" | awk '{print $1}')" == "$geosite_sha" ]] ||
  fail 'embedded geosite hash differs from tagged source asset'
[[ "$(sha256sum "$ROOT/config.example.json" | awk '{print $1}')" == "$config_sha" ]] ||
  fail 'embedded config-template hash differs from source'
[[ "$(sha256sum "$ROOT/script/v2node.sh" | awk '{print $1}')" == "$menu_sha" ]] ||
  fail 'embedded management-menu hash differs from source'

expected_entries="$(printf '%s\n' BUILDINFO LICENSE VERSION v2node | sort)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
size64=0
size_arm64=0

for target in 64 arm64-v8a; do
  archive="$ARTIFACTS/v2node-linux-$target.zip"
  [[ -f "$archive" ]] || fail "missing archive: $archive"
  actual_entries="$(unzip -Z1 "$archive" | sort)"
  [[ "$actual_entries" == "$expected_entries" ]] ||
    fail "unexpected archive entries for $target"
  [[ -z "$(unzip -Z1 "$archive" | sort | uniq -d)" ]] ||
    fail "duplicate archive entry for $target"
  mkdir -p "$tmp/$target"
  unzip -q "$archive" -d "$tmp/$target"
  cmp -s "$tmp/$target/LICENSE" "$ROOT/LICENSE" ||
    fail "LICENSE differs in $target archive"
  cmp -s "$tmp/$target/BUILDINFO" "$ROOT/BUILDINFO" ||
    fail "BUILDINFO differs in $target archive"
  [[ "$(<"$tmp/$target/VERSION")" == "$VERSION" ]] ||
    fail "wrong VERSION in $target archive"

  elf="$(od -An -tx1 -N20 "$tmp/$target/v2node" | tr -d '[:space:]')"
  [[ "${elf:0:12}" == '7f454c460201' ]] ||
    fail "v2node is not a Linux ELF64 binary for $target"
  machine="${elf:36:4}"
  case "$target:$machine" in
    64:3e00|arm64-v8a:b700) ;;
    *) fail "wrong ELF architecture for $target" ;;
  esac

  actual_archive_sha="$(sha256sum "$archive" | awk '{print $1}')"
  actual_binary_sha="$(sha256sum "$tmp/$target/v2node" | awk '{print $1}')"
  if [[ "$target" == 64 ]]; then
    [[ "$actual_archive_sha" == "$sha64" ]] ||
      fail 'amd64 installer archive hash mismatch'
    [[ "$actual_binary_sha" == "$binary_sha64" ]] ||
      fail 'amd64 inner binary hash mismatch'
    size64="$(stat -c %s "$archive")"
  else
    [[ "$actual_archive_sha" == "$sha_arm64" ]] ||
      fail 'arm64 installer archive hash mismatch'
    [[ "$actual_binary_sha" == "$binary_sha_arm64" ]] ||
      fail 'arm64 inner binary hash mismatch'
    size_arm64="$(stat -c %s "$archive")"
  fi
done

(cd "$ARTIFACTS" && sha256sum --check --status SHA256SUMS) ||
  fail 'artifact SHA256SUMS verification failed'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/SHA256SUMS" ||
  fail 'root checksum file differs'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/release/SHA256SUMS" ||
  fail 'release checksum file differs'

grep -Fq "\"version\": \"$VERSION\"" "$ROOT/release/manifest.json" ||
  fail 'manifest version mismatch'
grep -Fq "$sha64" "$ROOT/release/manifest.json" ||
  fail 'manifest lacks amd64 archive hash'
grep -Fq "$sha_arm64" "$ROOT/release/manifest.json" ||
  fail 'manifest lacks arm64 archive hash'
grep -Fq "\"size\": $size64" "$ROOT/release/manifest.json" ||
  fail 'manifest lacks amd64 size'
grep -Fq "\"size\": $size_arm64" "$ROOT/release/manifest.json" ||
  fail 'manifest lacks arm64 size'
grep -Fxq "version=$VERSION" "$ROOT/BUILDINFO" ||
  fail 'BUILDINFO version mismatch'
grep -Fq "readonly VERSION=\"$VERSION\"" "$ROOT/build/build.sh" ||
  fail 'build script version mismatch'
grep -Fq "ARG VERSION=$VERSION" "$ROOT/Dockerfile" ||
  fail 'Docker version mismatch'
grep -Fq "readonly DEFAULT_VERSION='$VERSION'" "$ROOT/deploy/install.sh" ||
  fail 'installer version mismatch'

grep -Fq 'download_and_verify_assets' "$ROOT/deploy/install.sh" ||
  fail 'standalone asset provisioning is missing'
grep -Fq 'write_service_candidates' "$ROOT/deploy/install.sh" ||
  fail 'standalone service provisioning is missing'
grep -Fq 'atomic_install "$STAGE_DIR/v2node-menu.sh" "$MENU_FILE" 0755' "$ROOT/deploy/install.sh" ||
  fail 'standalone management-menu installation is missing'
grep -Fq 'https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh' "$ROOT/README.md" ||
  fail 'README lacks the blank-VPS install URL'
grep -Fq 'mktemp /tmp/v2node-install.XXXXXX' "$ROOT/README.md" ||
  fail 'README bootstrap does not use an unpredictable temporary installer path'

printf 'PASS: immutable ram5 artifacts and standalone installer contract agree\n'
