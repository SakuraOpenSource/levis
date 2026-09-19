#!/usr/bin/env bash
# ==============================================================================
# Levis 一键安装脚本（Linux / macOS）
#
# 用法：
#   sudo bash install-levis.sh                 # 交互式安装（默认主程序）
#   sudo bash install-levis.sh --update        # 只升级二进制，保留 data/
#   sudo bash install-levis.sh --version v0.2.0
#
# 可选环境变量：
#   LEVIS_VERSION       版本（默认 latest，从 GitHub Releases 拉取）
#   LEVIS_INSTALL_DIR   安装目录（默认 /opt/levis）
#   LEVIS_DATA_DIR      数据目录（默认 /opt/levis/data）
#
# 目录约定：
#   /opt/levis            二进制（levis）+ data/（config.json + SQLite）
#   /etc/systemd/system/levis.service   Linux systemd 服务
# ==============================================================================
set -Eeuo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
REPO="SakuraOpenSource/levis"
INSTALL_DIR="${LEVIS_INSTALL_DIR:-/opt/levis}"
DATA_DIR="${LEVIS_DATA_DIR:-/opt/levis/data}"
VERSION="${LEVIS_VERSION:-latest}"
SERVICE="levis"
UPDATE=0
NO_START=0
GH_PROXY=""

log()  { echo -e "${GREEN}[levis]${NC} $*"; }
warn() { echo -e "${YELLOW}[levis]${NC} $*"; }
die()  { echo -e "${RED}[levis]${NC} $*" >&2; exit 1; }

usage() {
  sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --update) UPDATE=1; shift;;
    --version) VERSION="${2:-latest}"; shift 2;;
    --gh-proxy) GH_PROXY="${2:-}"; shift 2;;
    --no-start) NO_START=1; shift;;
    -h|--help) usage;;
    *) die "未知参数: $1（--help 查看用法）";;
  esac
done

run_root() { if [[ "$(id -u)" -eq 0 ]]; then "$@"; else sudo "$@"; fi; }

detect_platform() {
  local os arch
  os="$(uname -s)"; arch="$(uname -m)"
  case "$os" in
    Linux)  PLATFORM="linux";;
    Darwin) PLATFORM="darwin";;
    *) die "不支持的操作系统: $os（Windows 请从 GitHub Releases 手动下载 levis-windows-amd64.exe）";;
  esac
  case "$arch" in
    x86_64|amd64)  GOARCH="amd64";;
    arm64|aarch64) GOARCH="arm64";;
    *) die "不支持的 CPU 架构: $arch";;
  esac
  [[ "$PLATFORM$GOARCH" == "darwinarm64" || "$PLATFORM$GOARCH" == "darwinamd64" \
    || "$PLATFORM$GOARCH" == "linuxamd64" || "$PLATFORM$GOARCH" == "linuxarm64" ]] \
    || die "暂无 $PLATFORM/$GOARCH 的发布产物"
}

download() {
  local url="$1" out="$2"
  if [[ -n "$GH_PROXY" && "$url" == *"github.com"* ]]; then
    url="${GH_PROXY%/}/$url"
  fi
  log "下载 $url"
  if command -v curl >/dev/null 2>&1; then
    run_root curl -fSL --retry 3 -o "$out" "$url" || die "下载失败: $url"
  elif command -v wget >/dev/null 2>&1; then
    run_root wget -qO "$out" "$url" || die "下载失败: $url"
  else
    die "需要 curl 或 wget"
  fi
  [[ -s "$out" ]] || die "下载得到空文件: $url"
}

resolve_version() {
  [[ "$VERSION" != "latest" ]] && return
  log "获取最新 release 版本..."
  VERSION="$(run_root curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')" || true
  [[ -n "$VERSION" ]] || die "无法解析最新版本，请用 --version 指定"
  log "版本: $VERSION"
}

