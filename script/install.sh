#!/usr/bin/env bash
# Standalone installer for the v2node v0.4.4-ram5 fork.

set -Eeuo pipefail
IFS=$'\n\t'
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export LC_ALL=C
umask 077

readonly PRODUCT='v2node'
readonly DEFAULT_VERSION='v0.4.4-ram5'
readonly RELEASE_REPOSITORY='https://github.com/Duyvj/v2node'
readonly RAW_RELEASE_BASE='https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram5'
readonly RAW_BRANCH_BASE='https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4'
readonly DEFAULT_SHA256_64='db1d0e83fbfff5b7b243fa0e9f469230964b083cf3339a68d6a48ec58f93e038'
readonly DEFAULT_SHA256_ARM64='ad88e6d888318e4875a1e28b37fa360ba5605ef7cc4f44ba866e1b4c8f54c2fd'
readonly BINARY_SHA256_64='59757459adc467f6508abd02e338f4d2d033c7fb0b01dab8018010af1c285344'
readonly BINARY_SHA256_ARM64='8374a701efef03b6fbcd81c0a50f222d21c930e76ca5184b3db6a1d01b4b6dc9'
readonly GEOIP_SHA256='83797719facc092e210f8f8e0e5e0b0bdfe06ac90a3a4a3d6a6ab2d781a917ae'
readonly GEOSITE_SHA256='b9b5d8ae506f226356f783820c22508e522edd188dfce9cd87d4908621104a47'
readonly CONFIG_SHA256='dc8347dfc3030a32941f3407dec579b9a8cd2a18b8c00bd2b037cd97645bb71e'
readonly MENU_SHA256='7ec5426401c089b2c2af6ad6335c0cedb713b629b4f1004c55c0db3f2175ee2a'

readonly INSTALL_ROOT='/usr/local/v2node'
readonly BINARY_FILE="${INSTALL_ROOT}/v2node"
readonly CONFIG_DIR='/etc/v2node'
readonly CONFIG_FILE="${CONFIG_DIR}/config.json"
readonly MENU_FILE='/usr/bin/v2node'
readonly SYSTEMD_UNIT='/etc/systemd/system/v2node.service'
readonly SYSTEMD_DROPIN_DIR='/etc/systemd/system/v2node.service.d'
readonly SYSTEMD_DROPIN="${SYSTEMD_DROPIN_DIR}/90-v2node-ramfix.conf"
readonly OPENRC_UNIT='/etc/init.d/v2node'
readonly LOCK_DIR='/run/v2node-standalone-install.lock'
readonly MAX_PACKAGE_BYTES=134217728
readonly MAX_GEODATA_BYTES=67108864
readonly MAX_TEXT_BYTES=1048576
readonly MAX_EXPANDED_BYTES=268435456

VERSION_ARG=''
API_HOST_ARG=''
NODE_ID_ARG=''
API_KEY_ARG=''
PLATFORM=''
SERVICE_MANAGER=''
ARCH_ASSET=''
PACKAGE_SHA256=''
BINARY_SHA256=''
TMP_DIR=''
STAGE_DIR=''
BACKUP_DIR=''
CONFIG_SOURCE=''
NEW_CONFIG=0
START_AFTER_INSTALL=0
SERVICE_WAS_ACTIVE=0
SERVICE_WAS_ENABLED=0
TRANSACTION_ACTIVE=0
ROLLBACK_RUNNING=0
LOCK_HELD=0

GOMEMLIMIT=''
MEMORY_HIGH=''
MEMORY_MAX=''
MEMORY_SWAP_MAX=''
GOMEMLIMIT_MIB=0
MEMORY_HIGH_MIB=0
MEMORY_MAX_MIB=0
MEMORY_SWAP_MAX_MIB=0
HOST_RESERVE_MIB=0

MEMINFO_FILE='/proc/meminfo'
CGROUP_V2_MEMORY_MAX_FILE='/sys/fs/cgroup/memory.max'
CGROUP_V1_MEMORY_LIMIT_FILE='/sys/fs/cgroup/memory/memory.limit_in_bytes'

declare -a SNAPSHOT_PATHS=()
declare -a SNAPSHOT_LABELS=()
declare -a LIVE_DIRS=()
declare -a CREATED_DIRS=()
declare -a TEMP_LIVE_FILES=()

