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

cmp -s "$ROOT/deploy/install.sh" "$ROOT/script/install.sh" ||
  fail 'published installer copies differ'
bash -n "$ROOT/deploy/install.sh" || fail 'installer syntax check failed'
[[ "$BINARY_FILE" == '/usr/local/v2node/v2node' ]] ||
  fail 'binary path variables were not expanded'
[[ "$CONFIG_FILE" == '/etc/v2node/config.json' ]] ||
  fail 'config path variables were not expanded'
[[ "$SYSTEMD_DROPIN" == '/etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf' ]] ||
  fail 'systemd drop-in path variables were not expanded'

# Argument parsing stays compatible with the original panel flags while pinning
# every installation to the tested ram5 binary.
(
  VERSION_ARG=''
  API_HOST_ARG=''
  NODE_ID_ARG=''
  API_KEY_ARG=''
  parse_args v0.4.4-ram5 --api-host https://panel.example/ --node-id 7 --api-key secret
  [[ "$API_HOST_ARG:$NODE_ID_ARG:$API_KEY_ARG" == 'https://panel.example/:7:secret' ]]
) || fail 'complete panel arguments were not accepted'
if (
  VERSION_ARG=''
  API_HOST_ARG=''
  NODE_ID_ARG=''
  API_KEY_ARG=''
  parse_args --node-id 7
) >/dev/null 2>&1; then
  fail 'partial panel arguments were accepted'
fi
if (
  VERSION_ARG=''
  API_HOST_ARG=''
  NODE_ID_ARG=''
  API_KEY_ARG=''
  parse_args v9.9.9
) >/dev/null 2>&1; then
  fail 'an untested binary version was accepted'
fi

# Cgroup fixtures must select the tightest real limit.
MEMINFO_FILE="$tmp/meminfo"
CGROUP_V2_MEMORY_MAX_FILE="$tmp/memory.max"
CGROUP_V1_MEMORY_LIMIT_FILE="$tmp/memory.limit_in_bytes"
printf 'MemTotal:       4194304 kB\n' > "$MEMINFO_FILE"
printf 'max\n' > "$CGROUP_V2_MEMORY_MAX_FILE"
[[ "$(effective_memory_mib)" == 4096 ]] ||
  fail 'unlimited cgroup-v2 did not use host RAM'
printf '2147483648\n' > "$CGROUP_V2_MEMORY_MAX_FILE"
[[ "$(effective_memory_mib)" == 2048 ]] ||
  fail 'cgroup-v2 did not override host RAM'
printf '268435455\n' > "$CGROUP_V2_MEMORY_MAX_FILE"
[[ "$(effective_memory_mib)" == 255 ]] ||
  fail 'sub-256 MiB limit was rounded up'
if (resource_profile >/dev/null 2>&1); then
  fail 'less than 256 MiB was accepted'
fi
rm -f "$CGROUP_V2_MEMORY_MAX_FILE"
printf '9223372036854771712\n' > "$CGROUP_V1_MEMORY_LIMIT_FILE"
[[ "$(effective_memory_mib)" == 4096 ]] ||
  fail 'unlimited cgroup-v1 sentinel did not use host RAM'
printf '1073741824\n' > "$CGROUP_V1_MEMORY_LIMIT_FILE"
[[ "$(effective_memory_mib)" == 1024 ]] ||
  fail 'cgroup-v1 did not override host RAM'

assert_profile() {
  local test_mem="$1" expected="$2"
  effective_memory_mib() { printf '%s\n' "$test_mem"; }
  resource_profile >/dev/null
  [[ "$HOST_RESERVE_MIB:$GOMEMLIMIT_MIB:$MEMORY_HIGH_MIB:$MEMORY_MAX_MIB:$MEMORY_SWAP_MAX_MIB" == "$expected" ]] ||
    fail "unexpected ram5 profile at $test_mem MiB"
}

assert_profile 256 '64:128:144:192:128'
assert_profile 512 '128:256:288:384:128'
assert_profile 1024 '256:512:640:768:128'
assert_profile 2048 '384:1408:1536:1664:204'
assert_profile 4096 '614:3134:3308:3482:409'
assert_profile 8192 '1228:6268:6616:6964:512'

# The release ZIP contract, inner binary hash and ELF architecture are checked
# independently of the outer archive hash.
STAGE_DIR="$tmp/archive-stage"
mkdir -p "$STAGE_DIR/extracted"
cp "$ROOT/artifacts/v2node-linux-64.zip" "$STAGE_DIR/package.zip"
ARCH_ASSET='64'
BINARY_SHA256="$BINARY_SHA256_64"
validate_release_archive >/dev/null ||
  fail 'valid amd64 release archive was rejected'
