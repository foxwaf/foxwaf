#!/bin/bash
set -euo pipefail

# ─── 常量 ───────────────────────────────────────────────────────────────────
INSTALL_DIR="/data/foxwaf"
FOXWAF_BIN="/usr/local/bin/foxwaf"
VERSION="latest"
MODE=""
MIRROR=""
NO_START=false
FOXWAF_SERVER="${FOXWAF_SERVER:-server.foxwaf.cn}"
SERVER_API="http://${FOXWAF_SERVER}:8080/api/update/check"
SERVER_DOWNLOAD="http://${FOXWAF_SERVER}:8080/release"
WAF_DEFAULT_PORT=8088

MIRRORS_GITHUB="https://github.com/kabubu/foxwaf"
MIRRORS_GITCODE="https://gitcode.com/kabubu/foxwaf"
MIRRORS_GITEE="https://gitee.com/kabubu/foxwaf"
MIRRORS_GITLAB="https://gitlab.com/kabubu/foxwaf"
# Docker Hub 镜像名（与 release.sh 推送一致）；可用环境变量覆盖: FOXWAF_DOCKERHUB_IMAGE=myuser/foxwaf
MIRRORS_DOCKERHUB_IMAGE="${FOXWAF_DOCKERHUB_IMAGE:-loveyoudocker/foxwaf}"

# 公网 DNS 解析后走 --resolve，减轻本地 DNS 污染导致的下载失败
DNS_SERVERS=(8.8.8.8 223.5.5.5)
declare -a CURL_DNS_ARGS=()

