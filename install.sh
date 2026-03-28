#!/bin/bash
set -euo pipefail

# ============================================================================
#  FoxWAF 安装脚本
#  用法: curl -fsSL <URL>/install.sh | bash
#  参数:
#    --docker         使用 Docker 模式安装
#    --mirror NAME    指定首选镜像源 (github|gitcode|gitee|gitlab)
#    --version VER    安装指定版本 (默认: latest)
#    --dir PATH       安装目录 (默认: /data/foxwaf)
#    --no-start       安装后不自动启动
#    --uninstall      卸载 FoxWAF
# ============================================================================

INSTALL_DIR="/data/foxwaf"
DOCKER_IMAGE="kabubu/foxwaf"
DOCKER_COMPOSE_FILE="docker-compose.yml"
FOXWAF_BIN="/usr/local/bin/foxwaf"
SYSTEMD_SERVICE="/etc/systemd/system/foxwaf.service"
VERSION="latest"
MODE=""
MIRROR=""
NO_START=false
COLOR_RED='\033[0;31m'
COLOR_GREEN='\033[0;32m'
COLOR_YELLOW='\033[1;33m'
COLOR_CYAN='\033[0;36m'
COLOR_RESET='\033[0m'
COLOR_BOLD='\033[1m'

MIRROR_REPOS_GITHUB="https://github.com/kabubu/storage"
MIRROR_REPOS_GITCODE="https://gitcode.com/kabubu/storage"
MIRROR_REPOS_GITEE="https://gitee.com/kabubu/storage"
MIRROR_REPOS_GITLAB="https://gitlab.com/kabubu/storage"

SERVER_API="https://server.foxwaf.cn:8443/api/update/check"

log_info()  { echo -e "${COLOR_GREEN}[INFO]${COLOR_RESET}  $*"; }
log_warn()  { echo -e "${COLOR_YELLOW}[WARN]${COLOR_RESET}  $*"; }
log_error() { echo -e "${COLOR_RED}[ERROR]${COLOR_RESET} $*"; }
log_step()  { echo -e "${COLOR_CYAN}[STEP]${COLOR_RESET}  ${COLOR_BOLD}$*${COLOR_RESET}"; }

print_banner() {
    echo -e "${COLOR_CYAN}"
    cat << 'BANNER'

    ███████╗ ██████╗ ██╗  ██╗██╗    ██╗ █████╗ ███████╗
    ██╔════╝██╔═══██╗╚██╗██╔╝██║    ██║██╔══██╗██╔════╝
    █████╗  ██║   ██║ ╚███╔╝ ██║ █╗ ██║███████║█████╗
    ██╔══╝  ██║   ██║ ██╔██╗ ██║███╗██║██╔══██║██╔══╝
    ██║     ╚██████╔╝██╔╝ ██╗╚███╔███╔╝██║  ██║██║
    ╚═╝      ╚═════╝ ╚═╝  ╚═╝ ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝

BANNER
    echo -e "${COLOR_RESET}"
    echo -e "    ${COLOR_BOLD}Lightweight High-Performance Web Application Firewall${COLOR_RESET}"
    echo ""
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --docker)    MODE="docker"; shift ;;
            --mirror)    MIRROR="$2"; shift 2 ;;
            --version)   VERSION="$2"; shift 2 ;;
            --dir)       INSTALL_DIR="$2"; shift 2 ;;
            --no-start)  NO_START=true; shift ;;
            --uninstall) do_uninstall; exit 0 ;;
            -h|--help)   show_help; exit 0 ;;
            *) log_error "未知参数: $1"; show_help; exit 1 ;;
        esac
    done
}

show_help() {
    cat << 'EOF'
FoxWAF 安装脚本

用法:
  install.sh [选项]

选项:
  --docker         使用 Docker 模式安装
  --mirror NAME    指定首选镜像源 (github|gitcode|gitee|gitlab)
  --version VER    安装指定版本 (默认: latest)
  --dir PATH       安装目录 (默认: /data/foxwaf)
  --no-start       安装后不自动启动
  --uninstall      卸载 FoxWAF
  -h, --help       显示帮助

示例:
  # 默认安装 (自动选择最佳模式)
  bash install.sh

  # Docker 模式安装
  bash install.sh --docker

  # 指定 gitcode 镜像源安装
  bash install.sh --mirror gitcode

  # 安装指定版本
  bash install.sh --version 9.0.1
EOF
}

