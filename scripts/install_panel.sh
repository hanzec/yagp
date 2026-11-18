#!/bin/bash

# Yet Another Gost Panel (yagp) Binary Installation Script
# Original author: code-gopher
# Upstream project: https://github.com/code-gopher/gostPanel

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Check if running as root
if [ "$EUID" -ne 0 ]; then
  echo -e "${RED}错误: 请以 root 用户运行此脚本${NC}"
  exit 1
fi

# Configuration
APP_NAME="yagp"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="gost-panel"
CONFIG_DIR="/etc/gost-panel"
SERVICE_FILE="/etc/systemd/system/$SERVICE_NAME.service"
REPO=${YAGP_REPO:-hanzec/yagp}

# Detect Architecture
ARCH=$(uname -m)
case $ARCH in
    x86_64)  PLATFORM="linux-amd64" ;;
    aarch64|arm64) PLATFORM="linux-arm64" ;;
    *) echo -e "${RED}不支持的架构: $ARCH${NC}"; exit 1 ;;
esac

# Functions
function info() { echo -e "${GREEN}[INFO]${NC} $1"; }
function warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
function error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

function check_memory() {
    total_mem=$(free -m | grep Mem | awk '{print $2}')
    if [ "$total_mem" -le 128 ]; then
        error "系统内存不足 ($total_mem MB)。二进制部署模式要求内存必须大于 128MB。"
    fi
    info "内存检查通过: $total_mem MB"
}

function download_panel_archive() {
    local archive=$1 target=$2 url
    if [[ "$REPO" == hanzec/yagp && -z "${GH_PROXY+x}" ]]; then
        url="https://release.hanzec-overseas.cn/yagp/latest/$archive"
        info "正在下载发布包: $url"
        if curl -fL --connect-timeout 10 --speed-limit 1 --speed-time 20 "$url" -o "$target"; then
            return 0
        fi
    fi
    url="${GH_PROXY:-}https://github.com/$REPO/releases/latest/download/$archive"
    info "正在下载发布包: $url"
    curl -fL "$url" -o "$target"
}

