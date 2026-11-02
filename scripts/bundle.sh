#!/usr/bin/env bash
# 把安装件打成单个自包含脚本。
#
#   scripts/bundle.sh <version>                  → dist/hhe-installer-<ver>-linux-amd64.run（含二进制）
#   scripts/bundle.sh <version> --no-binary      → dist/hhe.sh（不含二进制，自己放）
#
# 结构是「脚本正文 + 标记行 + tar.gz 负载」。install.sh 启动时按标记
# 找到负载，解到临时目录，再用不带负载的干净副本继续执行——所以同一个
# 脚本既能从目录里跑，也能单文件跑，行为完全一致。
#
# 两种产物的区别只有一个：负载里有没有 homehub-extension。
#   .run  发给谁都能直接装，11MB
#   .sh   只有几十 KB，使用者把各自的二进制放在同一目录
#
# 不用 makeself：它要求目标机装 makeself 的运行时约定，而这里只需要
# 一段 tar 调用。

set -euo pipefail

VERSION=dev
WITH_BINARY=1
while [ $# -gt 0 ]; do
  case "$1" in
    --no-binary) WITH_BINARY=0 ;;
    --with-binary) WITH_BINARY=1 ;;
    -h|--help)
      sed -n '2,/^[^#]/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    -*) echo "error: 未知参数 $1" >&2; exit 1 ;;
    *) VERSION=$1 ;;
  esac
  shift
done

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
DIST=$ROOT/dist
# 与 scripts/install.sh 里的拼接结果保持一致。
MARKER="__HHE_PAYLOAD_""BELOW__"

if [ "$WITH_BINARY" -eq 1 ]; then
  OUT=$DIST/hhe-installer-${VERSION}-linux-amd64.run
else
  OUT=$DIST/hhe.sh
fi

# 自包含所需的资源。二进制是可选的那一个。
embedded=(
  hhe.service
  config.example.json
  backup.sh
  hhe-backup.service
  hhe-backup.timer
)
[ "$WITH_BINARY" -eq 1 ] && embedded=(homehub-extension "${embedded[@]}")

if [ "$WITH_BINARY" -eq 1 ]; then
  [ -x "$DIST/homehub-extension" ] || {
    echo "error: $DIST/homehub-extension 不存在，先跑 make build" >&2
    exit 1
  }
fi
for f in "${embedded[@]}"; do
  [ -f "$DIST/$f" ] || { echo "error: 缺少 dist/$f（先跑 make build）" >&2; exit 1; }
done
# 直接取仓库里的脚本，不用 dist/install.sh：后者是 make build 拷过去的
# 副本，改了源码却没重建时会悄悄打进旧版本。
INSTALL_SH=$ROOT/scripts/install.sh
[ -f "$INSTALL_SH" ] || { echo "error: 缺少 $INSTALL_SH" >&2; exit 1; }

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

for f in "${embedded[@]}"; do
  cp "$DIST/$f" "$staging/$f"
done

{
  cat "$INSTALL_SH"
  printf '\n%s\n' "$MARKER"
  tar czf - -C "$staging" .
} > "$OUT"

chmod 0755 "$OUT"

# 自检：负载能解开、必需文件都在。生成一个跑不起来的包比没有更糟。
payload_start=$(grep -a -m1 -n -- "$MARKER" "$OUT" | cut -d: -f1)
tail -n +"$((payload_start + 1))" "$OUT" > "$staging/verify.tar.gz"
tar tzf "$staging/verify.tar.gz" >/dev/null || {
  echo "error: 生成的包负载无法解压" >&2
  rm -f "$OUT"
  exit 1
}
for f in hhe.service config.example.json; do
  tar tzf "$staging/verify.tar.gz" | grep -qx "./$f" || {
    echo "error: 负载里缺少 $f" >&2
    rm -f "$OUT"
    exit 1
  }
done

size=$(du -h "$OUT" | cut -f1)
echo "==> $OUT"
echo "    $size，内嵌 $(tar tzf "$staging/verify.tar.gz" | grep -v "^\./$" | wc -l) 个文件"
echo
if [ "$WITH_BINARY" -eq 1 ]; then
  echo "部署（一个文件即可）："
  echo "  scp $OUT user@host:/tmp/"
  echo "  ssh user@host 'sudo bash /tmp/$(basename "$OUT") install'"
else
  echo "部署（脚本 + 你自己的二进制，放同一个目录）："
  echo "  scp $OUT dist/homehub-extension user@host:/tmp/hhe/"
  echo "  ssh user@host 'cd /tmp/hhe && sudo ./hhe.sh install'"
fi
