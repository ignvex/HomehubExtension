#!/usr/bin/env bash
# HomehubExtension 备份脚本
#
# 设计要点：
#   1. 数据库备份走二进制自带的 `backup` 子命令（内部用 VACUUM INTO），
#      不依赖系统装 sqlite3 CLI。直接 cp .db 会漏掉 WAL 里尚未合并的事务，
#      恢复出来的数据是旧的，而且整个过程不会报任何错。
#   2. blob 目录用 rsync --link-dest 做增量。媒体文件是内容寻址的，
#      地址变了内容必然变，所以未改动的文件只建硬链接，
#      7 份备份的实际占用约等于 1 份。
#   3. 数据库与 blob 必须来自同一时间点，否则会出现引用指向缺失文件。
#      所以两者在这一步里连续完成。
#
# 用法：
#   sudo install -m 0755 deploy/backup.sh /usr/bin/homehub-backup.sh
#   sudo install -m 0644 deploy/hhe-backup.{service,timer} /etc/systemd/system/
#   sudo install -d -o homehub -g homehub -m 0750 /var/backups/hhe
#   sudo systemctl daemon-reload && sudo systemctl enable --now hhe-backup.timer
#
# 调度交给 systemd timer（deploy/hhe-backup.timer），不用 cron：
# 日志直接进 journal 可以从 `systemctl status hhe-backup` 看，
# Persistent=true 能补跑关机期间错过的触发点，这些 cron 都要另写脚本解决。

set -euo pipefail

BIN="${HOMEHUB_BIN:-/usr/bin/homehub-extension}"
CONFIG="${HOMEHUB_CONFIG:-/etc/hhe/config.json}"
DATA_DIR="${HOMEHUB_DATA_DIR:-/var/lib/hhe}"
BACKUP_DIR="${HOMEHUB_BACKUP_DIR:-/var/backups/hhe}"
KEEP="${HOMEHUB_BACKUP_KEEP:-7}"

BLOBS="${DATA_DIR}/blobs"
STAMP="$(date +%Y-%m-%d)"
DEST="${BACKUP_DIR}/${STAMP}"

log() { printf '%s %s\n' "$(date -Is)" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

[ -x "$BIN" ] || die "找不到可执行文件: $BIN（用 HOMEHUB_BIN 指定）"
[ -r "$CONFIG" ] || die "找不到配置: $CONFIG（用 HOMEHUB_CONFIG 指定）"
[ -d "$BLOBS" ] || die "blob 目录不存在: $BLOBS"
command -v rsync >/dev/null || die "缺少 rsync"

# 同一天重复执行时覆盖当天的备份，而不是叠加。
mkdir -p "$DEST"

# ---- 1. 数据库 ----
log "备份数据库 → ${DEST}/homehub.db"
# 子命令内部写 .partial 再 rename，中断不会留下半个文件。
"$BIN" backup -config "$CONFIG" -out "$DEST" \
  || die "数据库备份失败"

# ---- 2. blob ----
#
# --link-dest 指向上一次成功备份：未改动的文件只建硬链接。
# 不加 --delete：目标是新建目录，本来就没有多余文件。
PREV="${BACKUP_DIR}/blobs-current"
LINK_ARG=()
if [ -d "$PREV" ]; then
  LINK_ARG=(--link-dest="$PREV")
fi

log "备份媒体文件 → ${DEST}/blobs"
mkdir -p "${DEST}/blobs"
rsync -a --numeric-ids "${LINK_ARG[@]}" "${BLOBS}/" "${DEST}/blobs/" \
  || die "rsync 失败"

# 记录「最近一次」，供下次 --link-dest 与人工排查。
ln -sfn "${DEST}/blobs" "$PREV"

# ---- 3. 清理 ----
#
# 只保留最近 N 份。日期目录名可直接按字典序比较，等价于时间序。
log "保留最近 ${KEEP} 份备份"
REMOVED=0
while IFS= read -r old; do
  [ -z "$old" ] && continue
  log "  删除 ${old}"
  rm -rf "${BACKUP_DIR:?}/${old}"
  REMOVED=$((REMOVED + 1))
done < <(
  find "$BACKUP_DIR" -maxdepth 1 -type d -name '20*' -printf '%f\n' \
    | sort -r \
    | tail -n +"$((KEEP + 1))"
)

DB_SIZE=$(du -h "${DEST}/homehub.db" | cut -f1)
BLOB_SIZE=$(du -sh "${DEST}/blobs" | cut -f1)
log "完成：${DEST}（数据库 ${DB_SIZE}，媒体 ${BLOB_SIZE}，清理 ${REMOVED} 份旧备份）"
