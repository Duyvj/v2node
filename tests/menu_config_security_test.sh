#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly MENU="$ROOT/script/v2node.sh"
readonly INSTALLER="$ROOT/script/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

bash -n "$MENU" || fail 'management menu syntax check failed'

for command in start stop restart status enable disable log generate update install uninstall version; do
  grep -Fq "\"$command\")" "$MENU" ||
    fail "original v2node command is missing: $command"
done

grep -Fq 'V2NODE_FORK_INSTALL_URL="https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh"' "$MENU" ||
  fail 'install/update URL is not pinned to the standalone branch'
grep -Fq 'V2NODE_FORK_MENU_URL="https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/v2node.sh"' "$MENU" ||
  fail 'menu self-update URL is not pinned to the standalone branch'
grep -Fq 'run_fork_installer "$version"' "$MENU" ||
  fail 'version update does not use the fork installer'
grep -Fq 'status=$?' "$MENU" ||
  fail 'installer failure status is not captured'
grep -Fq 'return "$status"' "$MENU" ||
  fail 'installer failure status is not propagated'
grep -Fq 'bash -n "$installer"' "$MENU" ||
  fail 'downloaded installer is not syntax-checked'
grep -Fq 'bash -n "$menu_tmp"' "$MENU" ||
  fail 'downloaded menu is not syntax-checked'
grep -Fq 'menu_tmp=$(mktemp /usr/bin/.v2node-menu.XXXXXX)' "$MENU" ||
  fail 'menu update is not staged on the destination filesystem'
grep -Fq 'expected_sha=$(sed -n' "$MENU" ||
  fail 'menu self-update does not load the installer-pinned checksum'
grep -Fq '"$actual_sha" != "$expected_sha"' "$MENU" ||
  fail 'menu self-update does not verify SHA-256'
grep -Fq "curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2" "$MENU" ||
  fail 'menu downloads do not enforce HTTPS failures'
grep -Fq 'chmod 600 /etc/v2node/config.json' "$MENU" ||
  fail 'generated config is not protected with mode 0600'
grep -Fq '"BufferSizeKB": 64' "$MENU" ||
  fail 'generated config omits the ram5 Runtime profile'
grep -Fq 'systemctl enable v2node >/dev/null 2>&1' "$MENU" ||
  fail 'config generation does not enable the previously unconfigured service'
grep -Fq '节点 ID 必须是正整数' "$MENU" ||
  fail 'generated config lacks node ID validation'
grep -Fq 'rm /etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf -f' "$MENU" ||
  fail 'uninstall does not remove the fork-owned RAM drop-in'

if grep -Eq 'raw\.githubusercontent\.com/wyx2685/v2node|--no-check-certificate|bash[[:space:]]+<\(|v2nodectl' "$MENU"; then
  fail 'insecure upstream update path or renamed CLI remains in menu'
fi

menu_sha="$(sha256sum "$MENU" | awk '{print $1}')"
embedded_sha="$(sed -n "s/^readonly MENU_SHA256='\\([0-9a-f]\\{64\\}\\)'$/\\1/p" "$INSTALLER")"
[[ "$embedded_sha" == "$menu_sha" ]] ||
  fail 'installer management-menu hash does not match script/v2node.sh'

printf 'PASS: original v2node CLI is fork-pinned and config-safe\n'
