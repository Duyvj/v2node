#!/bin/bash

set -o pipefail
umask 077

# The manager downloads and launches a privileged installer. Do not let an
# inherited search path, Bash startup file or loader configuration replace the
# system helpers or inject code into those child processes.
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
unset BASH_ENV ENV CDPATH GLOBIGNORE LD_PRELOAD LD_LIBRARY_PATH OPENSSL_CONF

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)
release_repo="Duyvj/v2node"
release_branch="main"
[[ -s /etc/v2node/release-repo ]] && release_repo=$(tr -d '\r\n' < /etc/v2node/release-repo)
[[ -s /etc/v2node/release-branch ]] && release_branch=$(tr -d '\r\n' < /etc/v2node/release-branch)
V2NODE_OPERATION_LOCK_FILE="/run/v2node-operation.lock"
V2NODE_OPERATION_LOCK_DIRECTORY="${V2NODE_OPERATION_LOCK_FILE}.d"
V2NODE_OPERATION_LOCK_BACKEND=""

acquire_v2node_operation_lock() {
    local owner="" stale_directory lock_parent
    if [[ -n "$V2NODE_OPERATION_LOCK_BACKEND" ]]; then
        return 0
    fi

    lock_parent="${V2NODE_OPERATION_LOCK_DIRECTORY%/*}"
    mkdir -p "$lock_parent" || return 1
    if ! mkdir "$V2NODE_OPERATION_LOCK_DIRECTORY" 2>/dev/null; then
        if [[ -r "$V2NODE_OPERATION_LOCK_DIRECTORY/pid" ]]; then
            owner=$(sed -n '1p' "$V2NODE_OPERATION_LOCK_DIRECTORY/pid")
        fi
        if [[ "$owner" =~ ^[0-9]+$ ]] && kill -0 "$owner" 2>/dev/null; then
            echo -e "${red}Một thao tác cài đặt/rollback V2Node khác đang chạy; hãy thử lại sau.${plain}"
            return 1
        fi
        if [[ ! "$owner" =~ ^[0-9]+$ ]]; then
            echo -e "${red}Khóa thao tác V2Node không có chủ sở hữu hợp lệ; hãy kiểm tra $V2NODE_OPERATION_LOCK_DIRECTORY thủ công.${plain}"
            return 1
        fi
        stale_directory="${V2NODE_OPERATION_LOCK_DIRECTORY}.stale.$$"
        mv "$V2NODE_OPERATION_LOCK_DIRECTORY" "$stale_directory" 2>/dev/null || return 1
        if ! mkdir "$V2NODE_OPERATION_LOCK_DIRECTORY" 2>/dev/null; then
            rm -rf "$stale_directory"
            return 1
        fi
        rm -rf "$stale_directory"
    fi
    printf '%s\n' "$$" > "$V2NODE_OPERATION_LOCK_DIRECTORY/pid" || {
        rm -rf "$V2NODE_OPERATION_LOCK_DIRECTORY"
        return 1
    }
    V2NODE_OPERATION_LOCK_BACKEND="mkdir"
}

release_v2node_operation_lock() {
    local owner=""
    if [[ "$V2NODE_OPERATION_LOCK_BACKEND" == "mkdir" ]]; then
        [[ -r "$V2NODE_OPERATION_LOCK_DIRECTORY/pid" ]] \
            && owner=$(sed -n '1p' "$V2NODE_OPERATION_LOCK_DIRECTORY/pid")
        if [[ "$owner" == "$$" ]]; then
            rm -rf "$V2NODE_OPERATION_LOCK_DIRECTORY"
        fi
    fi
    V2NODE_OPERATION_LOCK_BACKEND=""
}