check_root() {
    if [[ $EUID -ne 0 ]]; then
        log_error "请以 root 用户运行此脚本"
        exit 1
    fi
}

check_arch() {
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            log_error "不支持的架构: $ARCH (仅支持 x86_64/arm64)"
            exit 1
            ;;
    esac
    log_info "系统架构: $ARCH"
}

check_os() {
    if [[ "$(uname -s)" != "Linux" ]]; then
        log_error "FoxWAF 仅支持 Linux 系统"
        exit 1
    fi
    if command -v apt-get &>/dev/null; then
        PKG_MGR="apt-get"
    elif command -v yum &>/dev/null; then
        PKG_MGR="yum"
    elif command -v dnf &>/dev/null; then
        PKG_MGR="dnf"
    else
        PKG_MGR=""
    fi
    log_info "操作系统: $(uname -s) $(uname -r)"
}

check_deps() {
    local missing=()
    for cmd in curl; do
        if ! command -v "$cmd" &>/dev/null; then
            missing+=("$cmd")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        log_warn "缺少依赖: ${missing[*]}"
        if [[ -n "$PKG_MGR" ]]; then
            log_info "正在安装依赖..."
            $PKG_MGR install -y "${missing[@]}" >/dev/null 2>&1
        else
            log_error "请手动安装: ${missing[*]}"
            exit 1
        fi
    fi
}

check_docker() {
    if command -v docker &>/dev/null; then
        DOCKER_AVAILABLE=true
        DOCKER_VERSION=$(docker --version 2>/dev/null | grep -oP '\d+\.\d+' | head -1)
        log_info "Docker 已安装: $DOCKER_VERSION"
    else
        DOCKER_AVAILABLE=false
    fi
    if command -v docker-compose &>/dev/null || docker compose version &>/dev/null 2>&1; then
        COMPOSE_AVAILABLE=true
    else
        COMPOSE_AVAILABLE=false
    fi
}

auto_detect_mode() {
    if [[ -n "$MODE" ]]; then
        return
    fi
    if [[ "$DOCKER_AVAILABLE" == "true" && "$COMPOSE_AVAILABLE" == "true" ]]; then
        MODE="docker"
        log_info "检测到 Docker 环境，使用 Docker 模式安装"
    else
        MODE="bare"
        log_info "未检测到 Docker，使用裸机模式安装"
    fi
}

get_mirror_url() {
    local mirror_name="$1"
    case "$mirror_name" in
        github)  echo "$MIRROR_REPOS_GITHUB" ;;
        gitcode) echo "$MIRROR_REPOS_GITCODE" ;;
        gitee)   echo "$MIRROR_REPOS_GITEE" ;;
        gitlab)  echo "$MIRROR_REPOS_GITLAB" ;;
        *)       echo "" ;;
    esac
}

build_download_url() {
    local repo="$1"
    local version="$2"
    local file="$3"
    local platform="$4"
    local tag="v${version}"
    case "$platform" in
        gitlab)
            echo "${repo}/-/releases/${tag}/downloads/${file}"
            ;;
        *)
            echo "${repo}/releases/download/${tag}/${file}"
            ;;
    esac
}

get_latest_version_from_server() {
    log_info "从服务端获取最新版本信息..."
    local resp
    resp=$(curl -s --connect-timeout 10 -X POST "$SERVER_API" \
        -H "Content-Type: application/json" \
        -d '{"currentVersion":"0.0.0"}' 2>/dev/null) || true
    if [[ -n "$resp" ]]; then
        local ver
        ver=$(echo "$resp" | grep -oP '"version"\s*:\s*"[^"]*"' | head -1 | grep -oP '"[^"]*"$' | tr -d '"')
        if [[ -n "$ver" ]]; then
            log_info "服务端最新版本: $ver"
            VERSION="$ver"
            return 0
        fi
    fi
    log_warn "无法从服务端获取版本信息"
    return 1
}

