#!/usr/bin/env bash
# HomehubExtension 一体化运维脚本
#
# 安装包目录里执行（或直接运行单文件 .run）：
#   sudo ./install.sh install            全新安装（默认子命令）
#   sudo ./install.sh update             更新安装：保留配置与数据
#   sudo ./install.sh update --rollback  回滚到上一个二进制
#   sudo ./install.sh uninstall          卸载，保留配置与数据
#   sudo ./install.sh uninstall --purge  连配置、数据与运行用户一起删
#   sudo ./install.sh restart            重启服务
#   sudo ./install.sh start|stop|status|logs|enable|disable
#   sudo ./install.sh key import         导入宿主公钥（PEM 或单行 base64）
#   sudo ./install.sh key show           查看已配置公钥的指纹
#        ./install.sh key normalize      只校验并规范化一段公钥，不改系统
#   sudo ./install.sh doctor             一次自检：版本、服务、配置、公钥、健康
#        ./install.sh version            打印已安装版本
#
# 安装后脚本自身会装到 /usr/bin/hhe，之后上面的维护命令都可以直接写
#   sudo hhe update
#   sudo hhe key import --file homehub.pub
#
# 目录模式需要同目录存在：
#   homehub-extension、hhe.service、config.example.json（必需）
#   backup.sh、hhe-backup.service、hhe-backup.timer（可选，一起装才启用备份）
# 单文件 .run 模式已把这些内容打包在尾部，不需要额外文件。
#
# 通用选项：
#   -y, --yes       全部按默认值回答，不交互
#   --dry-run       只打印将要执行的动作，不改动系统
#
# 为什么公钥要单独做一个子命令：
#   这个字段存的是公钥「内容」，不是路径。手改 config.json 时最容易犯的错
#   是粘成 PEM body 的 DER base64（44 字节），它不是 Ed25519 公钥本体
#   （32 字节）。脚本在写入前就按运行时的同一套规则校验，并把结果规范化
#   成单行 base64，避免服务起不来之后才在日志里发现。

set -euo pipefail

BIN_NAME=homehub-extension
SVC_NAME=hhe
SVC_USER=homehub
BIN_DIR=/usr/bin
CONF_DIR=/etc/hhe
CONF_FILE=$CONF_DIR/config.json
DATA_DIR=/var/lib/hhe
BACKUP_DIR=/var/backups/hhe
UNIT_DIR=/etc/systemd/system
SELF_DEST=$BIN_DIR/hhe

# 标记分两段拼接：脚本正文里不能出现完整的标记字符串，否则单文件包
# 自检时会把自己的源码当成负载起点。
PAYLOAD_MARKER="__HHE_PAYLOAD_""BELOW__"