install_binary() {
  local asset="levis-$PLATFORM-$GOARCH"
  local tmp; tmp="$(mktemp)"
  download "https://github.com/$REPO/releases/download/$VERSION/$asset" "$tmp"

  # 二进制魔数校验：Linux ELF / macOS Mach-O 64。
  local magic; magic="$(od -An -tx1 -N4 "$tmp" 2>/dev/null | tr -d ' \n')"
  case "$PLATFORM" in
    linux)  [[ "$magic" == "7f454c46" ]] || die "下载内容不是有效的 Linux 二进制";;
    darwin) [[ "$magic" == "cffaedfe" || "$magic" == "feedfacf" || "$magic" == "cafebabe" ]] || die "下载内容不是有效的 macOS 二进制";;
  esac

  run_root install -m 755 "$tmp" "$INSTALL_DIR/levis"
  run_root rm -f "$tmp"
}

write_service_linux() {
  command -v systemctl >/dev/null 2>&1 || return 1
  run_root tee "/etc/systemd/system/$SERVICE.service" >/dev/null <<EOF
[Unit]
Description=Levis Billing System
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/levis -data $DATA_DIR
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  run_root systemctl daemon-reload
  run_root systemctl enable "$SERVICE" >/dev/null
  return 0
}

detect_platform

# 源码构建优先（仓库内执行时），否则拉 GitHub Releases。
if [[ -f "go.mod" && -d "cmd/levis" && -d "internal" ]]; then
  command -v go >/dev/null 2>&1 || die "源码构建需要 Go 1.26+（或删除 go.mod 使用 release 下载）"
  log "检测到源码，本地构建..."
  # 前端可选：存在 ../levis-frontend 时构建嵌入。
  if [[ -d "../levis-frontend" ]]; then
    log "构建前端..."
    if (cd ../levis-frontend && (command -v pnpm >/dev/null 2>&1 || npm install -g pnpm) \
        && pnpm install --frozen-lockfile && pnpm build); then
      run_root rm -rf internal/web/dist
      run_root mkdir -p internal/web/dist
      run_root cp -R ../levis-frontend/dist/. internal/web/dist/
      run_root touch internal/web/dist/.gitkeep
    else
      warn "前端构建失败，继续（后端无前端也可运行）"
    fi
  fi
  run_root mkdir -p "$INSTALL_DIR" "$DATA_DIR"
  log "编译 Levis..."
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "/tmp/levis-build.$$" ./cmd/levis
  run_root install -m 755 "/tmp/levis-build.$$" "$INSTALL_DIR/levis"
  run_root rm -f "/tmp/levis-build.$$"
else
  run_root mkdir -p "$INSTALL_DIR" "$DATA_DIR"
  resolve_version
  install_binary
fi

log "安装目录: $INSTALL_DIR"
log "数据目录: $DATA_DIR"

if write_service_linux; then
  if [[ "$NO_START" -eq 0 ]]; then
    run_root systemctl restart "$SERVICE"
    sleep 2
    if systemctl -q is-active "$SERVICE"; then
      log "服务已启动: systemctl status $SERVICE"
    else
      warn "服务未启动，查看日志: journalctl -u $SERVICE -n 50"
    fi
  else
    log "已跳过启动（--no-start）"
  fi
else
  if [[ "$(uname -s)" == "Darwin" ]]; then
    warn "macOS 未注册 launchd 服务，请手动运行:"
    echo "  $INSTALL_DIR/levis -data $DATA_DIR"
  else
    warn "未检测到 systemd，请手动运行:"
    echo "  $INSTALL_DIR/levis -data $DATA_DIR"
  fi
fi

PORT="$(grep -oE '"listen"[[:space:]]*:[[:space:]]*"[^"]+"' "$DATA_DIR/config.json" 2>/dev/null | grep -oE ':[0-9]+' | tr -d ':' || true)"
PORT="${PORT:-8080}"
log "完成。访问 http://<本机IP>:$PORT 完成安装/使用"