build_mirror_list() {
    MIRRORS=()
    MIRROR_PLATFORMS=()
    if [[ -n "$MIRROR" ]]; then
        local url
        url=$(get_mirror_url "$MIRROR")
        if [[ -n "$url" ]]; then
            MIRRORS+=("$url")
            MIRROR_PLATFORMS+=("$MIRROR")
        fi
    fi
    for m in gitcode github gitee gitlab; do
        local url
        url=$(get_mirror_url "$m")
        local already=false
        for existing in "${MIRRORS[@]+"${MIRRORS[@]}"}"; do
            if [[ "$existing" == "$url" ]]; then
                already=true
                break
            fi
        done
        if [[ "$already" == "false" && -n "$url" ]]; then
            MIRRORS+=("$url")
            MIRROR_PLATFORMS+=("$m")
        fi
    done
}

download_file() {
    local file="$1"
    local dest="$2"
    local version="$3"

    for i in "${!MIRRORS[@]}"; do
        local repo="${MIRRORS[$i]}"
        local platform="${MIRROR_PLATFORMS[$i]}"
        local url
        url=$(build_download_url "$repo" "$version" "$file" "$platform")
        log_info "尝试下载: [$platform] $url"
        if curl -fSL --connect-timeout 15 --max-time 300 -o "$dest" "$url" 2>/dev/null; then
            log_info "下载成功: $file ($platform)"
            return 0
        fi
        log_warn "下载失败: $platform, 尝试下一个镜像..."
    done
    log_error "所有镜像均下载失败: $file"
    return 1
}

verify_md5() {
    local file="$1"
    local md5_file="$2"
    if [[ ! -f "$md5_file" ]]; then
        log_warn "MD5 校验文件不存在，跳过校验"
        return 0
    fi
    local expected
    expected=$(cat "$md5_file" | awk '{print $1}' | tr '[:upper:]' '[:lower:]')
    local actual
    actual=$(md5sum "$file" | awk '{print $1}' | tr '[:upper:]' '[:lower:]')
    if [[ "$expected" == "$actual" ]]; then
        log_info "MD5 校验通过: $file"
        return 0
    else
        log_error "MD5 校验失败: 期望=$expected 实际=$actual"
        return 1
    fi
}

create_default_config() {
    if [[ -f "${INSTALL_DIR}/conf.yaml" ]]; then
        log_info "配置文件已存在，跳过创建"
        return
    fi
    cat > "${INSTALL_DIR}/conf.yaml" << 'CONFEOF'
server:
  addr: "0.0.0.0"
  port: 8080
  https: false

database:
  type: "sqlite3"

secureentry: "foxadmin"
username: "fox"
password: "fox"

update:
  check_interval_minutes: 10
CONFEOF
    log_info "已创建默认配置文件: ${INSTALL_DIR}/conf.yaml"
}

install_docker_mode() {
    log_step "Docker 模式安装..."

    mkdir -p "${INSTALL_DIR}"

    if [[ "$VERSION" == "latest" ]]; then
        get_latest_version_from_server || true
    fi

    if [[ "$VERSION" == "latest" ]]; then
        log_error "无法获取版本号，请使用 --version 指定"
        exit 1
    fi

    build_mirror_list

    local tmpdir
    tmpdir=$(mktemp -d)
    trap "rm -rf '$tmpdir'" EXIT

    log_step "下载 FoxWAF 文件..."
    download_file "waf" "${tmpdir}/waf" "$VERSION"
    download_file "source.enc" "${tmpdir}/source.enc" "$VERSION"

    download_file "waf.md5" "${tmpdir}/waf.md5" "$VERSION" || true
    download_file "source.enc.md5" "${tmpdir}/source.enc.md5" "$VERSION" || true

    if [[ -f "${tmpdir}/waf.md5" ]]; then
        verify_md5 "${tmpdir}/waf" "${tmpdir}/waf.md5"
    fi
    if [[ -f "${tmpdir}/source.enc.md5" ]]; then
        verify_md5 "${tmpdir}/source.enc" "${tmpdir}/source.enc.md5"
    fi

    cp "${tmpdir}/waf" "${INSTALL_DIR}/waf"
    chmod 755 "${INSTALL_DIR}/waf"
    cp "${tmpdir}/source.enc" "${INSTALL_DIR}/source.enc"

    create_default_config

    cat > "${INSTALL_DIR}/${DOCKER_COMPOSE_FILE}" << DEOF
services:
  foxwaf:
    image: debian:bookworm-slim
    container_name: foxwaf
    restart: unless-stopped
    network_mode: host
    volumes:
      - ${INSTALL_DIR}:/app
    working_dir: /app
    entrypoint: ["./waf"]
    environment:
      - TZ=Asia/Shanghai
    logging:
      driver: json-file
      options:
        max-size: "50m"
        max-file: "3"
DEOF
    log_info "已创建 docker-compose.yml"

    echo "$VERSION" > "${INSTALL_DIR}/.version"

    install_foxwaf_script

    rm -rf "$tmpdir"
    trap - EXIT

    if [[ "$NO_START" == "false" ]]; then
        log_step "启动 FoxWAF..."
        cd "${INSTALL_DIR}" && docker compose up -d
        log_info "FoxWAF 已启动"
    fi

    print_success
}