ORIG_ARGS=("$@")
CMD=install
KEY_SUB=show
OPT_YES=0
OPT_DRY=0
OPT_PURGE=0
OPT_NO_BACKUP=0
OPT_NO_START=0
OPT_ROLLBACK=0
OPT_FILE=""
OPT_KEY=""
OPT_FROM=""
OPT_FOLLOW=0
OPT_LINES=100
SRC_DIR=""
WITH_BACKUP=0
SELF_SCRIPT="${HHE_SELF_SCRIPT:-${BASH_SOURCE[0]}}"

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
note() { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_END"; }
die()  { printf '\n%serror:%s %s\n' "$C_ERR" "$C_END" "$*" >&2; exit 1; }

run() {
  if [ "$OPT_DRY" -eq 1 ]; then
    printf '    %s[dry-run]%s %s\n' "$C_DIM" "$C_END" "$*"
  else
    "$@"
  fi
}

have() { command -v "$1" >/dev/null 2>&1; }

# ask QUESTION DEFAULT(y|n) → 返回 0 表示 yes
ask() {
  local q="$1" def="$2" hint answer
  if [ "$OPT_YES" -eq 1 ] || [ ! -t 0 ]; then
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

# as_user USER CMD... → 以指定用户身份执行，用于验证权限而不是看权限位。
# runuser 来自 util-linux（Debian/Ubuntu 默认有）；没有就退回 su。
as_user() {
  local user="$1"; shift
  if have runuser; then
    runuser -u "$user" -- "$@"
  else
    su -s /bin/sh "$user" -c "$(printf '%q ' "$@")"
  fi
}

svc_group() {
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    id -gn "$SVC_USER"
  else
    printf '%s\n' "$SVC_USER"
  fi
}

need_root() { [ "$(id -u)" -eq 0 ] || die "需要 root：请用 sudo 执行"; }
need_systemd() {
  have systemctl || die "找不到 systemctl：本脚本面向 systemd 系统"
  [ -d /run/systemd/system ] || die "systemd 未在运行（容器里请改用镜像部署）"
}

is_installed() { [ -x "$BIN_DIR/$BIN_NAME" ] && [ -f "$UNIT_DIR/$SVC_NAME.service" ]; }

# ---- 单文件包自解压 ----

# 目录模式下直接返回；.run 模式下把尾部负载解到临时目录，
# 再用解出来的干净副本重新执行同一批参数。
maybe_unpack_payload() {
  [ -n "${HHE_PAYLOAD_DIR:-}" ] && return 0

  local marker_line tmp status
  marker_line=$(grep -a -m1 -n -- "$PAYLOAD_MARKER" "$0" 2>/dev/null | cut -d: -f1 || true)
  [ -n "$marker_line" ] || return 0

  tmp=$(mktemp -d)
  chmod 0700 "$tmp"
  mkdir -p "$tmp/payload"
  tail -n +"$((marker_line + 1))" "$0" > "$tmp/payload.tar.gz"
  tar xzf "$tmp/payload.tar.gz" -C "$tmp/payload" || {
    rm -rf "$tmp"
    die "单文件包的负载解压失败（文件可能不完整，请重新下载）"
  }
  # 干净副本：只保留标记之前的脚本正文，不含二进制负载。
  sed -n "1,$((marker_line - 1))p" "$0" > "$tmp/install.sh"
  chmod 0700 "$tmp/install.sh"

  export HHE_PAYLOAD_DIR="$tmp/payload"
  export HHE_SELF_SCRIPT="$tmp/install.sh"

  set +e
  bash "$tmp/install.sh" "${ORIG_ARGS[@]}"
  status=$?
  set -e
  rm -rf "$tmp"
  exit "$status"
}

# ---- JSON 读写 ----
#
# 只用 python3 或 jq：两者都没有时宁可让用户手改，也不要用 sed 去
# 拼 JSON——一个转义写错，服务下次启动就卡在配置解析上。

have_json_tool() { have python3 || have jq; }

json_get_str() { # FILE KEY_PATH
  local file=$1 path=$2
  [ -f "$file" ] || return 1
  if have python3; then
    python3 - "$file" "$path" <<'PY'
import json,sys
try:
    with open(sys.argv[1], encoding='utf-8') as f:
        cur = json.load(f)
except Exception:
    sys.exit(1)
for part in sys.argv[2].split('.'):
    if not isinstance(cur, dict) or part not in cur:
        sys.exit(0)
    cur = cur[part]
if isinstance(cur, str):
    sys.stdout.write(cur)
PY
  elif have jq; then
    jq -r 'getpath($p|split(".")) // "" | if type=="string" then . else "" end' \
      --arg p "$path" "$file"
  else
    return 1
  fi
}

json_set_str() { # FILE KEY_PATH VALUE
  local file=$1 path=$2 value=$3 tmp
  tmp=$(mktemp "$(dirname "$file")/.config.XXXXXX")
  if have python3; then
    python3 - "$file" "$path" "$value" "$tmp" <<'PY'
import json,sys
src, path, value, dst = sys.argv[1:5]
with open(src, encoding='utf-8') as f:
    cfg = json.load(f)
cur = cfg
parts = path.split('.')
for part in parts[:-1]:
    nxt = cur.get(part)
    if not isinstance(nxt, dict):
        nxt = {}
        cur[part] = nxt
    cur = nxt
cur[parts[-1]] = value
with open(dst, 'w', encoding='utf-8') as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)
    f.write('\n')
PY
  elif have jq; then
    jq --arg p "$path" --arg v "$value" 'setpath($p|split("."); $v)' "$file" > "$tmp"
  else
    rm -f "$tmp"
    return 1
  fi
  chmod 0640 "$tmp"
  chown root:"$(svc_group)" "$tmp" 2>/dev/null || true
  mv -f "$tmp" "$file"
}

# ---- 公钥规范化 ----

key_error() {
  printf '    %serror:%s %s\n' "$C_ERR" "$C_END" "$*" >&2
  return 1
}

# normalize_public_key: stdin → stdout，输出单行 base64（raw 32 字节）。
#
# 接受 PKIX PEM、raw Ed25519 PEM、32 字节公钥的 base64，以及
# PKIX DER 的 base64（44 字节）——最后一种是最常见的误粘形态，
# 但它的语义是「PEM body」，不是一个完整公钥，所以只取末尾 32 字节
# 并确认前缀是 Ed25519 的 PKIX 结构。其余长度一律拒绝。
normalize_public_key() {
  local input compact decoded_len prefix
  input=$(cat)

  if printf '%s' "$input" | grep -q -- '-----BEGIN'; then
    compact=$(printf '%s' "$input" | grep -v -- '-----' | tr -d ' \t\r\n')
  else
    compact=$(printf '%s' "$input" | tr -d ' \t\r\n')
  fi

  if [ -z "$compact" ]; then
    key_error "输入为空：需要 PKIX PEM 或单行 base64"
    return 1
  fi

  if ! decoded_len=$(printf '%s' "$compact" | base64 -d 2>/dev/null | wc -c); then
    key_error "不是合法的 base64。粘贴时要包含完整的公钥内容，不要带引号或多余字符"
    return 1
  fi
  decoded_len=$(printf '%s' "$decoded_len" | tr -d ' ')

  case "$decoded_len" in
    32)
      printf '%s\n' "$compact"
      ;;
    44)
      # PKIX Ed25519 SubjectPublicKeyInfo 的固定前缀（base64 前 16 字符，
      # 对应 DER 头 302a300506032b6570032100）。
      prefix=$(printf '%s' "$compact" | cut -c1-16)
      if [ "$prefix" != "MCowBQYDK2VwAyEA" ]; then
        key_error "这串 base64 解出 44 字节，但不是 Ed25519 的 PKIX 公钥
    需要的是 32 字节 raw 公钥的 base64，或整段 PKIX PEM"
        return 1
      fi
      printf '%s' "$compact" | base64 -d | tail -c 32 | base64 | tr -d '\n'
      printf '\n'
      ;;
    *)
      key_error "这串 base64 解出 ${decoded_len} 字节，Ed25519 公钥应当是 32 字节
    常见原因：粘的是 PEM body 那段 DER（44 字节），或者粘了别的东西
    从 .key 生成正确的一行：
      openssl pkey -in hub.key -pubout -outform DER | tail -c 32 | base64 -w0; echo"
      return 1
      ;;
  esac
}

