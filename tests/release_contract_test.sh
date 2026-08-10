#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly ARTIFACTS="${1:-$ROOT/artifacts}"
readonly VERSION="v0.4.4-ram2"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

command -v unzip >/dev/null 2>&1 || fail 'unzip is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" || fail 'installer copies differ'
if grep -Eq 'PLACEHOLDER|raw\.githubusercontent\.com/.*/main/' "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" "$ROOT/script/v2node.sh"; then
  fail 'placeholder or mutable main URL remains in release scripts'
fi

sha64="$(sed -n 's/^readonly DEFAULT_SHA256_64="\([0-9a-f]\{64\}\)"$/\1/p' "$ROOT/deploy/install.sh")"
sha_arm64="$(sed -n 's/^readonly DEFAULT_SHA256_ARM64="\([0-9a-f]\{64\}\)"$/\1/p' "$ROOT/deploy/install.sh")"
[[ ${#sha64} -eq 64 && ${#sha_arm64} -eq 64 ]] || fail 'embedded package hashes are invalid'

expected_entries="$(printf '%s\n' BUILDINFO LICENSE README.md VERSION geoip.dat geosite.dat v2node v2node-menu v2nodectl | sort)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT

for target in '64' 'arm64-v8a'; do
  archive="$ARTIFACTS/v2node-personal-${VERSION}-linux-${target}.zip"
  [[ -f "$archive" ]] || fail "missing archive: $archive"
  actual_entries="$(unzip -Z1 "$archive" | sort)"
  [[ "$actual_entries" == "$expected_entries" ]] || fail "unexpected archive entries for $target"
  [[ "$(unzip -Z1 "$archive" | sort | uniq -d)" == "" ]] || fail "duplicate archive entry for $target"
  mkdir -p "$tmp/$target"
  unzip -q "$archive" -d "$tmp/$target"
  cmp -s "$tmp/$target/v2nodectl" "$ROOT/deploy/v2nodectl.sh" || fail "controller differs in $target archive"
  cmp -s "$tmp/$target/v2node-menu" "$ROOT/script/v2node.sh" || fail "menu differs in $target archive"
  [[ "$(<"$tmp/$target/VERSION")" == "$VERSION" ]] || fail "wrong VERSION in $target archive"
  actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
  if [[ "$target" == 64 ]]; then
    [[ "$actual_sha" == "$sha64" ]] || fail 'amd64 installer hash mismatch'
  else
    [[ "$actual_sha" == "$sha_arm64" ]] || fail 'arm64 installer hash mismatch'
  fi
done

(cd "$ARTIFACTS" && sha256sum --check --status SHA256SUMS) || fail 'artifact SHA256SUMS verification failed'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/SHA256SUMS" || fail 'root checksum file differs'
cmp -s "$ARTIFACTS/SHA256SUMS" "$ROOT/release/SHA256SUMS" || fail 'release checksum file differs'

grep -Fq '"version": "v0.4.4-ram2"' "$ROOT/release/manifest.json" || fail 'manifest version mismatch'
grep -Fq "$sha64" "$ROOT/release/manifest.json" || fail 'manifest lacks amd64 hash'
grep -Fq "$sha_arm64" "$ROOT/release/manifest.json" || fail 'manifest lacks arm64 hash'

printf 'PASS: immutable release scripts, archives, helpers and checksums agree\n'
