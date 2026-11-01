#!/usr/bin/env bash
# HomehubExtension 一键安装脚本
#
# 把本脚本与 homehub-extension 二进制放在同一目录，然后：
#   sudo ./install.sh
#
# 流程：装二进制到 /usr/bin → 建 /etc/hhe 放配置 → 装 systemd 单元，
# 最后询问是否开机自启、是否立即启动。
#
# 同目录下需要有：
#   homehub-extension        必需，主程序
#   hhe.service              必需，systemd 单元
#   config.example.json      必需，配置模板（仅在 /etc/hhe/config.json 不存在时使用）
#   backup.sh                可选，备份脚本
#   hhe-backup.service/.timer 可选，每日备份定时器（与 backup.sh 一起装）
#
# 可选参数：
#   --yes       全部按默认值回答，不交互（默认值是「是」）
#   --dry-run   只打印将要执行的动作，不改动系统

set -euo pipefail

BIN_NAME=homehub-extension
SVC_NAME=hhe
SVC_USER=homehub
BIN_DIR=/usr/bin
CONF_DIR=/etc/hhe
DATA_DIR=/var/lib/hhe
BACKUP_DIR=/var/backups/hhe

ASSUME_YES=0
DRY_RUN=0

# 颜色只在终端下启用。不用 readonly：非 tty 时需要把它清空，
# 对 readonly 变量赋值会让脚本在 set -e 下直接退出。
if [ -t 1 ]; then
  C_OK=$'\033[32m' C_WARN=$'\033[33m' C_ERR=$'\033[31m' C_DIM=$'\033[2m' C_END=$'\033[0m'
else
  C_OK='' C_WARN='' C_ERR='' C_DIM='' C_END=''
fi

step() { printf '\n%s==>%s %s\n' "$C_DIM" "$C_END" "$*"; }
ok()   { printf '    %s✓%s %s\n' "$C_OK" "$C_END" "$*"; }
warn() { printf '    %s!%s %s\n' "$C_WARN" "$C_END" "$*"; }
die()  { printf '\n%serror:%s %s\n' "$C_ERR" "$C_END" "$*" >&2; exit 1; }

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '    %s[dry-run]%s %s\n' "$C_DIM" "$C_END" "$*"
  else
    "$@"
  fi
}

# ask QUESTION DEFAULT(y|n) → 返回 0 表示 yes
ask() {
  local q="$1" def="$2" hint answer
  if [ "$ASSUME_YES" -eq 1 ] || [ ! -t 0 ]; then
    [ "$def" = y ] && return 0 || return 1
  fi
  case "$def" in
    y) hint='[Y/n]' ;;
    n) hint='[y/N]' ;;
  esac
  while :; do
    printf '    %s %s ' "$q" "$hint"
    read -r answer || answer=''
    answer=${answer:-$def}
    case "$answer" in
      y|Y|yes|YES) return 0 ;;
      n|N|no|NO)   return 1 ;;
      *) printf '      请输入 y 或 n\n' ;;
    esac
  done
}