key_fingerprint() { # 单行 base64 → SHA-256 前 8 字节 hex（与宿主 fingerprintKey 一致）
  printf '%s' "$1" | base64 -d | sha256sum | cut -c1-16
}

read_key_input() {
  if [ -n "$OPT_FILE" ]; then
    [ -f "$OPT_FILE" ] || die "找不到公钥文件：$OPT_FILE"
    cat "$OPT_FILE"
  elif [ -n "$OPT_KEY" ]; then
    printf '%s\n' "$OPT_KEY"
  elif [ ! -t 0 ]; then
    cat
  else
    local first rest
    printf '    粘贴公钥（PKIX PEM 或单行 base64）。PEM 读到 END 行，base64 粘贴后按 Ctrl-D：\n' >&2
    IFS= read -r first || true
    if [[ "$first" == -----BEGIN* ]]; then
      printf '%s\n' "$first"
      while IFS= read -r rest; do
        printf '%s\n' "$rest"
        [[ "$rest" == -----END* ]] && break
      done
    else
      printf '%s\n' "$first"
    fi
  fi
}

# ---- 服务辅助 ----

health_addr() {
  local addr
  addr=$(json_get_str "$CONF_FILE" server.addr 2>/dev/null || true)
  if [ -z "$addr" ]; then
    addr=$(sed -n 's/.*"addr"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONF_FILE" 2>/dev/null | head -n1)
  fi
  addr=${addr:-127.0.0.1:8080}
  case "$addr" in
    0.0.0.0:*) addr="127.0.0.1:${addr#*:}" ;;
    "[::]:"*)  addr="127.0.0.1:${addr##*:}" ;;
    :*)        addr="127.0.0.1${addr}" ;;
  esac
  printf '%s\n' "$addr"
}

# wait_healthy [尝试次数]
#
# 只看端口有没有响应会误判：8080 被别的服务占着时脚本会报成功，
# 而 hhe 其实根本没起来。所以校验响应体。
wait_healthy() {
  local tries=${1:-30} i body addr
  addr=$(health_addr)
  if ! have curl; then
    systemctl is-active --quiet "$SVC_NAME"
    return
  fi
  for i in $(seq 1 "$tries"); do
    body=$(curl -fsS --max-time 2 "http://$addr/healthz" 2>/dev/null || true)
    case "$body" in
      *'"status":"ok"'*) return 0 ;;
    esac
    systemctl is-active --quiet "$SVC_NAME" || return 1
    sleep 1
  done
  return 1
}

print_failure() {
  printf '\n'
  systemctl --no-pager --lines=20 status "$SVC_NAME" || true
  printf '\n'
  note "排查：journalctl -u $SVC_NAME -n 50 --no-pager"
}

check_legacy_key_field() {
  [ -f "$CONF_FILE" ] || return 0
  if grep -q '"public_key_path"' "$CONF_FILE" 2>/dev/null; then
    warn "配置里还有已删除的 public_key_path 字段，服务会拒绝启动"
    note "改用：sudo hhe key import --file <公钥文件>"
    return 1
  fi
  return 0
}

# ---- 安装 ----

ensure_payload() {
  [ -f "$SRC_DIR/$BIN_NAME" ] || die "找不到 $BIN_NAME
    目录模式：把二进制与脚本放在同一目录，或先跑 make build 生成 dist/
    单文件模式：重新下载完整的 .run（当前文件缺少负载）"
  [ -f "$SRC_DIR/$SVC_NAME.service" ] || die "找不到 $SVC_NAME.service（systemd 单元）"
  [ -f "$SRC_DIR/config.example.json" ] || die "找不到 config.example.json（配置模板）"
}

detect_backup_bundle() {
  WITH_BACKUP=0
  if [ "$OPT_NO_BACKUP" -eq 0 ] && [ -f "$SRC_DIR/backup.sh" ] \
     && [ -f "$SRC_DIR/$SVC_NAME-backup.service" ] \
     && [ -f "$SRC_DIR/$SVC_NAME-backup.timer" ]; then
    WITH_BACKUP=1
  fi
}

prepare_user() {
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    ok "用户 $SVC_USER 已存在（主组 $(id -gn "$SVC_USER")）"
  else
    run useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$SVC_USER"
    ok "已创建系统用户 $SVC_USER"
  fi
}

install_self() {
  # 先写临时文件再 mv：脚本可能正在执行（update 时会覆盖自己），
  # install 直接截断正在运行的文件在某些文件系统上会让 bash 读到空洞。
  run install -m 0755 "$SELF_SCRIPT" "$SELF_DEST.new"
  run mv -f "$SELF_DEST.new" "$SELF_DEST"
  ok "$SELF_DEST（维护命令入口）"
}

prepare_dirs() {
  local group
  group=$(svc_group)
  # 目录的组必须是服务用户的主组。只写 `install -d -m 0750` 会建成
  # root:root，而 0750 对 other 不给任何权限——homehub 用户属于 other，
  # 连目录都进不去，随后读 config.json 就是 permission denied。
  # 文件本身 0640 root:homehub 明明可读，卡住的是父目录的搜索位。
  run install -d -o root -g "$group" -m 0750 "$CONF_DIR"
  # install -d 幂等：目录已存在时也会纠正权限与属主。
  # 不写成 `[ -d ... ] ||`：那样会跳过纠正，一个手工建错的
  # 数据目录就永远不会被修好。
  run install -d -o "$SVC_USER" -g "$group" -m 0750 "$DATA_DIR"
}