if (
  rm -rf "$STAGE_DIR/extracted"
  mkdir -p "$STAGE_DIR/extracted"
  ARCH_ASSET='arm64-v8a'
  BINARY_SHA256="$BINARY_SHA256_64"
  validate_release_archive
) >/dev/null 2>&1; then
  fail 'amd64 ELF was accepted as arm64'
fi

# New configs validate inputs, preserve the ram5 runtime defaults and escape JSON.
API_HOST_ARG='https://panel.example/'
NODE_ID_ARG='9'
API_KEY_ARG='a"b\c'
write_generated_config "$tmp/generated.json"
grep -Fq '"BufferSizeKB": 64' "$tmp/generated.json" ||
  fail 'generated config lost the ram5 buffer size'
grep -Fq '"ApiKey": "a\"b\\c"' "$tmp/generated.json" ||
  fail 'generated config did not JSON-escape the API key'
API_HOST_ARG='https://panel.example/'
NODE_ID_ARG='not-a-number'
API_KEY_ARG='secret'
if (write_generated_config "$tmp/invalid.json") >/dev/null 2>&1; then
  fail 'invalid node ID was accepted'
fi

# Candidate units retain the original service layout and add RAM controls only
# through a drop-in. OpenRC keeps GOMEMLIMIT without claiming cgroup controls.
effective_memory_mib() { printf '2048\n'; }
resource_profile >/dev/null
STAGE_DIR="$tmp/service-stage"
mkdir -p "$STAGE_DIR"
SERVICE_MANAGER='systemd'
write_service_candidates
grep -Fq 'ExecStart=/usr/local/v2node/v2node server' "$STAGE_DIR/v2node.service" ||
  fail 'systemd unit changed the original executable'
if grep -Eq 'Memory(High|Max|Limit|SwapMax)|GOMEMLIMIT' "$STAGE_DIR/v2node.service"; then
  fail 'RAM policy leaked into the original main unit'
fi
grep -Fq "Environment=\"GOMEMLIMIT=$GOMEMLIMIT\"" "$STAGE_DIR/90-v2node-ramfix.conf" ||
  fail 'systemd drop-in lacks GOMEMLIMIT'
grep -Fq "MemoryHigh=$MEMORY_HIGH" "$STAGE_DIR/90-v2node-ramfix.conf" ||
  fail 'systemd drop-in lacks MemoryHigh'
grep -Fq "MemoryMax=$MEMORY_MAX" "$STAGE_DIR/90-v2node-ramfix.conf" ||
  fail 'systemd drop-in lacks MemoryMax'
grep -Fq "MemorySwapMax=$MEMORY_SWAP_MAX" "$STAGE_DIR/90-v2node-ramfix.conf" ||
  fail 'systemd drop-in lacks MemorySwapMax'
SERVICE_MANAGER='openrc'
write_service_candidates
grep -Fq "export GOMEMLIMIT=\"$GOMEMLIMIT\"" "$STAGE_DIR/v2node.openrc" ||
  fail 'OpenRC unit lacks GOMEMLIMIT'

# Exact systemd properties are required when the host exposes them.
SERVICE_MANAGER='systemd'
MOCK_ENVIRONMENT="FOO=1 GOMEMLIMIT=$GOMEMLIMIT BAR=2"
systemctl() {
  local prop='' arg next=0
  for arg in "$@"; do
    if (( next == 1 )); then prop="$arg"; break; fi
    [[ "$arg" == -p ]] && next=1
  done
  case "$prop" in
    Environment) printf 'Environment=%s\n' "$MOCK_ENVIRONMENT" ;;
    MemoryHigh) printf 'MemoryHigh=%s\n' "$((MEMORY_HIGH_MIB * 1024 * 1024))" ;;
    MemoryMax) printf 'MemoryMax=%s\n' "$((MEMORY_MAX_MIB * 1024 * 1024))" ;;
    MemoryLimit) printf 'MemoryLimit=%s\n' "$((MEMORY_MAX_MIB * 1024 * 1024))" ;;
    MemorySwapMax) printf 'MemorySwapMax=%s\n' "$((MEMORY_SWAP_MAX_MIB * 1024 * 1024))" ;;
    *) return 1 ;;
  esac
}
verify_service_profile >/dev/null ||
  fail 'valid systemd RAM profile was rejected'