# 打印文件头部的连续注释行作为用法说明；遇到第一行代码就停。
usage() {
  sed -n '2,/^[^#]/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y)  ASSUME_YES=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

# ---- 前置检查 ----

step "检查环境"

[ "$(id -u)" -eq 0 ] || die "需要 root：请用 sudo 执行"
ok "root 权限"

command -v systemctl >/dev/null || die "找不到 systemctl：本脚本面向 systemd 系统"
[ -d /run/systemd/system ] || die "systemd 未在运行（容器里请改用镜像部署）"
ok "systemd 可用"

SRC_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

[ -f "$SRC_DIR/$BIN_NAME" ] || die "同目录找不到 $BIN_NAME
    把 homehub-extension 与本脚本放在一起，或先跑 make build 生成到 dist/"
ok "$BIN_NAME 存在（$SRC_DIR）"

[ -f "$SRC_DIR/$SVC_NAME.service" ] || die "同目录找不到 $SVC_NAME.service（systemd 单元）"
[ -f "$SRC_DIR/config.example.json" ] || die "同目录找不到 config.example.json（配置模板）"

WITH_BACKUP=0
if [ -f "$SRC_DIR/backup.sh" ] && [ -f "$SRC_DIR/$SVC_NAME-backup.service" ] \
   && [ -f "$SRC_DIR/$SVC_NAME-backup.timer" ]; then
  WITH_BACKUP=1
fi

if [ "$DRY_RUN" -eq 0 ] && [ -x "$SRC_DIR/$BIN_NAME" ]; then
  VER=$("$SRC_DIR/$BIN_NAME" version 2>/dev/null || true)
  [ -n "$VER" ] && ok "版本：$VER"
fi

# ---- 系统用户 ----

step "准备运行用户"
if id -u "$SVC_USER" >/dev/null 2>&1; then
  SVC_GROUP=$(id -gn "$SVC_USER")
  ok "用户 $SVC_USER 已存在（主组 $SVC_GROUP）"
else
  run useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$SVC_USER"
  SVC_GROUP=$SVC_USER
  ok "已创建系统用户 $SVC_USER"
fi

# ---- 二进制 ----

step "安装主程序"
run install -d -m 0755 "$BIN_DIR"
run install -m 0755 "$SRC_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME"
ok "$BIN_DIR/$BIN_NAME"

if [ "$WITH_BACKUP" -eq 1 ]; then
  run install -m 0755 "$SRC_DIR/backup.sh" "$BIN_DIR/homehub-backup.sh"
  ok "$BIN_DIR/homehub-backup.sh"
fi

# ---- 配置 ----

step "配置文件"
run install -d -m 0750 "$CONF_DIR"
if [ -f "$CONF_DIR/config.json" ]; then
  ok "保留已有配置（$CONF_DIR/config.json）"
  NEW_CONFIG=0
else
  # 用实际主组而不是写死的 $SVC_USER：用户可能是手工建的，
  # 主组未必与用户名同名，`-g homehub` 会直接失败。
  run install -m 0640 -o root -g "$SVC_GROUP" \
    "$SRC_DIR/config.example.json" "$CONF_DIR/config.json"
  ok "已写入配置模板（$CONF_DIR/config.json）"
  NEW_CONFIG=1
fi

[ -d "$DATA_DIR" ] || run install -d -o "$SVC_USER" -g "$SVC_GROUP" -m 0750 "$DATA_DIR"
ok "数据目录 $DATA_DIR"

# ---- systemd ----

step "安装 systemd 单元"
UNIT_DIR=/etc/systemd/system
run install -m 0644 "$SRC_DIR/$SVC_NAME.service" "$UNIT_DIR/$SVC_NAME.service"
ok "$UNIT_DIR/$SVC_NAME.service"

if [ "$WITH_BACKUP" -eq 1 ]; then
  run install -m 0644 "$SRC_DIR/$SVC_NAME-backup.service" \
    "$SRC_DIR/$SVC_NAME-backup.timer" "$UNIT_DIR/"
  run install -d -o "$SVC_USER" -g "$SVC_GROUP" -m 0750 "$BACKUP_DIR"
  ok "$UNIT_DIR/$SVC_NAME-backup.service + .timer"
fi

run systemctl daemon-reload
ok "systemctl daemon-reload"

# ---- 询问 ----

step "服务启停"
ENABLE_NOW=0
START_NOW=0

if ask "开机自动启动？" y; then ENABLE_NOW=1; fi
if ask "现在立即启动？" y; then START_NOW=1; fi

if [ "$ENABLE_NOW" -eq 1 ]; then
  run systemctl enable "$SVC_NAME"
  [ "$DRY_RUN" -eq 1 ] && ok "将设置开机自启" || ok "已设置开机自启"
else
  warn "未设置开机自启（之后可执行 systemctl enable $SVC_NAME）"
fi

if [ "$START_NOW" -eq 1 ]; then
  run systemctl restart "$SVC_NAME"
  [ "$DRY_RUN" -eq 1 ] && ok "将启动 $SVC_NAME" || ok "已启动 $SVC_NAME"
else
  warn "未启动（之后可执行 systemctl start $SVC_NAME）"
fi

if [ "$WITH_BACKUP" -eq 1 ]; then
  if [ "$ENABLE_NOW" -eq 1 ]; then
    run systemctl enable --now "$SVC_NAME-backup.timer"
    [ "$DRY_RUN" -eq 1 ] && ok "将启用每日备份定时器（03:00，保留 7 份）" \
      || ok "每日备份定时器已启用（03:00，保留 7 份）"
  else
    warn "备份定时器未启用（之后可执行 systemctl enable --now $SVC_NAME-backup.timer）"
  fi
fi

# ---- 健康检查 ----

if [ "$START_NOW" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
  step "等待健康检查"
  ADDR=$(sed -n 's/.*"addr"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONF_DIR/config.json" | head -n1)
  ADDR=${ADDR:-127.0.0.1:8080}
  case "$ADDR" in
    0.0.0.0:*) ADDR="127.0.0.1:${ADDR#*:}" ;;
    "[::]:"*)  ADDR="127.0.0.1:${ADDR##*:}" ;;
  esac

  HEALTHY=0
  for _ in $(seq 1 20); do
    if command -v curl >/dev/null 2>&1; then
      # 必须校验响应内容：只看端口有没有响应的话，8080 被别的服务占着
      # 时脚本会误报成功，而 hhe 其实根本没起来。
      body=$(curl -fsS --max-time 2 "http://$ADDR/healthz" 2>/dev/null || true)
      case "$body" in
        *'"status":"ok"'*) HEALTHY=1; break ;;
      esac
    else
      systemctl is-active --quiet "$SVC_NAME" && { HEALTHY=1; break; }
    fi
    sleep 1
  done

  if [ "$HEALTHY" -eq 1 ]; then
    ok "服务就绪（http://$ADDR/healthz）"
  else
    printf '\n'
    systemctl --no-pager --lines=15 status "$SVC_NAME" || true
    die "健康检查超时：$ADDR/healthz 未返回预期的 {\"status\":\"ok\"}
    常见原因：配置里的 server.addr 与实际不符、端口被占用、或启动失败。
    排查：journalctl -u $SVC_NAME -n 50"
  fi
fi

# ---- 收尾 ----

step "完成"

if [ "$NEW_CONFIG" -eq 1 ]; then
  warn "配置来自模板，**必须修改**：$CONF_DIR/config.json"
  printf '       %s至少设置 server.public_url 与 auth.hub.public_key_path（宿主对接用）%s\n' \
    "$C_DIM" "$C_END"
fi

PUBLIC_URL=$(sed -n 's/.*"public_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONF_DIR/config.json" 2>/dev/null | head -n1)
case "$PUBLIC_URL" in
  *example.com*) warn "server.public_url 仍是示例值：$PUBLIC_URL" ;;
esac

cat <<EOF
    ${C_DIM}查看状态${C_END}   systemctl status $SVC_NAME
    ${C_DIM}跟踪日志${C_END}   journalctl -u $SVC_NAME -f
    ${C_DIM}生成配对码${C_END} sudo -u $SVC_USER $BIN_DIR/$BIN_NAME enroll \\
                       -config $CONF_DIR/config.json -name "my-browser"
    ${C_DIM}重启服务${C_END}   systemctl restart $SVC_NAME

    ${C_DIM}注意：进程只处理 SIGINT/SIGTERM，不支持 reload。改完配置请 restart。${C_END}
EOF
