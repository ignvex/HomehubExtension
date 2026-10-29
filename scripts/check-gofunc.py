#!/usr/bin/env python3
"""检查超过 80 行的 Go 函数。"""

import pathlib
import re
import sys

LIMIT = 80


def main() -> int:
    bad = []
    for p in sorted(pathlib.Path(".").rglob("*.go")):
        if "node_modules" in p.parts or p.name.endswith("_test.go"):
            continue
        lines = p.read_text(encoding="utf-8").split("\n")
        i = 0
        while i < len(lines):
            m = re.match(r"^func\s+(\([^)]*\)\s*)?(\w+)", lines[i])
            if not m:
                i += 1
                continue
            start, name = i, m.group(2)
            depth, started, j = 0, False, i
            while j < len(lines):
                depth += lines[j].count("{") - lines[j].count("}")
                if "{" in lines[j]:
                    started = True
                if started and depth == 0:
                    break
                j += 1
            length = j - start + 1
            if length > LIMIT:
                bad.append(f"  {p}:{start + 1}  {name}  {length} 行")
            i = j + 1
    if bad:
        print("\n".join(bad))
        print(f"共 {len(bad)} 个函数超过 {LIMIT} 行")
        return 1
    print("  OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
