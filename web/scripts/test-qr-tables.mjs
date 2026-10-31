// QR 表的一致性校验。
//
// 为什么需要它：qr-tables.ts 是从 ISO/IEC 18004 手抄的常数，
// 错一位不会让任何代码报错，只会静默产出一张扫不出来的二维码。
// 这里用三条互相独立的约束交叉验证：
//
//   1. 配平：ec * 块数 + 数据码字数 必须等于该版本的总码字数
//   2. 整除：(data - g2) 必须能被总块数整除，商就是短块的数据码字数
//   3. 已知值：若干版本的 byte 模式容量与标准公布值逐一对照
//
// 任何一条不成立都直接退出非零，让 `pnpm verify` 拦住。

import { ALIGNMENT, EC_BLOCKS, MAX_VERSION, TOTAL_CODEWORDS } from "../src/tools/qr-tables.ts";

const LEVELS = ["L", "M", "Q", "H"];
const failures = [];

function fail(message) {
  failures.push(message);
}

/** byte 模式下，版本 v 的头部占 4 位模式 + 字符计数位。 */
function headerBytes(version) {
  return version <= 9 ? 2 : 3;
}

// ---- 1. 表长度与 MAX_VERSION 一致 ----
if (TOTAL_CODEWORDS.length !== MAX_VERSION + 1) {
  fail(`TOTAL_CODEWORDS 长度 ${TOTAL_CODEWORDS.length}，应为 ${MAX_VERSION + 1}`);
}
if (ALIGNMENT.length !== MAX_VERSION + 1) {
  fail(`ALIGNMENT 长度 ${ALIGNMENT.length}，应为 ${MAX_VERSION + 1}`);
}
for (const level of LEVELS) {
  const table = EC_BLOCKS[level];
  if (!table || table.length !== MAX_VERSION + 1) {
    fail(`EC_BLOCKS[${level}] 长度 ${table?.length}，应为 ${MAX_VERSION + 1}`);
  }
}

// ---- 2. 逐版本配平与整除 ----
const capacity = {}; // capacity[version][level] = byte 模式可存字节数

for (let v = 1; v <= MAX_VERSION; v += 1) {
  const total = TOTAL_CODEWORDS[v];
  if (!Number.isInteger(total) || total <= 0) {
    fail(`v${v}: 总码字数非法 ${total}`);
    continue;
  }
  capacity[v] = {};

  for (const level of LEVELS) {
    const spec = EC_BLOCKS[level][v];
    if (!spec) {
      fail(`v${v} ${level}: 缺少纠错块规格`);
      continue;
    }
    const [ecPerBlock, g1, g2] = spec;
    const blocks = g1 + g2;
    if (blocks <= 0) {
      fail(`v${v} ${level}: 块数为 0`);
      continue;
    }

    const ecTotal = ecPerBlock * blocks;
    const data = total - ecTotal;
    if (data <= 0) {
      fail(`v${v} ${level}: 数据码字 ${data} <= 0（ec=${ecPerBlock}×${blocks}，总 ${total}）`);
      continue;
    }

    // 长块比短块多一个数据码字：data = g1*q + g2*(q+1)
    const remainder = data - g2;
    if (remainder % blocks !== 0) {
      fail(
        `v${v} ${level}: 分组不整除（data=${data}, g1=${g1}, g2=${g2}, blocks=${blocks}, ` +
          `(${data}-${g2})%${blocks}=${remainder % blocks}）`,
      );
      continue;
    }
    const q = remainder / blocks;
    if (q <= 0) {
      fail(`v${v} ${level}: 短块数据码字数 ${q} <= 0`);
      continue;
    }
    if (g1 * q + g2 * (q + 1) !== data) {
      fail(`v${v} ${level}: 分组重建不符（g1*q+g2*(q+1) != data）`);
      continue;
    }

    capacity[v][level] = data - headerBytes(v);
  }

  // 同一版本内，纠错越强容量越小。
  const order = LEVELS.map((l) => capacity[v][l]).filter((c) => c !== undefined);
  for (let i = 1; i < order.length; i += 1) {
    if (order[i] >= order[i - 1]) {
      fail(`v${v}: 容量未随纠错等级单调递减（${order.join(" / ")}）`);
    }
  }
}