prepare_curl_dns() {
    CURL_DNS_ARGS=()
    local url="$1" host ip d
    [[ "$url" != http://* && "$url" != https://* ]] && return 0
    host="${url#*://}"
    host="${host%%/*}"
    host="${host%%:*}"
    [[ -z "$host" || "$host" == "$url" ]] && return 0
    ip=""
    if command -v dig &>/dev/null; then
        for d in "${DNS_SERVERS[@]}"; do
            ip=$(dig +short "$host" @"$d" A 2>/dev/null | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
            [[ -n "$ip" ]] && break
        done
    fi
    [[ -z "$ip" ]] && return 0
    CURL_DNS_ARGS=(--resolve "${host}:443:${ip}" --resolve "${host}:80:${ip}")
}

# ─── 颜色 & 符号 ────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'
BLUE='\033[0;34m'; MAGENTA='\033[0;35m'; BOLD='\033[1m'; DIM='\033[2m'
RESET='\033[0m'
SYM_OK="${GREEN}✓${RESET}"; SYM_FAIL="${RED}✗${RESET}"; SYM_WARN="${YELLOW}!${RESET}"
SYM_ARROW="${CYAN}›${RESET}"; SYM_DOT="${DIM}·${RESET}"

# ─── 辅助函数 ────────────────────────────────────────────────────────────────
_col() { tput cols 2>/dev/null || echo 80; }

log_ok()   { echo -e "  ${SYM_OK}  $*"; }
log_fail() { echo -e "  ${SYM_FAIL}  ${RED}$*${RESET}"; }
log_warn() { echo -e "  ${SYM_WARN}  ${YELLOW}$*${RESET}"; }
log_step() { echo -e "\n  ${SYM_ARROW}  ${BOLD}$*${RESET}"; }
log_dim()  { echo -e "     ${DIM}$*${RESET}"; }

die() { log_fail "$*"; exit 1; }

spinner() {
    local pid=$1 msg="${2:-}"
    local frames=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
    local i=0
    while kill -0 "$pid" 2>/dev/null; do
        printf "\r  ${CYAN}${frames[$i]}${RESET}  %s" "$msg"
        i=$(( (i + 1) % ${#frames[@]} ))
        sleep 0.08
    done
    wait "$pid" 2>/dev/null
    return $?
}

progress_bar() {
    local current=$1 total=$2 label="${3:-}" width=30
    local pct=$((current * 100 / total))
    local filled=$((current * width / total))
    local empty=$((width - filled))
    local bar=""
    for ((i=0; i<filled; i++)); do bar+="█"; done
    for ((i=0; i<empty; i++)); do bar+="░"; done
    printf "\r  ${SYM_DOT}  ${DIM}%-12s${RESET} ${BLUE}%s${RESET} ${DIM}%3d%%${RESET}" "$label" "$bar" "$pct"
}

download_with_progress() {
    local url="$1" dest="$2" label="${3:-下载中}"
    local tmpfile="${dest}.tmp" attempt total_size dl_pid cur_size ret
    prepare_curl_dns "$url"
    for attempt in 1 2 3; do
        rm -f "$tmpfile"
        total_size=$(curl -sI -L "${CURL_DNS_ARGS[@]}" "$url" 2>/dev/null | grep -i content-length | tail -1 | awk '{print $2}' | tr -d '\r') || true

        if [[ -n "$total_size" && "$total_size" -gt 0 ]] 2>/dev/null; then
            curl -fSL --connect-timeout 15 --max-time 600 "${CURL_DNS_ARGS[@]}" -o "$tmpfile" "$url" 2>/dev/null &
            dl_pid=$!
            while kill -0 "$dl_pid" 2>/dev/null; do
                if [[ -f "$tmpfile" ]]; then
                    cur_size=$(stat -c%s "$tmpfile" 2>/dev/null || echo 0)
                    progress_bar "$cur_size" "$total_size" "$label"
                fi
                sleep 0.3
            done
            wait "$dl_pid" 2>/dev/null
            ret=$?
            if [[ $ret -eq 0 ]]; then
                progress_bar "$total_size" "$total_size" "$label"
                echo ""
                mv "$tmpfile" "$dest"
                return 0
            fi
        else
            curl -fSL --connect-timeout 15 --max-time 600 "${CURL_DNS_ARGS[@]}" -o "$tmpfile" "$url" 2>/dev/null &
            spinner $! "$label"
            ret=$?
            if [[ $ret -eq 0 && -f "$tmpfile" ]]; then
                echo ""
                mv "$tmpfile" "$dest"
                return 0
            fi
        fi
        rm -f "$tmpfile"
        [[ "$attempt" -lt 3 ]] && sleep $((attempt * 2))
    done
    return 1
}

# ─── Banner ──────────────────────────────────────────────────────────────────
print_banner() {
    echo ""
    echo -e "  ${CYAN}${BOLD}"
    echo '   ███████╗ ██████╗ ██╗  ██╗██╗    ██╗ █████╗ ███████╗'
    echo '   ██╔════╝██╔═══██╗╚██╗██╔╝██║    ██║██╔══██╗██╔════╝'
    echo '   █████╗  ██║   ██║ ╚███╔╝ ██║ █╗ ██║███████║█████╗  '
    echo '   ██╔══╝  ██║   ██║ ██╔██╗ ██║███╗██║██╔══██║██╔══╝  '
    echo '   ██║     ╚██████╔╝██╔╝ ██╗╚███╔███╔╝██║  ██║██║     '
    echo '   ╚═╝      ╚═════╝ ╚═╝  ╚═╝ ╚══╝╚══╝ ╚═╝  ╚═╝╚═╝     '
    echo -e "  ${RESET}"
    echo -e "  ${DIM}Lightweight High-Performance Web Application Firewall${RESET}"
    echo ""
}

# ─── 参数解析 ────────────────────────────────────────────────────────────────
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --docker)    MODE="docker"; shift ;;
            --mirror)    MIRROR="${2:-}"; shift 2 ;;
            --version)   VERSION="${2:-}"; shift 2 ;;
            --dir)       INSTALL_DIR="${2:-}"; shift 2 ;;
            --no-start)  NO_START=true; shift ;;
            --uninstall) do_uninstall; exit 0 ;;
            -h|--help)   show_help; exit 0 ;;
            *) die "未知参数: $1（使用 --help 查看帮助）" ;;
        esac
    done
}