log()  { printf '[%s] %s\n' "$PRODUCT" "$*"; }
warn() { printf '[%s] WARNING: %s\n' "$PRODUCT" "$*" >&2; }
die()  { printf '[%s] ERROR: %s\n' "$PRODUCT" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  install.sh [v0.4.4-ram5] [--api-host URL --node-id ID --api-key KEY]

Installs the RAM-stable v2node fork directly on a blank VPS, or upgrades an
existing v2node installation while preserving /etc/v2node/config.json.

Supported systems: Debian, Ubuntu, CentOS, Rocky, AlmaLinux, Alpine and Arch.
Supported CPUs: amd64/x86_64 and arm64/aarch64.

Options:
  --api-host URL    Panel API URL used for a new configuration
  --node-id ID      Positive numeric node ID
  --api-key KEY     Panel API key
  -h, --help        Show this help

The three panel options must be supplied together. This fork is intentionally
pinned to v0.4.4-ram5; other version arguments are rejected.
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

parse_args() {
  while (($#)); do
    case "$1" in
      --api-host)
        [[ $# -ge 2 ]] || die '--api-host requires a value'
        API_HOST_ARG="$2"
        shift 2
        ;;
      --node-id)
        [[ $# -ge 2 ]] || die '--node-id requires a value'
        NODE_ID_ARG="$2"
        shift 2
        ;;
      --api-key)
        [[ $# -ge 2 ]] || die '--api-key requires a value'
        API_KEY_ARG="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      --*)
        die "unknown option: $1"
        ;;
      *)
        [[ -z "$VERSION_ARG" ]] || die "unexpected argument: $1"
        VERSION_ARG="$1"
        shift
        ;;
    esac
  done

  case "${VERSION_ARG:-$DEFAULT_VERSION}" in
    v0.4.4|v0.4.4-ram5) ;;
    *) die "this standalone fork only installs $DEFAULT_VERSION" ;;
  esac

  local supplied=0
  [[ -n "$API_HOST_ARG" ]] && supplied=$((supplied + 1))
  [[ -n "$NODE_ID_ARG" ]] && supplied=$((supplied + 1))
  [[ -n "$API_KEY_ARG" ]] && supplied=$((supplied + 1))
  (( supplied == 0 || supplied == 3 )) ||
    die '--api-host, --node-id and --api-key must be supplied together'
}

detect_platform() {
  local os_id='' os_like=''
  if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    os_id="${ID:-}"
    os_like="${ID_LIKE:-}"
  fi
  case " $os_id $os_like " in
    *' alpine '*)
      PLATFORM='alpine'
      SERVICE_MANAGER='openrc'
      ;;
    *' debian '*|*' ubuntu '*)
      PLATFORM='debian'
      SERVICE_MANAGER='systemd'
      ;;
    *' rhel '*|*' fedora '*|*' centos '*|*' rocky '*|*' almalinux '*)
      PLATFORM='rhel'
      SERVICE_MANAGER='systemd'
      ;;
    *' arch '*)
      PLATFORM='arch'
      SERVICE_MANAGER='systemd'
      ;;
    *)
      if [[ -f /etc/redhat-release ]]; then
        PLATFORM='rhel'
        SERVICE_MANAGER='systemd'
      else
        die 'unsupported Linux distribution'
      fi
      ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64)
      ARCH_ASSET='64'
      PACKAGE_SHA256="$DEFAULT_SHA256_64"
      BINARY_SHA256="$BINARY_SHA256_64"
      ;;
    aarch64|arm64)
      ARCH_ASSET='arm64-v8a'
      PACKAGE_SHA256="$DEFAULT_SHA256_ARM64"
      BINARY_SHA256="$BINARY_SHA256_ARM64"
      ;;
    *)
      die "unsupported architecture: $(uname -m)"
      ;;
  esac
}

install_dependencies() {
  local missing=0 command_name unzip_help=''
  if [[ "$PLATFORM" == alpine ]]; then
    log 'ensuring Alpine uses full unzip and coreutils implementations'
    apk add --no-cache bash ca-certificates curl unzip coreutils
    update-ca-certificates >/dev/null 2>&1 || true
    return 0
  fi
  for command_name in curl unzip sha256sum awk grep od stat install; do
    command -v "$command_name" >/dev/null 2>&1 || missing=1
  done
  if command -v unzip >/dev/null 2>&1; then
    unzip_help="$(unzip -hh 2>&1 || true)"
    [[ "$unzip_help" == *'zipinfo mode'* ]] || missing=1
  fi
  (( missing == 1 )) || return 0

  log 'installing required packages'
  case "$PLATFORM" in
    debian)
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y \
        bash ca-certificates curl unzip coreutils
      update-ca-certificates >/dev/null 2>&1 || true
      ;;
    rhel)
      if command -v dnf >/dev/null 2>&1; then
        dnf install -y bash ca-certificates curl unzip coreutils
      else
        yum install -y bash ca-certificates curl unzip coreutils
      fi
      update-ca-trust force-enable >/dev/null 2>&1 || true
      update-ca-trust extract >/dev/null 2>&1 || true
      ;;
    arch)
      pacman -Sy --noconfirm --needed bash ca-certificates curl unzip coreutils
      update-ca-trust >/dev/null 2>&1 || true
      ;;
  esac
}

assert_safe_dir() {
  local path="$1"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || die "unsafe directory: $path"
  fi
}

assert_safe_file_or_missing() {
  local path="$1" links
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "managed path is not a regular file: $path"
    links="$(stat -c %h "$path")"
    [[ "$links" == 1 ]] || die "managed path has an unsafe hard-link count: $path"
  fi
}

validate_live_state() {
  local path
  for path in /usr /usr/local /usr/bin /etc /run; do
    assert_safe_dir "$path"
  done
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    for path in /etc/systemd /etc/systemd/system; do
      assert_safe_dir "$path"
    done
  else
    assert_safe_dir /etc/init.d
  fi
  assert_safe_dir "$INSTALL_ROOT"
  assert_safe_dir "$CONFIG_DIR"
  [[ "$SERVICE_MANAGER" != systemd ]] || assert_safe_dir "$SYSTEMD_DROPIN_DIR"

  register_managed_paths
  for path in "${SNAPSHOT_PATHS[@]}"; do
    assert_safe_file_or_missing "$path"
  done
}

