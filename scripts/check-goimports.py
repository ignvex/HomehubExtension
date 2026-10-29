#!/usr/bin/env python3
"""检查 import 分组顺序：标准库 → 内部包 → 第三方库。

Golang Specification skill 的约定。
"""

import pathlib
import re
import sys


def classify(path: str) -> str:
    if "." not in path.split("/")[0]:
        return "std"
    if path.startswith("github.com/ignvex/HomehubExtension"):
        return "internal"
    return "third"


ORDER = {"std": 0, "internal": 1, "third": 2}


def main() -> int:
    bad = []
    for p in sorted(pathlib.Path(".").rglob("*.go")):
        if "node_modules" in p.parts:
            continue
        m = re.search(r"import \(\n(.*?)\n\)", p.read_text(encoding="utf-8"), re.S)
        if not m:
            continue

        seq = []
        for line in m.group(1).split("\n"):
            line = line.strip()
            if not line:
                continue
            mm = re.match(r'(?:\w+\s+)?"([^"]+)"', line)
            if mm:
                seq.append(classify(mm.group(1)))

        nums = [ORDER[g] for g in seq]
        if nums != sorted(nums):
            joined = " -> ".join(seq)
            bad.append("  %s: %s" % (p, joined))

    if bad:
        print("\n".join(bad))
        print("共 %d 个文件 import 分组顺序不合规" % len(bad))
        return 1
    print("  OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