show_help() {
    echo -e "
  ${BOLD}FoxWAF 安装脚本${RESET}

  ${BOLD}用法${RESET}
    install.sh [选项]

  ${BOLD}选项${RESET}
    --mirror NAME    首选镜像源 ${DIM}(github|gitcode|gitee|gitlab|dockerhub)${RESET}
    --version VER    指定版本号 ${DIM}(默认: 最新)${RESET}
    --dir PATH       安装目录 ${DIM}(默认: /data/foxwaf)${RESET}
    --no-start       安装后不自动启动
    --uninstall      卸载 FoxWAF
    -h, --help       显示帮助

  ${BOLD}示例${RESET}
    ${DIM}# Docker 模式安装${RESET}
    bash install.sh --docker

    ${DIM}# 指定 gitcode 镜像源${RESET}
    bash install.sh --docker --mirror gitcode

    ${DIM}# 安装指定版本到自定义目录${RESET}
    bash install.sh --version 1.0.0 --dir /opt/foxwaf
"
}

# ─── 系统检测 ────────────────────────────────────────────────────────────────
preflight() {
    log_step "系统检测"

    [[ $EUID -eq 0 ]] || die "请以 root 权限运行"
    log_ok "root 权限"

    [[ "$(uname -s)" == "Linux" ]] || die "仅支持 Linux"
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "不支持的架构: $ARCH" ;;
    esac
    log_ok "系统: Linux $(uname -r | cut -d- -f1) ($ARCH)"

    command -v curl &>/dev/null || {
        log_warn "正在安装 curl..."
        apt-get install -y curl &>/dev/null 2>&1 || yum install -y curl &>/dev/null 2>&1 || die "无法安装 curl"
    }
    log_ok "curl 就绪"

    DOCKER_OK=false; COMPOSE_OK=false
    if command -v docker &>/dev/null; then
        DOCKER_OK=true
        local dv; dv=$(docker --version 2>/dev/null | grep -oP '\d+\.\d+' | head -1)
        log_ok "Docker $dv"
        if docker compose version &>/dev/null 2>&1; then
            COMPOSE_OK=true
            log_ok "Docker Compose"
        fi
    else
        log_dim "Docker 未安装"
    fi
}

detect_mode() {
    [[ -n "$MODE" ]] && return
    if [[ "$DOCKER_OK" == "true" && "$COMPOSE_OK" == "true" ]]; then
        MODE="docker"
        log_ok "自动选择: Docker 模式"
    else
        die "需要 Docker 和 Docker Compose 才能安装 FoxWAF\n  安装 Docker: curl -fsSL https://get.docker.com | sh"
    fi
}

# ─── 版本获取 ────────────────────────────────────────────────────────────────
fetch_version() {
    [[ "$VERSION" != "latest" ]] && return
    log_step "获取最新版本"
    local resp attempt
    resp=""
    for attempt in 1 2 3; do
        prepare_curl_dns "$SERVER_API"
        resp=$(curl -s --connect-timeout 10 "${CURL_DNS_ARGS[@]}" -X POST "$SERVER_API" \
            -H "Content-Type: application/json" \
            -d '{"currentVersion":"0.0.0"}' 2>/dev/null) || true
        [[ -n "$resp" ]] && break
        sleep $((attempt * 2))
    done
    if [[ -n "$resp" ]]; then
        local ver
        ver=$(echo "$resp" | grep -oP '"version"\s*:\s*"[^"]*"' | head -1 | grep -oP '"[^"]*"$' | tr -d '"')
        if [[ -n "$ver" ]]; then
            VERSION="$ver"
            log_ok "最新版本: ${BOLD}$VERSION${RESET}"
            return
        fi
    fi
    die "无法获取版本信息（服务端不可达），请使用 --version 指定"
}

# ─── 镜像源 & 下载 ──────────────────────────────────────────────────────────
build_url() {
    local repo="$1" ver="$2" file="$3" platform="$4"
    local tag="v${ver}"
    case "$platform" in
        gitcode)
            local path="${repo#*gitcode.com/}"; path="${path%/}"
            echo "https://api.gitcode.com/api/v5/repos/${path}/releases/${tag}/attach_files/${file}/download" ;;
        gitlab)
            echo "${repo}/-/releases/${tag}/downloads/${file}" ;;
        *)
            echo "${repo}/releases/download/${tag}/${file}" ;;
    esac
}

