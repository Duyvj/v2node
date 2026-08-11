#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly ARTIFACTS="${1:-$ROOT/artifacts}"
readonly VERSION="v0.4.4-ram4"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

command -v unzip >/dev/null 2>&1 || fail 'unzip is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" || fail 'installer copies differ'
if grep -Eq 'PLACEHOLDER|TO_BE_FILLED|raw\.githubusercontent\.com/.*/main/' "$ROOT/deploy/install.sh" "$ROOT/script/install.sh"; then
  fail 'placeholder or mutable main URL remains in release installer'
fi

sha64="$(sed -n 's/^readonly DEFAULT_SHA256_64="\([0-9a-f]\{64\}\)"$/\1/p' "$ROOT/deploy/install.sh")"
sha_arm64="$(sed -n 's/^readonly DEFAULT_SHA256_ARM64="\([0-9a-f]\{64\}\)"$/\1/p' "$ROOT/deploy/install.sh")"
[[ ${#sha64} -eq 64 && ${#sha_arm64} -eq 64 ]] || fail 'embedded package hashes are invalid'
[[ "$sha64" != "$(printf '0%.0s' {1..64})" && "$sha_arm64" != "$(printf '0%.0s' {1..64})" ]] || fail 'embedded package hashes are placeholders'

expected_entries="$(printf '%s\n' BUILDINFO LICENSE VERSION v2node | sort)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
size64=0
size_arm64=0

for target in '64' 'arm64-v8a'; do
  archive="$ARTIFACTS/v2node-linux-${target}.zip"
  [[ -f "$archive" ]] || fail "missing archive: $archive"
  actual_entries="$(unzip -Z1 "$archive" | sort)"
  [[ "$actual_entries" == "$expected_entries" ]] || fail "unexpected archive entries for $target"
  [[ "$(unzip -Z1 "$archive" | sort | uniq -d)" == "" ]] || fail "duplicate archive entry for $target"
  mkdir -p "$tmp/$target"
  unzip -q "$archive" -d "$tmp/$target"
  cmp -s "$tmp/$target/LICENSE" "$ROOT/LICENSE" || fail "LICENSE differs in $target archive"
  cmp -s "$tmp/$target/BUILDINFO" "$ROOT/BUILDINFO" || fail "BUILDINFO differs in $target archive"
  [[ "$(<"$tmp/$target/VERSION")" == "$VERSION" ]] || fail "wrong VERSION in $target archive"
  elf="$(od -An -tx1 -N20 "$tmp/$target/v2node" | tr -d '[:space:]')"
  [[ "${elf:0:12}" == '7f454c460201' ]] || fail "v2node is not a Linux ELF64 binary for $target"
  machine="${elf:36:4}"
  case "$target:$machine" in 64:3e00|arm64-v8a:b700) ;; *) fail "wrong ELF architecture for $target" ;; esac
  actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
  if [[ "$target" == 64 ]]; then
    [[ "$actual_sha" == "$sha64" ]] || fail 'amd64 installer hash mismatch'
    size64="$(stat -c %s "$archive")"
  else
    [[ "$actual_sha" == "$sha_arm64" ]] || fail 'arm64 installer hash mismatch'
    size_arm64="$(stat -c %s "$archive")"
  fi
done

(cd "$ARTIFACTS" && sha256sum --check --status SHA256SUMS) || fail 'artifact SHA256SUMS verification failed'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/SHA256SUMS" || fail 'root checksum file differs'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/release/SHA256SUMS" || fail 'release checksum file differs'
grep -Fq '"version": "v0.4.4-ram4"' "$ROOT/release/manifest.json" || fail 'manifest version mismatch'
grep -Fq "$sha64" "$ROOT/release/manifest.json" || fail 'manifest lacks amd64 hash'
grep -Fq "$sha_arm64" "$ROOT/release/manifest.json" || fail 'manifest lacks arm64 hash'
grep -Fq "\"size\": $size64" "$ROOT/release/manifest.json" || fail 'manifest lacks amd64 size'
grep -Fq "\"size\": $size_arm64" "$ROOT/release/manifest.json" || fail 'manifest lacks arm64 size'
grep -Fq '"name": "v2node-v0.4.4-ram4-source.zip"' "$ROOT/release/manifest.json" || fail 'manifest source bundle mismatch'
grep -Fxq "version=$VERSION" "$ROOT/BUILDINFO" || fail 'BUILDINFO version mismatch'
grep -Fq "readonly VERSION=\"$VERSION\"" "$ROOT/build/build.sh" || fail 'build script version mismatch'
grep -Fq "ARG VERSION=$VERSION" "$ROOT/Dockerfile" || fail 'Docker version mismatch'
grep -Fq "readonly DEFAULT_VERSION=\"$VERSION\"" "$ROOT/deploy/install.sh" || fail 'installer version mismatch'
grep -Fq "$VERSION" "$ROOT/docs/FLEET_DEPLOYMENT.md" || fail 'fleet guide version mismatch'
grep -Fq "$sha64" "$ROOT/docs/FLEET_DEPLOYMENT.md" || fail 'fleet guide lacks amd64 hash'
grep -Fq "$sha_arm64" "$ROOT/docs/FLEET_DEPLOYMENT.md" || fail 'fleet guide lacks arm64 hash'

printf 'PASS: minimal immutable RAM-fix archives and checksums agree\n'
