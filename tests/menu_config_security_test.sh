#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly MENU="$ROOT/script/v2node.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

generator_block="$(awk '
  /^generate_v2node_config\(\)/ { inside=1 }
  /^generate_config_file\(\)/ { inside=0 }
  inside { print }
' "$MENU")"

prompt_block="$(awk '
  /^generate_config_file\(\)/ { inside=1 }
  /^open_ports\(\)/ { inside=0 }
  inside { print }
' "$MENU")"

grep -Eq '^[[:space:]]*umask[[:space:]]+077$' <<<"$generator_block" ||
  fail 'config generation does not use umask 077'
grep -Eq 'chmod[[:space:]]+0600[[:space:]]+"\$config_tmp"' <<<"$generator_block" ||
  fail 'temporary config is not restricted to mode 0600'
grep -Eq 'chmod[[:space:]]+0600[[:space:]]+"\$config_file"' <<<"$generator_block" ||
  fail 'final config mode 0600 is not enforced'
grep -Fq '""|"https://example.com"|"https://example.com/")' <<<"$generator_block" ||
  fail 'example.com placeholder is not rejected by the generator'
grep -Eq 'read[[:space:]]+-[^[:space:]]*s[^[:space:]]*[[:space:]].*api_key' <<<"$prompt_block" ||
  fail 'API key input is not hidden with read -s'
grep -Fq "printf '\\n'" <<<"$prompt_block" ||
  fail 'hidden API key prompt does not print a clean trailing newline'
if grep -Fq 'api_host=${api_host:-https://example.com/}' <<<"$prompt_block"; then
  fail 'example.com remains configured as a live default'
fi

printf 'PASS: management menu config credential and permission invariants\n'