MIRROR_ORDER=(gitcode github gitee gitlab dockerhub)

get_repo() {
    case "$1" in
        github)  echo "$MIRRORS_GITHUB" ;;
        gitcode) echo "$MIRRORS_GITCODE" ;;
        gitee)   echo "$MIRRORS_GITEE" ;;
        gitlab)  echo "$MIRRORS_GITLAB" ;;
        dockerhub) echo "" ;;
    esac
}

build_mirror_order() {
    ORDERED_MIRRORS=()
    if [[ -n "$MIRROR" ]]; then
        ORDERED_MIRRORS+=("$MIRROR")
    fi
    for m in "${MIRROR_ORDER[@]}"; do
        local dup=false
        for e in "${ORDERED_MIRRORS[@]+"${ORDERED_MIRRORS[@]}"}"; do
            [[ "$e" == "$m" ]] && dup=true && break
        done
        [[ "$dup" == "false" ]] && ORDERED_MIRRORS+=("$m")
    done
}

download_file() {
    local file="$1" dest="$2" ver="$3" label="${4:-$1}"
    for m in "${ORDERED_MIRRORS[@]}"; do
        local repo; repo=$(get_repo "$m")
        [[ -z "$repo" ]] && continue
        local url; url=$(build_url "$repo" "$ver" "$file" "$m")
        if download_with_progress "$url" "$dest" "$label"; then
            log_dim "来源: $m"
            return 0
        fi
    done
    local fb="${SERVER_DOWNLOAD}/${ver}/${file}"
    if download_with_progress "$fb" "$dest" "$label"; then
        log_dim "来源: 服务端(兜底)"
        return 0
    fi
    return 1
}

verify_md5() {
    local file="$1" md5_file="$2"
    [[ ! -f "$md5_file" ]] && return 0
    local expected actual
    expected=$(awk '{print $1}' "$md5_file" | tr '[:upper:]' '[:lower:]')
    actual=$(md5sum "$file" | awk '{print $1}')
    [[ "$expected" == "$actual" ]]
}

# 从 Docker Hub 拉取 foxwaf 镜像并打 tag 为 kabubu/foxwaf:<ver>（与 compose 一致）
_pull_foxwaf_from_dockerhub() {
    local ver="$1" src="${MIRRORS_DOCKERHUB_IMAGE}:${ver}" dst="kabubu/foxwaf:${ver}" attempt
    for attempt in 1 2 3; do
        log_dim "尝试 docker pull ${src} (${attempt}/3)..."
        if docker pull "$src"; then
            docker tag "$src" "$dst" 2>/dev/null || true
            log_ok "已从 Docker Hub 拉取并标记为 ${dst}"
            return 0
        fi
        [[ "$attempt" -lt 3 ]] && sleep $((attempt * 2))
    done
    return 1
}

# 按 MIRROR_ORDER 依次尝试：Git 附件 tar 下载 → 服务端 tar → Docker Hub pull
download_foxwaf_image_bundle() {
    local tmp="$1" ver="$2" m repo url
    build_mirror_order
    for m in "${ORDERED_MIRRORS[@]}"; do
        if [[ "$m" == "dockerhub" ]]; then
            if _pull_foxwaf_from_dockerhub "$ver"; then
                log_dim "来源: dockerhub (${MIRRORS_DOCKERHUB_IMAGE})"
                return 2
            fi
            continue
        fi
        repo=$(get_repo "$m")
        [[ -z "$repo" ]] && continue
        url=$(build_url "$repo" "$ver" "foxwaf-image.tar.gz" "$m")
        if download_with_progress "$url" "${tmp}/image.tar.gz" "Docker 镜像"; then
            log_dim "来源: $m"
            return 0
        fi
    done
    local fb="${SERVER_DOWNLOAD}/${ver}/foxwaf-image.tar.gz"
    if download_with_progress "$fb" "${tmp}/image.tar.gz" "Docker 镜像(服务端)"; then
        log_dim "来源: 服务端(兜底)"
        return 0
    fi
    if _pull_foxwaf_from_dockerhub "$ver"; then
        log_dim "来源: dockerhub (${MIRRORS_DOCKERHUB_IMAGE}) 兜底"
        return 2
    fi
    return 1
}