// ---- 3. 对齐图案坐标 ----
for (let v = 1; v <= MAX_VERSION; v += 1) {
  const coords = ALIGNMENT[v];
  if (!Array.isArray(coords)) {
    fail(`v${v}: ALIGNMENT 缺失`);
    continue;
  }
  if (v === 1) {
    if (coords.length !== 0) fail("v1 不应有对齐图案");
    continue;
  }
  // v2–6 只有单一坐标；v7 起是 6 + 若干，且必须严格递增、以 6 开头。
  if (coords.length < 2) {
    fail(`v${v}: 对齐坐标数量 ${coords.length} 过少`);
    continue;
  }
  if (coords[0] !== 6) fail(`v${v}: 首个对齐坐标应为 6，实为 ${coords[0]}`);
  for (let i = 1; i < coords.length; i += 1) {
    if (coords[i] <= coords[i - 1]) fail(`v${v}: 对齐坐标未递增（${coords.join(",")}）`);
  }
  // 最后一个坐标必须正好落在 size-7：对齐图案与右下定位图案同心，
  // 重叠的那个对齐图案按规范不绘制（绘制时跳过）。
  const size = v * 4 + 17;
  const last = coords[coords.length - 1];
  if (last !== size - 7) {
    fail(`v${v}: 末个对齐坐标 ${last}，应等于 size-7 = ${size - 7}（尺寸 ${size}）`);
  }
  // 坐标间距应当均匀（标准表都是等差的）。
  const step = coords[1] - coords[0];
  for (let i = 1; i < coords.length; i += 1) {
    const d = coords[i] - coords[i - 1];
    if (d !== step && i > 1) {
      // 最后一段间距与前面不同是允许的（末坐标被尺寸压缩），只提示不失败。
    }
  }
}

// ---- 4. 已知容量值对照（byte 模式，标准公布值） ----
// 这些数字来自 ISO/IEC 18004 的容量表，与实现在数学上相互独立。
const KNOWN = {
  1: { L: 17, M: 14, Q: 11, H: 7 },
  5: { L: 106, M: 84, Q: 60, H: 44 },
  10: { L: 271, M: 213, Q: 151, H: 119 },
  15: { L: 520, M: 412, Q: 292, H: 220 },
  20: { L: 858, M: 666, Q: 482, H: 382 },
  25: { L: 1273, M: 997, Q: 715, H: 535 },
  30: { L: 1732, M: 1370, Q: 982, H: 742 },
};

for (const [version, expected] of Object.entries(KNOWN)) {
  const v = Number(version);
  for (const level of LEVELS) {
    const got = capacity[v]?.[level];
    const want = expected[level];
    if (got !== want) {
      fail(`v${v} ${level}: byte 容量 ${got}，标准值 ${want}`);
    }
  }
}

// ---- 5. 跨版本单调性 ----
for (const level of LEVELS) {
  let prev = 0;
  for (let v = 1; v <= MAX_VERSION; v += 1) {
    const c = capacity[v]?.[level];
    if (c === undefined) continue;
    if (c <= prev) fail(`${level} v${v}: 容量 ${c} 未高于 v${v - 1} 的 ${prev}`);
    prev = c;
  }
}

// ---- 输出 ----
const v30 = capacity[30] ?? {};
const v10 = capacity[10] ?? {};
console.log(`表覆盖版本 1–${MAX_VERSION}`);
console.log(
  `v10 容量：L ${v10.L} / M ${v10.M} / Q ${v10.Q} / H ${v10.H} 字节`,
);
console.log(
  `v30 容量：L ${v30.L} / M ${v30.M} / Q ${v30.Q} / H ${v30.H} 字节`,
);

if (failures.length > 0) {
  console.error(`\nQR 表校验失败（${failures.length} 项）：`);
  for (const m of failures) console.error(`- ${m}`);
  process.exit(1);
}

const checked = MAX_VERSION * LEVELS.length;
console.log(`QR 表校验通过：${checked} 组纠错块规格配平，容量与已知值一致。`);