# 配置已经写好后才能做的检查。与建目录分开：写 config.json 之前
# 目录必须存在，而「服务用户能读配置」在文件写出之前无从谈起。
check_permissions() {
  local group
  group=$(svc_group)
  if [ "$OPT_DRY" -eq 1 ]; then return 0; fi

  if as_user "$SVC_USER" test -w "$DATA_DIR" 2>/dev/null; then
    ok "服务用户 $SVC_USER 可写数据目录"
  else
    ls -ld "$DATA_DIR" 2>/dev/null || true
    die "服务用户 $SVC_USER 写不了 $DATA_DIR
    修复：chown -R $SVC_USER:$group $DATA_DIR"
  fi

  # 以服务用户的身份实际读一次配置。
  #
  # 光看权限位不够：目录可能是 0750 root:root（服务用户属于 other），
  # 也可能是 ACL 或挂载选项在挡。这类问题只会在 ExecStartPre 阶段
  # 炸成一句 "permission denied"，跟配置文件本身的权限毫无关系——
  # stat() 只需要路径上每一级目录的搜索位。
  if as_user "$SVC_USER" test -r "$CONF_FILE" 2>/dev/null; then
    ok "服务用户 $SVC_USER 可读配置"
  else
    printf '\n'
    namei -l "$CONF_FILE" 2>/dev/null || ls -ld "$CONF_DIR" "$CONF_FILE"
    die "服务用户 $SVC_USER 读不到 $CONF_FILE
    注意：stat() 只需要路径上每一级目录的搜索位（x），不需要文件本身的读权限。
    报 permission denied 时先看上面每一级目录的权限，而不是文件的。
    修复：chown root:$group $CONF_DIR && chmod 0750 $CONF_DIR"
  fi
}

install_units() {
  run install -m 0644 "$SRC_DIR/$SVC_NAME.service" "$UNIT_DIR/$SVC_NAME.service"
  ok "$UNIT_DIR/$SVC_NAME.service"

  if [ "$WITH_BACKUP" -eq 1 ]; then
    run install -m 0755 "$SRC_DIR/backup.sh" "$BIN_DIR/homehub-backup.sh"
    run install -m 0644 "$SRC_DIR/$SVC_NAME-backup.service" \
      "$SRC_DIR/$SVC_NAME-backup.timer" "$UNIT_DIR/"
    run install -d -o "$SVC_USER" -g "$(svc_group)" -m 0750 "$BACKUP_DIR"
    ok "$BIN_DIR/homehub-backup.sh + 备份单元"
  fi

  run systemctl daemon-reload
  ok "systemctl daemon-reload"
}

cmd_install() {
  need_root
  need_systemd
  SRC_DIR=$(resolve_src_dir)
  ensure_payload

  # 重跑安装包等于更新：保留配置与数据，走带备份与回滚的 update。
  # 反过来把 update 当 install 跑则会重建 .prev 之前的状态，没得回滚。
  if is_installed; then
    step "检测到已安装的 $SVC_NAME"
    note "按「更新」处理：保留配置与数据，替换二进制与单元"
    cmd_update
    return
  fi

  detect_backup_bundle

  step "检查环境"
  ok "root 权限"
  ok "systemd 可用"
  ok "$BIN_NAME 存在（$SRC_DIR）"
  if [ "$OPT_DRY" -eq 0 ] && [ -x "$SRC_DIR/$BIN_NAME" ]; then
    local ver
    ver=$("$SRC_DIR/$BIN_NAME" version 2>/dev/null || true)
    [ -n "$ver" ] && ok "版本：$ver"
  fi

  step "准备运行用户"
  prepare_user

  step "安装主程序"
  run install -d -m 0755 "$BIN_DIR"
  run install -m 0755 "$SRC_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME"
  ok "$BIN_DIR/$BIN_NAME"
  install_self

  step "配置文件"
  prepare_dirs
  local new_config=0
  if [ -f "$CONF_FILE" ]; then
    ok "保留已有配置（$CONF_FILE）"
  else
    # 用实际主组而不是写死的 $SVC_USER：用户可能是手工建的，
    # 主组未必与用户名同名，`-g homehub` 会直接失败。
    run install -m 0640 -o root -g "$(svc_group)" \
      "$SRC_DIR/config.example.json" "$CONF_FILE"
    ok "已写入配置模板（$CONF_FILE）"
    new_config=1
  fi
  check_permissions

  step "安装 systemd 单元"
  install_units

  step "服务启停"
  local enable_now=0 start_now=0
  if ask "开机自动启动？" y; then enable_now=1; fi
  if [ "$OPT_NO_START" -eq 0 ] && ask "现在立即启动？" y; then start_now=1; fi

  if [ "$enable_now" -eq 1 ]; then
    run systemctl enable "$SVC_NAME"
    [ "$OPT_DRY" -eq 1 ] && ok "将设置开机自启" || ok "已设置开机自启"
  else
    warn "未设置开机自启（之后可执行 systemctl enable $SVC_NAME）"
  fi

  if [ "$start_now" -eq 1 ]; then
    run systemctl restart "$SVC_NAME"
    [ "$OPT_DRY" -eq 1 ] && ok "将启动 $SVC_NAME" || ok "已启动 $SVC_NAME"
  else
    warn "未启动（之后可执行 systemctl start $SVC_NAME）"
  fi

  if [ "$WITH_BACKUP" -eq 1 ] && [ "$enable_now" -eq 1 ]; then
    run systemctl enable --now "$SVC_NAME-backup.timer"
    [ "$OPT_DRY" -eq 1 ] && ok "将启用每日备份定时器（03:00，保留 7 份）" \
      || ok "每日备份定时器已启用（03:00，保留 7 份）"
  elif [ "$WITH_BACKUP" -eq 1 ]; then
    warn "备份定时器未启用（之后可执行 systemctl enable --now $SVC_NAME-backup.timer）"
  fi

  if [ "$start_now" -eq 1 ] && [ "$OPT_DRY" -eq 0 ]; then
    step "等待健康检查"
    if wait_healthy 20; then
      ok "服务就绪（http://$(health_addr)/healthz）"
    else
      print_failure
      die "健康检查超时：/healthz 未返回预期的 {\"status\":\"ok\"}
    常见原因：配置里的 server.addr 与实际不符、端口被占用、或启动失败。"
    fi
  fi

  finish_install "$new_config"
}

