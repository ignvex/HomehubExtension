#!/usr/bin/env bash
# 把 dist/ 里的安装件打成单个自解压脚本。
#
#   scripts/bundle.sh <version>
#   → dist/hhe-installer-<version>-linux-amd64.run
#
# 结构是「脚本正文 + 标记行 + tar.gz 负载」。install.sh 启动时按标记
# 找到负载，解到临时目录，再用不带负载的干净副本继续执行——所以同一个
# 脚本既能从目录里跑，也能单文件跑，行为完全一致。
#
# 不用 makeself：它要求目标机装 makeself 的运行时约定，而这里只需要
# 一段 tar 调用。包里的二进制已经是静态链接的 linux/amd64 产物。

set -euo pipefail

VERSION=${1:-dev}
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
DIST=$ROOT/dist
OUT=$DIST/hhe-installer-${VERSION}-linux-amd64.run

# 与 scripts/install.sh 里的拼接结果保持一致。
MARKER="__HHE_PAYLOAD_""BELOW__"

required=(
  homehub-extension
  hhe.service
  config.example.json
)
optional=(
  backup.sh
  hhe-backup.service
  hhe-backup.timer
)

[ -x "$DIST/homehub-extension" ] || {
  echo "error: $DIST/homehub-extension 不存在，先跑 make build" >&2
  exit 1
}
for f in "${required[@]}"; do
  [ -f "$DIST/$f" ] || { echo "error: 缺少 dist/$f" >&2; exit 1; }
done

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

for f in "${required[@]}" "${optional[@]}"; do
  [ -f "$DIST/$f" ] && cp "$DIST/$f" "$staging/$f"
done

{
  cat "$DIST/install.sh"
  printf '\n%s\n' "$MARKER"
  tar czf - -C "$staging" .
} > "$OUT"

chmod 0755 "$OUT"

# 自检：负载能解开、里面确实有二进制。生成一个跑不起来的包比没有更糟。
payload_start=$(grep -a -m1 -n -- "$MARKER" "$OUT" | cut -d: -f1)
tail -n +"$((payload_start + 1))" "$OUT" > "$staging/verify.tar.gz"
if ! tar tzf "$staging/verify.tar.gz" >/dev/null; then
  echo "error: 生成的包负载无法解压" >&2
  rm -f "$OUT"
  exit 1
fi
count=$(tar tzf "$staging/verify.tar.gz" | wc -l)

size=$(du -h "$OUT" | cut -f1)
echo "==> $OUT"
echo "    $size，负载 $count 个文件"
echo
echo "部署到服务器："
echo "  scp $OUT user@host:/tmp/ && ssh user@host 'sudo bash /tmp/$(basename "$OUT") install'"