if [[ ! "$release_repo" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] \
    || [[ ! "$release_branch" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] \
    || [[ "$release_branch" == *..* ]] || [[ "$release_branch" == *//* ]]; then
    echo "Invalid V2Node release source in /etc/v2node/release-{repo,branch}." >&2
    exit 1
fi
# check root
[[ $EUID -ne 0 ]] && echo -e "${red}Lỗi:${plain} Phải chạy script này bằng tài khoản root!\n" && exit 1

# check os
if [[ -f /etc/redhat-release ]]; then
    release="centos"
elif cat /etc/issue | grep -Eqi "alpine"; then
    release="alpine"
elif cat /etc/issue | grep -Eqi "debian"; then
    release="debian"
elif cat /etc/issue | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /etc/issue | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "debian"; then
    release="debian"
elif cat /proc/version | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /proc/version | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "arch"; then
    release="arch"
else
    echo -e "${red}Không xác định được phiên bản hệ điều hành, vui lòng liên hệ tác giả script!${plain}\n" && exit 1
fi

arch=$(uname -m)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "amd64" ]]; then
    arch="64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64-v8a"
elif [[ $arch == "s390x" ]]; then
    arch="s390x"
else
    arch="64"
    echo -e "${red}Không xác định được kiến trúc, sử dụng kiến trúc mặc định: ${arch}${plain}"
fi

if [ "$(getconf WORD_BIT)" != '32' ] && [ "$(getconf LONG_BIT)" != '64' ] ; then
    echo "Phần mềm không hỗ trợ hệ điều hành 32-bit (x86). Vui lòng sử dụng hệ điều hành 64-bit (x86_64); nếu phát hiện sai, hãy liên hệ tác giả."
    exit 2
fi

# os version
if [[ -f /etc/os-release ]]; then
    os_version=$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)
fi
if [[ -z "$os_version" && -f /etc/lsb-release ]]; then
    os_version=$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)
fi

if [[ x"${release}" == x"centos" ]]; then
    if [[ ${os_version} -le 6 ]]; then
        echo -e "${red}Vui lòng sử dụng CentOS 7 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
    if [[ ${os_version} -eq 7 ]]; then
        echo -e "${red}Lưu ý: CentOS 7 không hỗ trợ giao thức Hysteria 1/2!${plain}\n"
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}Vui lòng sử dụng Ubuntu 16 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}Vui lòng sử dụng Debian 8 hoặc phiên bản mới hơn!${plain}\n" && exit 1
    fi
fi

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -rp "$1 [mặc định: $2]: " temp
        if [[ x"${temp}" == x"" ]]; then
            temp=$2
        fi
    else
        read -rp "$1 [y/n]: " temp
    fi
    if [[ x"${temp}" == x"y" || x"${temp}" == x"Y" ]]; then
        return 0
    else
        return 1
    fi
}

confirm_restart() {
    confirm "Bạn có muốn khởi động lại v2node không?" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}Nhấn Enter để quay lại menu chính: ${plain}" && read temp
    show_menu
}

validate_release_version() {
    [[ "$1" =~ ^v?[0-9]+\.[0-9]+(\.[0-9]+)?([-+_][A-Za-z0-9.-]+)?$ ]]
}

sha256_file() {
    openssl dgst -sha256 "$1" 2>/dev/null | awk '{print tolower($NF)}'
}

latest_release_version() {
    local repository="$1"
    local metadata version
    metadata=$(curl --fail --location --silent --show-error --retry 3 --connect-timeout 15 \
        --proto '=https' --tlsv1.2 \
        "https://api.github.com/repos/${repository}/releases/latest") || return 1
    version=$(printf '%s\n' "$metadata" | awk -F'"' '/"tag_name":/ {print $4; exit}')
    validate_release_version "$version" || return 1
    printf '%s\n' "$version"
}