finish_install() {
  local new_config=$1 public_url
  step "完成"

  if [ "$new_config" -eq 1 ]; then
    warn "配置来自模板，**必须修改**：$CONF_FILE"
    note "至少设置 server.public_url；宿主对接还要导入公钥（见下）"
  fi

  public_url=$(json_get_str "$CONF_FILE" server.public_url 2>/dev/null || true)
  case "$public_url" in
    *example.com*) warn "server.public_url 仍是示例值：$public_url" ;;
  esac

  check_legacy_key_field || true

  local key b64
  key=$(json_get_str "$CONF_FILE" auth.hub.public_key 2>/dev/null || true)
  if [ -n "$key" ]; then
    if b64=$(printf '%s' "$key" | normalize_public_key 2>/dev/null); then
      ok "宿主公钥已配置（指纹 $(key_fingerprint "$b64")）"
    else
      warn "配置里的公钥无法解析，签名通道不会启用"
      note "重新导入：sudo hhe key import --file homehub.pub"
    fi
  else
    warn "宿主公钥未配置：/hub/*、/fetch、/avatar 会返回 503"
    note "导入：sudo hhe key import --file homehub.pub"
  fi

  cat <<EOF

    ${C_DIM}查看状态${C_END}   systemctl status $SVC_NAME
    ${C_DIM}跟踪日志${C_END}   journalctl -u $SVC_NAME -f
    ${C_DIM}导入公钥${C_END}   sudo hhe key import --file homehub.pub
    ${C_DIM}查看指纹${C_END}   sudo hhe key show
    ${C_DIM}生成配对码${C_END} sudo -u $SVC_USER $BIN_DIR/$BIN_NAME enroll \\
                       -config $CONF_FILE -name "my-browser"
    ${C_DIM}更新安装${C_END}   sudo hhe update
    ${C_DIM}卸载${C_END}       sudo hhe uninstall

    ${C_DIM}注意：进程只处理 SIGINT/SIGTERM，不支持 reload。改完配置请 restart。${C_END}
EOF
}

# ---- 更新 ----

cmd_update() {
  need_root
  need_systemd
  is_installed || die "还没有安装：先执行 sudo $0 install"

  local prev="$BIN_DIR/$BIN_NAME.prev"

  if [ "$OPT_ROLLBACK" -eq 1 ]; then
    [ -x "$prev" ] || die "没有可回滚的二进制（$prev）"
    step "回滚主程序"
    run install -m 0755 "$prev" "$BIN_DIR/$BIN_NAME"
    ok "已恢复上一个二进制"
    run systemctl restart "$SVC_NAME"
    if [ "$OPT_DRY" -eq 0 ]; then
      wait_healthy 30 || { print_failure; die "回滚后健康检查仍失败"; }
      ok "服务就绪"
    fi
    return 0
  fi

  SRC_DIR=$(resolve_src_dir)
  if [ ! -f "$SRC_DIR/$BIN_NAME" ]; then
    die "找不到安装包
    已安装的 hhe 自己不带新版本，更新时要提供安装件：
      sudo bash /path/to/hhe-installer-<ver>-linux-amd64.run install
    或先把新版 dist/ 放到某个目录，再指定：
      sudo hhe update --from /path/to/dist"
  fi
  ensure_payload
  detect_backup_bundle

  step "检查更新包"
  local new_ver old_ver
  old_ver=$("$BIN_DIR/$BIN_NAME" version 2>/dev/null || echo unknown)
  new_ver=$("$SRC_DIR/$BIN_NAME" version 2>/dev/null || echo unknown)
  ok "当前：$old_ver"
  ok "目标：$new_ver"

  if [ "$old_ver" = "$new_ver" ] && [ "$OPT_YES" -eq 0 ]; then
    warn "版本相同，可能只是在重装"
  fi

  step "备份"
  run install -m 0755 "$BIN_DIR/$BIN_NAME" "$prev"
  ok "二进制备份到 $prev"

  local stamp backup_conf
  stamp=$(date +%Y%m%d-%H%M%S)
  backup_conf="$CONF_FILE.bak-$stamp"
  run install -m 0640 -o root -g "$(svc_group)" "$CONF_FILE" "$backup_conf"
  ok "配置备份到 $backup_conf"

  step "安装新版本"
  run install -m 0755 "$SRC_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME"
  ok "$BIN_DIR/$BIN_NAME"
  install_self
  install_units

  step "重启并验证"
  run systemctl restart "$SVC_NAME"
  if [ "$OPT_DRY" -eq 1 ]; then
    ok "将重启 $SVC_NAME"
    return 0
  fi

  if wait_healthy 30; then
    ok "服务就绪（$(health_addr)/healthz）"
  else
    warn "新版本启动失败，自动回滚"
    print_failure
    install -m 0755 "$prev" "$BIN_DIR/$BIN_NAME"
    systemctl restart "$SVC_NAME" || true
    if wait_healthy 30; then
      warn "已回滚到 $old_ver，服务恢复"
    else
      print_failure
      die "回滚后仍不健康：请检查配置与日志"
    fi
    die "更新中止：新版本未通过健康检查，已回滚"
  fi

  check_legacy_key_field || true

  step "完成"
  ok "$old_ver → $new_ver"
  note "回滚：sudo hhe update --rollback"
  note "配置备份：$backup_conf"
}

