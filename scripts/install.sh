#!/usr/bin/env bash
set -euo pipefail

REPO="9star-academy/bakctl"
BIN_NAME="bakctl"
INSTALL_DIR="${BAKCTL_INSTALL_DIR:-/usr/local/bin}"

fail() { echo "错误: $*" >&2; exit 1; }
command -v curl >/dev/null || fail "需要 curl"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) fail "不支持的架构 $ARCH" ;;
esac
case "$OS" in linux|darwin) ;; *) fail "不支持的操作系统 $OS" ;; esac

TAG="${BAKCTL_VERSION:-}"
if [ -z "$TAG" ]; then
  RELEASE="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" || fail "无法查询最新版本；可用 BAKCTL_VERSION 指定版本"
  TAG="$(printf '%s\n' "$RELEASE" | sed -nE 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' | head -n 1)"
fi
[[ "$TAG" =~ ^v[0-9][A-Za-z0-9._-]*$ ]] || fail "无效的发布版本"
FILE="${BIN_NAME}-${OS}-${ARCH}"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
WORK_DIR="$(mktemp -d)"
INSTALL_TMP=""
trap 'rm -rf "$WORK_DIR"; if [ -n "$INSTALL_TMP" ]; then rm -f "$INSTALL_TMP"; fi' EXIT

printf '==> 下载 %s (%s) [%s/%s]...\n' "$BIN_NAME" "$TAG" "$OS" "$ARCH"
curl -fsSL "${BASE_URL}/${FILE}" -o "$WORK_DIR/$FILE" || fail "二进制下载失败"
curl -fsSL "${BASE_URL}/SHA256SUMS" -o "$WORK_DIR/SHA256SUMS" || fail "发布缺少 SHA256SUMS；请使用包含校验和的新版本或从源码构建"
EXPECTED="$(awk -v file="$FILE" '$2 == file { print $1 }' "$WORK_DIR/SHA256SUMS")"
[[ "$EXPECTED" =~ ^[0-9a-fA-F]{64}$ ]] || fail "发布校验和缺失或格式错误"
if command -v sha256sum >/dev/null; then
  ACTUAL="$(sha256sum "$WORK_DIR/$FILE" | awk '{print $1}')"
elif command -v shasum >/dev/null; then
  ACTUAL="$(shasum -a 256 "$WORK_DIR/$FILE" | awk '{print $1}')"
else
  fail "需要 sha256sum 或 shasum"
fi
[ "$ACTUAL" = "$EXPECTED" ] || fail "下载文件校验失败，未替换已安装版本"

mkdir -p "$INSTALL_DIR" || fail "无法创建安装目录；请使用 sudo 或设置 BAKCTL_INSTALL_DIR"
[ ! -d "$INSTALL_DIR/$BIN_NAME" ] || fail "安装目标是目录，拒绝替换: $INSTALL_DIR/$BIN_NAME"
INSTALL_TMP="$(mktemp "$INSTALL_DIR/.bakctl.XXXXXX")" || fail "安装目录不可写；请使用 sudo 或设置 BAKCTL_INSTALL_DIR"
install -m 0755 "$WORK_DIR/$FILE" "$INSTALL_TMP"
mv -f "$INSTALL_TMP" "$INSTALL_DIR/$BIN_NAME"
INSTALL_TMP=""
printf '✔ %s %s 已安装至 %s\n' "$BIN_NAME" "$TAG" "$INSTALL_DIR/$BIN_NAME"
