#!/usr/bin/env bash
# Transactional RAM-fix overlay for an existing upstream v2node installation.
# It leaves config, geodata, the upstream menu and the main service file untouched.

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

readonly PRODUCT="v2node"
readonly DEFAULT_VERSION="v0.4.4-ram4"
readonly RELEASE_REPOSITORY="https://github.com/Duyvj/v2node"
readonly DEFAULT_SHA256_64="783183398c053c41571881ca3c22bbbdd40ad59628312cb9d16056bdc9a8af9a"
readonly DEFAULT_SHA256_ARM64="2b9705727595ce2f08790a3b1411dcce55840005b75340a92a41afb26cf7a8d7"
readonly ORIGINAL_ROOT="/usr/local/v2node"
readonly ORIGINAL_BINARY="${ORIGINAL_ROOT}/v2node"
readonly CONFIG_FILE="/etc/v2node/config.json"
readonly MENU_FILE="/usr/bin/v2node"
readonly SERVICE_NAME="v2node.service"
readonly DROPIN_DIR="/etc/systemd/system/${SERVICE_NAME}.d"
readonly DROPIN_FILE="${DROPIN_DIR}/90-v2node-ramfix.conf"
readonly SUPPORT_DIR="/usr/local/lib/v2node-ramfix"
readonly PERSISTED_INSTALLER="${SUPPORT_DIR}/install.sh"
readonly BACKUP_ROOT="/var/backups/v2node-ramfix"
readonly LOCK_FILE="/run/lock/v2node-ramfix-install.lock"
readonly MAX_COMPRESSED_BYTES=134217728
readonly MAX_EXPANDED_BYTES=268435456

VERSION="$DEFAULT_VERSION"
PACKAGE_PATH=""
PACKAGE_URL=""
PACKAGE_SHA256=""
KEEP_BACKUPS=3
ROLLBACK_REQUESTED=0

TMP_DIR=""
BACKUP_DIR=""
SELF_SOURCE="${BASH_SOURCE[0]:-}"
SERVICE_WAS_ACTIVE="inactive"
TRANSACTION_ACTIVE=0

GOMEMLIMIT=""
MEMORY_HIGH=""
MEMORY_MAX=""
MEMORY_SWAP_MAX=""
GOMEMLIMIT_MIB=0
MEMORY_HIGH_MIB=0
MEMORY_MAX_MIB=0
MEMORY_SWAP_MAX_MIB=0
HOST_RESERVE_MIB=0

MEMINFO_FILE="/proc/meminfo"
CGROUP_V2_MEMORY_MAX_FILE="/sys/fs/cgroup/memory.max"
CGROUP_V1_MEMORY_LIMIT_FILE="/sys/fs/cgroup/memory/memory.limit_in_bytes"

log()  { printf '[%s] %s\n' "$PRODUCT" "$*"; }
warn() { printf '[%s] WARNING: %s\n' "$PRODUCT" "$*" >&2; }
die()  { printf '[%s] ERROR: %s\n' "$PRODUCT" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  install.sh [options]
  install.sh --rollback

This is an overlay for a v2node installation created by the upstream installer.
It replaces only /usr/local/v2node/v2node and adds a systemd RAM-limit drop-in.
It does not modify config.json, geodata, /usr/bin/v2node or v2node.service.

Options:
  --package FILE             Use a local release ZIP (requires --sha256)
  --package-url HTTPS_URL    Use a custom HTTPS ZIP (requires --sha256)
  --sha256 HASH              Exact SHA-256 for a custom package
  --version VERSION          Release label (default: v0.4.4-ram4)
  --keep-backups N           Retain N committed backups (default: 3)
  --rollback                 Restore the state before the latest overlay
  -h, --help                 Show this help

Run from a downloaded regular file; piped and symlinked execution is rejected.
This build is compatible only with upstream v0.4.4; it refuses newer baselines.
EOF
}

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

