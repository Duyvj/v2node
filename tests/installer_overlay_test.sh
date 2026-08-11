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

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT

# Every supported memory size must preserve Go heap < soft pressure < hard cap.
for test_mem in 256 479 480 767 768 1279 1280 1599 1600 2048 4096; do
  effective_memory_mib() { printf '%s\n' "$test_mem"; }
  resource_profile >/dev/null
  (( GOMEMLIMIT_MIB < MEMORY_HIGH_MIB && MEMORY_HIGH_MIB < MEMORY_MAX_MIB )) ||
    fail "invalid memory ordering at ${test_mem} MiB"
  (( MEMORY_SWAP_MAX_MIB >= 128 && MEMORY_SWAP_MAX_MIB <= 512 )) ||
    fail "invalid swap ceiling at ${test_mem} MiB"
done

# Health validation must reject a service that repeatedly changes PID (flapping).
printf '0\n' > "$tmp/health-counter"
systemctl() {
  local prop='' arg next=0
  if [[ "$1" == is-active ]]; then printf 'active\n'; return 0; fi
  for arg in "$@"; do
    if (( next == 1 )); then prop="$arg"; break; fi
    [[ "$arg" == -p ]] && next=1
  done
  case "$prop" in
    MainPID)
      local count
      count="$(<"$tmp/health-counter")"
      count=$((count + 1))
      printf '%s\n' "$count" > "$tmp/health-counter"
      printf '%s\n' "$((1000 + count))"
      ;;
    ExecMainStatus|NRestarts) printf '0\n' ;;
    *) return 1 ;;
  esac
}
sleep() { :; }
if health_check; then
  fail 'health check accepted a continuously flapping service'
fi
unset -f systemctl sleep

# Build a complete format-3 backup without touching system paths.
BACKUP_DIR="$tmp/backup"
mkdir -p "$BACKUP_DIR" "$tmp/original" "$tmp/dropin-dir" "$tmp/support-dir"
printf '%s' 'original-binary' > "$tmp/original/v2node"
printf '%s' 'old-dropin' > "$tmp/original/dropin"
printf '%s' 'old-installer' > "$tmp/original/installer"
backup_path "$tmp/original/v2node" binary
backup_path "$tmp/original/dropin" dropin
backup_path "$tmp/original/installer" installer
record_directory_state "$tmp/dropin-dir" dropin-dir
record_directory_state "$tmp/support-dir" support-dir

for label in config service menu root-geoip root-geosite etc-geoip etc-geosite; do
  printf '%s' "$label-original" > "$tmp/original/$label"
  snapshot_path "$tmp/original/$label" "$label"
done
printf '%s\n' active > "$BACKUP_DIR/service-state"
printf '%s' 'candidate-binary' > "$tmp/original/candidate"
sha256_file "$tmp/original/candidate" > "$BACKUP_DIR/candidate-binary.sha256"
printf '3\n' > "$BACKUP_DIR/backup-format"
finalize_backup
touch "$BACKUP_DIR/prepared"
validate_backup "$BACKUP_DIR" || fail 'fresh complete overlay backup was rejected'
assert_untouched_files "$BACKUP_DIR" || fail 'untouched baseline did not validate'

# Rollback accepts its candidate/original binary, but refuses a later replacement.
cp "$tmp/original/candidate" "$tmp/original/live-binary"
rollback_binary_is_safe "$BACKUP_DIR" "$tmp/original/live-binary" || fail 'candidate binary was rejected for rollback'
cp "$BACKUP_DIR/binary" "$tmp/original/live-binary"
rollback_binary_is_safe "$BACKUP_DIR" "$tmp/original/live-binary" || fail 'already-restored binary was rejected'
printf '%s' 'newer-upstream-binary' > "$tmp/original/live-binary"
if rollback_binary_is_safe "$BACKUP_DIR" "$tmp/original/live-binary" 2>/dev/null; then
  fail 'rollback accepted a binary replaced after overlay installation'
fi

# Missing checksum coverage must fail even when all remaining checksums are valid.
cp "$BACKUP_DIR/backup-manifest.sha256" "$tmp/complete-manifest"
awk '!/\/binary$/' "$BACKUP_DIR/backup-manifest.sha256" > "$tmp/trimmed-manifest"
mv -f "$tmp/trimmed-manifest" "$BACKUP_DIR/backup-manifest.sha256"
if validate_backup "$BACKUP_DIR" 2>/dev/null; then
  fail 'validator accepted a manifest missing binary coverage'
fi
cp "$tmp/complete-manifest" "$BACKUP_DIR/backup-manifest.sha256"

# Modified rollback payload and modified untouched files must both be detected.
printf '%s' 'corrupt-binary' > "$BACKUP_DIR/binary"
if validate_backup "$BACKUP_DIR" 2>/dev/null; then
  fail 'validator accepted a modified binary backup'
fi
cp "$tmp/original/v2node" "$BACKUP_DIR/binary"
cp "$tmp/complete-manifest" "$BACKUP_DIR/backup-manifest.sha256"
printf '%s' 'changed-config' > "$tmp/original/config"
if assert_untouched_files "$BACKUP_DIR" 2>/dev/null; then
  fail 'untouched-file guard missed a config change'
fi