# ---- 卸载 ----

cmd_uninstall() {
  need_root
  need_systemd

  local purge=0
  [ "$OPT_PURGE" -eq 1 ] && purge=1

  if [ "$OPT_YES" -eq 0 ] && [ -t 0 ]; then
    if [ "$purge" -eq 1 ]; then
      warn "--purge 会删除配置、数据库、blob 与运行用户，无法恢复"
      ask "确认彻底卸载 $SVC_NAME？" n || die "已取消"
    else
      ask "确认卸载 $SVC_NAME？配置与数据会保留" y || die "已取消"
    fi
  fi

  step "停止并禁用服务"
  run systemctl disable --now "$SVC_NAME" 2>/dev/null || true
  run systemctl disable --now "$SVC_NAME-backup.timer" 2>/dev/null || true
  ok "服务已停止"

  step "移除单元与程序"
  run rm -f "$UNIT_DIR/$SVC_NAME.service" \
    "$UNIT_DIR/$SVC_NAME-backup.service" "$UNIT_DIR/$SVC_NAME-backup.timer"
  run systemctl daemon-reload
  ok "systemd 单元已移除"

  run rm -f "$BIN_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME.prev" "$BIN_DIR/homehub-backup.sh"
  ok "已移除 $BIN_DIR 下的程序"

  if [ "$purge" -eq 1 ]; then
    step "删除配置与数据"
    run rm -rf "$CONF_DIR" "$DATA_DIR" "$BACKUP_DIR"
    ok "已删除 $CONF_DIR、$DATA_DIR、$BACKUP_DIR"
    if id -u "$SVC_USER" >/dev/null 2>&1; then
      run userdel "$SVC_USER" 2>/dev/null || run userdel -f "$SVC_USER" 2>/dev/null || true
      ok "已删除用户 $SVC_USER"
    fi
    warn "公钥、配对设备与已抓取的条目都随数据一起删除了"
  else
    step "保留的内容"
    note "配置：$CONF_DIR"
    note "数据：$DATA_DIR（数据库、blob）"
    note "备份：$BACKUP_DIR"
    note "彻底删除：sudo bash <安装包> uninstall --purge"
  fi

  # 脚本自己在最后删：bash 已经把正文读进内存，之后不再读 $0，
  # 但 usage() 依赖 $0，所以这必须是最后一步。
  run rm -f "$SELF_DEST"
  step "完成"
  ok "已卸载 $SVC_NAME"
}

# ---- 公钥 ----

cmd_key() {
  local sub=${KEY_SUB:-show}
  case "$sub" in
    normalize) key_normalize ;;
    show)      key_show ;;
    import)    key_import ;;
    *) die "未知的 key 子命令：$sub（可用：import、show、normalize）" ;;
  esac
}

# 只校验并规范化，不写任何文件。不需要 root，可以用来先确认粘贴的内容。
key_normalize() {
  local b64
  b64=$(read_key_input | normalize_public_key) || exit 1
  printf '%s\n' "$b64"
  printf '%s指纹%s %s\n' "$C_DIM" "$C_END" "$(key_fingerprint "$b64")" >&2
}

key_show() {
  [ -f "$CONF_FILE" ] || die "找不到配置：$CONF_FILE"
  local raw b64
  raw=$(json_get_str "$CONF_FILE" auth.hub.public_key 2>/dev/null || true)
  if [ -z "$raw" ]; then
    warn "未配置 auth.hub.public_key"
    note "导入：sudo hhe key import --file homehub.pub"
    return 1
  fi
  if ! b64=$(printf '%s' "$raw" | normalize_public_key 2>/dev/null); then
    warn "配置里的公钥无法解析（服务应该已经拒绝启用签名通道）"
    note "重新导入：sudo hhe key import --file homehub.pub"
    return 1
  fi
  ok "公钥指纹 $(key_fingerprint "$b64")"
  note "共 ${#b64} 字符的 base64：${b64:0:12}…（完整内容在 $CONF_FILE）"
  if systemctl is-active --quiet "$SVC_NAME" 2>/dev/null; then
    note "服务运行中；换公钥后需要 restart 才生效"
  fi
}

