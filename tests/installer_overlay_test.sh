#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=../deploy/install.sh
source "$ROOT/deploy/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_file_content() {
  local path="$1" expected="$2"
  [[ -f "$path" ]] || fail "missing regular file: $path"
  [[ "$(<"$path")" == "$expected" ]] || fail "unexpected content: $path"
}

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
BACKUP_DIR="$tmp/backup"
mkdir -p "$BACKUP_DIR" "$tmp/legacy" "$tmp/current"

# A legacy upstream binary must survive replacement and rollback byte-for-byte.
printf '%s' 'upstream-binary' > "$tmp/legacy/v2node"
printf '%s' 'ram-binary' > "$tmp/current/v2node"
backup_path "$tmp/legacy/v2node" legacy-binary
atomic_symlink "$tmp/current/v2node" "$tmp/legacy/v2node"
assert_file_content "$tmp/legacy/v2node" 'ram-binary'
if [[ "${MSYS:-}" == *winsymlinks* ]]; then
  [[ -L "$tmp/legacy/v2node" ]] || fail 'compatibility path is not a real symlink'
fi
restore_path "$BACKUP_DIR" legacy-binary "$tmp/legacy/v2node"
[[ ! -L "$tmp/legacy/v2node" ]] || fail 'legacy binary was restored as a symlink'
assert_file_content "$tmp/legacy/v2node" 'upstream-binary'

# A path absent before install must be removed by rollback.
backup_path "$tmp/legacy/v2nodectl" controller
printf '%s' 'new-controller' > "$tmp/legacy/v2nodectl"
restore_path "$BACKUP_DIR" controller "$tmp/legacy/v2nodectl"
[[ ! -e "$tmp/legacy/v2nodectl" && ! -L "$tmp/legacy/v2nodectl" ]] || fail 'new controller survived rollback'

# Corrupt/ambiguous backups must be rejected before they can delete live files.
mkdir -p "$tmp/corrupt"
printf '%s' 'payload' > "$tmp/corrupt/menu"
: > "$tmp/corrupt/menu.missing"
if validate_marker_pair "$tmp/corrupt" menu 2>/dev/null; then
  fail 'ambiguous backup marker pair was accepted'
fi

# Every supported memory size must preserve Go heap < soft pressure < hard cap.
for test_mem in 256 479 480 767 768 1279 1280 1599 1600 2048 4096; do
  effective_memory_mib() { printf '%s\n' "$test_mem"; }
  NO_RESOURCE_PROFILE=0
  resource_profile >/dev/null
  (( GOMEMLIMIT_MIB < MEMORY_HIGH_MIB && MEMORY_HIGH_MIB < MEMORY_MAX_MIB )) ||
    fail "invalid memory ordering at ${test_mem} MiB"
  (( MEMORY_SWAP_MAX_MIB >= 128 && MEMORY_SWAP_MAX_MIB <= 512 )) ||
    fail "invalid swap ceiling at ${test_mem} MiB"
done

# Existing symlink ownership/target semantics must also be retained.
printf '%s' 'old-menu' > "$tmp/legacy/menu-target"
if ln -s "$tmp/legacy/menu-target" "$tmp/legacy/menu-link" 2>/dev/null && [[ -L "$tmp/legacy/menu-link" ]]; then
  old_target="$(readlink "$tmp/legacy/menu-link")"
  backup_path "$tmp/legacy/menu-link" menu
  rm -f -- "$tmp/legacy/menu-link"
  printf '%s' 'new-menu' > "$tmp/legacy/menu-link"
  restore_path "$BACKUP_DIR" menu "$tmp/legacy/menu-link"
  [[ -L "$tmp/legacy/menu-link" ]] || fail 'legacy menu symlink type was not restored'
  [[ "$(readlink "$tmp/legacy/menu-link")" == "$old_target" ]] || fail 'legacy menu symlink target changed'
else
  printf 'SKIP: native symlink test is unavailable on this host\n'
fi

# The finalized rollback payload must detect truncation before live mutation.
BACKUP_DIR="$tmp/manifest-backup"
mkdir -p "$BACKUP_DIR"
printf '%s' 'original-service' > "$BACKUP_DIR/service"
for label in config.json sysctl legacy-binary legacy-geoip legacy-geosite menu controller installer; do
  : > "$BACKUP_DIR/$label.missing"
done
for label in config-dir state-dir install-root candidate-release; do
  : > "$BACKUP_DIR/$label.present"
done
: > "$BACKUP_DIR/previous-current"
printf '%s\n' "${RELEASES_DIR}/test-release" > "$BACKUP_DIR/candidate-release"
printf '%s\n' inactive > "$BACKUP_DIR/service-state"
printf '%s\n' not-found > "$BACKUP_DIR/service-enabled"
printf '%s\n' 60 > "$BACKUP_DIR/swappiness-before"
printf '%s\n' '# original fstab' > "$BACKUP_DIR/fstab-original"
printf '%s\n' 2 > "$BACKUP_DIR/backup-format"
finalize_backup
touch "$BACKUP_DIR/prepared"
validate_backup "$BACKUP_DIR" || fail 'fresh complete backup was rejected'
sha256sum --check --status "$BACKUP_DIR/backup-manifest.sha256" || fail 'fresh backup manifest did not verify'
cp "$BACKUP_DIR/backup-manifest.sha256" "$tmp/complete-backup-manifest.sha256"
awk '!/\/service$/' "$BACKUP_DIR/backup-manifest.sha256" > "$BACKUP_DIR/backup-manifest.sha256.trimmed"
mv -f "$BACKUP_DIR/backup-manifest.sha256.trimmed" "$BACKUP_DIR/backup-manifest.sha256"
if validate_backup "$BACKUP_DIR" 2>/dev/null; then
  fail 'rollback validator accepted a manifest missing payload coverage'