parse_args() {
  while (($#)); do
    case "$1" in
      --package) [[ $# -ge 2 ]] || die '--package needs a path'; PACKAGE_PATH="$2"; shift 2 ;;
      --package-url) [[ $# -ge 2 ]] || die '--package-url needs an URL'; PACKAGE_URL="$2"; shift 2 ;;
      --sha256) [[ $# -ge 2 ]] || die '--sha256 needs a hash'; PACKAGE_SHA256="${2,,}"; shift 2 ;;
      --version) [[ $# -ge 2 ]] || die '--version needs a value'; VERSION="$2"; shift 2 ;;
      --keep-backups) [[ $# -ge 2 ]] || die '--keep-backups needs a number'; KEEP_BACKUPS="$2"; shift 2 ;;
      --rollback) ROLLBACK_REQUESTED=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  [[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || die 'version contains unsafe characters'
  [[ "$KEEP_BACKUPS" =~ ^[1-9][0-9]*$ ]] || die '--keep-backups must be a positive integer'
  if [[ -n "$PACKAGE_PATH" && -n "$PACKAGE_URL" ]]; then
    die 'use either --package or --package-url, not both'
  fi
}

arch_asset() {
  case "$(uname -m)" in
    x86_64|amd64) printf '64\n' ;;
    aarch64|arm64) printf 'arm64-v8a\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

select_default_package() {
  local asset="$1"
  if [[ -n "$PACKAGE_PATH" || -n "$PACKAGE_URL" ]]; then
    [[ -n "$PACKAGE_SHA256" ]] || die 'custom packages require --sha256'
    return
  fi
  [[ "$VERSION" == "$DEFAULT_VERSION" ]] || die 'a custom --version requires --package or --package-url'
  PACKAGE_URL="${RELEASE_REPOSITORY}/releases/download/${DEFAULT_VERSION}/v2node-linux-${asset}.zip"
  case "$asset" in
    64) PACKAGE_SHA256="$DEFAULT_SHA256_64" ;;
    arm64-v8a) PACKAGE_SHA256="$DEFAULT_SHA256_ARM64" ;;
    *) die "unsupported release asset: $asset" ;;
  esac
  log "using pinned ${DEFAULT_VERSION} package"
}

effective_memory_mib() {
  local value mem_total=0 cgroup_limit=0 cgroup_limited=0
  mem_total="$(awk '/^MemTotal:/ { print int($2 / 1024) }' "$MEMINFO_FILE")"
  if [[ -r "$CGROUP_V2_MEMORY_MAX_FILE" ]]; then
    value="$(cat "$CGROUP_V2_MEMORY_MAX_FILE")"
    if [[ "$value" =~ ^[0-9]+$ ]] && (( value > 0 )); then
      cgroup_limit=$((value / 1024 / 1024))
      cgroup_limited=1
    fi
  fi
  if (( cgroup_limited == 0 )) && [[ -r "$CGROUP_V1_MEMORY_LIMIT_FILE" ]]; then
    value="$(cat "$CGROUP_V1_MEMORY_LIMIT_FILE")"
    if [[ "$value" =~ ^[0-9]+$ ]] && (( value > 0 && value < 9223372036854771712 )); then
      cgroup_limit=$((value / 1024 / 1024))
      cgroup_limited=1
    fi
  fi
  if (( cgroup_limited == 1 && (mem_total == 0 || cgroup_limit < mem_total) )); then
    printf '%s\n' "$cgroup_limit"
  else
    printf '%s\n' "$mem_total"
  fi
}

resource_profile() {
  local mem max_reserve high_headroom max_high_headroom go_headroom max_go_headroom
  mem="$(effective_memory_mib)"
  (( mem >= 256 )) || die "at least 256 MiB effective memory is required (detected ${mem} MiB)"

  # Capacity-oriented profile: reserve enough memory for the OS, but do not
  # penalize small VPSes by reserving more than one quarter of effective RAM.
  HOST_RESERVE_MIB=$((mem * 15 / 100))
  (( HOST_RESERVE_MIB >= 384 )) || HOST_RESERVE_MIB=384
  max_reserve=$((mem / 4))
  (( HOST_RESERVE_MIB <= max_reserve )) || HOST_RESERVE_MIB=$max_reserve
  MEMORY_MAX_MIB=$((mem - HOST_RESERVE_MIB))

  # MemoryHigh is an emergency pressure threshold, not the normal operating
  # target. Keep it close to MemoryMax so legitimate concurrent traffic is not
  # throttled early.
  high_headroom=$((MEMORY_MAX_MIB * 5 / 100))
  (( high_headroom >= 128 )) || high_headroom=128
  max_high_headroom=$((MEMORY_MAX_MIB / 4))
  (( high_headroom <= max_high_headroom )) || high_headroom=$max_high_headroom
  MEMORY_HIGH_MIB=$((MEMORY_MAX_MIB - high_headroom))

  # Leave non-heap/runtime/socket headroom below the cgroup pressure threshold
  # while allowing the Go collector to use materially more memory under load.
  go_headroom=$((MEMORY_MAX_MIB * 10 / 100))
  (( go_headroom >= 256 )) || go_headroom=256
  max_go_headroom=$((MEMORY_MAX_MIB / 3))
  (( go_headroom <= max_go_headroom )) || go_headroom=$max_go_headroom
  GOMEMLIMIT_MIB=$((MEMORY_MAX_MIB - go_headroom))

  MEMORY_SWAP_MAX_MIB=$((mem * 10 / 100))
  (( MEMORY_SWAP_MAX_MIB >= 128 )) || MEMORY_SWAP_MAX_MIB=128
  (( MEMORY_SWAP_MAX_MIB <= 512 )) || MEMORY_SWAP_MAX_MIB=512
  (( GOMEMLIMIT_MIB < MEMORY_HIGH_MIB && MEMORY_HIGH_MIB < MEMORY_MAX_MIB )) ||
    die 'invalid resource profile ordering'
  GOMEMLIMIT="${GOMEMLIMIT_MIB}MiB"
  MEMORY_HIGH="${MEMORY_HIGH_MIB}M"
  MEMORY_MAX="${MEMORY_MAX_MIB}M"
  MEMORY_SWAP_MAX="${MEMORY_SWAP_MAX_MIB}M"
  log "effective memory: ${mem} MiB; host reserve=${HOST_RESERVE_MIB} MiB; GOMEMLIMIT=${GOMEMLIMIT}; MemoryHigh=${MEMORY_HIGH}; MemoryMax=${MEMORY_MAX}; MemorySwapMax=${MEMORY_SWAP_MAX}"
}

sha256_file() {
  sha256sum "$1" | awk '{print tolower($1)}'
}

migrate_ram2_if_needed() {
  if [[ ! -L "$ORIGINAL_BINARY" && ! -L "${ORIGINAL_ROOT}/current" ]]; then
    return
  fi
  die 'older ram1/ram2 layout detected; reinstall upstream v0.4.4 first, then run this minimal overlay (no files were changed)'
}

validate_original_install() {
  local fragment exec_start installed_version version_output
  migrate_ram2_if_needed
  [[ -d "$ORIGINAL_ROOT" && ! -L "$ORIGINAL_ROOT" ]] || die "upstream install root is missing or unsafe: $ORIGINAL_ROOT"
  [[ -f "$ORIGINAL_BINARY" && ! -L "$ORIGINAL_BINARY" && -x "$ORIGINAL_BINARY" ]] ||
    die "install upstream v2node first; expected a regular executable at $ORIGINAL_BINARY"
  [[ -f "$CONFIG_FILE" && ! -L "$CONFIG_FILE" && -r "$CONFIG_FILE" ]] ||
    die "existing upstream config is missing or unsafe: $CONFIG_FILE"
  [[ -f "$MENU_FILE" && ! -L "$MENU_FILE" && -x "$MENU_FILE" ]] ||
    die "existing upstream management command is missing or unsafe: $MENU_FILE"
  fragment="$(systemctl show "$SERVICE_NAME" -p FragmentPath --value 2>/dev/null || true)"
  [[ "$fragment" == /* && -f "$fragment" && ! -L "$fragment" ]] ||
    die 'the upstream v2node systemd service is missing or unsafe'
  exec_start="$(systemctl show "$SERVICE_NAME" -p ExecStart --value 2>/dev/null || true)"
  [[ "$exec_start" == *"$ORIGINAL_BINARY"* ]] ||
    die "the existing service does not execute $ORIGINAL_BINARY"
  version_output="$("$ORIGINAL_BINARY" version 2>/dev/null || true)"
  installed_version="$(awk 'NR == 1 { print $2 }' <<<"$version_output")"
  case "$installed_version" in
    v0.4.4|v0.4.4-ram3|v0.4.4-ram4) ;;
    "") die 'could not determine the installed upstream v2node version' ;;
    *) die "this RAM fix is pinned to upstream v0.4.4, but the installed binary reports $installed_version" ;;
  esac
}

download_and_verify() {
  local actual asset declared_size elf_header elf_machine entry_size package_size total_size=0
  local -a curl_args transfer_status
  declare -A zip_entries=()
  [[ "$PACKAGE_SHA256" =~ ^[0-9a-f]{64}$ ]] || die '--sha256 must be a 64-character lowercase hex hash'
  if [[ -n "$PACKAGE_URL" ]]; then
    [[ "$PACKAGE_URL" == https://* ]] || die 'package URL must use https://'
    require_cmd curl
    require_cmd head
    curl_args=(--fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 600 --max-filesize "$MAX_COMPRESSED_BYTES")
    if curl --retry-all-errors --version >/dev/null 2>&1; then
      curl_args+=(--retry-all-errors)
    fi
    set +e
    curl "${curl_args[@]}" "$PACKAGE_URL" | head -c "$((MAX_COMPRESSED_BYTES + 1))" > "$TMP_DIR/package.zip"
    transfer_status=("${PIPESTATUS[@]}")
    set -e
    (( transfer_status[0] != 63 )) || die 'compressed package exceeds the 128 MiB safety limit'
    (( transfer_status[0] == 0 )) || die "package download failed (curl exit ${transfer_status[0]})"
    (( transfer_status[1] == 0 )) || die "package download limiter failed (head exit ${transfer_status[1]})"
  else
    [[ -r "$PACKAGE_PATH" && -f "$PACKAGE_PATH" && ! -L "$PACKAGE_PATH" ]] || die "package is not a readable regular file: $PACKAGE_PATH"
    cp -- "$PACKAGE_PATH" "$TMP_DIR/package.zip"
  fi
  PACKAGE_PATH="$TMP_DIR/package.zip"
  package_size="$(stat -c %s "$PACKAGE_PATH")"
  (( package_size > 0 && package_size <= MAX_COMPRESSED_BYTES )) || die 'compressed package size is invalid'
  actual="$(sha256_file "$PACKAGE_PATH")"
  [[ "$actual" == "$PACKAGE_SHA256" ]] || die "package SHA-256 mismatch (got $actual)"
  log "verified package SHA-256: $actual"

  while IFS= read -r asset; do
    [[ -n "$asset" ]] || continue
    [[ "$asset" != /* && "$asset" != *'..'* && "$asset" != *'/'* ]] || die "unsafe ZIP entry: $asset"
    [[ -z "${zip_entries[$asset]+x}" ]] || die "duplicate ZIP entry: $asset"
    zip_entries["$asset"]=1
    case "$asset" in
      v2node|LICENSE|VERSION|BUILDINFO) ;;
      *) die "unexpected ZIP entry: $asset" ;;
    esac
  done < <(unzip -Z1 "$PACKAGE_PATH")
  for asset in v2node LICENSE VERSION BUILDINFO; do
    [[ -n "${zip_entries[$asset]+x}" ]] || die "package missing $asset"
  done
  declared_size="$(unzip -l "$PACKAGE_PATH" | awk '$1 ~ /^[0-9]+$/ && $2 ~ /^[0-9][0-9][0-9][0-9]-/ && $3 ~ /^[0-9][0-9]:/ { total += $1 } END { printf "%.0f", total }')"
  [[ "$declared_size" =~ ^[0-9]+$ ]] || die 'could not determine expanded package size'
  (( declared_size <= MAX_EXPANDED_BYTES )) || die 'declared package size exceeds the 256 MiB safety limit'
  unzip -tqq "$PACKAGE_PATH" >/dev/null || die 'package ZIP integrity test failed'
  unzip -q "$PACKAGE_PATH" -d "$TMP_DIR/extracted"
  for asset in "${!zip_entries[@]}"; do
    [[ -f "$TMP_DIR/extracted/$asset" && ! -L "$TMP_DIR/extracted/$asset" ]] || die "package entry is not a regular file: $asset"
    [[ "$(stat -c %h "$TMP_DIR/extracted/$asset")" == 1 ]] || die "package entry has an unsafe hard-link count: $asset"
    entry_size="$(stat -c %s "$TMP_DIR/extracted/$asset")"
    (( total_size += entry_size ))
  done
  (( total_size <= MAX_EXPANDED_BYTES )) || die 'package expands beyond the 256 MiB safety limit'
  [[ "$(<"$TMP_DIR/extracted/VERSION")" == "$VERSION" ]] || die 'package VERSION does not match requested version'
  chmod 0755 "$TMP_DIR/extracted/v2node"
  elf_header="$(od -An -tx1 -N20 "$TMP_DIR/extracted/v2node" | tr -d '[:space:]')"
  [[ "${elf_header:0:12}" == '7f454c460201' ]] || die 'v2node is not a 64-bit little-endian ELF binary'
  elf_machine="${elf_header:36:4}"
  case "$(arch_asset):$elf_machine" in
    64:3e00|arm64-v8a:b700) ;;
    *) die "binary architecture does not match host (ELF machine $elf_machine)" ;;
  esac
}

backup_path() {
  local path="$1" label="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "refusing non-regular managed path: $path"
    cp -a --no-dereference "$path" "$BACKUP_DIR/$label"
  else
    : > "$BACKUP_DIR/$label.missing"
  fi
}

record_directory_state() {
  local path="$1" label="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || die "unsafe managed directory: $path"
    : > "$BACKUP_DIR/$label.present"
  else
    : > "$BACKUP_DIR/$label.missing"
  fi
}

snapshot_path() {
  local path="$1" label="$2"
  [[ "$path" == /* && "$path" != *$'\n'* ]] || die "unsafe snapshot path: $path"
  printf '%s\n' "$path" > "$BACKUP_DIR/$label.path"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "snapshot path is not a regular file: $path"
    : > "$BACKUP_DIR/$label.present"
    sha256_file "$path" > "$BACKUP_DIR/$label.sha256"
    stat -c '%a:%u:%g:%s:%Y' "$path" > "$BACKUP_DIR/$label.stat"
  else
    : > "$BACKUP_DIR/$label.missing"
  fi
}

validate_marker_pair() {
  local backup="$1" label="$2" payload=0 missing=0
  [[ -f "$backup/$label" && ! -L "$backup/$label" ]] && payload=1
  [[ -f "$backup/$label.missing" ]] && missing=1
  (( payload + missing == 1 )) || { warn "backup marker pair is incomplete or ambiguous: $label"; return 1; }
}

validate_presence_pair() {
  local backup="$1" label="$2" present=0 missing=0
  [[ -f "$backup/$label.present" ]] && present=1
  [[ -f "$backup/$label.missing" ]] && missing=1
  (( present + missing == 1 )) || { warn "backup state pair is incomplete or ambiguous: $label"; return 1; }
}

validate_snapshot_record() {
  local backup="$1" label="$2"
  [[ -f "$backup/$label.path" ]] || { warn "snapshot path is missing: $label"; return 1; }
  validate_presence_pair "$backup" "$label" || return 1
  if [[ -f "$backup/$label.present" ]]; then
    [[ -f "$backup/$label.sha256" && -f "$backup/$label.stat" ]] || { warn "snapshot metadata is incomplete: $label"; return 1; }
    [[ "$(<"$backup/$label.sha256")" =~ ^[0-9a-f]{64}$ ]] || { warn "snapshot hash is invalid: $label"; return 1; }
  else
    [[ ! -e "$backup/$label.sha256" && ! -e "$backup/$label.stat" ]] || { warn "missing snapshot has unexpected metadata: $label"; return 1; }
  fi
}

finalize_backup() {
  local path manifest_tmp="$BACKUP_DIR/.manifest.$$"
  : > "$manifest_tmp"
  for path in "$BACKUP_DIR"/*; do
    [[ -f "$path" && ! -L "$path" ]] || continue
    sha256sum "$path" >> "$manifest_tmp"
  done
  mv -Tf "$manifest_tmp" "$BACKUP_DIR/backup-manifest.sha256"
}

validate_backup() {
  local backup="$1" checksum_line manifest_path manifest_name path entries=0
  local checksum_pattern='^([0-9a-f]{64})[[:space:]][ *](.+)$'
  declare -A covered=()
  [[ -d "$backup" && ! -L "$backup" && -f "$backup/prepared" ]] || { warn "invalid backup: $backup"; return 1; }
  [[ "$(cat "$backup/backup-format" 2>/dev/null || true)" == 3 ]] || { warn "unsupported backup format: $backup"; return 1; }
  for label in binary dropin installer; do validate_marker_pair "$backup" "$label" || return 1; done
  for label in dropin-dir support-dir; do validate_presence_pair "$backup" "$label" || return 1; done
  for label in config service menu root-geoip root-geosite etc-geoip etc-geosite; do
    validate_snapshot_record "$backup" "$label" || return 1
  done
  [[ -f "$backup/service-state" && -f "$backup/backup-manifest.sha256" ]] || { warn 'backup state metadata is incomplete'; return 1; }
  [[ -f "$backup/candidate-binary.sha256" && "$(<"$backup/candidate-binary.sha256")" =~ ^[0-9a-f]{64}$ ]] || {
    warn 'candidate binary hash is missing or invalid'; return 1;
  }
  case "$(<"$backup/service-state")" in active|inactive|failed|unknown) ;; *) warn 'invalid saved service state'; return 1 ;; esac
  while IFS= read -r checksum_line; do
    [[ "$checksum_line" =~ $checksum_pattern ]] || { warn 'backup manifest contains an invalid record'; return 1; }
    manifest_path="${BASH_REMATCH[2]}"
    manifest_name="${manifest_path##*/}"
    [[ -n "$manifest_name" && "$manifest_path" == "$backup/$manifest_name" && -f "$manifest_path" && ! -L "$manifest_path" ]] || {
      warn 'backup manifest contains an unsafe entry'; return 1;
    }
    case "$manifest_name" in backup-manifest.sha256|prepared|committed|rolled-back) warn "backup manifest covers mutable state: $manifest_name"; return 1 ;; esac
    [[ -z "${covered[$manifest_name]+x}" ]] || { warn "backup manifest contains a duplicate: $manifest_name"; return 1; }
    covered["$manifest_name"]=1
    entries=$((entries + 1))
  done < "$backup/backup-manifest.sha256"
  (( entries > 0 )) || { warn 'backup manifest is empty'; return 1; }
  for path in "$backup"/*; do
    [[ -f "$path" && ! -L "$path" ]] || continue
    manifest_name="${path##*/}"
    case "$manifest_name" in backup-manifest.sha256|prepared|committed|rolled-back) continue ;; esac
    [[ -n "${covered[$manifest_name]+x}" ]] || { warn "backup manifest lacks coverage for: $manifest_name"; return 1; }
  done
  sha256sum --check --status "$backup/backup-manifest.sha256" || { warn 'backup checksum verification failed'; return 1; }
}

snapshot_existing_state() {
  local stamp fragment state
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$BACKUP_ROOT"
  BACKUP_DIR="$(mktemp -d "${BACKUP_ROOT}/${stamp}-${VERSION}.XXXXXX")"
  chmod 0700 "$BACKUP_DIR"
  backup_path "$ORIGINAL_BINARY" binary
  backup_path "$DROPIN_FILE" dropin
  backup_path "$PERSISTED_INSTALLER" installer
  record_directory_state "$DROPIN_DIR" dropin-dir
  record_directory_state "$SUPPORT_DIR" support-dir
  fragment="$(systemctl show "$SERVICE_NAME" -p FragmentPath --value)"
  snapshot_path "$CONFIG_FILE" config
  snapshot_path "$fragment" service
  snapshot_path "$MENU_FILE" menu
  snapshot_path "${ORIGINAL_ROOT}/geoip.dat" root-geoip
  snapshot_path "${ORIGINAL_ROOT}/geosite.dat" root-geosite
  snapshot_path "/etc/v2node/geoip.dat" etc-geoip
  snapshot_path "/etc/v2node/geosite.dat" etc-geosite
  state="$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)"
  [[ -n "$state" ]] || state=unknown
  case "$state" in active|inactive|failed|unknown) ;; *) die "service is in a transitional state ($state); retry after it settles" ;; esac
  printf '%s\n' "$state" > "$BACKUP_DIR/service-state"
  sha256_file "$TMP_DIR/extracted/v2node" > "$BACKUP_DIR/candidate-binary.sha256"
  printf '3\n' > "$BACKUP_DIR/backup-format"
  finalize_backup
  touch "$BACKUP_DIR/prepared"
  validate_backup "$BACKUP_DIR" || die "backup self-validation failed: $BACKUP_DIR"
  log "backup: $BACKUP_DIR"
}