# ─── Docker 模式安装 ─────────────────────────────────────────────────────────
install_docker() {
    log_step "下载 (Docker 模式)"
    [[ "$DOCKER_OK" != "true" ]] && die "Docker 未安装，请先安装: curl -fsSL https://get.docker.com | bash"

    mkdir -p "$INSTALL_DIR"
    build_mirror_order

    local tmp; tmp=$(mktemp -d)
    trap "rm -rf '$tmp'" EXIT

    local dlrc
    download_foxwaf_image_bundle "$tmp" "$VERSION"
    dlrc=$?
    [[ "$dlrc" -eq 1 ]] && die "镜像获取失败（Git 附件、服务端与 Docker Hub 均不可用）"

    if [[ "$dlrc" -eq 2 ]]; then
        log_ok "使用 Docker Hub 镜像，跳过 tar 导入"
    else
        download_file "foxwaf-image.tar.gz.md5" "$tmp/image.md5" "$VERSION" "镜像校验" || true

        if [[ -f "$tmp/image.md5" ]]; then
            if verify_md5 "$tmp/image.tar.gz" "$tmp/image.md5"; then
                log_ok "MD5 校验通过"
            else
                die "镜像 MD5 校验失败，文件可能损坏"
            fi
        fi

        log_step "导入镜像"
        docker load -i "$tmp/image.tar.gz" &
        spinner $! "正在导入 Docker 镜像"
        echo ""
        log_ok "Docker 镜像已导入"
    fi

    log_step "配置"
    cat > "$INSTALL_DIR/docker-compose.yml" << DEOF
services:
  foxwaf:
    image: kabubu/foxwaf:${VERSION}
    container_name: foxwaf
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./conf.yaml:/app/conf.yaml
      - ./data:/app/data
      - ./waf.db:/app/waf.db
    environment:
      - TZ=Asia/Shanghai
      - SERVER=1
    logging:
      driver: json-file
      options:
        max-size: "50m"
        max-file: "3"
DEOF
    log_ok "Compose 配置已生成"

    create_config
    touch "${INSTALL_DIR}/waf.db"
    echo "$VERSION" > "$INSTALL_DIR/.version"
    install_foxwaf_bin

    rm -rf "$tmp"; trap - EXIT

    if [[ "$NO_START" == "false" ]]; then
        log_step "启动服务"
        cd "$INSTALL_DIR" && docker compose up -d &>/dev/null &
        spinner $! "正在启动 FoxWAF"
        echo ""
        sleep 1
        if docker inspect foxwaf &>/dev/null && [[ "$(docker inspect foxwaf --format '{{.State.Running}}' 2>/dev/null)" == "true" ]]; then
            log_ok "FoxWAF 运行中"
        else
            log_warn "容器可能未正常启动，请检查: foxwaf logs"
        fi
    fi
}

# ─── 公共 ────────────────────────────────────────────────────────────────────
create_config() {
    [[ -f "$INSTALL_DIR/conf.yaml" ]] && { log_dim "配置文件已存在，跳过"; return; }
    mkdir -p "$INSTALL_DIR/data"
    cat > "$INSTALL_DIR/conf.yaml" << 'EOF'
Database:
    DBName: waf.db
Server:
    Addr: 0.0.0.0
    CertFile: ""
    HTTPRedirectPort: 0
    HTTPS: false
    KeyFile: ""
    Port: 8088
Update:
    CheckIntervalMinutes: 0
    IgnoredVersion: ""
    MaxBackupDays: 0
    MaxBackupVersions: 0
    UpdateStrategy: ""
password: 776cb326ab0cd5f0a974c1b9606044d8485201f2db19cf8e3749bdee5f36e200
secureentry: fox
username: fox
EOF
    log_ok "默认配置已生成"
}