key_import() {
  need_root
  [ -f "$CONF_FILE" ] || die "找不到配置：$CONF_FILE（先执行 install）"
  have_json_tool || die "需要 python3 或 jq 来安全地修改 config.json
    两者都没有时请手动编辑：把下面这行写进 auth.hub.public_key（值用单行 base64）"

  local b64 fp backup stamp
  b64=$(read_key_input | normalize_public_key) || die "公钥校验失败，配置未改动"
  fp=$(key_fingerprint "$b64")

  # 与旧值相同就不必重启：减少一次无谓的服务抖动。
  local current
  current=$(json_get_str "$CONF_FILE" auth.hub.public_key 2>/dev/null || true)
  if [ -n "$current" ]; then
    if [ "$(printf '%s' "$current" | normalize_public_key 2>/dev/null || true)" = "$b64" ]; then
      ok "公钥未变化（指纹 $fp）"
      return 0
    fi
  fi

  step "写入公钥"
  stamp=$(date +%Y%m%d-%H%M%S)
  backup="$CONF_FILE.bak-$stamp"
  run install -m 0640 -o root -g "$(svc_group)" "$CONF_FILE" "$backup"
  ok "配置备份 $backup"

  run json_set_str "$CONF_FILE" auth.hub.public_key "$b64"
  ok "auth.hub.public_key 已更新（指纹 $fp）"

  # 用服务自己的迁移入口做一次加载校验：它走的就是 ExecStartPre 那条路径，
  # 能通过就意味着服务能读到这份配置。失败则立刻回滚，不把坏配置留给重启。
  if [ "$OPT_DRY" -eq 0 ] && [ -x "$BIN_DIR/$BIN_NAME" ]; then
    if ! as_user "$SVC_USER" "$BIN_DIR/$BIN_NAME" migrate -config "$CONF_FILE" >/dev/null 2>&1; then
      warn "新配置未通过加载校验，回滚"
      install -m 0640 -o root -g "$(svc_group)" "$backup" "$CONF_FILE"
      die "公钥写入失败，已恢复原配置。用 hhe key normalize 先验证内容"
    fi
    ok "配置加载校验通过"
  fi

  if systemctl is-active --quiet "$SVC_NAME" 2>/dev/null; then
    if ask "重启 $SVC_NAME 让公钥生效？" y; then
      run systemctl restart "$SVC_NAME"
      if [ "$OPT_DRY" -eq 0 ]; then
        if wait_healthy 30; then
          ok "服务就绪，签名通道应已启用"
        else
          print_failure
          die "重启后健康检查失败"
        fi
      fi
    else
      warn "公钥已写入，但需要 systemctl restart $SVC_NAME 才生效"
    fi
  fi

  step "完成"
  note "指纹 $fp；宿主设置页显示的指纹应当与它一致"
  note "在宿主点「重新探测」，fetch_available 与 avatar_available 应为 true"
}

# ---- 服务维护 ----

cmd_service() {
  local action=$1

  # status 与 logs 只需要读权限：普通用户也应该能看一眼服务状态。
  case "$action" in
    status|logs) ;;
    *) need_root ;;
  esac

  case "$action" in
    start|stop|restart)
      need_systemd
      run systemctl "$action" "$SVC_NAME"
      if [ "$OPT_DRY" -eq 0 ] && [ "$action" != stop ]; then
        if wait_healthy 30; then
          ok "服务就绪（$(health_addr)/healthz）"
        else
          print_failure
          die "$action 后健康检查失败"
        fi
      fi
      ;;
    enable|disable)
      need_systemd
      run systemctl "$action" "$SVC_NAME"
      ok "$SVC_NAME 已$([ "$action" = enable ] && echo '设为开机自启' || echo '取消开机自启')"
      ;;
    status)
      need_systemd
      printf '%s\n' "--- 已安装版本 ---"
      "$BIN_DIR/$BIN_NAME" version 2>/dev/null || warn "未安装 $BIN_DIR/$BIN_NAME"
      printf '\n%s\n' "--- systemd ---"
      systemctl --no-pager --lines=10 status "$SVC_NAME" || true
      printf '\n%s\n' "--- 健康检查 ---"
      local addr body
      addr=$(health_addr)
      body=$(curl -fsS --max-time 2 "http://$addr/healthz" 2>/dev/null || true)
      case "$body" in
        *'"status":"ok"'*) ok "$addr/healthz" ;;
        *) warn "$addr/healthz 无响应" ;;
      esac
      printf '\n%s\n' "--- 公钥 ---"
      key_show || true
      ;;
    logs)
      need_systemd
      if [ "$OPT_FOLLOW" -eq 1 ]; then
        journalctl -u "$SVC_NAME" -n "$OPT_LINES" -f
      else
        journalctl -u "$SVC_NAME" -n "$OPT_LINES" --no-pager
      fi
      ;;
  esac
}

# ---- 自检 ----

cmd_doctor() {
  local problems=0 group addr body version

  step "程序"
  if is_installed; then
    version=$("$BIN_DIR/$BIN_NAME" version 2>/dev/null || echo unknown)
    ok "$version"
  else
    warn "未安装（缺少 $BIN_DIR/$BIN_NAME 或 $UNIT_DIR/$SVC_NAME.service）"
    problems=$((problems + 1))
  fi

  step "运行用户与目录"
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    group=$(id -gn "$SVC_USER")
    ok "用户 $SVC_USER（主组 $group）"
  else
    warn "用户 $SVC_USER 不存在"
    group=$SVC_USER
    problems=$((problems + 1))
  fi

  if [ -f "$CONF_FILE" ]; then
    ok "配置 $CONF_FILE"
    if as_user "$SVC_USER" test -r "$CONF_FILE" 2>/dev/null; then
      ok "服务用户可读配置"
    elif [ "$(id -u)" -eq 0 ]; then
      warn "服务用户读不到配置"
      namei -l "$CONF_FILE" 2>/dev/null || true
      problems=$((problems + 1))
    fi
  else
    warn "缺少配置 $CONF_FILE"
    problems=$((problems + 1))
  fi

  if [ -d "$DATA_DIR" ] && as_user "$SVC_USER" test -w "$DATA_DIR" 2>/dev/null; then
    ok "数据目录可写 $DATA_DIR"
  else
    warn "数据目录不可写或不存在：$DATA_DIR"
    problems=$((problems + 1))
  fi

  step "公钥"
  if check_legacy_key_field; then
    local raw b64
    raw=$(json_get_str "$CONF_FILE" auth.hub.public_key 2>/dev/null || true)
    if [ -z "$raw" ]; then
      warn "未配置 auth.hub.public_key（/hub/*、/fetch、/avatar 会 503）"
    elif b64=$(printf '%s' "$raw" | normalize_public_key 2>/dev/null); then
      ok "可解析，指纹 $(key_fingerprint "$b64")"
    else
      warn "公钥无法解析：签名通道不会启用"
      problems=$((problems + 1))
    fi
  else
    problems=$((problems + 1))
  fi

  step "服务"
  if have systemctl; then
    if systemctl is-active --quiet "$SVC_NAME"; then
      ok "$SVC_NAME 运行中"
      addr=$(health_addr)
      body=$(curl -fsS --max-time 2 "http://$addr/healthz" 2>/dev/null || true)
      case "$body" in
        *'"status":"ok"'*) ok "健康检查 $addr/healthz" ;;
        *) warn "健康检查无响应：$addr/healthz"; problems=$((problems + 1)) ;;
      esac
      body=$(curl -fsS --max-time 2 "http://$addr/readyz" 2>/dev/null || true)
      case "$body" in
        *'"ready":true'*) ok "就绪检查 /readyz" ;;
        *) warn "就绪检查未通过：/readyz"; problems=$((problems + 1)) ;;
      esac
    else
      warn "$SVC_NAME 未运行"
      problems=$((problems + 1))
    fi
  else
    warn "找不到 systemctl"
  fi

  step "磁盘"
  df -h "$DATA_DIR" 2>/dev/null | tail -n1 || warn "无法读取 $DATA_DIR 的磁盘占用"

  printf '\n'
  if [ "$problems" -eq 0 ]; then
    ok "没有发现问题"
    return 0
  fi
  warn "$problems 项需要处理（见上）"
  return 1
}