MOCK_ENVIRONMENT="NOT_GOMEMLIMIT=$GOMEMLIMIT"
if (verify_service_profile) >/dev/null 2>&1; then
  fail 'substring environment key was accepted as GOMEMLIMIT'
fi
unset -f systemctl

# Snapshot restoration covers both replaced and newly-created files.
SERVICE_MANAGER='openrc'
SNAPSHOT_PATHS=("$tmp/live-a" "$tmp/live-b")
SNAPSHOT_LABELS=(a b)
printf 'before' > "$tmp/live-a"
TMP_DIR="$tmp/transaction"
mkdir -p "$TMP_DIR"
service() { return 1; }
rc-update() { return 1; }
snapshot_state
printf 'after' > "$tmp/live-a"
printf 'new' > "$tmp/live-b"
CREATED_DIRS=()
TEMP_LIVE_FILES=()
TRANSACTION_ACTIVE=1
ROLLBACK_RUNNING=0
restore_snapshot >/dev/null 2>&1
[[ "$(<"$tmp/live-a")" == before ]] ||
  fail 'rollback did not restore a replaced file'
[[ ! -e "$tmp/live-b" ]] ||
  fail 'rollback did not remove a newly-created file'
unset -f service rc-update

# Exercise the real atomic install path against a disposable blank filesystem.
# A transformed copy changes only absolute roots; the production functions and
# candidate assets remain unchanged.
export FIXTURE_ROOT="$tmp/blank-root"
export INSTALLER_SOURCE="$ROOT/deploy/install.sh"
export FIXTURE_ARCHIVE="$ROOT/artifacts/v2node-linux-64.zip"
export FIXTURE_GEOIP="$ROOT/assets/geoip.dat"
export FIXTURE_GEOSITE="$ROOT/assets/geosite.dat"
export FIXTURE_CONFIG="$ROOT/config.example.json"
export FIXTURE_MENU="$ROOT/script/v2node.sh"
bash -c '
  set -Eeuo pipefail
  mkdir -p \
    "$FIXTURE_ROOT/usr/local" \
    "$FIXTURE_ROOT/usr/bin" \
    "$FIXTURE_ROOT/etc/systemd/system" \
    "$FIXTURE_ROOT/etc"
  source <(
    sed \
      -e "s|^readonly INSTALL_ROOT=.*|readonly INSTALL_ROOT=\"$FIXTURE_ROOT/usr/local/v2node\"|" \
      -e "s|^readonly CONFIG_DIR=.*|readonly CONFIG_DIR=\"$FIXTURE_ROOT/etc/v2node\"|" \
      -e "s|^readonly MENU_FILE=.*|readonly MENU_FILE=\"$FIXTURE_ROOT/usr/bin/v2node\"|" \
      -e "s|^readonly SYSTEMD_UNIT=.*|readonly SYSTEMD_UNIT=\"$FIXTURE_ROOT/etc/systemd/system/v2node.service\"|" \
      -e "s|^readonly SYSTEMD_DROPIN_DIR=.*|readonly SYSTEMD_DROPIN_DIR=\"$FIXTURE_ROOT/etc/systemd/system/v2node.service.d\"|" \
      -e "s|^readonly OPENRC_UNIT=.*|readonly OPENRC_UNIT=\"$FIXTURE_ROOT/etc/init.d/v2node\"|" \
      "$INSTALLER_SOURCE"
  )
  install() {
    local mode=
    while (($#)); do
      case "$1" in
        -o|-g) shift 2 ;;
        -m) mode="$2"; shift 2 ;;
        --) shift; break ;;
        *) break ;;
      esac
    done
    command install -m "$mode" "$@"
  }
  STAGE_DIR="$FIXTURE_ROOT/stage"
  mkdir -p "$STAGE_DIR/extracted"
  unzip -q "$FIXTURE_ARCHIVE" -d "$STAGE_DIR/extracted"
  cp "$FIXTURE_GEOIP" "$STAGE_DIR/geoip.dat"
  cp "$FIXTURE_GEOSITE" "$STAGE_DIR/geosite.dat"
  cp "$FIXTURE_CONFIG" "$STAGE_DIR/config.example.json"
  cp "$FIXTURE_MENU" "$STAGE_DIR/v2node-menu.sh"
  GOMEMLIMIT=1408MiB
  MEMORY_HIGH=1536M
  MEMORY_MAX=1664M
  MEMORY_SWAP_MAX=204M
  SERVICE_MANAGER=systemd
  write_service_candidates
  mkdir() {
    if [[ "$1" == -m ]]; then shift 2; fi
    command mkdir "$@"
  }
  CONFIG_SOURCE="$STAGE_DIR/config.example.json"
  NEW_CONFIG=1
  register_managed_paths
  create_live_directories
  install_candidate_files
  [[ -f "$FIXTURE_ROOT/usr/local/v2node/v2node" ]]
  [[ -f "$FIXTURE_ROOT/usr/bin/v2node" ]]
  [[ -f "$FIXTURE_ROOT/etc/v2node/config.json" ]]
  [[ -f "$FIXTURE_ROOT/etc/systemd/system/v2node.service" ]]
  [[ -f "$FIXTURE_ROOT/etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf" ]]
  cmp -s "$FIXTURE_ROOT/etc/v2node/config.json" "$FIXTURE_CONFIG"