install_bare_mode() {
    log_step "裸机模式安装..."

    mkdir -p "${INSTALL_DIR}"

    if [[ "$VERSION" == "latest" ]]; then
        get_latest_version_from_server || true
    fi

    if [[ "$VERSION" == "latest" ]]; then
        log_error "无法获取版本号，请使用 --version 指定"
        exit 1
    fi

    build_mirror_list

    local tmpdir
    tmpdir=$(mktemp -d)
    trap "rm -rf '$tmpdir'" EXIT

    log_step "下载 FoxWAF 文件..."
    download_file "waf" "${tmpdir}/waf" "$VERSION"
    download_file "source.enc" "${tmpdir}/source.enc" "$VERSION"

    download_file "waf.md5" "${tmpdir}/waf.md5" "$VERSION" || true
    download_file "source.enc.md5" "${tmpdir}/source.enc.md5" "$VERSION" || true

    if [[ -f "${tmpdir}/waf.md5" ]]; then
        verify_md5 "${tmpdir}/waf" "${tmpdir}/waf.md5"
    fi
    if [[ -f "${tmpdir}/source.enc.md5" ]]; then
        verify_md5 "${tmpdir}/source.enc" "${tmpdir}/source.enc.md5"
    fi

    cp "${tmpdir}/waf" "${INSTALL_DIR}/waf"
    chmod 755 "${INSTALL_DIR}/waf"
    cp "${tmpdir}/source.enc" "${INSTALL_DIR}/source.enc"

    create_default_config

    echo "$VERSION" > "${INSTALL_DIR}/.version"

    cat > "$SYSTEMD_SERVICE" << SEOF
[Unit]
Description=FoxWAF Web Application Firewall
After=network.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/waf
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SEOF
    systemctl daemon-reload
    log_info "已创建 systemd 服务"

    install_foxwaf_script

    rm -rf "$tmpdir"
    trap - EXIT

    if [[ "$NO_START" == "false" ]]; then
        log_step "启动 FoxWAF..."
        systemctl enable foxwaf >/dev/null 2>&1
        systemctl start foxwaf
        log_info "FoxWAF 已启动"
    fi

    print_success
}

install_foxwaf_script() {
    log_info "安装管理脚本: $FOXWAF_BIN"

    local script_url=""
    for m in gitcode github gitee; do
        local repo
        repo=$(get_mirror_url "$m")
        local url="${repo}/raw/main/foxwaf"
        if curl -fsSL --connect-timeout 5 -o /dev/null "$url" 2>/dev/null; then
            script_url="$url"
            break
        fi
    done

    if [[ -n "$script_url" ]]; then
        curl -fsSL -o "$FOXWAF_BIN" "$script_url"
    else
        generate_foxwaf_script
    fi

    chmod +x "$FOXWAF_BIN"
    sed -i "s|^INSTALL_DIR=.*|INSTALL_DIR=\"${INSTALL_DIR}\"|" "$FOXWAF_BIN" 2>/dev/null || true
    sed -i "s|^MODE=.*|MODE=\"${MODE}\"|" "$FOXWAF_BIN" 2>/dev/null || true
    log_info "管理脚本已安装: foxwaf"
}