install_foxwaf_bin() {
    log_step "安装管理工具"
    local ok=false tmp
    tmp=$(mktemp)

    if download_file "foxwaf" "$tmp" "$VERSION" "foxwaf 脚本" 2>/dev/null; then
        if head -1 "$tmp" | grep -q '^#!/bin/bash'; then
            cp "$tmp" "$FOXWAF_BIN"; ok=true
        fi
    fi

    if [[ "$ok" != "true" ]]; then
        local u try
        for u in "https://raw.githubusercontent.com/kabubu/foxwaf/main/foxwaf" "https://gitee.com/kabubu/foxwaf/raw/main/foxwaf"; do
            for try in 1 2 3; do
                prepare_curl_dns "$u"
                if curl -fsSL --connect-timeout 8 "${CURL_DNS_ARGS[@]}" -o "$tmp" "$u" 2>/dev/null && head -1 "$tmp" | grep -q '^#!/bin/bash'; then
                    cp "$tmp" "$FOXWAF_BIN"; ok=true; break 2
                fi
                [[ "$try" -lt 3 ]] && sleep $((try * 2))
            done
        done
    fi

    rm -f "$tmp"
    [[ "$ok" != "true" ]] && generate_foxwaf_script
    chmod +x "$FOXWAF_BIN"
    sed -i "s|^INSTALL_DIR=.*|INSTALL_DIR=\"${INSTALL_DIR}\"|" "$FOXWAF_BIN" 2>/dev/null || true
    log_ok "foxwaf 命令已安装"
}

generate_foxwaf_script() {
    cat > "$FOXWAF_BIN" << 'FEOF'
#!/bin/bash
INSTALL_DIR="/data/foxwaf"
CONTAINER="foxwaf"
R='\033[0;31m'; G='\033[0;32m'; Y='\033[1;33m'; C='\033[0;36m'; B='\033[1m'; D='\033[2m'; N='\033[0m'
ok() { echo -e "  ${G}✓${N}  $*"; }; err() { echo -e "  ${R}✗${N}  $*"; }; wrn() { echo -e "  ${Y}!${N}  $*"; }
is_docker() { [[ -f "${INSTALL_DIR}/docker-compose.yml" ]]; }
do_start() { if is_docker; then if docker inspect "$CONTAINER" &>/dev/null; then docker start "$CONTAINER"; else cd "$INSTALL_DIR" && docker compose up -d; fi; else cd "$INSTALL_DIR" && nohup ./waf > waf.log 2>&1 & echo $! > waf.pid; fi; ok "已启动"; }
do_stop()  { if is_docker; then docker stop "$CONTAINER" 2>/dev/null || docker kill "$CONTAINER" 2>/dev/null; else [[ -f "$INSTALL_DIR/waf.pid" ]] && kill "$(cat "$INSTALL_DIR/waf.pid")" 2>/dev/null; rm -f "$INSTALL_DIR/waf.pid"; pkill -f "$INSTALL_DIR/waf" 2>/dev/null; fi; ok "已停止"; }
do_restart() { do_stop 2>/dev/null; sleep 1; do_start; }
do_status() {
  echo -e "\n  ${C}${B}FoxWAF 状态${N}\n"
  [[ -f "$INSTALL_DIR/.version" ]] && echo -e "  版本  $(cat "$INSTALL_DIR/.version")"
  echo -e "  目录  $INSTALL_DIR"
  if is_docker; then echo -e "  模式  Docker"
    if ! docker inspect "$CONTAINER" &>/dev/null; then echo -e "  状态  ${Y}容器不存在${N}"
    elif [[ "$(docker inspect "$CONTAINER" --format '{{.State.Running}}' 2>/dev/null)" == "true" ]]; then
      local _cid _up; _cid=$(docker inspect "$CONTAINER" --format '{{.Id}}' 2>/dev/null)
      _up=$(docker ps --no-trunc --filter "id=${_cid}" --format '{{.RunningFor}}' 2>/dev/null | head -1)
      echo -e "  状态  ${G}运行中${N}"; echo -e "  容器  $(echo "$_cid" | cut -c1-12)  ${_up}"
    else echo -e "  状态  ${R}已停止${N}"; fi
  else echo -e "  模式  裸机"
    local p=""; [[ -f "$INSTALL_DIR/waf.pid" ]] && p=$(cat "$INSTALL_DIR/waf.pid")
    if [[ -n "$p" ]] && kill -0 "$p" 2>/dev/null; then echo -e "  状态  ${G}运行中${N}  PID $p"
    else echo -e "  状态  ${R}已停止${N}"; fi
  fi; echo ""
}
do_logs() { if is_docker; then docker logs -f --tail 100 "$CONTAINER" 2>/dev/null || (cd "$INSTALL_DIR" && docker compose logs -f --tail 100); else [[ -f "$INSTALL_DIR/waf.log" ]] && tail -f -n 100 "$INSTALL_DIR/waf.log" || err "无日志"; fi; }
do_version() { [[ -f "$INSTALL_DIR/.version" ]] && echo "FoxWAF $(cat "$INSTALL_DIR/.version")" || echo "FoxWAF (unknown)"; }
case "${1:-}" in start) do_start;; stop) do_stop;; restart) do_restart;; status) do_status;; logs) do_logs;; version) do_version;; *) echo -e "\n  ${B}foxwaf${N} start|stop|restart|status|logs|version\n";; esac
FEOF
}