' || fail 'disposable blank-root installation fixture failed'

# Static safety and ordering invariants.
grep -Fq "RAW_BRANCH_BASE='https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4'" "$ROOT/deploy/install.sh" ||
  fail 'installer is not pinned to the standalone branch'
grep -Fq "RAW_RELEASE_BASE='https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram5'" "$ROOT/deploy/install.sh" ||
  fail 'immutable support asset tag is missing'
grep -Fq 'assert_safe_file_or_missing "$path"' "$ROOT/deploy/install.sh" ||
  fail 'managed file safety gate is missing'
grep -Fq 'TRANSACTION_ACTIVE=1' "$ROOT/deploy/install.sh" ||
  fail 'transaction rollback gate is missing'
grep -Fq "readonly LOCK_DIR='/run/v2node-standalone-install.lock'" "$ROOT/deploy/install.sh" ||
  fail 'installer lock is not protected by the root-owned /run directory'
grep -Fq "if [[ \"\$PLATFORM\" == alpine ]]; then" "$ROOT/deploy/install.sh" ||
  fail 'Alpine full-tool bootstrap is missing'
grep -Fq "apk add --no-cache bash ca-certificates curl unzip coreutils" "$ROOT/deploy/install.sh" ||
  fail 'Alpine does not force the Info-ZIP/coreutils packages'
restore_block="$(awk '/^restore_snapshot\(\)/{inside=1} /^cleanup_on_exit\(\)/{inside=0} inside{print}' "$ROOT/deploy/install.sh")"
grep -Fq 'set +u' <<<"$restore_block" ||
  fail 'rollback is unsafe for empty arrays on Bash before 4.4'
if grep -Fq -- '--value' "$ROOT/deploy/install.sh"; then
  fail 'installer uses systemctl --value, which breaks supported old systemd releases'
fi
grep -Fq 'if (( START_AFTER_INSTALL == 1 )); then' "$ROOT/deploy/install.sh" ||
  fail 'service enablement is not conditional on a usable panel config'
if grep -Eq 'validate_original_install|install upstream v2node first|rm -rf /usr/local/v2node|wyx2685/v2node/(master|main)/script' "$ROOT/deploy/install.sh"; then
  fail 'overlay-only or destructive upstream behavior remains'
fi
if grep -Eq 'v2nodectl|s390x|arch="64".*default' "$ROOT/deploy/install.sh"; then
  fail 'renamed CLI or unsafe architecture fallback remains'
fi

awk '
  /^main\(\)/ { inside=1 }
  inside && /validate_live_state/ { live=NR }
  inside && /download_and_verify_assets/ { download=NR }
  inside && /resource_profile/ { profile=NR }
  inside && /prepare_config_candidate/ { config=NR }
  inside && /install_standalone/ { install=NR }
  inside && /^}/ {
    exit ! (live < download && download < profile && profile < config && config < install)
  }
' "$ROOT/deploy/install.sh" ||
  fail 'main does not verify all assets before live installation'

awk '
  /^install_standalone\(\)/ { inside=1 }
  inside && /snapshot_state/ { snapshot=NR }
  inside && /TRANSACTION_ACTIVE=1/ { armed=NR }
  inside && /stop_service_for_install/ { stop=NR }
  inside && /install_candidate_files/ { files=NR }
  inside && /verify_installed_files/ { verify=NR }
  inside && /health_check/ { health=NR }
  inside && /TRANSACTION_ACTIVE=0/ { commit=NR }
  inside && /^}/ {
    exit ! (snapshot < armed && armed < stop && stop < files && files < verify &&
      verify < health && health < commit)
  }
' "$ROOT/deploy/install.sh" ||
  fail 'standalone transaction ordering invariant failed'

printf 'PASS: standalone installer, ram5 profile and rollback invariants\n'