generate_foxwaf_script() {
    cat > "$FOXWAF_BIN" << 'SCRIPTEOF'
#!/bin/bash
INSTALL_DIR="/data/foxwaf"
MODE="docker"
SCRIPTEOF
    cat >> "$FOXWAF_BIN" << 'SCRIPTEOF2'

COLOR_RED='\033[0;31m'
COLOR_GREEN='\033[0;32m'
COLOR_YELLOW='\033[1;33m'
COLOR_CYAN='\033[0;36m'
COLOR_RESET='\033[0m'

log_info()  { echo -e "${COLOR_GREEN}[INFO]${COLOR_RESET}  $*"; }
log_error() { echo -e "${COLOR_RED}[ERROR]${COLOR_RESET} $*"; }
log_warn()  { echo -e "${COLOR_YELLOW}[WARN]${COLOR_RESET}  $*"; }

detect_mode() {
    if [[ -f "${INSTALL_DIR}/docker-compose.yml" ]]; then
        MODE="docker"
    elif systemctl list-unit-files foxwaf.service &>/dev/null 2>&1; then
        MODE="bare"
    fi
}

do_start() {
    detect_mode
    if [[ "$MODE" == "docker" ]]; then
        cd "$INSTALL_DIR" && docker compose up -d
    else
        systemctl start foxwaf
    fi
    log_info "FoxWAF 已启动"
}

do_stop() {
    detect_mode
    if [[ "$MODE" == "docker" ]]; then
        cd "$INSTALL_DIR" && docker compose down
    else
        systemctl stop foxwaf
    fi
    log_info "FoxWAF 已停止"
}

do_restart() {
    detect_mode
    if [[ "$MODE" == "docker" ]]; then
        cd "$INSTALL_DIR" && docker compose restart
    else
        systemctl restart foxwaf
    fi
    log_info "FoxWAF 已重启"
}

do_status() {
    detect_mode
    echo -e "${COLOR_CYAN}═══════════════════ FoxWAF 状态 ═══════════════════${COLOR_RESET}"
    echo ""
    if [[ -f "${INSTALL_DIR}/.version" ]]; then
        echo -e "  版本:     $(cat "${INSTALL_DIR}/.version")"
    fi
    echo -e "  安装目录: ${INSTALL_DIR}"
    echo -e "  运行模式: ${MODE}"
    echo ""

    if [[ "$MODE" == "docker" ]]; then
        if docker ps --filter "name=foxwaf" --format '{{.Status}}' 2>/dev/null | grep -q "Up"; then
            echo -e "  状态:     ${COLOR_GREEN}运行中${COLOR_RESET}"
            docker ps --filter "name=foxwaf" --format "  容器ID:   {{.ID}}\n  启动时间: {{.RunningFor}}\n  端口:     {{.Ports}}" 2>/dev/null
        else
            echo -e "  状态:     ${COLOR_RED}已停止${COLOR_RESET}"
        fi
    else
        if systemctl is-active foxwaf &>/dev/null; then
            echo -e "  状态:     ${COLOR_GREEN}运行中${COLOR_RESET}"
            local pid
            pid=$(systemctl show foxwaf --property=MainPID --value 2>/dev/null)
            if [[ -n "$pid" && "$pid" != "0" ]]; then
                echo -e "  PID:      $pid"
                local mem
                mem=$(ps -p "$pid" -o rss= 2>/dev/null | awk '{printf "%.1f MB", $1/1024}')
                echo -e "  内存:     $mem"
            fi
        else
            echo -e "  状态:     ${COLOR_RED}已停止${COLOR_RESET}"
        fi
    fi

    if [[ -f "${INSTALL_DIR}/conf.yaml" ]]; then
        local port
        port=$(grep -oP 'port:\s*\K\d+' "${INSTALL_DIR}/conf.yaml" 2>/dev/null | head -1)
        if [[ -n "$port" ]]; then
            echo -e "  管理面板: http://localhost:${port}"
        fi
    fi
    echo ""
    echo -e "${COLOR_CYAN}══════════════════════════════════════════════════${COLOR_RESET}"
}

do_logs() {
    detect_mode
    if [[ "$MODE" == "docker" ]]; then
        cd "$INSTALL_DIR" && docker compose logs -f --tail 100
    else
        journalctl -u foxwaf -f --no-pager -n 100
    fi
}

do_export() {
    local backup_dir="${INSTALL_DIR}/backup"
    mkdir -p "$backup_dir"
    local timestamp
    timestamp=$(date +%Y%m%d_%H%M%S)
    local backup_file="${backup_dir}/foxwaf-backup-${timestamp}.tar.gz"

    log_info "开始导出备份..."

    local tmpdir
    tmpdir=$(mktemp -d)

    cp -a "${INSTALL_DIR}/conf.yaml" "$tmpdir/" 2>/dev/null || true
    cp -a "${INSTALL_DIR}/source.enc" "$tmpdir/" 2>/dev/null || true
    cp -a "${INSTALL_DIR}/waf" "$tmpdir/" 2>/dev/null || true
    cp -a "${INSTALL_DIR}/.version" "$tmpdir/" 2>/dev/null || true
    cp -a "${INSTALL_DIR}/docker-compose.yml" "$tmpdir/" 2>/dev/null || true

    for subdir in data certificates plugins history; do
        if [[ -d "${INSTALL_DIR}/${subdir}" ]]; then
            cp -a "${INSTALL_DIR}/${subdir}" "$tmpdir/" 2>/dev/null || true
        fi
    done

    for dbfile in "${INSTALL_DIR}"/*.db "${INSTALL_DIR}"/data/*.db; do
        if [[ -f "$dbfile" ]]; then
            cp -a "$dbfile" "$tmpdir/" 2>/dev/null || true
        fi
    done

    detect_mode
    if [[ "$MODE" == "docker" ]]; then
        log_info "导出 Docker 镜像..."
        local image_name
        image_name=$(grep -oP 'image:\s*\K\S+' "${INSTALL_DIR}/docker-compose.yml" 2>/dev/null | head -1)
        if [[ -n "$image_name" ]]; then
            docker save "$image_name" -o "${tmpdir}/docker-image.tar" 2>/dev/null || log_warn "Docker 镜像导出失败"
        fi
    fi

    tar -czf "$backup_file" -C "$tmpdir" .
    rm -rf "$tmpdir"

    local size
    size=$(du -sh "$backup_file" | awk '{print $1}')
    log_info "备份完成: $backup_file ($size)"
    echo "$backup_file"
}

do_import() {
    local backup_file="$1"
    if [[ -z "$backup_file" ]]; then
        local latest
        latest=$(ls -t "${INSTALL_DIR}/backup"/foxwaf-backup-*.tar.gz 2>/dev/null | head -1)
        if [[ -z "$latest" ]]; then
            log_error "请指定备份文件路径: foxwaf import <path>"
            exit 1
        fi
        backup_file="$latest"
        log_info "使用最新备份: $backup_file"
    fi

    if [[ ! -f "$backup_file" ]]; then
        log_error "备份文件不存在: $backup_file"
        exit 1
    fi

    log_info "开始恢复备份: $backup_file"

    do_stop 2>/dev/null || true

    local tmpdir
    tmpdir=$(mktemp -d)
    tar -xzf "$backup_file" -C "$tmpdir"

    mkdir -p "$INSTALL_DIR"

    for item in "$tmpdir"/*; do
        local name
        name=$(basename "$item")
        if [[ "$name" == "docker-image.tar" ]]; then
            continue
        fi
        if [[ "$name" == "backup" ]]; then
            continue
        fi
        cp -a "$item" "${INSTALL_DIR}/"
    done

    if [[ -f "${tmpdir}/docker-image.tar" ]]; then
        log_info "导入 Docker 镜像..."
        docker load -i "${tmpdir}/docker-image.tar" 2>/dev/null || log_warn "Docker 镜像导入失败"
    fi

    chmod 755 "${INSTALL_DIR}/waf" 2>/dev/null || true
    rm -rf "$tmpdir"

    log_info "恢复完成，正在启动..."
    do_start
}

do_update() {
    log_info "检查更新..."
    local resp
    local current_version="0.0.0"
    if [[ -f "${INSTALL_DIR}/.version" ]]; then
        current_version=$(cat "${INSTALL_DIR}/.version")
    fi
    resp=$(curl -s --connect-timeout 10 -X POST "https://server.foxwaf.cn:8443/api/update/check" \
        -H "Content-Type: application/json" \
        -d "{\"currentVersion\":\"${current_version}\"}" 2>/dev/null) || true

    if [[ -z "$resp" ]]; then
        log_error "无法连接更新服务器"
        exit 1
    fi

    local has_update
    has_update=$(echo "$resp" | grep -oP '"hasUpdate"\s*:\s*\K(true|false)' | head -1)
    if [[ "$has_update" != "true" ]]; then
        log_info "当前已是最新版本: $current_version"
        exit 0
    fi

    local new_version
    new_version=$(echo "$resp" | grep -oP '"version"\s*:\s*"[^"]*"' | head -1 | grep -oP '"[^"]*"$' | tr -d '"')
    log_info "发现新版本: $new_version (当前: $current_version)"

    read -rp "是否更新? [y/N] " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        log_info "已取消更新"
        exit 0
    fi

    log_info "开始更新到 $new_version ..."

    cp "${INSTALL_DIR}/waf" "${INSTALL_DIR}/waf.bak" 2>/dev/null || true
    cp "${INSTALL_DIR}/source.enc" "${INSTALL_DIR}/source.enc.bak" 2>/dev/null || true

    build_mirror_list

    local tmpdir
    tmpdir=$(mktemp -d)
    trap "rm -rf '$tmpdir'" EXIT

    if download_file "waf" "${tmpdir}/waf" "$new_version" && \
       download_file "source.enc" "${tmpdir}/source.enc" "$new_version"; then

        download_file "waf.md5" "${tmpdir}/waf.md5" "$new_version" || true
        download_file "source.enc.md5" "${tmpdir}/source.enc.md5" "$new_version" || true

        local ok=true
        if [[ -f "${tmpdir}/waf.md5" ]]; then
            verify_md5 "${tmpdir}/waf" "${tmpdir}/waf.md5" || ok=false
        fi
        if [[ -f "${tmpdir}/source.enc.md5" ]]; then
            verify_md5 "${tmpdir}/source.enc" "${tmpdir}/source.enc.md5" || ok=false
        fi

        if [[ "$ok" == "true" ]]; then
            do_stop 2>/dev/null || true
            cp "${tmpdir}/waf" "${INSTALL_DIR}/waf"
            chmod 755 "${INSTALL_DIR}/waf"
            cp "${tmpdir}/source.enc" "${INSTALL_DIR}/source.enc"
            echo "$new_version" > "${INSTALL_DIR}/.version"
            do_start
            log_info "更新成功: $new_version"
            rm -f "${INSTALL_DIR}/waf.bak" "${INSTALL_DIR}/source.enc.bak"
        else
            log_error "校验失败，回滚..."
            mv "${INSTALL_DIR}/waf.bak" "${INSTALL_DIR}/waf" 2>/dev/null || true
            mv "${INSTALL_DIR}/source.enc.bak" "${INSTALL_DIR}/source.enc" 2>/dev/null || true
        fi
    else
        log_error "下载失败，保持当前版本"
        rm -f "${INSTALL_DIR}/waf.bak" "${INSTALL_DIR}/source.enc.bak"
    fi

    rm -rf "$tmpdir"
    trap - EXIT
}

do_uninstall() {
    log_warn "即将卸载 FoxWAF"
    read -rp "确认卸载? 数据将被保留在 ${INSTALL_DIR} [y/N] " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        log_info "已取消"
        exit 0
    fi

    detect_mode
    do_stop 2>/dev/null || true

    if [[ "$MODE" == "docker" ]]; then
        cd "$INSTALL_DIR" && docker compose down --rmi local 2>/dev/null || true
    fi

    if [[ -f "$SYSTEMD_SERVICE" ]]; then
        systemctl disable foxwaf 2>/dev/null || true
        rm -f "$SYSTEMD_SERVICE"
        systemctl daemon-reload
    fi

    rm -f "$FOXWAF_BIN"

    log_info "FoxWAF 已卸载 (数据保留在 ${INSTALL_DIR})"
    log_info "如需彻底删除数据: rm -rf ${INSTALL_DIR}"
}

do_version() {
    if [[ -f "${INSTALL_DIR}/.version" ]]; then
        echo "FoxWAF $(cat "${INSTALL_DIR}/.version")"
    else
        echo "FoxWAF (版本未知)"
    fi
}

build_mirror_list() {
    MIRRORS=()
    MIRROR_PLATFORMS=()
    for m in gitcode github gitee gitlab; do
        local url
        url=$(get_mirror_url "$m")
        if [[ -n "$url" ]]; then
            MIRRORS+=("$url")
            MIRROR_PLATFORMS+=("$m")
        fi
    done
}

show_usage() {
    cat << 'USAGE'
FoxWAF 管理工具

用法: foxwaf <command>

命令:
  start       启动 FoxWAF
  stop        停止 FoxWAF
  restart     重启 FoxWAF
  status      查看运行状态
  logs        查看实时日志
  update      检查并应用更新
  export      导出数据备份
  import      从备份恢复
  uninstall   卸载 FoxWAF
  version     查看版本
USAGE
}

case "${1:-}" in
    start)     do_start ;;
    stop)      do_stop ;;
    restart)   do_restart ;;
    status)    do_status ;;
    logs)      do_logs ;;
    export)    do_export ;;
    import)    do_import "${2:-}" ;;
    update)    do_update ;;
    uninstall) do_uninstall ;;
    version)   do_version ;;
    *)         show_usage; exit 1 ;;
esac
SCRIPTEOF2
}

print_success() {
    echo ""
    echo -e "${COLOR_GREEN}════════════════════════════════════════════════════════${COLOR_RESET}"
    echo -e "${COLOR_GREEN}  FoxWAF 安装成功!${COLOR_RESET}"
    echo -e "${COLOR_GREEN}════════════════════════════════════════════════════════${COLOR_RESET}"
    echo ""
    echo -e "  安装目录:  ${INSTALL_DIR}"
    echo -e "  运行模式:  ${MODE}"
    echo -e "  版本:      ${VERSION}"
    echo ""
    if [[ -f "${INSTALL_DIR}/conf.yaml" ]]; then
        local port
        port=$(grep -oP 'port:\s*\K\d+' "${INSTALL_DIR}/conf.yaml" 2>/dev/null | head -1)
        local entry
        entry=$(grep -oP 'secureentry:\s*"\K[^"]+' "${INSTALL_DIR}/conf.yaml" 2>/dev/null | head -1)
        if [[ -n "$port" ]]; then
            echo -e "  管理面板:  http://<服务器IP>:${port}/${entry:-foxadmin}"
            echo -e "  默认账号:  fox / fox"
        fi
    fi
    echo ""
    echo -e "  常用命令:"
    echo -e "    foxwaf status     查看运行状态"
    echo -e "    foxwaf logs       查看日志"
    echo -e "    foxwaf restart    重启服务"
    echo -e "    foxwaf export     备份数据"
    echo -e "    foxwaf update     检查更新"
    echo ""
    echo -e "${COLOR_YELLOW}  重要: 请及时修改默认密码!${COLOR_RESET}"
    echo ""
}

do_uninstall() {
    log_warn "即将卸载 FoxWAF"
    read -rp "确认卸载? 数据将被保留在 ${INSTALL_DIR} [y/N] " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
        log_info "已取消"
        exit 0
    fi

    check_docker

    if [[ -f "${INSTALL_DIR}/docker-compose.yml" ]]; then
        cd "$INSTALL_DIR" && docker compose down 2>/dev/null || true
    fi
    if [[ -f "$SYSTEMD_SERVICE" ]]; then
        systemctl stop foxwaf 2>/dev/null || true
        systemctl disable foxwaf 2>/dev/null || true
        rm -f "$SYSTEMD_SERVICE"
        systemctl daemon-reload
    fi
    rm -f "$FOXWAF_BIN"
    log_info "FoxWAF 已卸载 (数据保留在 ${INSTALL_DIR})"
    log_info "如需彻底删除数据: rm -rf ${INSTALL_DIR}"
}

main() {
    print_banner
    parse_args "$@"
    check_root
    check_os
    check_arch
    check_deps
    check_docker
    auto_detect_mode

    if [[ "$MODE" == "docker" ]]; then
        if [[ "$DOCKER_AVAILABLE" != "true" ]]; then
            log_error "Docker 模式需要安装 Docker"
            log_info "安装 Docker: curl -fsSL https://get.docker.com | bash"
            exit 1
        fi
        install_docker_mode
    else
        install_bare_mode
    fi
}

main "$@"