# ─── 卸载 ────────────────────────────────────────────────────────────────────
do_uninstall() {
    echo ""
    log_warn "即将卸载 FoxWAF"
    read -rp "  确认卸载? 数据保留在 ${INSTALL_DIR} [y/N] " c
    [[ "$c" != "y" && "$c" != "Y" ]] && { echo "  已取消"; exit 0; }
    if [[ -f "$INSTALL_DIR/docker-compose.yml" ]]; then
        cd "$INSTALL_DIR" && docker compose down 2>/dev/null || true
    fi
    if [[ -f "$INSTALL_DIR/waf.pid" ]]; then
        kill "$(cat "$INSTALL_DIR/waf.pid")" 2>/dev/null || true
        rm -f "$INSTALL_DIR/waf.pid"
    fi
    pkill -f "$INSTALL_DIR/waf" 2>/dev/null || true
    rm -f "$FOXWAF_BIN"
    log_ok "已卸载（数据保留: $INSTALL_DIR）"
}

# ─── 安装完成 ────────────────────────────────────────────────────────────────
print_success() {
    local port entry
    port=$(grep -i '^\s*Port:' "$INSTALL_DIR/conf.yaml" 2>/dev/null | head -1 | awk '{print $2}')
    entry=$(grep -i '^\s*secureentry:' "$INSTALL_DIR/conf.yaml" 2>/dev/null | head -1 | awk '{print $2}')

    echo ""
    echo -e "  ${GREEN}${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo -e "  ${GREEN}${BOLD}  安装完成${RESET}"
    echo -e "  ${GREEN}${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo ""
    echo -e "  ${DIM}版本${RESET}      $VERSION"
    echo -e "  ${DIM}目录${RESET}      $INSTALL_DIR"
    echo -e "  ${DIM}模式${RESET}      $MODE"
    [[ -n "$port" ]] && echo -e "  ${DIM}面板${RESET}      http://<IP>:${port}/${entry:-foxadmin}"
    echo ""
    echo -e "  ${DIM}账号${RESET}      fox / fox  ${RED}${BOLD}← 请立即修改${RESET}"
    echo ""
    echo -e "  ${DIM}常用命令:${RESET}"
    echo -e "    foxwaf status     ${DIM}运行状态${RESET}"
    echo -e "    foxwaf logs       ${DIM}查看日志${RESET}"
    echo -e "    foxwaf restart    ${DIM}重启服务${RESET}"
    echo -e "    foxwaf export     ${DIM}备份数据${RESET}"
    echo -e "    foxwaf update     ${DIM}检查更新${RESET}"
    echo ""
}

# ─── main ────────────────────────────────────────────────────────────────────
main() {
    print_banner
    parse_args "$@"
    preflight
    detect_mode
    fetch_version

    install_docker

    print_success
}

main "$@"