# Restore helpers replace the binary and remove paths that were originally absent.
printf '%s' 'patched-binary' > "$tmp/original/v2node"
restore_path "$BACKUP_DIR" binary "$tmp/original/v2node" || fail 'binary restore failed'
[[ "$(<"$tmp/original/v2node")" == 'original-binary' ]] || fail 'wrong restored binary content'
rm -f "$BACKUP_DIR/dropin"
: > "$BACKUP_DIR/dropin.missing"
printf '%s' 'candidate-dropin' > "$tmp/original/candidate-dropin"
restore_path "$BACKUP_DIR" dropin "$tmp/original/candidate-dropin" || fail 'missing drop-in restore failed'
[[ ! -e "$tmp/original/candidate-dropin" ]] || fail 'new drop-in survived rollback'

cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" || fail 'published installer differs from deploy installer'
grep -Fq 'ORIGINAL_BINARY="${ORIGINAL_ROOT}/v2node"' "$ROOT/deploy/install.sh" || fail 'upstream binary path changed'
grep -Fq 'DROPIN_FILE="${DROPIN_DIR}/90-v2node-ramfix.conf"' "$ROOT/deploy/install.sh" || fail 'RAM drop-in path is missing'
grep -Fq 'snapshot_path "$CONFIG_FILE" config' "$ROOT/deploy/install.sh" || fail 'config preservation guard is missing'
grep -Fq 'snapshot_path "$MENU_FILE" menu' "$ROOT/deploy/install.sh" || fail 'menu preservation guard is missing'
grep -Fq 'snapshot_path "$fragment" service' "$ROOT/deploy/install.sh" || fail 'main service preservation guard is missing'
grep -Fq 'MemoryHigh=${MEMORY_HIGH}' "$ROOT/deploy/install.sh" || fail 'MemoryHigh is missing'
grep -Fq 'MemoryMax=${MEMORY_MAX}' "$ROOT/deploy/install.sh" || fail 'MemoryMax is missing'
grep -Fq 'MemorySwapMax=${MEMORY_SWAP_MAX}' "$ROOT/deploy/install.sh" || fail 'MemorySwapMax is missing'
grep -Fq 'v0.4.4|v0.4.4-ram3)' "$ROOT/deploy/install.sh" || fail 'upstream-version gate is missing'
grep -Fq 'set -o noclobber' "$ROOT/deploy/install.sh" || fail 'lock creation is not symlink-safe'
grep -Fq 'exec 9<>"$LOCK_FILE"' "$ROOT/deploy/install.sh" || fail 'lock file is opened with a truncating mode'
if grep -Eq 'CURRENT_LINK|RELEASES_DIR|v2node-menu|v2nodectl|/swapfile|SYSCTL_FILE|write_service\(' "$ROOT/deploy/install.sh"; then
  fail 'installer still contains replacement-layout, menu/controller, swap, sysctl or main-service logic'
fi

install_block="$(awk '
  /^install_overlay_files\(\)/ { inside=1 }
  /^verify_service_profile\(\)/ { inside=0 }
  inside { print }
' "$ROOT/deploy/install.sh")"
if grep -Eq 'CONFIG_FILE|MENU_FILE|geoip|geosite|v2node\.service[^.]' <<<"$install_block"; then
  fail 'overlay write block touches an upstream-managed file'
fi

awk '
  /^install_overlay\(\)/ { inside=1 }
  inside && /validate_original_install/ { original=NR }
  inside && /download_and_verify/ { package=NR }
  inside && /snapshot_existing_state/ { backup=NR }
  inside && /TRANSACTION_ACTIVE=1/ { armed=NR }
  inside && /stop_service/ && !stop { stop=NR }
  inside && /install_overlay_files/ { live=NR }
  inside && /health_check/ { health=NR }
  inside && /touch "\$BACKUP_DIR\/committed"/ { commit=NR }
  inside && /^}/ { exit ! (original < package && package < backup && backup < armed && armed < stop && stop < live && live < health && health < commit) }
' "$ROOT/deploy/install.sh" || fail 'overlay transaction ordering invariant failed'

awk '
  /^main\(\)/ { inside=1 }
  inside && /ROLLBACK_REQUESTED == 1/ { rollback=NR }
  inside && /require_cmd unzip/ { unzip=NR }
  inside && /^}/ { exit ! (rollback && unzip && rollback < unzip) }
' "$ROOT/deploy/install.sh" || fail 'emergency rollback still depends on install-only ZIP tools'

restore_block="$(awk '
  /^restore_backup\(\)/ { inside=1 }
  /^cleanup_old_backups\(\)/ { inside=0 }
  inside { print }
' "$ROOT/deploy/install.sh")"
grep -Fq "upstream-managed files changed after the overlay; they will remain untouched" <<<"$restore_block" ||
  fail 'rollback does not tolerate later upstream/config drift'
grep -Fq '(( failed == 0 )) || return 1' <<<"$restore_block" ||
  fail 'rollback can remove its installer after an incomplete restore'
grep -Fq 'health_check || failed=1' <<<"$restore_block" ||
  fail 'rollback does not require the restored active service to remain healthy'

migration_block="$(awk '
  /^migrate_ram2_if_needed\(\)/ { inside=1 }
  /^validate_original_install\(\)/ { inside=0 }
  inside { print }
' "$ROOT/deploy/install.sh")"
grep -Fq 'reinstall upstream v0.4.4 first' <<<"$migration_block" ||
  fail 'legacy ram1/ram2 layout is not rejected safely'
if grep -Eq 'bash |--rollback|restore_backup' <<<"$migration_block"; then
  fail 'minimal overlay still mutates legacy ram1/ram2 state automatically'
fi

printf 'PASS: minimal v2node RAM overlay backup and preservation invariants\n'
