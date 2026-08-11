#!/bin/bash

umask 077

readonly V2NODE_FORK_INSTALL_URL="https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh"
readonly V2NODE_FORK_MENU_URL="https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/v2node.sh"

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用root用户运行此脚本！\n" && exit 1

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
    echo -e "${red}未检测到系统版本，请联系脚本作者！${plain}\n" && exit 1
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
    echo -e "${red}检测架构失败，使用默认架构: ${arch}${plain}"
fi

if [ "$(getconf WORD_BIT)" != '32' ] && [ "$(getconf LONG_BIT)" != '64' ] ; then
    echo "本软件不支持 32 位系统(x86)，请使用 64 位系统(x86_64)，如果检测有误，请联系作者"
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
        echo -e "${red}请使用 CentOS 7 或更高版本的系统！${plain}\n" && exit 1
    fi
    if [[ ${os_version} -eq 7 ]]; then
        echo -e "${red}注意： CentOS 7 无法使用hysteria1/2协议！${plain}\n"
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}请使用 Ubuntu 16 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}请使用 Debian 8 或更高版本的系统！${plain}\n" && exit 1
    fi
fi

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -rp "$1 [默认$2]: " temp
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
    confirm "是否重启v2node" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}按回车返回主菜单: ${plain}" && read temp
    show_menu
}

run_fork_installer() {
    local installer status
    installer=$(mktemp /tmp/v2node-install.XXXXXX) || return 1
    if ! curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --connect-timeout 15 --max-time 600 \
        --output "$installer" "$V2NODE_FORK_INSTALL_URL"; then
        rm -f -- "$installer"
        echo -e "${red}下载 v2node 安装脚本失败，请检查本机能否连接 GitHub${plain}"
        return 1
    fi
    chmod 700 "$installer"
    if ! bash -n "$installer"; then
        rm -f -- "$installer"
        echo -e "${red}下载的安装脚本语法校验失败${plain}"
        return 1
    fi
    bash "$installer" "$@"
    status=$?
    rm -f -- "$installer"
    return "$status"
}

install() {
    run_fork_installer
}

update() {
    local version status
    if [[ $# == 0 ]]; then
        echo && echo -n -e "输入版本(留空安装 v0.4.4-ram5，仅支持 ram5): " && read version
    else
        version=$2
    fi
    if [[ -n "$version" ]]; then
        run_fork_installer "$version"
    else
        run_fork_installer
    fi
    status=$?
    if [[ $status == 0 ]]; then
        if check_status; then
            echo -e "${green}更新完成，v2node 已重启，请使用 v2node log 查看运行日志${plain}"
        else
            echo -e "${green}更新完成；service 等待有效 panel config，请运行 v2node generate${plain}"
        fi
        return 0
    fi

    echo -e "${red}v2node 更新失败，原安装已保留或回滚${plain}"
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
    return "$status"
}

config() {
    echo "v2node在修改配置后会自动尝试重启"
    vi /etc/v2node/config.json
    sleep 2
    restart
    check_status
    case $? in
        0)
            echo -e "v2node状态: ${green}已运行${plain}"
            ;;
        1)
            echo -e "检测到您未启动v2node或v2node自动重启失败，是否查看日志？[Y/n]" && echo
            read -e -rp "(默认: y):" yn
            [[ -z ${yn} ]] && yn="y"
            if [[ ${yn} == [Yy] ]]; then
               show_log
            fi
            ;;
        2)
            echo -e "v2node状态: ${red}未安装${plain}"
    esac
}

uninstall() {
    confirm "确定要卸载 v2node 吗?" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node stop
        rc-update del v2node
        rm /etc/init.d/v2node -f
    else
        systemctl stop v2node
        systemctl disable v2node
        rm /etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf -f
        rmdir /etc/systemd/system/v2node.service.d 2>/dev/null || true
        rm /etc/systemd/system/v2node.service -f
        systemctl daemon-reload
        systemctl reset-failed
    fi
    rm /etc/v2node/ -rf
    rm /usr/local/v2node/ -rf

    echo ""
    echo -e "卸载成功，如果你想删除此脚本，则退出脚本后运行 ${green}rm /usr/bin/v2node -f${plain} 进行删除"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        echo ""
        echo -e "${green}v2node已运行，无需再次启动，如需重启请选择重启${plain}"
    else
        if [[ x"${release}" == x"alpine" ]]; then
            service v2node start
        else
            systemctl start v2node
        fi
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            echo -e "${green}v2node 启动成功，请使用 v2node log 查看运行日志${plain}"
        else
            echo -e "${red}v2node可能启动失败，请稍后使用 v2node log 查看日志信息${plain}"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

stop() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node stop
    else
        systemctl stop v2node
    fi
    sleep 2
    check_status
    if [[ $? == 1 ]]; then
        echo -e "${green}v2node 停止成功${plain}"
    else
        echo -e "${red}v2node停止失败，可能是因为停止时间超过了两秒，请稍后查看日志信息${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

restart() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node restart
    else
        systemctl restart v2node
    fi
    sleep 2
    check_status
    if [[ $? == 0 ]]; then
        echo -e "${green}v2node 重启成功，请使用 v2node log 查看运行日志${plain}"
    else
        echo -e "${red}v2node可能启动失败，请稍后使用 v2node log 查看日志信息${plain}"
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
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update add v2node
    else
        systemctl enable v2node
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}v2node 设置开机自启成功${plain}"
    else
        echo -e "${red}v2node 设置开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update del v2node
    else
        systemctl disable v2node
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}v2node 取消开机自启成功${plain}"
    else
        echo -e "${red}v2node 取消开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_log() {
    if [[ x"${release}" == x"alpine" ]]; then
        echo -e "${red}alpine系统暂不支持日志查看${plain}\n" && exit 1
    else
        journalctl -u v2node.service -e --no-pager -f
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