fi
cp "$tmp/complete-backup-manifest.sha256" "$BACKUP_DIR/backup-manifest.sha256"
printf '%s' 'truncated' > "$BACKUP_DIR/service"
if sha256sum --check --status "$BACKUP_DIR/backup-manifest.sha256"; then
  fail 'modified backup payload passed checksum validation'
fi
if validate_backup "$BACKUP_DIR" 2>/dev/null; then
  fail 'rollback validator accepted a modified backup payload'
fi

# A valid checksum over a truncated symlink manifest must not hide rollback links.
rm -f "$BACKUP_DIR/backup-manifest.sha256" "$BACKUP_DIR/prepared" "$BACKUP_DIR/service" "$BACKUP_DIR/symlink-targets"
printf '%s' 'original-service' > "$tmp/original-service"
if ln -s "$tmp/original-service" "$BACKUP_DIR/service" 2>/dev/null && [[ -L "$BACKUP_DIR/service" ]]; then
  finalize_backup
  touch "$BACKUP_DIR/prepared"
  validate_backup "$BACKUP_DIR" || fail 'fresh symlink backup was rejected'
  : > "$BACKUP_DIR/symlink-targets"
  awk '!/\/symlink-targets$/' "$BACKUP_DIR/backup-manifest.sha256" > "$BACKUP_DIR/backup-manifest.sha256.trimmed"
  sha256sum "$BACKUP_DIR/symlink-targets" >> "$BACKUP_DIR/backup-manifest.sha256.trimmed"
  mv -f "$BACKUP_DIR/backup-manifest.sha256.trimmed" "$BACKUP_DIR/backup-manifest.sha256"
  sha256sum --check --status "$BACKUP_DIR/backup-manifest.sha256" || fail 'tampered symlink manifest checksum setup failed'
  if validate_backup "$BACKUP_DIR" 2>/dev/null; then
    fail 'rollback validator accepted a symlink manifest missing payload coverage'
  fi
else
  printf 'SKIP: symlink manifest coverage test is unavailable on this host\n'
fi

cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" || fail 'published installer differs from deploy installer'
grep -Fq 'v2nodectl|v2node-menu' "$ROOT/deploy/install.sh" || fail 'management tools are not package-whitelisted'
grep -Fq 'restore_path "$backup" legacy-binary "$LEGACY_BINARY"' "$ROOT/deploy/install.sh" || fail 'legacy rollback hook is missing'
grep -Fq '"$BACKUP_DIR/config.json.missing"' "$ROOT/deploy/install.sh" || fail 'missing-config backup marker is inconsistent'
grep -Fq 'preserving existing config.json byte-for-byte' "$ROOT/deploy/install.sh" || fail 'config preservation invariant is missing'
grep -Fq 'MemoryMax=${MEMORY_MAX}' "$ROOT/deploy/install.sh" || fail 'hard memory ceiling is missing'
grep -Fq 'MemorySwapMax=${MEMORY_SWAP_MAX}' "$ROOT/deploy/install.sh" || fail 'swap ceiling is missing'
grep -Fq 'mv -Tf "${MENU_FILE}.new.$$" "$MENU_FILE"' "$ROOT/deploy/install.sh" || fail 'menu replacement is not no-dereference'
if grep -Fq 'raw.githubusercontent.com/wyx2685/v2node/master/script' "$ROOT/script/v2node.sh"; then
  fail 'management menu can reinstall upstream over the RAM build'
fi
if grep -Fq 'raw.githubusercontent.com/' "$ROOT/script/v2node.sh"; then
  fail 'management menu executes mutable remote shell content'
fi
awk '
  /^backup_state\(\)/ { inside=1 }
  inside && /finalize_backup/ { finalized=NR }
  inside && /touch "\$BACKUP_DIR\/prepared"/ { prepared=NR }
  inside && /validate_backup "\$BACKUP_DIR"/ { validated=NR }
  inside && /^}/ { exit ! (finalized < prepared && prepared < validated) }
' "$ROOT/deploy/install.sh" || fail 'backup is not self-validated before live mutation'
awk '
  /^install_release\(\)/ { inside=1 }
  inside && /backup_state/ { backup=NR }
  inside && /TRANSACTION_ACTIVE=1/ { armed=NR }
  inside && /quiesce_existing_service/ { stop=NR }
  inside && /install_config_if_requested/ { live=NR }
  inside && /health_check/ { health=NR }
  inside && /touch "\$BACKUP_DIR\/committed"/ { commit=NR }
  inside && /^}/ { exit ! (backup < armed && armed < stop && stop < live && live < health && health < commit) }
' "$ROOT/deploy/install.sh" || fail 'transaction/service ordering invariant failed'

printf 'PASS: installer overlay backup, compatibility and rollback invariants\n'
