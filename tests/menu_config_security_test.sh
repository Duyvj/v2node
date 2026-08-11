#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly MENU="$ROOT/script/v2node.sh"
readonly INSTALLER="$ROOT/deploy/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The source copy remains the upstream v0.4.4 menu; the overlay never deploys it.
if grep -Eq 'Duyvj|ramfix|RAM fix|v2nodectl|personal_installer|packaged_menu' "$MENU"; then
  fail 'upstream v2node menu contains RAM-release management changes'
fi
for command in start stop restart status enable disable log generate update install uninstall version; do
  grep -Fq "\"$command\")" "$MENU" || fail "upstream v2node command is missing: $command"
done
grep -Fq 'readonly MENU_FILE="/usr/bin/v2node"' "$INSTALLER" || fail 'installer does not identify the upstream menu path'
grep -Fq 'snapshot_path "$MENU_FILE" menu' "$INSTALLER" || fail 'installer does not snapshot the menu invariant'
if grep -Eq 'install .*MENU_FILE|mv .*MENU_FILE|rm .*MENU_FILE|cp .*MENU_FILE' "$INSTALLER"; then
  fail 'RAM overlay writes or removes the upstream v2node menu'
fi

printf 'PASS: original v2node management command is preserved\n'