register_managed_paths() {
  SNAPSHOT_PATHS=(
    "$BINARY_FILE"
    "$INSTALL_ROOT/LICENSE"
    "$INSTALL_ROOT/VERSION"
    "$INSTALL_ROOT/BUILDINFO"
    "$INSTALL_ROOT/geoip.dat"
    "$INSTALL_ROOT/geosite.dat"
    "$INSTALL_ROOT/config.json"
    "$CONFIG_DIR/geoip.dat"
    "$CONFIG_DIR/geosite.dat"
    "$CONFIG_FILE"
    "$MENU_FILE"
  )
  SNAPSHOT_LABELS=(
    binary license version buildinfo root-geoip root-geosite root-config
    etc-geoip etc-geosite config menu
  )
  LIVE_DIRS=("$INSTALL_ROOT" "$CONFIG_DIR")
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    SNAPSHOT_PATHS+=("$SYSTEMD_UNIT" "$SYSTEMD_DROPIN")
    SNAPSHOT_LABELS+=(systemd-unit systemd-dropin)
    LIVE_DIRS+=("$SYSTEMD_DROPIN_DIR")
  else
    SNAPSHOT_PATHS+=("$OPENRC_UNIT")
    SNAPSHOT_LABELS+=(openrc-unit)
  fi
}

acquire_lock() {
  local owner=''
  assert_safe_dir /run
  if mkdir -m 0700 "$LOCK_DIR" 2>/dev/null; then
    :
  else
    [[ -d "$LOCK_DIR" && ! -L "$LOCK_DIR" ]] || die "unsafe installer lock: $LOCK_DIR"
    if [[ -r "$LOCK_DIR/pid" ]]; then
      owner="$(<"$LOCK_DIR/pid")"
    fi
    if [[ "$owner" =~ ^[0-9]+$ ]] && kill -0 "$owner" 2>/dev/null; then
      die "another installer is running with PID $owner"
    fi
    rm -f -- "$LOCK_DIR/pid"
    rmdir -- "$LOCK_DIR" 2>/dev/null || die "stale installer lock cannot be removed: $LOCK_DIR"
    mkdir -m 0700 "$LOCK_DIR" || die 'could not acquire installer lock'
  fi
  printf '%s\n' "$$" > "$LOCK_DIR/pid"
  LOCK_HELD=1
}

release_lock() {
  (( LOCK_HELD == 1 )) || return 0
  rm -f -- "$LOCK_DIR/pid"
  rmdir -- "$LOCK_DIR" 2>/dev/null || true
  LOCK_HELD=0
}

sha256_file() {
  sha256sum "$1" | awk '{print tolower($1)}'
}

verify_sha256() {
  local path="$1" expected="$2" label="$3" actual
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "invalid embedded SHA-256 for $label"
  actual="$(sha256_file "$path")"
  [[ "$actual" == "$expected" ]] ||
    die "$label SHA-256 mismatch (expected $expected, got $actual)"
  log "verified $label"
}