# Older releases can update the hostless listener in the original block-style template.
function legacy_panel_port() {
    awk -v port="$2" '
        /^[^[:space:]#]/ { server = $0 == "server:" }
        server && /^  port: ":[0-9]+"$/ { $0 = "  port: \":" port "\""; matches++ }
        { print }
        END { if (matches != 1) exit 1 }
    ' "$1"
}

function install_panel() {
    check_memory
    TEMP_DIR=$(mktemp -d)
    trap 'rm -rf -- "$TEMP_DIR"' EXIT
    TAR_FILE="$TEMP_DIR/$APP_NAME.tar.gz"

    if ! download_panel_archive "$APP_NAME-$PLATFORM.tar.gz" "$TAR_FILE"; then
        error "无法下载 $APP_NAME-$PLATFORM.tar.gz，请确认 $REPO 已发布 yagp 安装包。"
    fi
    
    info "正在解压..."
    if ! tar -xzf "$TAR_FILE" -C "$TEMP_DIR"; then
        error "无法解压发布包"
    fi
    EXTRACTED_BIN="$TEMP_DIR/$APP_NAME-$PLATFORM"
    if [ ! -f "$EXTRACTED_BIN" ] || [ ! -f "$TEMP_DIR/LICENSE" ]; then
        error "发布包缺少二进制文件或许可证"
    fi

    # 处理配置目录
    mkdir -p "$CONFIG_DIR/config" "$CONFIG_DIR/data"
    local config="$CONFIG_DIR/config/config.yaml" address= staged
    chmod +x "$EXTRACTED_BIN"
    if [[ ! -f "$config" ]]; then
        cp "$TEMP_DIR/config/config.yaml" "$config"
    fi
    local supports_port=false
    if "$EXTRACTED_BIN" -help 2>&1 | grep -q -- '-install-port'; then
        supports_port=true
    fi
    if [[ -n $1 ]]; then
        config=$(readlink -f "$config")
        staged=$(mktemp "${config}.XXXXXX")
        cp -p "$config" "$staged"
        local port=$((10#$1))
        if [[ $supports_port == true ]]; then
            if "$EXTRACTED_BIN" -c "$config" -install-port "$port" > "$staged"; then
                address=$("$EXTRACTED_BIN" -c "$staged" -install-port current) || address=
            fi
        elif legacy_panel_port "$config" "$port" > "$staged"; then
            address=":$port"
        fi
        if [[ ${address##*:} != "$port" ]]; then
            rm -f "$staged"
            error "修改监听端口失败，原配置已保留；使用旧版本时请检查 server.port 的格式"
        fi
        mv -f "$staged" "$config"
    elif [[ $supports_port == true ]]; then
        address=$("$EXTRACTED_BIN" -c "$config" -install-port current)
    fi
    cp "$TEMP_DIR/LICENSE" "$CONFIG_DIR/LICENSE"
    mv "$EXTRACTED_BIN" "$INSTALL_DIR/$APP_NAME"

    # Create systemd service
    info "创建系统服务..."
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Yet Another Gost Panel (yagp)
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$CONFIG_DIR
ExecStart=$INSTALL_DIR/$APP_NAME -c $CONFIG_DIR/config/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"

    info "yagp 安装完成并已启动！"
    systemctl status "$SERVICE_NAME" --no-pager
    
    echo -e "\n${GREEN}==============================================${NC}"
    echo -e "   yagp 安装成功！"
    case "$address" in
        '') echo "   监听地址见 $config 中的 server.port" ;;
        :*|'[::]:'*|0.0.0.0:*)
            local port=${address##*:} ip internal_ip
            internal_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
            if [[ -z $internal_ip ]]; then
                internal_ip=$(ip -o route get to 8.8.8.8 2>/dev/null | sed -n 's/.*src \([0-9.]\+\).*/\1/p')
            fi
            for ip in "$(curl -s https://api.ipify.org || curl -s ifconfig.me)" "${internal_ip:-127.0.0.1}"; do
                [[ -n $ip ]] || continue
                [[ $ip != *:* ]] || ip="[$ip]"
                echo "   访问地址: http://$ip:$port"
            done
            ;;
        *) echo "   访问地址: http://$address" ;;
    esac
    echo -e "   管理员登录由 Authentik 正向认证代理处理"
    echo -e "${GREEN}==============================================${NC}\n"
}

function uninstall_panel() {
    info "正在卸载 yagp..."
    if ! systemctl stop "$SERVICE_NAME"; then
        case $(systemctl is-active "$SERVICE_NAME" || true) in
            inactive|failed) ;;
            *) error "无法确认 $SERVICE_NAME 已停止，保留安装文件" ;;
        esac
    fi
    if [[ -f "$SERVICE_FILE" ]]; then
        systemctl disable "$SERVICE_NAME"
    fi
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    rm -f "$INSTALL_DIR/$APP_NAME"
    warn "二进制文件已移除。配置文件目录 $CONFIG_DIR (包含数据库) 已保留。"
    info "卸载完成。"
}

# Main
ACTION="install"
PORT=""

# Check arguments
if [ -n "$1" ]; then
    # If first arg is a number, assume it's the port and action is install
    if [[ "$1" =~ ^[0-9]+$ ]]; then
        PORT="$1"
    elif [ "$1" == "install" ]; then
        ACTION="install"
        if [ "$2" == "--port" ]; then
            PORT="$3"
            [ -n "$PORT" ] || error "请为 --port 指定端口号"
        else
            PORT="$2"
        fi
    elif [ "$1" == "uninstall" ]; then
        ACTION="uninstall"
    else
        echo "用法: $0 [port] 或 $0 install [--port] [port] 或 $0 uninstall"
        exit 1
    fi
fi
if [[ -n "$PORT" ]]; then
    if ! [[ "$PORT" =~ ^[0-9]{1,5}$ ]] || (( 10#$PORT < 1 || 10#$PORT > 65535 )); then
        error "端口号必须在 1 到 65535 之间"
    fi
fi

case $ACTION in
    install)
        install_panel "$PORT"
        ;;
    uninstall)
        uninstall_panel
        ;;
esac