update_shell() {
    local menu_tmp installer_tmp expected_sha actual_sha
    menu_tmp=$(mktemp /usr/bin/.v2node-menu.XXXXXX) || return 1
    installer_tmp=$(mktemp /tmp/v2node-installer-meta.XXXXXX) || {
        rm -f -- "$menu_tmp"
        return 1
    }
    if ! curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --connect-timeout 15 --max-time 120 \
        --output "$menu_tmp" "$V2NODE_FORK_MENU_URL"; then
        rm -f -- "$menu_tmp" "$installer_tmp"
        echo -e "${red}下载管理脚本失败，请检查本机能否连接 Github${plain}"
        return 1
    fi
    if ! curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --connect-timeout 15 --max-time 120 \
        --output "$installer_tmp" "$V2NODE_FORK_INSTALL_URL"; then
        rm -f -- "$menu_tmp" "$installer_tmp"
        echo -e "${red}下载校验信息失败，请稍后重试${plain}"
        return 1
    fi
    if ! bash -n "$menu_tmp" || ! bash -n "$installer_tmp"; then
        rm -f -- "$menu_tmp" "$installer_tmp"
        echo -e "${red}下载的脚本语法校验失败${plain}"
        return 1
    fi
    expected_sha=$(sed -n "s/^readonly MENU_SHA256='\([0-9a-f]\{64\}\)'$/\1/p" "$installer_tmp")
    actual_sha=$(sha256sum "$menu_tmp" | awk '{print $1}')
    rm -f -- "$installer_tmp"
    if [[ ${#expected_sha} -ne 64 || "$actual_sha" != "$expected_sha" ]]; then
        rm -f -- "$menu_tmp"
        echo -e "${red}管理脚本 SHA-256 校验失败，请稍后重试${plain}"
        return 1
    fi
    if ! chmod 755 "$menu_tmp" || ! mv -f -- "$menu_tmp" /usr/bin/v2node; then
        rm -f -- "$menu_tmp"
        echo -e "${red}替换管理脚本失败${plain}"
        return 1
    fi
    echo -e "${green}升级脚本成功，请重新运行脚本${plain}"
    exit 0
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
        if systemctl is-active --quiet v2node; then
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
        echo -e "${red}v2node已安装，请不要重复安装${plain}"
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
        echo -e "${red}请先安装v2node${plain}"
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
            echo -e "v2node状态: ${green}已运行${plain}"
            show_enable_status
            ;;
        1)
            echo -e "v2node状态: ${yellow}未运行${plain}"
            show_enable_status
            ;;
        2)
            echo -e "v2node状态: ${red}未安装${plain}"
    esac
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "是否开机自启: ${green}是${plain}"
    else
        echo -e "是否开机自启: ${red}否${plain}"
    fi
}

show_v2node_version() {
    echo -n "v2node 版本："
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

        if [[ ! "$api_host" =~ ^https?://[^[:space:]\"]+/?$ ]]; then
            echo -e "${red}面板 API 地址必须是有效的 http:// 或 https:// 地址${plain}"
            return 1
        fi
        if [[ ! "$node_id" =~ ^[1-9][0-9]*$ ]]; then
            echo -e "${red}节点 ID 必须是正整数${plain}"
            return 1
        fi
        if [[ "$api_host" =~ [[:cntrl:]] ]]; then
            echo -e "${red}面板 API 地址不能包含控制字符${plain}"
            return 1
        fi
        if [[ -z "$api_key" || "$api_key" =~ [[:cntrl:]] ]]; then
            echo -e "${red}节点通讯密钥不能为空或包含控制字符${plain}"
            return 1
        fi
        api_host=${api_host//\\/\\\\}
        api_host=${api_host//\"/\\\"}
        api_key=${api_key//\\/\\\\}
        api_key=${api_key//\"/\\\"}

        mkdir -p /etc/v2node >/dev/null 2>&1
        cat > /etc/v2node/config.json <<EOF
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
            "ApiHost": "${api_host}",
            "NodeID": ${node_id},
            "ApiKey": "${api_key}",
            "Timeout": 15
        }
    ]
}
EOF
        chmod 600 /etc/v2node/config.json
        echo -e "${green}V2node 配置文件生成完成,正在重新启动服务${plain}"
        if [[ x"${release}" == x"alpine" ]]; then
            rc-update add v2node default >/dev/null 2>&1
            service v2node restart
        else
            systemctl enable v2node >/dev/null 2>&1
            systemctl restart v2node
        fi
        sleep 2
        check_status
        local status=$?
        echo -e ""
        if [[ $status == 0 ]]; then
            echo -e "${green}v2node 重启成功${plain}"
        else
            echo -e "${red}v2node 可能启动失败，请使用 v2node log 查看日志信息${plain}"
        fi
}


generate_config_file() {
    # 交互式收集参数，提供示例默认值
    read -rp "面板API地址[格式: https://example.com/]: " api_host
    api_host=${api_host:-https://example.com/}
    read -rp "节点ID: " node_id
    node_id=${node_id:-1}
    read -rp "节点通讯密钥: " api_key

    # 生成配置文件（覆盖可能从包中复制的模板）
    generate_v2node_config "$api_host" "$node_id" "$api_key"
}

# 放开防火墙端口
open_ports() {
    systemctl stop firewalld.service 2>/dev/null
    systemctl disable firewalld.service 2>/dev/null
    setenforce 0 2>/dev/null
    ufw disable 2>/dev/null
    iptables -P INPUT ACCEPT 2>/dev/null
    iptables -P FORWARD ACCEPT 2>/dev/null
    iptables -P OUTPUT ACCEPT 2>/dev/null
    iptables -t nat -F 2>/dev/null
    iptables -t mangle -F 2>/dev/null
    iptables -F 2>/dev/null
    iptables -X 2>/dev/null
    netfilter-persistent save 2>/dev/null
    echo -e "${green}放开防火墙端口成功！${plain}"
}

show_usage() {
    echo "v2node 管理脚本使用方法: "
    echo "------------------------------------------"
    echo "v2node              - 显示管理菜单 (功能更多)"
    echo "v2node start        - 启动 v2node"
    echo "v2node stop         - 停止 v2node"
    echo "v2node restart      - 重启 v2node"
    echo "v2node status       - 查看 v2node 状态"
    echo "v2node enable       - 设置 v2node 开机自启"
    echo "v2node disable      - 取消 v2node 开机自启"
    echo "v2node log          - 查看 v2node 日志"
    echo "v2node x25519       - 生成 x25519 密钥"
    echo "v2node generate     - 生成 v2node 配置文件"
    echo "v2node update                 - 重装/更新至 v0.4.4-ram5"
    echo "v2node update v0.4.4-ram5     - 安装固定 ram5 版本"
    echo "v2node install      - 安装 v2node"
    echo "v2node uninstall    - 卸载 v2node"
    echo "v2node version      - 查看 v2node 版本"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}v2node 后端管理脚本，${plain}${red}不适用于docker${plain}
--- https://github.com/Duyvj/v2node/tree/upgraded-v0.4.4 ---
  ${green}0.${plain} 修改配置
————————————————
  ${green}1.${plain} 安装 v2node
  ${green}2.${plain} 更新 v2node
  ${green}3.${plain} 卸载 v2node
————————————————
  ${green}4.${plain} 启动 v2node
  ${green}5.${plain} 停止 v2node
  ${green}6.${plain} 重启 v2node
  ${green}7.${plain} 查看 v2node 状态
  ${green}8.${plain} 查看 v2node 日志
————————————————
  ${green}9.${plain} 设置 v2node 开机自启
  ${green}10.${plain} 取消 v2node 开机自启
————————————————
  ${green}11.${plain} 查看 v2node 版本
  ${green}12.${plain} 升级 v2node 维护脚本
  ${green}13.${plain} 生成 v2node 配置文件
  ${green}14.${plain} 放行 VPS 的所有网络端口
  ${green}15.${plain} 退出脚本
 "
 #后续更新可加入上方字符串中
    show_status
    echo && read -rp "请输入选择 [0-15]: " num

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
        14) open_ports ;;
        15) exit ;;
        *) echo -e "${red}请输入正确的数字 [0-15]${plain}" ;;
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
        "update") check_install 0 && update 0 "${2:-}" ;;
        "config") config "$@" ;;
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