download_file() {
  local url="$1" destination="$2" max_bytes="$3" size block_limit
  [[ "$url" == https://* ]] || die "refusing non-HTTPS URL: $url"
  block_limit=$(((max_bytes + 511) / 512))
  rm -f -- "$destination.part"
  if ! (
    ulimit -f "$block_limit"
    curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --retry 3 --connect-timeout 15 --max-time 600 \
      --output "$destination.part" "$url"
  ); then
    rm -f -- "$destination.part"
    die "download failed: $url"
  fi
  [[ -f "$destination.part" && ! -L "$destination.part" ]] ||
    die "download did not create a regular file: $url"
  size="$(stat -c %s "$destination.part")"
  (( size > 0 && size <= max_bytes )) ||
    die "download size is invalid for $url"
  mv -f -- "$destination.part" "$destination"
}

download_and_verify_assets() {
  local package_url
  STAGE_DIR="$TMP_DIR/stage"
  mkdir -m 0700 "$STAGE_DIR" "$STAGE_DIR/extracted"
  package_url="$RELEASE_REPOSITORY/releases/download/$DEFAULT_VERSION/v2node-linux-$ARCH_ASSET.zip"

  download_file "$package_url" "$STAGE_DIR/package.zip" "$MAX_PACKAGE_BYTES"
  verify_sha256 "$STAGE_DIR/package.zip" "$PACKAGE_SHA256" 'release archive'

  download_file "$RAW_RELEASE_BASE/assets/geoip.dat" \
    "$STAGE_DIR/geoip.dat" "$MAX_GEODATA_BYTES"
  verify_sha256 "$STAGE_DIR/geoip.dat" "$GEOIP_SHA256" 'geoip.dat'

  download_file "$RAW_RELEASE_BASE/assets/geosite.dat" \
    "$STAGE_DIR/geosite.dat" "$MAX_GEODATA_BYTES"
  verify_sha256 "$STAGE_DIR/geosite.dat" "$GEOSITE_SHA256" 'geosite.dat'

  download_file "$RAW_RELEASE_BASE/config.example.json" \
    "$STAGE_DIR/config.example.json" "$MAX_TEXT_BYTES"
  verify_sha256 "$STAGE_DIR/config.example.json" "$CONFIG_SHA256" 'config template'

  download_file "$RAW_BRANCH_BASE/script/v2node.sh" \
    "$STAGE_DIR/v2node-menu.sh" "$MAX_TEXT_BYTES"
  verify_sha256 "$STAGE_DIR/v2node-menu.sh" "$MENU_SHA256" 'management menu'
  bash -n "$STAGE_DIR/v2node-menu.sh" || die 'management menu syntax check failed'

  validate_release_archive
}

validate_release_archive() {
  local entry declared_size total_size=0 entry_size elf_header elf_machine
  local -A entries=()

  while IFS= read -r entry; do
    [[ -n "$entry" ]] || continue
    [[ "$entry" != /* && "$entry" != *'..'* && "$entry" != *'/'* ]] ||
      die "unsafe ZIP entry: $entry"
    [[ -z "${entries[$entry]+x}" ]] || die "duplicate ZIP entry: $entry"
    entries["$entry"]=1
    case "$entry" in
      v2node|LICENSE|VERSION|BUILDINFO) ;;
      *) die "unexpected ZIP entry: $entry" ;;
    esac
  done < <(unzip -Z1 "$STAGE_DIR/package.zip")

  for entry in v2node LICENSE VERSION BUILDINFO; do
    [[ -n "${entries[$entry]+x}" ]] || die "release archive is missing $entry"
  done

  declared_size="$(unzip -l "$STAGE_DIR/package.zip" |
    awk '$1 ~ /^[0-9]+$/ && $2 ~ /^[0-9][0-9][0-9][0-9]-/ && $3 ~ /^[0-9][0-9]:/ { total += $1 } END { printf "%.0f", total }')"
  [[ "$declared_size" =~ ^[0-9]+$ ]] || die 'could not read expanded archive size'
  (( declared_size <= MAX_EXPANDED_BYTES )) || die 'release archive is too large when expanded'
  unzip -tqq "$STAGE_DIR/package.zip" >/dev/null || die 'release archive integrity check failed'
  unzip -q "$STAGE_DIR/package.zip" -d "$STAGE_DIR/extracted"

  for entry in "${!entries[@]}"; do
    [[ -f "$STAGE_DIR/extracted/$entry" && ! -L "$STAGE_DIR/extracted/$entry" ]] ||
      die "release entry is not a regular file: $entry"
    [[ "$(stat -c %h "$STAGE_DIR/extracted/$entry")" == 1 ]] ||
      die "release entry has an unsafe hard-link count: $entry"
    entry_size="$(stat -c %s "$STAGE_DIR/extracted/$entry")"
    total_size=$((total_size + entry_size))
  done
  (( total_size <= MAX_EXPANDED_BYTES )) || die 'extracted release exceeds its safety limit'
  [[ "$(<"$STAGE_DIR/extracted/VERSION")" == "$DEFAULT_VERSION" ]] ||
    die 'release VERSION does not match the pinned version'
  verify_sha256 "$STAGE_DIR/extracted/v2node" "$BINARY_SHA256" 'v2node binary'

  elf_header="$(od -An -tx1 -N20 "$STAGE_DIR/extracted/v2node" | tr -d '[:space:]')"
  [[ "${elf_header:0:12}" == '7f454c460201' ]] ||
    die 'v2node is not a 64-bit little-endian ELF binary'
  elf_machine="${elf_header:36:4}"
  case "$ARCH_ASSET:$elf_machine" in
    64:3e00|arm64-v8a:b700) ;;
    *) die "v2node ELF architecture does not match this host ($elf_machine)" ;;
  esac
  chmod 0755 "$STAGE_DIR/extracted/v2node"
}

effective_memory_mib() {
  local value mem_total=0 cgroup_limit=0 cgroup_limited=0 cgroup_v2_present=0
  mem_total="$(awk '/^MemTotal:/ { print int($2 / 1024) }' "$MEMINFO_FILE")"
  if [[ -r "$CGROUP_V2_MEMORY_MAX_FILE" ]]; then
    cgroup_v2_present=1
    value="$(<"$CGROUP_V2_MEMORY_MAX_FILE")"
    if [[ "$value" =~ ^[0-9]+$ ]]; then
      cgroup_limit=$((value / 1024 / 1024))
      cgroup_limited=1
    fi
  fi
  if (( cgroup_v2_present == 0 && cgroup_limited == 0 )) &&
      [[ -r "$CGROUP_V1_MEMORY_LIMIT_FILE" ]]; then
    value="$(<"$CGROUP_V1_MEMORY_LIMIT_FILE")"
    if [[ "$value" =~ ^[0-9]+$ ]] &&
        (( value > 0 && value < 9223372036854771712 )); then
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
  (( mem >= 256 )) ||
    die "at least 256 MiB effective memory is required (detected $mem MiB)"

  HOST_RESERVE_MIB=$((mem * 15 / 100))
  (( HOST_RESERVE_MIB >= 384 )) || HOST_RESERVE_MIB=384
  max_reserve=$((mem / 4))
  (( HOST_RESERVE_MIB <= max_reserve )) || HOST_RESERVE_MIB=$max_reserve
  MEMORY_MAX_MIB=$((mem - HOST_RESERVE_MIB))

  high_headroom=$((MEMORY_MAX_MIB * 5 / 100))
  (( high_headroom >= 128 )) || high_headroom=128
  max_high_headroom=$((MEMORY_MAX_MIB / 4))
  (( high_headroom <= max_high_headroom )) || high_headroom=$max_high_headroom
  MEMORY_HIGH_MIB=$((MEMORY_MAX_MIB - high_headroom))

  go_headroom=$((MEMORY_MAX_MIB * 10 / 100))
  (( go_headroom >= 256 )) || go_headroom=256
  max_go_headroom=$((MEMORY_MAX_MIB / 3))
  (( go_headroom <= max_go_headroom )) || go_headroom=$max_go_headroom
  GOMEMLIMIT_MIB=$((MEMORY_MAX_MIB - go_headroom))

  MEMORY_SWAP_MAX_MIB=$((mem * 10 / 100))
  (( MEMORY_SWAP_MAX_MIB >= 128 )) || MEMORY_SWAP_MAX_MIB=128
  (( MEMORY_SWAP_MAX_MIB <= 512 )) || MEMORY_SWAP_MAX_MIB=512
  (( GOMEMLIMIT_MIB < MEMORY_HIGH_MIB && MEMORY_HIGH_MIB < MEMORY_MAX_MIB )) ||
    die 'invalid memory profile ordering'

  GOMEMLIMIT="${GOMEMLIMIT_MIB}MiB"
  MEMORY_HIGH="${MEMORY_HIGH_MIB}M"
  MEMORY_MAX="${MEMORY_MAX_MIB}M"
  MEMORY_SWAP_MAX="${MEMORY_SWAP_MAX_MIB}M"
  log "RAM profile: host reserve=$HOST_RESERVE_MIB MiB, GOMEMLIMIT=$GOMEMLIMIT, MemoryHigh=$MEMORY_HIGH, MemoryMax=$MEMORY_MAX"
}

validate_panel_values() {
  [[ "$API_HOST_ARG" =~ ^https?://[^[:space:]\"]+/?$ ]] ||
    die '--api-host must be a valid http:// or https:// URL'
  [[ ! "$API_HOST_ARG" =~ [[:cntrl:]] ]] ||
    die '--api-host must contain no control characters'
  [[ "$NODE_ID_ARG" =~ ^[1-9][0-9]*$ ]] ||
    die '--node-id must be a positive integer'
  [[ -n "$API_KEY_ARG" && ! "$API_KEY_ARG" =~ [[:cntrl:]] ]] ||
    die '--api-key must be non-empty and contain no control characters'
}

json_escape() {
  local value="$1"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  printf '%s' "$value"
}

write_generated_config() {
  local destination="$1" escaped_host escaped_key
  validate_panel_values
  escaped_host="$(json_escape "$API_HOST_ARG")"
  escaped_key="$(json_escape "$API_KEY_ARG")"
  cat > "$destination" <<EOF
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  },
  "Runtime": {
    "MinPollIntervalSeconds": 30,
    "MaxPollIntervalSeconds": 3600,
    "BufferSizeKB": 64,
    "MaxTrackedIPsPerUser": 256,
    "MaxTrackedIPsPerNode": 32768,
    "MaxPanelResponseBytes": 16777216,
    "MaxUsers": 100000
  },
  "Nodes": [
    {
      "ApiHost": "$escaped_host",
      "NodeID": $NODE_ID_ARG,
      "ApiKey": "$escaped_key",
      "Timeout": 15
    }
  ]
}
EOF
  chmod 0600 "$destination"
}

prepare_config_candidate() {
  if [[ -f "$CONFIG_FILE" ]]; then
    local existing_hash
    existing_hash="$(sha256_file "$CONFIG_FILE")"
    if [[ "$existing_hash" == "$CONFIG_SHA256" ]]; then
      if [[ -n "$API_HOST_ARG" ]]; then
        CONFIG_SOURCE="$STAGE_DIR/generated-config.json"
        write_generated_config "$CONFIG_SOURCE"
        NEW_CONFIG=1
        START_AFTER_INSTALL=1
      else
        CONFIG_SOURCE=''
        NEW_CONFIG=0
        START_AFTER_INSTALL=0
        warn 'the untouched example config is preserved; run v2node generate before starting the service'
      fi
      return
    fi
    CONFIG_SOURCE=''
    NEW_CONFIG=0
    START_AFTER_INSTALL=1
    if [[ -n "$API_HOST_ARG" ]]; then
      warn "existing $CONFIG_FILE is preserved; supplied panel arguments were ignored"
    fi
    if grep -Eq '"MemoryLimit"[[:space:]]*:' "$CONFIG_FILE"; then
      warn 'config Runtime.MemoryLimit overrides the automatic GOMEMLIMIT profile when non-empty'
    fi
    return
  fi

  NEW_CONFIG=1
  if [[ -n "$API_HOST_ARG" ]]; then
    CONFIG_SOURCE="$STAGE_DIR/generated-config.json"
    write_generated_config "$CONFIG_SOURCE"
    START_AFTER_INSTALL=1
    return
  fi

  if [[ -t 0 && -t 1 ]]; then
    local answer=''
    printf 'Create the panel configuration now? [Y/n]: '
    read -r answer
    if [[ ! "$answer" =~ ^[Nn]$ ]]; then
      read -r -p 'Panel API URL (for example https://panel.example/): ' API_HOST_ARG
      read -r -p 'Node ID: ' NODE_ID_ARG
      read -r -p 'Panel API key: ' API_KEY_ARG
      CONFIG_SOURCE="$STAGE_DIR/generated-config.json"
      write_generated_config "$CONFIG_SOURCE"
      START_AFTER_INSTALL=1
      return
    fi
  fi

  CONFIG_SOURCE="$STAGE_DIR/config.example.json"
  START_AFTER_INSTALL=0
}

write_service_candidates() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    cat > "$STAGE_DIR/v2node.service" <<'EOF'
[Unit]
Description=v2node Service
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
WorkingDirectory=/usr/local/v2node/
ExecStart=/usr/local/v2node/v2node server
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
    cat > "$STAGE_DIR/90-v2node-ramfix.conf" <<EOF
[Service]
Environment="GOMEMLIMIT=$GOMEMLIMIT"
MemoryAccounting=yes
MemoryHigh=$MEMORY_HIGH
MemoryMax=$MEMORY_MAX
MemoryLimit=$MEMORY_MAX
MemorySwapMax=$MEMORY_SWAP_MAX
EOF
  else
    cat > "$STAGE_DIR/v2node.openrc" <<EOF
#!/sbin/openrc-run

name="v2node"
description="v2node"
command="/usr/local/v2node/v2node"
command_args="server"
command_user="root"
command_background="yes"
pidfile="/run/v2node.pid"
export GOMEMLIMIT="$GOMEMLIMIT"

depend() {
    need net
}
EOF
  fi
}

snapshot_state() {
  local index path label
  BACKUP_DIR="$TMP_DIR/backup"
  mkdir -m 0700 "$BACKUP_DIR"
  for index in "${!SNAPSHOT_PATHS[@]}"; do
    path="${SNAPSHOT_PATHS[$index]}"
    label="${SNAPSHOT_LABELS[$index]}"
    if [[ -e "$path" ]]; then
      cp -p -- "$path" "$BACKUP_DIR/$label"
      : > "$BACKUP_DIR/$label.present"
    else
      : > "$BACKUP_DIR/$label.missing"
    fi
  done

  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl is-active --quiet v2node.service 2>/dev/null && SERVICE_WAS_ACTIVE=1 || true
    systemctl is-enabled --quiet v2node.service 2>/dev/null && SERVICE_WAS_ENABLED=1 || true
  else
    service v2node status >/dev/null 2>&1 && SERVICE_WAS_ACTIVE=1 || true
    rc-update show default 2>/dev/null | grep -Eq '(^|[[:space:]])v2node([[:space:]]|$)' &&
      SERVICE_WAS_ENABLED=1 || true
  fi
}

create_live_directories() {
  local path
  for path in "${LIVE_DIRS[@]}"; do
    if [[ -e "$path" ]]; then
      assert_safe_dir "$path"
    else
      mkdir -m 0755 -- "$path"
      CREATED_DIRS+=("$path")
    fi
  done
}

atomic_install() {
  local source="$1" destination="$2" mode="$3" temporary
  assert_safe_file_or_missing "$destination"
  temporary="$destination.new.$$"
  [[ ! -e "$temporary" && ! -L "$temporary" ]] ||
    die "temporary install path already exists: $temporary"
  TEMP_LIVE_FILES+=("$temporary")
  install -o root -g root -m "$mode" -- "$source" "$temporary"
  mv -f -- "$temporary" "$destination"
}

stop_service_for_install() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    if systemctl is-active --quiet v2node.service 2>/dev/null; then
      systemctl stop v2node.service
    fi
  elif service v2node status >/dev/null 2>&1; then
    service v2node stop
  fi
}

install_candidate_files() {
  atomic_install "$STAGE_DIR/extracted/v2node" "$BINARY_FILE" 0755
  atomic_install "$STAGE_DIR/extracted/LICENSE" "$INSTALL_ROOT/LICENSE" 0644
  atomic_install "$STAGE_DIR/extracted/VERSION" "$INSTALL_ROOT/VERSION" 0644
  atomic_install "$STAGE_DIR/extracted/BUILDINFO" "$INSTALL_ROOT/BUILDINFO" 0644
  atomic_install "$STAGE_DIR/geoip.dat" "$INSTALL_ROOT/geoip.dat" 0644
  atomic_install "$STAGE_DIR/geosite.dat" "$INSTALL_ROOT/geosite.dat" 0644
  atomic_install "$STAGE_DIR/config.example.json" "$INSTALL_ROOT/config.json" 0644
  atomic_install "$STAGE_DIR/geoip.dat" "$CONFIG_DIR/geoip.dat" 0644
  atomic_install "$STAGE_DIR/geosite.dat" "$CONFIG_DIR/geosite.dat" 0644
  atomic_install "$STAGE_DIR/v2node-menu.sh" "$MENU_FILE" 0755

  if (( NEW_CONFIG == 1 )); then
    atomic_install "$CONFIG_SOURCE" "$CONFIG_FILE" 0600
  else
    chown root:root "$CONFIG_FILE"
    chmod 0600 "$CONFIG_FILE"
  fi

  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    atomic_install "$STAGE_DIR/v2node.service" "$SYSTEMD_UNIT" 0644
    atomic_install "$STAGE_DIR/90-v2node-ramfix.conf" "$SYSTEMD_DROPIN" 0644
  else
    atomic_install "$STAGE_DIR/v2node.openrc" "$OPENRC_UNIT" 0755
  fi
}

verify_installed_files() {
  verify_sha256 "$BINARY_FILE" "$BINARY_SHA256" 'installed v2node binary'
  verify_sha256 "$INSTALL_ROOT/geoip.dat" "$GEOIP_SHA256" 'installed root geoip.dat'
  verify_sha256 "$INSTALL_ROOT/geosite.dat" "$GEOSITE_SHA256" 'installed root geosite.dat'
  verify_sha256 "$CONFIG_DIR/geoip.dat" "$GEOIP_SHA256" 'installed config geoip.dat'
  verify_sha256 "$CONFIG_DIR/geosite.dat" "$GEOSITE_SHA256" 'installed config geosite.dat'
  verify_sha256 "$INSTALL_ROOT/config.json" "$CONFIG_SHA256" 'installed config template'
  verify_sha256 "$MENU_FILE" "$MENU_SHA256" 'installed management menu'
  [[ "$(stat -c %a "$BINARY_FILE")" == 755 ]] || die 'installed binary mode is not 0755'
  [[ "$(stat -c %a "$MENU_FILE")" == 755 ]] || die 'installed menu mode is not 0755'
  [[ "$(stat -c %a "$CONFIG_FILE")" == 600 ]] || die 'installed config mode is not 0600'
  [[ "$(stat -c %u:%g "$BINARY_FILE")" == 0:0 ]] || die 'installed binary is not root-owned'
  [[ "$(stat -c %u:%g "$CONFIG_FILE")" == 0:0 ]] || die 'installed config is not root-owned'
  "$BINARY_FILE" version 2>&1 | grep -Fq "$DEFAULT_VERSION" ||
    die 'installed binary reports an unexpected version'
}

systemd_property() {
  local property="$1" output
  output="$(systemctl show v2node.service -p "$property" 2>/dev/null || true)"
  if [[ "$output" == "$property="* ]]; then
    printf '%s\n' "${output#*=}"
  else
    # Also accept raw values from mocks and non-systemctl compatible wrappers.
    printf '%s\n' "$output"
  fi
}

verify_service_profile() {
  local environment actual_high actual_max actual_limit actual_swap
  [[ "$SERVICE_MANAGER" == systemd ]] || return 0
  environment="$(systemd_property Environment)"
  [[ " $environment " == *" GOMEMLIMIT=$GOMEMLIMIT "* ]] ||
    die 'systemd did not apply GOMEMLIMIT'

  actual_high="$(systemd_property MemoryHigh)"
  actual_max="$(systemd_property MemoryMax)"
  actual_limit="$(systemd_property MemoryLimit)"
  actual_swap="$(systemd_property MemorySwapMax)"

  if [[ "$actual_high" =~ ^[0-9]+$ ]]; then
    [[ "$actual_high" == "$((MEMORY_HIGH_MIB * 1024 * 1024))" ]] ||
      die 'systemd applied an unexpected MemoryHigh'
  else
    warn 'this systemd version does not expose MemoryHigh; GOMEMLIMIT remains active'
  fi
  if [[ "$actual_max" =~ ^[0-9]+$ ]]; then
    [[ "$actual_max" == "$((MEMORY_MAX_MIB * 1024 * 1024))" ]] ||
      die 'systemd applied an unexpected MemoryMax'
  elif [[ "$actual_limit" =~ ^[0-9]+$ ]]; then
    [[ "$actual_limit" == "$((MEMORY_MAX_MIB * 1024 * 1024))" ]] ||
      die 'systemd applied an unexpected MemoryLimit'
  else
    warn 'this systemd version does not expose MemoryMax; GOMEMLIMIT remains active'
  fi
  if [[ "$actual_swap" =~ ^[0-9]+$ ]]; then
    [[ "$actual_swap" == "$((MEMORY_SWAP_MAX_MIB * 1024 * 1024))" ]] ||
      die 'systemd applied an unexpected MemorySwapMax'
  else
    warn 'this systemd version does not expose MemorySwapMax'
  fi
}

enable_service() {
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl daemon-reload
    if (( START_AFTER_INSTALL == 1 )); then
      systemctl enable v2node.service
    else
      systemctl disable v2node.service >/dev/null 2>&1 || true
    fi
  else
    if (( START_AFTER_INSTALL == 1 )); then
      rc-update add v2node default
    else
      rc-update del v2node default >/dev/null 2>&1 || true
    fi
    warn 'OpenRC applies GOMEMLIMIT, but systemd MemoryHigh/MemoryMax controls are unavailable'
  fi
}

health_check() {
  local previous_pid='' previous_restarts='' pid restarts stable=0 attempt
  if [[ "$SERVICE_MANAGER" == openrc ]]; then
    service v2node start
    for attempt in 1 2 3 4; do
      sleep 3
      service v2node status >/dev/null 2>&1 || return 1
    done
    return 0
  fi

  systemctl start v2node.service
  for attempt in 1 2 3 4 5 6; do
    sleep 3
    systemctl is-active --quiet v2node.service || return 1
    pid="$(systemd_property MainPID)"
    restarts="$(systemd_property NRestarts)"
    [[ -n "$restarts" ]] || restarts=0
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    if [[ "$pid" == "$previous_pid" && "$restarts" == "$previous_restarts" ]]; then
      stable=$((stable + 1))
    else
      stable=0
    fi
    previous_pid="$pid"
    previous_restarts="$restarts"
  done
  (( stable >= 3 ))
}

restore_snapshot() {
  local index path label failed=0
  (( ROLLBACK_RUNNING == 0 )) || return 1
  ROLLBACK_RUNNING=1
  warn 'installation failed; restoring the previous v2node state'
  set +e
  set +u

  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl stop v2node.service >/dev/null 2>&1
  else
    service v2node stop >/dev/null 2>&1
  fi

  for index in "${!SNAPSHOT_PATHS[@]}"; do
    path="${SNAPSHOT_PATHS[$index]}"
    label="${SNAPSHOT_LABELS[$index]}"
    if [[ -f "$BACKUP_DIR/$label.present" ]]; then
      cp -p -- "$BACKUP_DIR/$label" "$path" || failed=1
    else
      rm -f -- "$path" || failed=1
    fi
  done
  for path in "${TEMP_LIVE_FILES[@]}"; do
    rm -f -- "$path"
  done
  for ((index=${#CREATED_DIRS[@]}-1; index>=0; index--)); do
    rmdir -- "${CREATED_DIRS[$index]}" >/dev/null 2>&1 || true
  done

  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    systemctl daemon-reload >/dev/null 2>&1 || failed=1
    if (( SERVICE_WAS_ENABLED == 1 )); then
      systemctl enable v2node.service >/dev/null 2>&1 || failed=1
    else
      systemctl disable v2node.service >/dev/null 2>&1 || true
    fi
    if (( SERVICE_WAS_ACTIVE == 1 )); then
      systemctl start v2node.service >/dev/null 2>&1 || failed=1
    fi
  else
    if (( SERVICE_WAS_ENABLED == 1 )); then
      rc-update add v2node default >/dev/null 2>&1 || failed=1
    else
      rc-update del v2node default >/dev/null 2>&1 || true
    fi
    if (( SERVICE_WAS_ACTIVE == 1 )); then
      service v2node start >/dev/null 2>&1 || failed=1
    fi
  fi
  TRANSACTION_ACTIVE=0
  set -u
  set -e
  (( failed == 0 )) || warn 'rollback completed with errors; inspect the service and files manually'
}

cleanup_on_exit() {
  local status=$?
  trap - EXIT INT TERM HUP
  if (( TRANSACTION_ACTIVE == 1 )); then
    restore_snapshot || true
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
  release_lock
  exit "$status"
}

install_standalone() {
  snapshot_state
  TRANSACTION_ACTIVE=1
  stop_service_for_install
  create_live_directories
  install_candidate_files
  enable_service
  verify_installed_files
  verify_service_profile

  if (( START_AFTER_INSTALL == 1 )); then
    health_check || die 'v2node did not remain healthy after installation'
  else
    if [[ "$SERVICE_MANAGER" == systemd ]]; then
      systemctl stop v2node.service >/dev/null 2>&1 || true
    else
      service v2node stop >/dev/null 2>&1 || true
    fi
  fi
  TRANSACTION_ACTIVE=0
}

main() {
  parse_args "$@"
  (( EUID == 0 )) || die 'run this installer as root'
  [[ "$(uname -s)" == Linux ]] || die 'this installer supports Linux only'
  detect_platform
  acquire_lock
  install_dependencies
  for command_name in curl unzip sha256sum awk grep od stat install tr mv cp chmod chown; do
    require_cmd "$command_name"
  done
  if [[ "$SERVICE_MANAGER" == systemd ]]; then
    require_cmd systemctl
    [[ -d /run/systemd/system ]] || die 'systemd is not running on this host'
  else
    require_cmd service
    require_cmd rc-update
  fi
  validate_live_state

  TMP_DIR="$(mktemp -d /tmp/v2node-install.XXXXXX)"
  chmod 0700 "$TMP_DIR"
  download_and_verify_assets
  resource_profile
  prepare_config_candidate
  write_service_candidates
  install_standalone

  log "$DEFAULT_VERSION installed successfully"
  log 'management command: v2node'
  if (( START_AFTER_INSTALL == 1 )); then
    log 'service is enabled and running'
  else
    log 'service is disabled and stopped until panel configuration is created'
    log 'run: v2node generate'
  fi
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then
  trap cleanup_on_exit EXIT
  trap 'exit 130' INT TERM HUP
  main "$@"
fi