assert_snapshot() {
  local backup="$1" label="$2" path expected_hash expected_stat
  path="$(<"$backup/$label.path")"
  if [[ -f "$backup/$label.present" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || { warn "untouched file disappeared or changed type: $path"; return 1; }
    expected_hash="$(<"$backup/$label.sha256")"
    expected_stat="$(<"$backup/$label.stat")"
    [[ "$(sha256_file "$path")" == "$expected_hash" ]] || { warn "untouched file content changed: $path"; return 1; }
    [[ "$(stat -c '%a:%u:%g:%s:%Y' "$path")" == "$expected_stat" ]] || { warn "untouched file metadata changed: $path"; return 1; }
  else
    [[ ! -e "$path" && ! -L "$path" ]] || { warn "untouched path was unexpectedly created: $path"; return 1; }
  fi
}

assert_untouched_files() {
  local backup="$1" label
  for label in config service menu root-geoip root-geosite etc-geoip etc-geosite; do
    assert_snapshot "$backup" "$label" || return 1
  done
}

stop_service() {
  local i
  systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  for ((i = 1; i <= 30; i++)); do
    systemctl is-active --quiet "$SERVICE_NAME" || return 0
    sleep 1
  done
  warn 'v2node service did not stop within 30 seconds'
  return 1
}

prepare_directory() {
  local path="$1" mode="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || die "unsafe directory: $path"
  else
    install -d -m "$mode" "$path"
  fi
}

install_overlay_files() {
  local binary_tmp="${ORIGINAL_ROOT}/.v2node-ramfix.$$" dropin_tmp installer_tmp
  prepare_directory "$DROPIN_DIR" 0755
  prepare_directory "$SUPPORT_DIR" 0755
  install -m 0755 -o root -g root "$TMP_DIR/extracted/v2node" "$binary_tmp"
  mv -Tf "$binary_tmp" "$ORIGINAL_BINARY"
  dropin_tmp="${DROPIN_FILE}.new.$$"
  cat > "$dropin_tmp" <<EOF
[Service]
Environment=GOMEMLIMIT=${GOMEMLIMIT}
MemoryAccounting=yes
MemoryHigh=${MEMORY_HIGH}
MemoryMax=${MEMORY_MAX}
MemorySwapMax=${MEMORY_SWAP_MAX}
EOF
  chmod 0644 "$dropin_tmp"
  chown root:root "$dropin_tmp"
  mv -Tf "$dropin_tmp" "$DROPIN_FILE"
  installer_tmp="${PERSISTED_INSTALLER}.new.$$"
  install -m 0700 -o root -g root "$SELF_SOURCE" "$installer_tmp"
  mv -Tf "$installer_tmp" "$PERSISTED_INSTALLER"
}

verify_service_profile() {
  local actual_high actual_max actual_swap environment effective_high effective_max
  actual_high="$(systemctl show "$SERVICE_NAME" -p MemoryHigh --value)"
  actual_max="$(systemctl show "$SERVICE_NAME" -p MemoryMax --value)"
  actual_swap="$(systemctl show "$SERVICE_NAME" -p MemorySwapMax --value)"
  environment="$(systemctl show "$SERVICE_NAME" -p Environment --value)"
  [[ "$actual_high" == "$((MEMORY_HIGH_MIB * 1024 * 1024))" ]] || die 'systemd did not apply MemoryHigh'
  [[ "$actual_max" == "$((MEMORY_MAX_MIB * 1024 * 1024))" ]] || die 'systemd did not apply MemoryMax'
  [[ "$actual_swap" == "$((MEMORY_SWAP_MAX_MIB * 1024 * 1024))" ]] || die 'systemd did not apply MemorySwapMax'
  [[ " $environment " == *" GOMEMLIMIT=${GOMEMLIMIT} "* ]] || die 'systemd did not apply GOMEMLIMIT'
  effective_high="$(systemctl show "$SERVICE_NAME" -p EffectiveMemoryHigh --value 2>/dev/null || true)"
  effective_max="$(systemctl show "$SERVICE_NAME" -p EffectiveMemoryMax --value 2>/dev/null || true)"
  if [[ "$effective_high" =~ ^[0-9]+$ ]] && (( effective_high < MEMORY_HIGH_MIB * 1024 * 1024 )); then
    warn "a parent cgroup lowers EffectiveMemoryHigh to $effective_high bytes"
  fi
  if [[ "$effective_max" =~ ^[0-9]+$ ]] && (( effective_max < MEMORY_MAX_MIB * 1024 * 1024 )); then
    warn "a parent cgroup lowers EffectiveMemoryMax to $effective_max bytes"
  fi
}

health_check() {
  local i active pid status restarts consecutive=0 last_pid='' last_restarts=''
  for ((i = 1; i <= 45; i++)); do
    active="$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)"
    pid="$(systemctl show "$SERVICE_NAME" -p MainPID --value 2>/dev/null || true)"
    status="$(systemctl show "$SERVICE_NAME" -p ExecMainStatus --value 2>/dev/null || true)"
    restarts="$(systemctl show "$SERVICE_NAME" -p NRestarts --value 2>/dev/null || true)"
    if [[ "$active" == active && "$pid" =~ ^[1-9][0-9]*$ && ( -z "$status" || "$status" == 0 ) ]]; then
      if [[ "$pid" == "$last_pid" && "$restarts" == "$last_restarts" ]]; then
        consecutive=$((consecutive + 1))
      else
        consecutive=1
        last_pid="$pid"
        last_restarts="$restarts"
      fi
      (( consecutive >= 10 )) && return 0
    else
      consecutive=0
    fi
    sleep 1
  done
  return 1
}

atomic_restore() {
  local source="$1" destination="$2" tmp
  [[ -f "$source" && ! -L "$source" ]] || return 1
  [[ ! -e "$destination" || ( -f "$destination" && ! -L "$destination" ) ]] || return 1
  tmp="$(dirname -- "$destination")/.v2node-restore.$$.$(basename -- "$destination")"
  rm -f -- "$tmp"
  cp -a --no-dereference "$source" "$tmp" || return 1
  mv -Tf "$tmp" "$destination"
}

restore_path() {
  local backup="$1" label="$2" destination="$3"
  validate_marker_pair "$backup" "$label" || return 1
  if [[ -f "$backup/$label.missing" ]]; then
    [[ ! -e "$destination" || ( -f "$destination" && ! -L "$destination" ) ]] || return 1
    rm -f -- "$destination"
  else
    atomic_restore "$backup/$label" "$destination"
  fi
}

rollback_binary_is_safe() {
  local backup="$1" live_binary="$2" live_hash original_hash candidate_hash
  [[ ! -e "$live_binary" && ! -L "$live_binary" ]] && return 0
  [[ -f "$live_binary" && ! -L "$live_binary" ]] || {
    warn "refusing rollback because the live binary changed type: $live_binary"
    return 1
  }
  live_hash="$(sha256_file "$live_binary")"
  original_hash="$(sha256_file "$backup/binary")"
  candidate_hash="$(<"$backup/candidate-binary.sha256")"
  [[ "$live_hash" == "$candidate_hash" || "$live_hash" == "$original_hash" ]] || {
    warn 'refusing rollback because v2node was replaced after this RAM overlay; reinstall a matching upstream version first'
    return 1
  }
}

restore_backup() {
  local backup="$1" state failed=0
  validate_backup "$backup" || return 1
  rollback_binary_is_safe "$backup" "$ORIGINAL_BINARY" || return 1
  if ! assert_untouched_files "$backup"; then
    warn 'upstream-managed files changed after the overlay; they will remain untouched'
  fi
  stop_service || return 1
  restore_path "$backup" binary "$ORIGINAL_BINARY" || failed=1
  restore_path "$backup" dropin "$DROPIN_FILE" || failed=1
  systemctl daemon-reload || failed=1
  state="$(<"$backup/service-state")"
  if [[ "$state" == active ]]; then
    systemctl start "$SERVICE_NAME" || failed=1
    health_check || failed=1
  else
    systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  fi
  (( failed == 0 )) || return 1
  restore_path "$backup" installer "$PERSISTED_INSTALLER" || return 1
  if [[ -f "$backup/dropin-dir.missing" ]]; then rmdir "$DROPIN_DIR" 2>/dev/null || true; fi
  if [[ -f "$backup/support-dir.missing" ]]; then rmdir "$SUPPORT_DIR" 2>/dev/null || true; fi
}

cleanup_old_backups() {
  local dir count=0
  while IFS= read -r dir; do
    [[ "$dir" == "${BACKUP_ROOT}/"* && -d "$dir" && ! -L "$dir" ]] || continue
    [[ -f "$dir/committed" || -f "$dir/rolled-back" ]] || continue
    count=$((count + 1))
    if (( count > KEEP_BACKUPS )); then
      rm -rf -- "$dir"
    fi
  done < <(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort -r)
}

rollback_latest() {
  local backup candidate
  backup=""
  while IFS= read -r candidate; do
    if [[ -f "$candidate/committed" && ! -f "$candidate/rolled-back" && "$(cat "$candidate/backup-format" 2>/dev/null || true)" == 3 ]]; then
      backup="$candidate"
      break
    fi
  done < <(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort -r)
  [[ -n "$backup" ]] || die 'no committed v2node RAM-fix backup found'
  log "rolling back $backup"
  restore_backup "$backup" || die 'rollback failed; inspect the warnings above before retrying'
  touch "$backup/rolled-back"
  log 'rollback complete; original v2node menu/config/geodata/service remain unchanged'
}

install_overlay() {
  local asset
  validate_original_install
  asset="$(arch_asset)"
  select_default_package "$asset"
  resource_profile
  download_and_verify
  snapshot_existing_state
  SERVICE_WAS_ACTIVE="$(<"$BACKUP_DIR/service-state")"
  TRANSACTION_ACTIVE=1
  stop_service || die 'could not quiesce the existing v2node service; no live file was changed'
  install_overlay_files
  systemctl daemon-reload
  verify_service_profile
  systemctl start "$SERVICE_NAME"
  health_check || die 'health check failed; automatic rollback will restore the original binary'
  assert_untouched_files "$BACKUP_DIR" || die 'an upstream-managed file changed during overlay installation'
  if [[ "$SERVICE_WAS_ACTIVE" != active ]]; then
    stop_service || die 'could not restore the original stopped service state'
  fi
  touch "$BACKUP_DIR/committed"
  TRANSACTION_ACTIVE=0
  cleanup_old_backups || warn 'could not remove one or more old RAM-fix backups'
  log "installed ${VERSION}; management remains: v2node"
  systemctl show "$SERVICE_NAME" -p ActiveState -p MainPID -p NRestarts -p MemoryCurrent -p MemoryHigh -p MemoryMax -p MemorySwapMax
}

cleanup_on_exit() {
  local rc=$?
  trap - EXIT
  set +e
  if (( rc != 0 && TRANSACTION_ACTIVE == 1 )) && [[ -n "$BACKUP_DIR" && -d "$BACKUP_DIR" ]]; then
    warn 'installation failed; restoring the pre-overlay binary and drop-in'
    if restore_backup "$BACKUP_DIR"; then
      touch "$BACKUP_DIR/rolled-back"
      warn 'automatic rollback finished'
    else
      warn "automatic rollback failed; backup retained at $BACKUP_DIR"
    fi
    TRANSACTION_ACTIVE=0
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
  rm -f -- "${ORIGINAL_ROOT}/.v2node-ramfix.$$" "${DROPIN_FILE}.new.$$" "${PERSISTED_INSTALLER}.new.$$"
  exit "$rc"
}

acquire_lock() {
  local lock_dir lock_mode lock_group lock_mode_value
  lock_dir="$(dirname -- "$LOCK_FILE")"
  [[ -d "$lock_dir" && ! -L "$lock_dir" ]] || die "unsafe lock directory: $lock_dir"
  [[ "$(stat -c %u "$lock_dir")" == 0 ]] || die "lock directory is not root-owned: $lock_dir"
  lock_mode="$(stat -c %a "$lock_dir")"
  lock_group="$(stat -c %g "$lock_dir")"
  lock_mode_value=$((8#$lock_mode))
  if (( (lock_mode_value & 0002) != 0 && (lock_mode_value & 01000) == 0 )); then
    die "world-writable lock directory lacks the sticky bit: $lock_dir"
  fi
  if (( (lock_mode_value & 0020) != 0 && lock_group != 0 && (lock_mode_value & 01000) == 0 )); then
    die "untrusted group-writable lock directory lacks the sticky bit: $lock_dir"
  fi
  if [[ ! -e "$LOCK_FILE" && ! -L "$LOCK_FILE" ]]; then
    (umask 077; set -o noclobber; : > "$LOCK_FILE") 2>/dev/null || true
  fi
  [[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" ]] || die "unsafe lock file: $LOCK_FILE"
  [[ "$(stat -c %u "$LOCK_FILE")" == 0 && "$(stat -c %h "$LOCK_FILE")" == 1 ]] ||
    die 'lock file ownership/link count is unsafe'
  exec 9<>"$LOCK_FILE"
  flock -n 9 || die 'another v2node RAM-fix transaction is running'
}

main() {
  parse_args "$@"
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die 'run as root'
  [[ -d /run/systemd/system ]] || die 'systemd is required'
  require_cmd systemctl
  require_cmd sha256sum
  require_cmd flock
  require_cmd stat
  require_cmd find
  acquire_lock
  trap cleanup_on_exit EXIT
  if (( ROLLBACK_REQUESTED == 1 )); then
    rollback_latest
    return
  fi
  require_cmd awk
  require_cmd unzip
  require_cmd install
  require_cmd od
  require_cmd mktemp
  TMP_DIR="$(mktemp -d /tmp/v2node-ramfix.XXXXXX)"
  chmod 0700 "$TMP_DIR"
  [[ -n "$SELF_SOURCE" && -f "$SELF_SOURCE" && ! -L "$SELF_SOURCE" ]] ||
    die 'download this installer to a regular file before running it; piped/symlinked execution is unsupported'
  install_overlay
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