cmd_version() {
  if [ -x "$BIN_DIR/$BIN_NAME" ]; then
    "$BIN_DIR/$BIN_NAME" version
  else
    warn "未安装 $BIN_DIR/$BIN_NAME"
  fi
  printf 'installer %s\n' "$(sha256sum "$SELF_SCRIPT" 2>/dev/null | cut -c1-12 || echo unknown)"
}

resolve_src_dir() {
  if [ -n "${HHE_PAYLOAD_DIR:-}" ]; then
    printf '%s\n' "$HHE_PAYLOAD_DIR"
    return
  fi
  if [ -n "$OPT_FROM" ]; then
    [ -d "$OPT_FROM" ] || die "--from 需要一个目录（解包后的 dist/）：$OPT_FROM"
    (cd -- "$OPT_FROM" && pwd)
    return
  fi
  # 装到 /usr/bin 之后，脚本目录里只有 homehub-extension，没有 unit 与
  # 配置模板。要求两个文件都在，避免把 /usr/bin 误当成安装包目录。
  local here
  here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  if [ -f "$here/$BIN_NAME" ] && [ -f "$here/$SVC_NAME.service" ]; then
    printf '%s\n' "$here"
  elif [ -f "$PWD/$BIN_NAME" ] && [ -f "$PWD/$SVC_NAME.service" ]; then
    printf '%s\n' "$PWD"
  else
    printf '%s\n' "$PWD"
  fi
}

# ---- 解析 ----

case "${1:-}" in
  ""|-*) ;;
  *) CMD=$1; shift ;;
esac

if [ "$CMD" = key ]; then
  case "${1:-}" in
    import|show|normalize) KEY_SUB=$1; shift ;;
    "") ;;
    -*) ;;
    *) die "未知的 key 子命令：$1（可用：import、show、normalize）" ;;
  esac
fi

while [ $# -gt 0 ]; do
  case "$1" in
    -y|--yes)     OPT_YES=1 ;;
    --dry-run)    OPT_DRY=1 ;;
    --purge)      OPT_PURGE=1 ;;
    --no-backup)  OPT_NO_BACKUP=1 ;;
    --no-start)   OPT_NO_START=1 ;;
    --rollback)   OPT_ROLLBACK=1 ;;
    --from)       shift; [ $# -gt 0 ] || die "--from 需要一个目录"; OPT_FROM=$1 ;;
    --from=*)     OPT_FROM=${1#*=} ;;
    -f|--follow)  OPT_FOLLOW=1 ;;
    -)            : ;;  # 显式从 stdin 读（key import/normalize）
    -h|--help)    usage ;;
    --file)       shift; [ $# -gt 0 ] || die "--file 需要一个路径"; OPT_FILE=$1 ;;
    --file=*)     OPT_FILE=${1#*=} ;;
    --key)        shift; [ $# -gt 0 ] || die "--key 需要一个值"; OPT_KEY=$1 ;;
    --key=*)      OPT_KEY=${1#*=} ;;
    -n|--lines)   shift; [ $# -gt 0 ] || die "-n 需要一个数字"; OPT_LINES=$1 ;;
    --lines=*)    OPT_LINES=${1#*=} ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

case "$CMD" in
  install|setup)     maybe_unpack_payload; cmd_install ;;
  update|upgrade)    maybe_unpack_payload; cmd_update ;;
  uninstall|remove)  maybe_unpack_payload; cmd_uninstall ;;
  key)               cmd_key ;;
  restart|start|stop|status|logs|enable|disable)
                     cmd_service "$CMD" ;;
  doctor)            cmd_doctor ;;
  version)           cmd_version ;;
  help)              usage ;;
  *) die "未知子命令：$CMD（用 --help 查看用法）" ;;
esac

# 单文件包里，负载紧跟在脚本正文之后。没有这句的话，不需要解包的
# 子命令（key、doctor、version）执行完会继续往下读，把标记行和 tar
# 字节当成 shell 命令跑。目录模式下这行只是正常退出。
exit 0