download_verified_script_asset() {
    local repository="$1"
    local version="$2"
    local asset_name="$3"
    local destination="$4"
    local metadata expected actual asset_url asset_size

    validate_release_version "$version" || return 1
    case "$asset_name" in
        install.sh|v2node.sh) ;;
        *) return 1 ;;
    esac

    metadata=$(curl --fail --location --silent --show-error --retry 3 --connect-timeout 15 \
        --proto '=https' --tlsv1.2 \
        "https://api.github.com/repos/${repository}/releases/tags/${version}") || return 1
    expected=$(printf '%s\n' "$metadata" | awk -v asset="$asset_name" '
        /"name":/ { selected = index($0, "\"" asset "\"") > 0 }
        selected && /"digest": "sha256:/ {
            value=$0
            sub(/^.*"digest": "sha256:/, "", value)
            sub(/".*$/, "", value)
            print tolower(value)
            exit
        }
    ')
    if [[ ! "$expected" =~ ^[a-f0-9]{64}$ ]]; then
        echo -e "${red}Release ${version} không công bố SHA-256 cho ${asset_name}; từ chối chạy script đặc quyền.${plain}"
        return 1
    fi

    asset_url="https://github.com/${repository}/releases/download/${version}/${asset_name}"
    if ! curl --fail --location --silent --show-error --retry 3 --connect-timeout 15 \
        --proto '=https' --tlsv1.2 "$asset_url" -o "$destination"; then
        rm -f "$destination"
        return 1
    fi
    asset_size=$(wc -c < "$destination" | tr -d '[:space:]')
    actual=$(sha256_file "$destination")
    if [[ ! "$asset_size" =~ ^[0-9]+$ ]] || (( asset_size < 1024 || asset_size > 1048576 )) \
        || [[ ! "$actual" =~ ^[a-f0-9]{64}$ ]] || [[ "$actual" != "$expected" ]] \
        || ! bash -n "$destination"; then
        rm -f "$destination"
        echo -e "${red}Script ${asset_name} không vượt qua xác minh SHA-256/cú pháp; từ chối thực thi.${plain}"
        return 1
    fi
}

run_installer() {
    local temporary status trusted_installer_ref target_version=""
    if [[ $# -gt 0 && "$1" != --* ]]; then
        target_version="$1"
        shift
        if [[ ! "$target_version" =~ ^v?[0-9]+\.[0-9]+(\.[0-9]+)?([-+_][A-Za-z0-9.-]+)?$ ]]; then
            echo -e "${red}Phiên bản V2Node đích không hợp lệ.${plain}"
            return 1
        fi
    fi

    # The requested binary version is data, never the source of installer
    # code. Always run the loader from the latest trusted release so selecting
    # an older runtime cannot revive an installer without checksum/rollback
    # enforcement.
    trusted_installer_ref=$(latest_release_version "$release_repo")
    if [[ -z "$trusted_installer_ref" ]]; then
        echo -e "${red}Không xác định được release tag hợp lệ cho installer.${plain}"
        return 1
    fi
    temporary=$(mktemp) || return 1
    if ! download_verified_script_asset "$release_repo" "$trusted_installer_ref" install.sh "$temporary"; then
        rm -f "$temporary"
        echo -e "${red}Không tải được installer đã xác minh từ release bảo mật mới nhất.${plain}"
        return 1
    fi
    if [[ -n "$target_version" ]]; then
        bash "$temporary" "$target_version" "$@"
    else
        bash "$temporary" "$trusted_installer_ref" "$@"
    fi
    status=$?
    rm -f "$temporary"
    return "$status"
}

install() {
    local install_status
    run_installer --release-repo "$release_repo" --release-branch "$release_branch"
    install_status=$?
    if [[ $install_status == 0 ]]; then
        if [[ $# == 0 ]]; then
            start
        else
            start 0
        fi
    fi
    return "$install_status"
}

update() {
    local update_status
    if [[ $# == 0 ]]; then
        echo && echo -n -e "Nhập phiên bản cần cài (mặc định là mới nhất): " && read version
    else
        version=$2
    fi
    if [[ -n "$version" ]]; then
        run_installer "$version" --release-repo "$release_repo" --release-branch "$release_branch"
    else
        run_installer --release-repo "$release_repo" --release-branch "$release_branch"
    fi
    update_status=$?
    if [[ $update_status == 0 ]]; then
        echo -e "${green}Cập nhật hoàn tất và v2node đã tự khởi động lại. Dùng v2node log để xem nhật ký.${plain}"
        exit 0
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
    return "$update_status"
}

validate_runtime_geodata() {
    local runtime_directory="$1"
    local file
    for file in geoip.dat geosite.dat; do
        if [[ ! -s "$runtime_directory/$file" ]] \
            || [[ $(wc -c < "$runtime_directory/$file") -lt 1024 ]]; then
            echo -e "${red}Runtime dự phòng không chứa $file hợp lệ; từ chối rollback.${plain}"
            return 1
        fi
    done
}

install_runtime_geodata() (
    local runtime_directory="$1"
    local destination="${2:-/etc/v2node}"
    local transaction_directory file restore_file

    validate_runtime_geodata "$runtime_directory" || return 1
    mkdir -p "$destination" || return 1
    transaction_directory=$(mktemp -d "$destination/.geodata.XXXXXX") || return 1
    trap 'rm -rf "$transaction_directory"' EXIT

    for file in geoip.dat geosite.dat; do
        install -m 0644 "$runtime_directory/$file" "$transaction_directory/new-$file" || return 1
        if [[ -e "$destination/$file" ]]; then
            cp -p "$destination/$file" "$transaction_directory/old-$file" || return 1
        else
            : > "$transaction_directory/no-old-$file"
        fi
    done

    for file in geoip.dat geosite.dat; do
        if ! mv -f "$transaction_directory/new-$file" "$destination/$file"; then
            for restore_file in geoip.dat geosite.dat; do
                if [[ -e "$transaction_directory/old-$restore_file" ]]; then
                    mv -f "$transaction_directory/old-$restore_file" "$destination/$restore_file" || true
                elif [[ -e "$transaction_directory/no-old-$restore_file" ]]; then
                    rm -f "$destination/$restore_file"
                fi
            done
            return 1
        fi
    done
)

swap_runtime_trees() {
    local current_directory="${1:-/usr/local/v2node}"
    local previous_directory="${2:-/usr/local/v2node.previous}"
    local transaction_directory held_runtime transaction_parent

    [[ -d "$current_directory" && -d "$previous_directory" ]] || return 1
    transaction_parent="${current_directory%/*}"
    transaction_directory=$(mktemp -d "$transaction_parent/.v2node-swap.XXXXXX") || return 1
    held_runtime="$transaction_directory/runtime"

    if ! mv "$current_directory" "$held_runtime"; then
        rmdir "$transaction_directory" 2>/dev/null || true
        return 1
    fi
    if ! mv "$previous_directory" "$current_directory"; then
        mv "$held_runtime" "$current_directory" || true
        rmdir "$transaction_directory" 2>/dev/null || true
        return 1
    fi
    if ! mv "$held_runtime" "$previous_directory"; then
        mv "$current_directory" "$previous_directory" || true
        mv "$held_runtime" "$current_directory" || true
        rmdir "$transaction_directory" 2>/dev/null || true
        return 1
    fi
    rmdir "$transaction_directory" 2>/dev/null || true
}

start_v2node_service() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node start >/dev/null 2>&1 || return 1
    else
        systemctl start v2node >/dev/null 2>&1 || return 1
    fi
    start_terminal_service
}

stop_v2node_service() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node-terminal stop >/dev/null 2>&1 || true
        service v2node stop >/dev/null 2>&1 || true
    else
        systemctl stop v2node-terminal >/dev/null 2>&1 || true
        systemctl stop v2node >/dev/null 2>&1 || true
    fi
}

terminal_execution_disabled() {
    local value="${DISABLE_EXECUTE:-}"
    if [[ "$value" != "0" && "$value" != "1" && -f /etc/v2node/terminal.env && ! -L /etc/v2node/terminal.env ]]; then
        value=$(sed -n 's/^DISABLE_EXECUTE=\([01]\)$/\1/p' /etc/v2node/terminal.env | head -n 1)
    fi
    [[ "$value" == "1" ]]
}

runtime_supports_terminal() {
    [[ -x /usr/local/v2node/v2node ]] && /usr/local/v2node/v2node terminal --help >/dev/null 2>&1
}

remove_terminal_service() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node-terminal stop >/dev/null 2>&1 || true
        rc-update del v2node-terminal default >/dev/null 2>&1 || true
        rm -f /etc/init.d/v2node-terminal
    else
        systemctl disable --now v2node-terminal >/dev/null 2>&1 || true
        rm -f /etc/systemd/system/v2node-terminal.service
        systemctl daemon-reload >/dev/null 2>&1 || true
        systemctl reset-failed v2node-terminal >/dev/null 2>&1 || true
    fi
}

install_terminal_service_unit() {
    if [[ x"${release}" == x"alpine" ]]; then
        cat <<'EOF' > /etc/init.d/v2node-terminal
#!/sbin/openrc-run
name="v2node-terminal"
description="v2node outbound terminal relay"
command="/usr/local/v2node/v2node"
command_args="terminal"
command_user="root"
pidfile="/run/v2node-terminal.pid"
command_background="yes"
start_pre() {
    if [ -f /etc/v2node/terminal.env ]; then
        . /etc/v2node/terminal.env
        export DISABLE_EXECUTE
    fi
}
depend() { need net; }
EOF
        chmod 0755 /etc/init.d/v2node-terminal
        return $?
    fi
    cat <<'EOF' > /etc/systemd/system/v2node-terminal.service
[Unit]
Description=v2node outbound terminal relay
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
WorkingDirectory=/usr/local/v2node/
EnvironmentFile=-/etc/v2node/terminal.env
ExecStart=/usr/local/v2node/v2node terminal
TimeoutStopSec=45s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload >/dev/null 2>&1
}

enable_terminal_service() {
    if terminal_execution_disabled || ! runtime_supports_terminal; then
        return 0
    fi
    install_terminal_service_unit || return 1
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update add v2node-terminal default >/dev/null 2>&1
    else
        systemctl enable v2node-terminal >/dev/null 2>&1
    fi
}

start_terminal_service() {
    if terminal_execution_disabled || [[ ! -f /etc/v2node/config.json ]] || ! grep -q '"AgentID"' /etc/v2node/config.json 2>/dev/null; then
        remove_terminal_service
        return 0
    fi
    if ! runtime_supports_terminal; then
        remove_terminal_service
        return 0
    fi
    enable_terminal_service || return 1
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node-terminal restart >/dev/null 2>&1 || return 1
        sleep 1
        service v2node-terminal status >/dev/null 2>&1
    else
        systemctl restart v2node-terminal >/dev/null 2>&1 || return 1
        sleep 1
        systemctl is-active --quiet v2node-terminal
    fi
}

restore_runtime_after_failed_rollback() {
    if ! swap_runtime_trees; then
        echo -e "${red}Không thể đưa runtime ban đầu trở lại vị trí hoạt động; giữ dịch vụ dừng.${plain}"
        return 1
    fi
    if ! install_runtime_geodata /usr/local/v2node; then
        echo -e "${red}Runtime ban đầu đã trở lại nhưng GeoIP/GeoSite không thể khôi phục; giữ dịch vụ dừng.${plain}"
        return 1
    fi
    start_v2node_service || return 1
    sleep 2
    check_status
}

rollback() (
    acquire_v2node_operation_lock || return 1
    trap release_v2node_operation_lock EXIT
    if [[ ! -x /usr/local/v2node.previous/v2node ]]; then
        echo -e "${red}Chưa có bản trước để quay lại. Hãy cập nhật thành công ít nhất một lần trước.${plain}"
        return 1
    fi
    local previous_version expected_checksum actual_checksum service_status=0
    if [[ ! -r /usr/local/v2node.previous/.v2node.sha256 ]]; then
        echo -e "${red}Bản dự phòng không có checksum tin cậy; từ chối thực thi.${plain}"
        return 1
    fi
    expected_checksum=$(awk 'NR==1 {print tolower($1)}' /usr/local/v2node.previous/.v2node.sha256)
    actual_checksum=$(openssl dgst -sha256 /usr/local/v2node.previous/v2node 2>/dev/null | awk '{print tolower($NF)}')
    if [[ ! "$expected_checksum" =~ ^[a-f0-9]{64}$ ]] \
        || [[ ! "$actual_checksum" =~ ^[a-f0-9]{64}$ ]] \
        || [[ "$expected_checksum" != "$actual_checksum" ]]; then
        echo -e "${red}Checksum bản dự phòng không khớp; từ chối rollback.${plain}"
        return 1
    fi
    previous_version=$(/usr/local/v2node.previous/v2node version 2>/dev/null | awk 'NR==1 {print $2}')
    if [[ ! "$previous_version" =~ ^v?([0-9]+)\.([0-9]+) ]] || (( ${BASH_REMATCH[1]} < 1 )) || (( ${BASH_REMATCH[1]} == 1 && ${BASH_REMATCH[2]} < 2 )); then
        echo -e "${red}Bản dự phòng ${previous_version:-không xác định} chưa hỗ trợ điều khiển Agent tự động; từ chối rollback để tránh mất kết nối quản trị.${plain}"
        return 1
    fi
    if ! validate_runtime_geodata /usr/local/v2node.previous; then
        return 1
    fi
    echo -e "${yellow}Đang quay lại bản V2Node trước...${plain}"
    stop_v2node_service
    if ! swap_runtime_trees; then
        echo -e "${red}Không thể hoán đổi runtime; bản hiện hành được giữ nguyên.${plain}"
        start_v2node_service || true
        return 1
    fi
    if ! install_runtime_geodata /usr/local/v2node; then
        echo -e "${red}Không thể kích hoạt GeoIP/GeoSite của bản rollback; đang khôi phục runtime ban đầu.${plain}"
        restore_runtime_after_failed_rollback || true
        return 1
    fi
    start_v2node_service || service_status=$?
    sleep 2
    if [[ $service_status == 0 ]] && check_status; then
        echo -e "${green}Đã quay lại bản trước. Dùng v2node version và v2node log để kiểm tra.${plain}"
        return 0
    fi

    echo -e "${red}Bản rollback không khởi động; đang khôi phục runtime ban đầu.${plain}"
    stop_v2node_service
    if restore_runtime_after_failed_rollback; then
        echo -e "${red}Rollback thất bại; runtime và GeoIP/GeoSite ban đầu đã được khôi phục.${plain}"
    else
        echo -e "${red}Rollback thất bại và không thể tự khôi phục đầy đủ; dịch vụ được giữ dừng.${plain}"
    fi
    return 1
)

config() {
    echo "v2node sẽ tự khởi động lại sau khi bạn chỉnh sửa cấu hình"
    nano /etc/v2node/config.json
    sleep 2
    restart
    check_status
    case $? in
        0)
            echo -e "Trạng thái v2node: ${green}đang chạy${plain}"
            ;;
        1)
            echo -e "v2node chưa chạy hoặc tự khởi động lại thất bại. Bạn có muốn xem nhật ký không? [Y/n]" && echo
            read -e -rp "(mặc định: y):" yn
            [[ -z ${yn} ]] && yn="y"
            if [[ ${yn} == [Yy] ]]; then
               show_log
            fi
            ;;
        2)
            echo -e "Trạng thái v2node: ${red}chưa cài đặt${plain}"
    esac
}

uninstall() {
    confirm "Bạn có chắc muốn gỡ cài đặt v2node không?" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    acquire_v2node_operation_lock || return 1
    trap release_v2node_operation_lock EXIT
    stop_v2node_service
    remove_terminal_service
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update del v2node
        rm /etc/init.d/v2node -f
    else
        systemctl disable v2node
        rm /etc/systemd/system/v2node.service -f
        systemctl daemon-reload
        systemctl reset-failed
    fi
    rm /etc/v2node/ -rf
    rm /usr/local/v2node/ -rf
    rm /usr/local/v2node.previous/ -rf
    release_v2node_operation_lock

    echo ""
    echo -e "Gỡ cài đặt thành công. Nếu muốn xóa cả script này, hãy thoát rồi chạy ${green}rm /usr/bin/v2node -f${plain}"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        if grep -q '"AgentID"' /etc/v2node/config.json 2>/dev/null; then
            if start_terminal_service; then
                echo ""
                echo -e "${green}v2node và dịch vụ terminal riêng đang ở trạng thái mong muốn.${plain}"
            else
                echo -e "${red}v2node đang chạy nhưng dịch vụ terminal riêng không khởi động được.${plain}"
            fi
        else
            echo ""
            echo -e "${green}v2node đang chạy bình thường.${plain}"
        fi
    else
        start_v2node_service
        local start_status=$?
        sleep 2
        if [[ $start_status == 0 ]] && check_status; then
            echo -e "${green}Khởi động v2node thành công. Dùng v2node log để xem nhật ký.${plain}"
        else
            echo -e "${red}Có thể v2node khởi động thất bại. Vui lòng dùng v2node log để kiểm tra nhật ký.${plain}"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

stop() {
    stop_v2node_service
    sleep 2
    check_status
    if [[ $? == 1 ]]; then
        echo -e "${green}Dừng v2node thành công${plain}"
    else
        echo -e "${red}Dừng v2node thất bại, có thể tiến trình cần hơn 2 giây. Vui lòng kiểm tra nhật ký sau.${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

restart() {
    stop_v2node_service
    start_v2node_service
    local restart_status=$?
    sleep 2
    if [[ $restart_status == 0 ]] && check_status; then
        echo -e "${green}Khởi động lại v2node thành công. Dùng v2node log để xem nhật ký.${plain}"
    else
        echo -e "${red}Có thể v2node khởi động thất bại. Vui lòng dùng v2node log để kiểm tra nhật ký.${plain}"
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

status() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node status
    else
        systemctl status v2node --no-pager -l
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

enable() {
    local status=0
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update add v2node || status=$?
    else
        systemctl enable v2node || status=$?
    fi
    enable_terminal_service || status=$?
    if [[ $status == 0 ]]; then
        echo -e "${green}Đã bật tự khởi động v2node cùng hệ thống${plain}"
    else
        echo -e "${red}Không thể bật tự khởi động v2node cùng hệ thống${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    local status=0
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update del v2node || status=$?
        rc-update del v2node-terminal default >/dev/null 2>&1 || true
    else
        systemctl disable v2node || status=$?
        systemctl disable v2node-terminal >/dev/null 2>&1 || true
    fi
    if [[ $status == 0 ]]; then
        echo -e "${green}Đã tắt tự khởi động v2node cùng hệ thống${plain}"
    else
        echo -e "${red}Không thể tắt tự khởi động v2node cùng hệ thống${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_log() {
    if [[ x"${release}" == x"alpine" ]]; then
        echo -e "${red}Alpine hiện chưa hỗ trợ xem nhật ký bằng chức năng này${plain}\n" && exit 1
    else
        journalctl -u v2node.service -e --no-pager -f
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

update_shell() {
    local temporary manager_ref
    manager_ref=$(latest_release_version "$release_repo")
    if [[ -z "$manager_ref" ]]; then
        echo -e "${red}Không xác định được release tag hợp lệ cho manager script.${plain}"
        return 1
    fi
    acquire_v2node_operation_lock || return 1
    trap release_v2node_operation_lock EXIT
    temporary=$(mktemp /usr/bin/.v2node.XXXXXX) || {
        release_v2node_operation_lock
        return 1
    }
    if ! download_verified_script_asset "$release_repo" "$manager_ref" v2node.sh "$temporary"; then
        rm -f "$temporary"
        echo ""
        echo -e "${red}Tải script thất bại. Vui lòng kiểm tra kết nối tới GitHub.${plain}"
        release_v2node_operation_lock
        before_show_menu
    else
        if chmod 0755 "$temporary" && mv -f "$temporary" /usr/bin/v2node; then
            echo -e "${green}Nâng cấp script thành công. Vui lòng chạy lại script.${plain}" && exit 0
        fi
        rm -f "$temporary"
        echo -e "${red}Không thể cài manager script theo giao dịch atomic; manager hiện tại được giữ nguyên.${plain}"
        release_v2node_operation_lock
        return 1
    fi
}

# 0: running, 1: not running, 2: not installed
check_status() {
    if [[ ! -f /usr/local/v2node/v2node ]]; then
        return 2
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(service v2node status | awk '{print $3}')
        if [[ x"${temp}" == x"started" ]]; then
            return 0
        else
            return 1
        fi
    else
        temp=$(systemctl status v2node | grep Active | awk '{print $3}' | cut -d "(" -f2 | cut -d ")" -f1)
        if [[ x"${temp}" == x"running" ]]; then
            return 0
        else
            return 1
        fi
    fi
}

check_enabled() {
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(rc-update show | grep v2node)
        if [[ x"${temp}" == x"" ]]; then
            return 1
        else
            return 0
        fi
    else
        temp=$(systemctl is-enabled v2node)
        if [[ x"${temp}" == x"enabled" ]]; then
            return 0
        else
            return 1;
        fi
    fi
}

check_uninstall() {
    check_status
    if [[ $? != 2 ]]; then
        echo ""
        echo -e "${red}v2node đã được cài đặt, vui lòng không cài lại.${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

check_install() {
    check_status
    if [[ $? == 2 ]]; then
        echo ""
        echo -e "${red}Vui lòng cài đặt v2node trước.${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

show_status() {
    check_status
    case $? in
        0)
            echo -e "Trạng thái v2node: ${green}đang chạy${plain}"
            show_enable_status
            ;;
        1)
            echo -e "Trạng thái v2node: ${yellow}đã dừng${plain}"
            show_enable_status
            ;;
        2)
            echo -e "Trạng thái v2node: ${red}chưa cài đặt${plain}"
    esac
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "Tự khởi động cùng hệ thống: ${green}có${plain}"
    else
        echo -e "Tự khởi động cùng hệ thống: ${red}không${plain}"
    fi
}

show_v2node_version() {
    echo -n "Phiên bản v2node: "
    /usr/local/v2node/v2node version
    echo ""
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

generate_v2node_config() {
    local api_host="$1"
    local node_id="$2"
    local api_key="$3"

    mkdir -p /etc/v2node >/dev/null 2>&1
    cat > /etc/v2node/config.json <<EOF
{
    "type": "v2board",
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "ConnectionConfig": {
        "Handshake": 15,
        "ConnIdle": 300,
        "UplinkOnly": 2,
        "DownlinkOnly": 4,
        "BufferSize": 128,
        "DisableUDPContentSniffing": true,
        "MaxConnectionsPerUser": 1024,
        "MaxConnections": 65535
    },
    "Resource": {
        "Profile": "standard",
        "MemLimitMB": 512,
        "GOGC": 80,
        "BufferSize": 128,
        "ConnectionIdle": 300,
        "DisableSniffing": true,
        "PeriodicMemoryReleaseInterval": 0
    },
    "Nodes": [
        {
            "ApiHost": "${api_host}",
            "NodeID": ${node_id},
            "ApiKey": "${api_key}",
            "Timeout": 15,
            "DisableSniffing": true
        }
    ]
}
EOF
    chmod 600 /etc/v2node/config.json
    echo -e "${green}Đã tạo cấu hình V2Board thành công tại /etc/v2node/config.json${plain}"
    restart 0
}

generate_config_file() {
    read -rp "Địa chỉ API Panel [ví dụ: https://ezviet.xyz]: " api_host
    if [[ -z "$api_host" ]]; then
        echo -e "${red}Địa chỉ API không được để trống!${plain}"
        return 1
    fi
    read -rp "Node ID: " node_id
    if [[ -z "$node_id" || ! "$node_id" =~ ^[0-9]+$ ]]; then
        echo -e "${red}Node ID phải là số nguyên dương!${plain}"
        return 1
    fi
    read -rp "Node Api Key: " api_key
    if [[ -z "$api_key" ]]; then
        echo -e "${red}Api Key không được để trống!${plain}"
        return 1
    fi

    generate_v2node_config "$api_host" "$node_id" "$api_key"
}

show_usage() {
    echo "Cách sử dụng script quản lý v2node: "
    echo "------------------------------------------"
    echo "v2node              - Hiển thị menu quản lý (đầy đủ chức năng)"
    echo "v2node start        - Khởi động v2node"
    echo "v2node stop         - Dừng v2node"
    echo "v2node restart      - Khởi động lại v2node"
    echo "v2node status       - Xem trạng thái v2node"
    echo "v2node enable       - Bật tự khởi động cùng hệ thống"
    echo "v2node disable      - Tắt tự khởi động cùng hệ thống"
    echo "v2node log          - Xem nhật ký v2node"
    echo "v2node x25519       - Tạo khóa x25519"
    echo "v2node generate     - Tạo cấu hình kết nối V2Board / XBoard"
    echo "v2node update       - Cập nhật v2node"
    echo "v2node update x.x.x - Cài phiên bản v2node chỉ định"
    echo "v2node rollback     - Quay lại bản v2node trước đó"
    echo "v2node install      - Cài đặt v2node"
    echo "v2node uninstall    - Gỡ cài đặt v2node"
    echo "v2node version      - Xem phiên bản v2node"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}Script quản lý v2node, ${plain}${red}không dùng cho Docker${plain}
--- https://github.com/Duyvj/v2node ---
  ${green}0.${plain} Chỉnh sửa cấu hình
————————————————
  ${green}1.${plain} Cài đặt v2node
  ${green}2.${plain} Cập nhật v2node
  ${green}3.${plain} Gỡ cài đặt v2node
————————————————
  ${green}4.${plain} Khởi động v2node
  ${green}5.${plain} Dừng v2node
  ${green}6.${plain} Khởi động lại v2node
  ${green}7.${plain} Xem trạng thái v2node
  ${green}8.${plain} Xem nhật ký v2node
————————————————
  ${green}9.${plain} Bật tự khởi động cùng hệ thống
  ${green}10.${plain} Tắt tự khởi động cùng hệ thống
————————————————
  ${green}11.${plain} Xem phiên bản v2node
  ${green}12.${plain} Nâng cấp script quản lý v2node
  ${green}13.${plain} Tạo cấu hình kết nối V2Board / XBoard
  ${green}14.${plain} Thoát script
 "
 # Có thể bổ sung chức năng mới vào menu phía trên
    show_status
    echo && read -rp "Vui lòng chọn [0-14]: " num

    case "${num}" in
        0) config ;;
        1) check_uninstall && install ;;
        2) check_install && update ;;
        3) check_install && uninstall ;;
        4) check_install && start ;;
        5) check_install && stop ;;
        6) check_install && restart ;;
        7) check_install && status ;;
        8) check_install && show_log ;;
        9) check_install && enable ;;
        10) check_install && disable ;;
        11) check_install && show_v2node_version ;;
        12) update_shell ;;
        13) generate_config_file ;;
        14) exit ;;
        *) echo -e "${red}Vui lòng nhập đúng một số từ 0 đến 15.${plain}" ;;
    esac
}


if [[ $# > 0 ]]; then
    case $1 in
        "start") check_install 0 && start 0 ;;
        "stop") check_install 0 && stop 0 ;;
        "restart") check_install 0 && restart 0 ;;
        "status") check_install 0 && status 0 ;;
        "enable") check_install 0 && enable 0 ;;
        "disable") check_install 0 && disable 0 ;;
        "log") check_install 0 && show_log 0 ;;
        "update") check_install 0 && update 0 $2 ;;
        "rollback") check_install 0 && rollback 0 ;;
        "config") config $* ;;
        "generate") generate_config_file ;;
        "install") check_uninstall 0 && install 0 ;;
        "uninstall") check_install 0 && uninstall 0 ;;
        "version") check_install 0 && show_v2node_version 0 ;;
        "update_shell") update_shell ;;
        *) show_usage
    esac
else
    show_menu
fi
