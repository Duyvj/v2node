#!/usr/bin/env bash
# v2node-personal transactional installer/upgrader.
# It consumes a pinned, checksum-verified package; it never resolves upstream latest.

set -Eeuo pipefail
IFS=$'\n\t'
umask 077

readonly PRODUCT="v2node-personal"
readonly DEFAULT_VERSION="v0.4.4-personal.1"
readonly INSTALL_ROOT="/usr/local/v2node"
readonly RELEASES_DIR="${INSTALL_ROOT}/releases"
readonly CURRENT_LINK="${INSTALL_ROOT}/current"
readonly SERVICE_NAME="v2node.service"
readonly SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}"
readonly CONFIG_DIR="/etc/v2node"
readonly CONFIG_FILE="${CONFIG_DIR}/config.json"
readonly SYSCTL_FILE="/etc/sysctl.d/90-v2node-personal.conf"
readonly BACKUP_ROOT="/var/backups/v2node"
readonly LOCK_FILE="/run/lock/v2node-personal-install.lock"
readonly MAX_COMPRESSED_BYTES=134217728
SCRIPT_DIR=""
if [[ -n "${BASH_SOURCE[0]:-}" && -d "$(dirname -- "${BASH_SOURCE[0]}")" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
fi

VERSION="$DEFAULT_VERSION"
PACKAGE_PATH=""
PACKAGE_URL=""
PACKAGE_SHA256=""
CONFIG_SOURCE=""
API_HOST=""
NODE_ID=""
API_KEY_FILE=""
API_KEY_STDIN=0
NO_RESOURCE_PROFILE=0
NO_SWAP=0
KEEP_RELEASES=3

TMP_DIR=""
BACKUP_DIR=""
RELEASE_DIR=""
STAGING_DIR=""
SERVICE_WAS_ACTIVE="inactive"
TRANSACTION_ACTIVE=0

log()  { printf '[%s] %s\n' "$PRODUCT" "$*"; }
warn() { printf '[%s] WARNING: %s\n' "$PRODUCT" "$*" >&2; }
die()  { printf '[%s] ERROR: %s\n' "$PRODUCT" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  install.sh --package FILE --sha256 HASH [options]
  install.sh --package-url HTTPS_URL --sha256 HASH [options]
  install.sh --rollback

Required for a new install:
  --package FILE             Local package, or --package-url HTTPS_URL
  --sha256 HASH              Exact SHA-256 of that package
  --config-file FILE         Root-only JSON config (recommended), or:
  --api-host URL --node-id ID --api-key-file FILE

Options:
  --version VERSION          Release label (default: v0.4.4-personal.1)
  --keep-releases N          Retain N staged releases (default: 3)
  --api-key-stdin            Read the API key without putting it in argv
  --no-resource-profile      Do not create swap/sysctl/service guardrails
  --no-swap                  Skip emergency swap creation
  -h, --help                 Show this help

The installer never downloads the floating upstream 'latest' release.
EOF
}

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

parse_args() {
  while (($#)); do
    case "$1" in
      --package) [[ $# -ge 2 ]] || die "--package needs a path"; PACKAGE_PATH="$2"; shift 2 ;;
      --package-url) [[ $# -ge 2 ]] || die "--package-url needs an URL"; PACKAGE_URL="$2"; shift 2 ;;
      --sha256) [[ $# -ge 2 ]] || die "--sha256 needs a hash"; PACKAGE_SHA256="${2,,}"; shift 2 ;;
      --version) [[ $# -ge 2 ]] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
      --config-file) [[ $# -ge 2 ]] || die "--config-file needs a path"; CONFIG_SOURCE="$2"; shift 2 ;;
      --api-host) [[ $# -ge 2 ]] || die "--api-host needs a URL"; API_HOST="$2"; shift 2 ;;
      --node-id) [[ $# -ge 2 ]] || die "--node-id needs an integer"; NODE_ID="$2"; shift 2 ;;
      --api-key-file) [[ $# -ge 2 ]] || die "--api-key-file needs a path"; API_KEY_FILE="$2"; shift 2 ;;
      --api-key-stdin) API_KEY_STDIN=1; shift ;;
      --no-resource-profile) NO_RESOURCE_PROFILE=1; shift ;;
      --no-swap) NO_SWAP=1; shift ;;
      --keep-releases) [[ $# -ge 2 ]] || die "--keep-releases needs an integer"; KEEP_RELEASES="$2"; shift 2 ;;
      --rollback) ROLLBACK_REQUESTED=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  [[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || die "version contains unsafe characters"
  [[ "$KEEP_RELEASES" =~ ^([2-9]|[1-9][0-9]+)$ ]] || die "--keep-releases must be at least 2"
}

arch_asset() {
  case "$(uname -m)" in
    x86_64|amd64) printf '64\n' ;;
    aarch64|arm64) printf 'arm64-v8a\n' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

effective_memory_mib() {
  local value mem_total=0 cgroup_limit=0
  mem_total="$(awk '/^MemTotal:/ { print int($2 / 1024) }' /proc/meminfo)"
  if [[ -r /sys/fs/cgroup/memory.max ]]; then
    value="$(cat /sys/fs/cgroup/memory.max)"
    if [[ "$value" =~ ^[0-9]+$ ]] && (( value > 0 )); then
      cgroup_limit=$((value / 1024 / 1024))
    fi
  fi
  if (( cgroup_limit == 0 )) && [[ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]]; then
    value="$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)"
    if [[ "$value" =~ ^[0-9]+$ ]] && (( value > 0 && value < 9223372036854771712 )); then
      cgroup_limit=$((value / 1024 / 1024))
    fi
  fi
  if (( cgroup_limit > 0 && (mem_total == 0 || cgroup_limit < mem_total) )); then
    printf '%s\n' "$cgroup_limit"
  else
    printf '%s\n' "$mem_total"
  fi
}

resource_profile() {
  local mem
  if (( NO_RESOURCE_PROFILE == 1 )); then
    GOMEMLIMIT=""
    MEMORY_HIGH="infinity"
    SWAP_MIB=0
    RESOURCE_ENV='# resource profile disabled'
    RESOURCE_MEMORY='# resource profile disabled'
    return
  fi
  mem="$(effective_memory_mib)"
  if (( mem < 768 )); then
    GOMEMLIMIT="256MiB"; MEMORY_HIGH="384M"; SWAP_MIB=768
  elif (( mem < 1280 )); then
    GOMEMLIMIT="448MiB"; MEMORY_HIGH="600M"; SWAP_MIB=1024
  elif (( mem < 2560 )); then
    GOMEMLIMIT="896MiB"; MEMORY_HIGH="1280M"; SWAP_MIB=1024
  else
    GOMEMLIMIT="$((mem * 45 / 100))MiB"
    MEMORY_HIGH="$((mem * 65 / 100))M"
    SWAP_MIB=1024
  fi
  RESOURCE_ENV="Environment=GOMEMLIMIT=${GOMEMLIMIT}"
  RESOURCE_MEMORY="MemoryHigh=${MEMORY_HIGH}"
  log "effective memory: ${mem} MiB; GOMEMLIMIT=${GOMEMLIMIT}; MemoryHigh=${MEMORY_HIGH}"
}

sha256_file() {
  sha256sum "$1" | awk '{print tolower($1)}'
}

safe_config_source() {
  if [[ -n "$CONFIG_SOURCE" ]]; then
    [[ -r "$CONFIG_SOURCE" ]] || die "config file is not readable: $CONFIG_SOURCE"
    return
  fi
  [[ -f "$CONFIG_FILE" ]] && return
  [[ -n "$API_HOST" && -n "$NODE_ID" ]] || die "new install needs --config-file or API parameters"
  [[ "$NODE_ID" =~ ^[1-9][0-9]*$ ]] || die "--node-id must be a positive integer"
  if [[ -z "$API_KEY_FILE" && "$API_KEY_STDIN" -eq 0 ]]; then
    die "new install needs --api-key-file or --api-key-stdin"
  fi
}

generate_config() {
  local key key_file
  if [[ -n "$CONFIG_SOURCE" ]]; then
    cp -- "$CONFIG_SOURCE" "$TMP_DIR/config.json"
    return
  fi
  if [[ -f "$CONFIG_FILE" ]]; then
    return
  fi
  if [[ -n "$API_KEY_FILE" ]]; then
    [[ -r "$API_KEY_FILE" ]] || die "API key file is not readable"
    key="$(head -n 1 -- "$API_KEY_FILE")"
  else
    if ! IFS= read -r -s -p 'v2node API key: ' key; then
      [[ -n "$key" ]] || die "API key is empty"
    fi
    printf '\n' >&2
  fi
  [[ -n "$key" ]] || die "API key is empty"
  key_file="$TMP_DIR/.api-key"
  printf '%s' "$key" > "$key_file"
  chmod 0600 "$key_file"
  if command -v jq >/dev/null 2>&1; then
    jq -n --arg host "$API_HOST" --argjson id "$NODE_ID" --rawfile key "$key_file" \
      '{Log:{Level:"warning",Output:"",Access:"none"},Runtime:{MinPollIntervalSeconds:30,MaxPollIntervalSeconds:3600,BufferSizeKB:64},Nodes:[{ApiHost:$host,NodeID:$id,ApiKey:$key,Timeout:15}]}' \
      > "$TMP_DIR/config.json"
  elif command -v python3 >/dev/null 2>&1; then
    API_HOST="$API_HOST" NODE_ID="$NODE_ID" API_KEY_FILE="$key_file" python3 - <<'PY' > "$TMP_DIR/config.json"
import json, os
with open(os.environ["API_KEY_FILE"], "r", encoding="utf-8") as handle:
    api_key = handle.read()
print(json.dumps({
    "Log": {"Level": "warning", "Output": "", "Access": "none"},
    "Runtime": {"MinPollIntervalSeconds": 30, "MaxPollIntervalSeconds": 3600, "BufferSizeKB": 64},
    "Nodes": [{"ApiHost": os.environ["API_HOST"], "NodeID": int(os.environ["NODE_ID"]), "ApiKey": api_key, "Timeout": 15}],
}, separators=(",", ":")))
PY
  else
    die "install jq or python3, or provide --config-file"
  fi
  key=''
  rm -f -- "$key_file"
}

download_and_verify() {
  local expected actual asset elf_header elf_machine entry_size package_size declared_size total_size=0
  local -a curl_args transfer_status
  declare -A zip_entries=()
  [[ "$PACKAGE_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "--sha256 must be a 64-character hex hash"
  require_cmd stat
  if [[ -n "$PACKAGE_PATH" && -n "$PACKAGE_URL" ]]; then
    die "use either --package or --package-url, not both"
  fi
  if [[ -z "$PACKAGE_PATH" && -z "$PACKAGE_URL" ]]; then
    die "package source is required"
  fi
  if [[ -n "$PACKAGE_URL" ]]; then
    [[ "$PACKAGE_URL" == https://* ]] || die "package URL must use https://"
    require_cmd curl
    require_cmd head
    curl_args=(
      --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2
      --retry 3 --connect-timeout 15 --max-time 600
      --max-filesize "$MAX_COMPRESSED_BYTES"
    )
    if curl --retry-all-errors --version >/dev/null 2>&1; then
      curl_args+=(--retry-all-errors)
    fi
    set +e
    curl "${curl_args[@]}" "$PACKAGE_URL" |
      head -c "$((MAX_COMPRESSED_BYTES + 1))" > "$TMP_DIR/package.zip"
    transfer_status=("${PIPESTATUS[@]}")
    set -e
    PACKAGE_PATH="$TMP_DIR/package.zip"
    package_size="$(stat -c %s "$PACKAGE_PATH")"
    (( package_size <= MAX_COMPRESSED_BYTES )) || die 'compressed package exceeds the 128 MiB safety limit'
    (( transfer_status[0] != 63 )) || die 'compressed package exceeds the 128 MiB safety limit'
    (( transfer_status[0] == 0 )) || die "package download failed (curl exit ${transfer_status[0]})"
    (( transfer_status[1] == 0 )) || die "package download size limiter failed (head exit ${transfer_status[1]})"
  else
    [[ -r "$PACKAGE_PATH" ]] || die "package is not readable: $PACKAGE_PATH"
    package_size="$(stat -c %s "$PACKAGE_PATH")"
    (( package_size <= MAX_COMPRESSED_BYTES )) || die 'compressed package exceeds the 128 MiB safety limit'
    cp -- "$PACKAGE_PATH" "$TMP_DIR/package.zip"
    PACKAGE_PATH="$TMP_DIR/package.zip"
  fi
  actual="$(sha256_file "$PACKAGE_PATH")"
  expected="${PACKAGE_SHA256,,}"
  [[ "$actual" == "$expected" ]] || die "package SHA-256 mismatch (got $actual)"
  log "verified package SHA-256: $actual"

  require_cmd unzip
  require_cmd od
  package_size="$(stat -c %s "$PACKAGE_PATH")"
  (( package_size <= MAX_COMPRESSED_BYTES )) || die 'compressed package exceeds the 128 MiB safety limit'
  while IFS= read -r asset; do
    [[ -n "$asset" ]] || continue
    [[ "$asset" != /* && "$asset" != *'..'* && "$asset" != *'/'* ]] || die "unsafe ZIP entry: $asset"
    [[ -z "${zip_entries[$asset]+x}" ]] || die "duplicate ZIP entry: $asset"
    zip_entries["$asset"]=1
    case "$asset" in
      v2node|geoip.dat|geosite.dat|LICENSE|README.md|VERSION|BUILDINFO) ;;
      *) die "unexpected ZIP entry: $asset" ;;
    esac
  done < <(unzip -Z1 "$PACKAGE_PATH")
  for asset in v2node geoip.dat geosite.dat; do
    [[ -n "${zip_entries[$asset]+x}" ]] || die "package missing $asset"
  done
  declared_size="$(unzip -l "$PACKAGE_PATH" | awk '$1 ~ /^[0-9]+$/ && $2 ~ /^[0-9][0-9][0-9][0-9]-/ && $3 ~ /^[0-9][0-9]:/ { total += $1 } END { printf "%.0f", total }')"
  [[ "$declared_size" =~ ^[0-9]+$ ]] || die 'could not determine uncompressed package size'
  (( declared_size <= 536870912 )) || die 'declared package size exceeds the 512 MiB safety limit'
  unzip -tqq "$PACKAGE_PATH" >/dev/null || die 'package ZIP integrity test failed'
  unzip -q "$PACKAGE_PATH" -d "$TMP_DIR/extracted"
  for asset in "${!zip_entries[@]}"; do
    [[ -f "$TMP_DIR/extracted/$asset" && ! -L "$TMP_DIR/extracted/$asset" ]] || die "package entry is not a regular file: $asset"
    [[ "$(stat -c %h "$TMP_DIR/extracted/$asset")" == 1 ]] || die "package entry has an unsafe hard-link count: $asset"
    entry_size="$(stat -c %s "$TMP_DIR/extracted/$asset")"
    (( total_size += entry_size ))
  done
  (( total_size <= 536870912 )) || die 'package expands beyond the 512 MiB safety limit'
  chmod 0755 "$TMP_DIR/extracted/v2node"
  asset="$(arch_asset)"
  elf_header="$(od -An -tx1 -N20 "$TMP_DIR/extracted/v2node" | tr -d '[:space:]')"
  [[ "${elf_header:0:12}" == '7f454c460201' ]] || die 'v2node is not a 64-bit little-endian ELF binary'
  elf_machine="${elf_header:36:4}"
  case "$asset:$elf_machine" in
    64:3e00|arm64-v8a:b700) ;;
    *) die "binary architecture does not match host (ELF machine $elf_machine)" ;;
  esac
}

backup_state() {
  local stamp target enabled_state service_fragment
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$BACKUP_ROOT"
  BACKUP_DIR="$(mktemp -d "${BACKUP_ROOT}/${stamp}-${PACKAGE_SHA256:0:12}.XXXXXX")"
  chmod 0700 "$BACKUP_DIR"
  if [[ -e "$CURRENT_LINK" && ! -L "$CURRENT_LINK" ]]; then
    die "refusing unexpected non-symlink path: $CURRENT_LINK"
  elif [[ -L "$CURRENT_LINK" ]]; then
    target="$(readlink -f "$CURRENT_LINK")"
    [[ "$target" == "${RELEASES_DIR}/"* && -d "$target" ]] || die "refusing unexpected or missing current target: $target"
    printf '%s\n' "$target" > "$BACKUP_DIR/previous-current"
  else
    : > "$BACKUP_DIR/previous-current"
  fi
  [[ ! -L "$SERVICE_FILE" ]] || die "refusing symlinked or masked service file: $SERVICE_FILE"
  [[ ! -L "$SYSCTL_FILE" ]] || die "refusing symlinked sysctl file: $SYSCTL_FILE"
  [[ ! -e "$SERVICE_FILE" || -f "$SERVICE_FILE" ]] || die "service path is not a regular file: $SERVICE_FILE"
  [[ ! -e "$SYSCTL_FILE" || -f "$SYSCTL_FILE" ]] || die "sysctl path is not a regular file: $SYSCTL_FILE"
  [[ ! -e "$CONFIG_FILE" || -f "$CONFIG_FILE" || -L "$CONFIG_FILE" ]] || die "config path is not a regular file: $CONFIG_FILE"
  if [[ -e "$SERVICE_FILE" ]]; then cp -a --no-dereference "$SERVICE_FILE" "$BACKUP_DIR/service"; else : > "$BACKUP_DIR/service.missing"; fi
  service_fragment="$(systemctl show "$SERVICE_NAME" -p FragmentPath --value 2>/dev/null || true)"
  if [[ -n "$service_fragment" ]]; then
    [[ "$service_fragment" == /* && -f "$service_fragment" ]] || die "effective service fragment is unsafe or missing: $service_fragment"
    printf '%s\n' "$service_fragment" > "$BACKUP_DIR/service-fragment"
  fi
  if [[ -e "$SERVICE_FILE" || -n "$service_fragment" ]]; then
    touch "$BACKUP_DIR/service-present"
  fi
  if [[ -e "$CONFIG_FILE" || -L "$CONFIG_FILE" ]]; then cp -a --no-dereference "$CONFIG_FILE" "$BACKUP_DIR/config.json"; else : > "$BACKUP_DIR/config.missing"; fi
  if [[ -e "$SYSCTL_FILE" ]]; then cp -a --no-dereference "$SYSCTL_FILE" "$BACKUP_DIR/sysctl"; else : > "$BACKUP_DIR/sysctl.missing"; fi
  printf '%s\n' "$(systemctl is-active "$SERVICE_NAME" 2>/dev/null || true)" > "$BACKUP_DIR/service-state"
  enabled_state="$(systemctl is-enabled "$SERVICE_NAME" 2>/dev/null || true)"
  [[ "$enabled_state" != masked && "$enabled_state" != masked-runtime ]] || die 'service is masked; unmask it before installing'
  printf '%s\n' "$enabled_state" > "$BACKUP_DIR/service-enabled"
  printf '%s\n' "$(sysctl -n vm.swappiness 2>/dev/null || true)" > "$BACKUP_DIR/swappiness-before"
  touch "$BACKUP_DIR/prepared"
  log "backup: $BACKUP_DIR"
}

install_config_if_requested() {
  if [[ -f "$TMP_DIR/config.json" ]]; then
    install -d -m 0700 "$CONFIG_DIR"
    install -m 0600 -o root -g root "$TMP_DIR/config.json" "${CONFIG_FILE}.new.$$"
    mv -f "${CONFIG_FILE}.new.$$" "$CONFIG_FILE"
  fi
  if [[ -f "$CONFIG_FILE" && ! -L "$CONFIG_FILE" ]]; then
    chown root:root "$CONFIG_FILE"
    chmod 0600 "$CONFIG_FILE"
  elif [[ -L "$CONFIG_FILE" ]]; then
    warn 'existing config is a symlink; preserving its target and permissions'
  fi
}

apply_swap_and_sysctl() {
  local total_swap free_bytes fstab_tmp
  (( NO_RESOURCE_PROFILE == 1 )) && return
  install -d -m 0755 /etc/sysctl.d
  cat > "$TMP_DIR/sysctl.conf" <<'EOF'
# v2node-personal: emergency swap policy
vm.swappiness = 10
EOF
  install -m 0644 "$TMP_DIR/sysctl.conf" "$SYSCTL_FILE"
  sysctl -q -w vm.swappiness=10 || warn "could not apply vm.swappiness immediately"
  (( NO_SWAP == 1 )) && return
  total_swap="$(awk '/^SwapTotal:/ { print $2 }' /proc/meminfo)"
  (( total_swap >= 524288 )) && return
  [[ -e /swapfile || -L /swapfile ]] && { warn '/swapfile exists; leaving it untouched'; return; }
  free_bytes="$(df --output=avail -B1 / | tail -n 1 | tr -d ' ')"
  (( free_bytes >= (SWAP_MIB + 512) * 1024 * 1024 )) || { warn 'not enough disk for emergency swap'; return; }
  require_cmd dd
  require_cmd mkswap
  require_cmd swapon
  require_cmd swapoff
  log "creating ${SWAP_MIB} MiB emergency swap"
  touch "$BACKUP_DIR/swap-transaction-started"
  dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MIB" status=none
  chmod 0600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  if ! grep -Eq '^[[:space:]]*/swapfile[[:space:]]' /etc/fstab; then
    fstab_tmp="$TMP_DIR/fstab"
    cp -a /etc/fstab "$fstab_tmp"
    printf '\n/swapfile none swap sw 0 0 # v2node-personal\n' >> "$fstab_tmp"
    touch "$BACKUP_DIR/fstab-line-added"
    install -m 0644 "$fstab_tmp" /etc/fstab
  fi
}

write_service() {
  local service_tmp="$TMP_DIR/v2node.service"
  cat > "$service_tmp" <<EOF
[Unit]
Description=v2node-personal
Wants=network-online.target
After=network-online.target nss-lookup.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User=root
Group=root
UMask=0077
WorkingDirectory=${CURRENT_LINK}
ExecStart=${CURRENT_LINK}/v2node server
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
KillSignal=SIGTERM
${RESOURCE_ENV}
MemoryAccounting=yes
CPUAccounting=yes
${RESOURCE_MEMORY}
TasksMax=512
LimitNOFILE=65536
LimitCORE=0
LogRateLimitIntervalSec=30s
LogRateLimitBurst=500
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ReadWritePaths=${CONFIG_DIR} /var/lib/v2node /run/v2node
RuntimeDirectory=v2node

[Install]
WantedBy=multi-user.target
EOF
  install -m 0644 "$service_tmp" "$SERVICE_FILE"
  install -d -m 0750 /var/lib/v2node "$CONFIG_DIR"
}

stage_release() {
  local id asset
  id="${VERSION}-${PACKAGE_SHA256:0:12}"
  RELEASE_DIR="${RELEASES_DIR}/${id}"
  install -d -m 0755 "$RELEASES_DIR"
  if [[ -e "$RELEASE_DIR" ]]; then
    [[ -d "$RELEASE_DIR" && ! -L "$RELEASE_DIR" ]] || die "existing release path is unsafe: $RELEASE_DIR"
    for asset in v2node geoip.dat geosite.dat; do
      [[ -f "$RELEASE_DIR/$asset" && ! -L "$RELEASE_DIR/$asset" ]] || die "existing release is missing a regular $asset"
      [[ "$(sha256_file "$RELEASE_DIR/$asset")" == "$(sha256_file "$TMP_DIR/extracted/$asset")" ]] || die "existing release $asset does not match verified package"
    done
    log "release already staged: $RELEASE_DIR"
    return
  fi
  STAGING_DIR="$(mktemp -d "${RELEASES_DIR}/.staging-${id}.XXXXXX")"
  chmod 0755 "$STAGING_DIR"
  cp -a "$TMP_DIR/extracted/." "$STAGING_DIR/"
  chown -R root:root "$STAGING_DIR"
  chmod 0755 "$STAGING_DIR/v2node"
  chmod 0644 "$STAGING_DIR/geoip.dat" "$STAGING_DIR/geosite.dat"
  printf '%s\n' "$VERSION" > "$STAGING_DIR/VERSION"
  mv -T "$STAGING_DIR" "$RELEASE_DIR"
  STAGING_DIR=""
}

switch_current() {
  local link_tmp="${INSTALL_ROOT}/.current.$$"
  rm -f "$link_tmp"
  ln -s "$RELEASE_DIR" "$link_tmp"
  mv -Tf "$link_tmp" "$CURRENT_LINK"
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
      if (( consecutive >= 10 )); then
        return 0
      fi
    else
      consecutive=0
    fi
    sleep 1
  done
  systemctl status "$SERVICE_NAME" --no-pager -l >&2 || true
  return 1
}

restore_created_swap() {
  local backup="$1" fstab_tmp
  [[ -f "$backup/swap-transaction-started" ]] || return 0
  if awk 'NR > 1 && $1 == "/swapfile" { found=1 } END { exit !found }' /proc/swaps; then
    if ! swapoff /swapfile; then
      warn 'could not disable installer-created /swapfile; leaving it configured'
      return 1
    fi
  fi
  if [[ -f "$backup/fstab-line-added" && -f /etc/fstab ]]; then
    fstab_tmp="$(mktemp /etc/.fstab.v2node.XXXXXX)"
    awk '$0 != "/swapfile none swap sw 0 0 # v2node-personal"' /etc/fstab > "$fstab_tmp"
    chmod --reference=/etc/fstab "$fstab_tmp" 2>/dev/null || chmod 0644 "$fstab_tmp"
    chown --reference=/etc/fstab "$fstab_tmp" 2>/dev/null || chown root:root "$fstab_tmp"
    mv -f "$fstab_tmp" /etc/fstab
  fi
  rm -f -- /swapfile
}

restore_backup() {
  local backup="$1" target previous_state previous_enabled link_tmp previous_swappiness previous_fragment restored_fragment failed=0
  [[ -d "$backup" && -f "$backup/prepared" ]] || { warn "invalid backup: $backup"; return 1; }
  target="$(cat "$backup/previous-current" 2>/dev/null || true)"
  if [[ -n "$target" && ( "$target" != "${RELEASES_DIR}/"* || ! -d "$target" ) ]]; then
    warn 'unsafe or missing backup release target'
    return 1
  fi
  systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    warn 'could not stop candidate service during rollback'
    failed=1
  fi
  if [[ -e "$SERVICE_FILE" ]]; then
    systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || failed=1
  fi
  restore_created_swap "$backup" || failed=1
  rm -f -- "$SERVICE_FILE" || failed=1
  if [[ -e "$backup/service" || -L "$backup/service" ]]; then
    cp -a --no-dereference "$backup/service" "$SERVICE_FILE" || failed=1
  fi
  if [[ -n "$target" ]]; then
    link_tmp="${INSTALL_ROOT}/.rollback-current.$$"
    rm -f -- "$link_tmp"
    ln -s "$target" "$link_tmp" && mv -Tf "$link_tmp" "$CURRENT_LINK" || failed=1
  else
    rm -f "$CURRENT_LINK" || failed=1
  fi
  rm -f -- "$CONFIG_FILE" || failed=1
  if [[ -e "$backup/config.json" || -L "$backup/config.json" ]]; then
    cp -a --no-dereference "$backup/config.json" "$CONFIG_FILE" || failed=1
  fi
  rm -f -- "$SYSCTL_FILE" || failed=1
  if [[ -e "$backup/sysctl" || -L "$backup/sysctl" ]]; then
    cp -a --no-dereference "$backup/sysctl" "$SYSCTL_FILE" || failed=1
  fi
  previous_swappiness="$(cat "$backup/swappiness-before" 2>/dev/null || true)"
  if [[ "$previous_swappiness" =~ ^[0-9]+$ ]]; then
    sysctl -q -w "vm.swappiness=$previous_swappiness" 2>/dev/null || failed=1
  fi
  systemctl daemon-reload || failed=1
  previous_fragment="$(cat "$backup/service-fragment" 2>/dev/null || true)"
  if [[ -n "$previous_fragment" && ! -e "$backup/service" ]]; then
    restored_fragment="$(systemctl show "$SERVICE_NAME" -p FragmentPath --value 2>/dev/null || true)"
    if [[ "$restored_fragment" != "$previous_fragment" ]]; then
      warn "effective service fragment was not restored (expected $previous_fragment, got ${restored_fragment:-none})"
      failed=1
    fi
  fi
  previous_enabled="$(cat "$backup/service-enabled" 2>/dev/null || true)"
  if [[ -f "$backup/service-present" || -e "$backup/service" ]]; then
    case "$previous_enabled" in
      enabled) systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || failed=1 ;;
      enabled-runtime) systemctl enable --runtime "$SERVICE_NAME" >/dev/null 2>&1 || failed=1 ;;
      *) systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || failed=1 ;;
    esac
  fi
  if [[ -f "$backup/service-present" || -e "$backup/service" ]]; then
    previous_state="$(cat "$backup/service-state" 2>/dev/null || true)"
    if [[ "$previous_state" == active ]]; then
      systemctl restart "$SERVICE_NAME" || failed=1
      if (( failed == 0 )); then health_check || failed=1; fi
    else
      systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || failed=1
    fi
  fi
  if [[ -n "$target" && "$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)" != "$target" ]]; then
    failed=1
  fi
  (( failed == 0 ))
}

cleanup_on_exit() {
  local rc=$?
  trap - EXIT
  if (( rc != 0 && TRANSACTION_ACTIVE == 1 )) && [[ -n "$BACKUP_DIR" && -d "$BACKUP_DIR" ]]; then
    set +e
    warn "transaction failed; restoring $BACKUP_DIR"
    if restore_backup "$BACKUP_DIR"; then
      touch "$BACKUP_DIR/rolled-back"
      warn "automatic rollback finished"
    else
      warn "AUTOMATIC ROLLBACK FAILED; inspect $BACKUP_DIR immediately"
    fi
    TRANSACTION_ACTIVE=0
  fi
  if [[ -n "$STAGING_DIR" && "$STAGING_DIR" == "${RELEASES_DIR}/.staging-"* ]]; then
    rm -rf -- "$STAGING_DIR"
  fi
  rm -rf -- "${TMP_DIR:-}"
  exit "$rc"
}

rollback_latest() {
  local backup
  backup="$(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort -r | while IFS= read -r candidate; do
    if [[ -f "$candidate/committed" && ! -f "$candidate/rolled-back" ]]; then
      candidate_target="$(cat "$candidate/previous-current" 2>/dev/null || true)"
      if [[ -z "$candidate_target" || -d "$candidate_target" ]]; then
        printf '%s\n' "$candidate"
        break
      fi
    fi
  done)"
  [[ -n "$backup" ]] || die "no v2node backup found"
  log "rolling back to $backup"
  restore_backup "$backup"
  touch "$backup/rolled-back"
  log "rollback complete"
}

cleanup_old_releases() {
  local count=0 dir current backup target
  declare -A protected=()
  (( KEEP_RELEASES > 0 )) || return
  current="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
  if [[ -n "$current" ]]; then
    protected["$current"]=1
    count=1
  fi
  while IFS= read -r backup; do
    [[ -f "$backup/committed" && ! -f "$backup/rolled-back" ]] || continue
    target="$(cat "$backup/previous-current" 2>/dev/null || true)"
    [[ "$target" == "${RELEASES_DIR}/"* && -d "$target" ]] || continue
    if [[ -z "${protected[$target]+x}" ]]; then
      protected["$target"]=1
      count=$((count + 1))
    fi
    (( count >= KEEP_RELEASES )) && break
  done < <(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort -r)
  if (( count < KEEP_RELEASES )); then
    while IFS= read -r dir; do
      if [[ -z "${protected[$dir]+x}" ]]; then
        protected["$dir"]=1
        count=$((count + 1))
      fi
      (( count >= KEEP_RELEASES )) && break
    done < <(find "$RELEASES_DIR" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' 2>/dev/null | sort -nr | sed 's/^[^ ]* //')
  fi
  while IFS= read -r dir; do
    if [[ -z "${protected[$dir]+x}" ]]; then
      rm -rf -- "$dir"
    fi
  done < <(find "$RELEASES_DIR" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' 2>/dev/null | sort -nr | sed 's/^[^ ]* //')
}

install_release() {
  local asset
  require_cmd systemctl; require_cmd awk; require_cmd sha256sum; require_cmd unzip; require_cmd install; require_cmd flock
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die 'run as root'
  [[ -f /etc/os-release ]] || die '/etc/os-release missing'
  [[ -d /run/systemd/system ]] || die 'systemd is required'
  asset="$(arch_asset)"
  log "target asset: linux-${asset}"
  safe_config_source
  resource_profile
  download_and_verify
  backup_state
  TRANSACTION_ACTIVE=1
  generate_config
  apply_swap_and_sysctl
  stage_release
  install_config_if_requested
  write_service
  switch_current
  systemctl daemon-reload
  SERVICE_WAS_ACTIVE="$(cat "$BACKUP_DIR/service-state")"
  if [[ "$SERVICE_WAS_ACTIVE" == active ]]; then
    systemctl restart "$SERVICE_NAME"
  else
    systemctl start "$SERVICE_NAME"
  fi
  if ! health_check; then
    die "health check failed; automatic rollback will restore the previous release"
  fi
  if [[ -f "$BACKUP_DIR/service-present" || -f "$BACKUP_DIR/service" ]]; then
    systemctl disable "$SERVICE_NAME" >/dev/null
    case "$(cat "$BACKUP_DIR/service-enabled")" in
      enabled) systemctl enable "$SERVICE_NAME" >/dev/null ;;
      enabled-runtime) systemctl enable --runtime "$SERVICE_NAME" >/dev/null ;;
      *) systemctl disable "$SERVICE_NAME" >/dev/null ;;
    esac
    if [[ "$SERVICE_WAS_ACTIVE" != active ]]; then
      systemctl stop "$SERVICE_NAME"
    fi
  else
    systemctl enable "$SERVICE_NAME" >/dev/null
  fi
  touch "$BACKUP_DIR/committed"
  TRANSACTION_ACTIVE=0
  if [[ -f "$SCRIPT_DIR/v2nodectl.sh" ]]; then
    install -m 0755 "$SCRIPT_DIR/v2nodectl.sh" /usr/local/bin/v2nodectl || warn 'could not install v2nodectl helper'
  fi
  if [[ -n "$SCRIPT_DIR" && "$SCRIPT_DIR/install.sh" != "${INSTALL_ROOT}/install.sh" ]]; then
    install -m 0700 "$SCRIPT_DIR/install.sh" "${INSTALL_ROOT}/install.sh" || warn 'could not persist installer for rollback'
  fi
  cleanup_old_releases || warn 'could not remove one or more old releases'
  log "installed $VERSION at $CURRENT_LINK"
  systemctl show "$SERVICE_NAME" -p ActiveState -p MainPID -p ExecMainStatus -p MemoryHigh -p LimitNOFILE
}

main() {
  local ROLLBACK_REQUESTED=0
  parse_args "$@"
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die 'run as root'
  require_cmd mktemp
  require_cmd flock
  require_cmd stat
  acquire_lock
  if [[ "${ROLLBACK_REQUESTED:-0}" -eq 1 ]]; then
    rollback_latest
    return
  fi
  TMP_DIR="$(mktemp -d /tmp/v2node-personal.XXXXXX)"
  chmod 0700 "$TMP_DIR"
  trap cleanup_on_exit EXIT
  install_release
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
  [[ "$(stat -c %u "$LOCK_FILE")" == 0 && "$(stat -c %h "$LOCK_FILE")" == 1 ]] || die "lock file ownership/link count is unsafe"
  exec 9<>"$LOCK_FILE"
  flock -n 9 || die 'another v2node-personal transaction is running'
}

main "$@"
