// QR 码编码（byte mode，版本 1–30）。
//
// 为什么自己写：项目零运行时依赖，而二维码库通常 20–40KB。
// 只实现文本/链接最常用的 range——byte mode 编码 + 版本 1–30
// （版本 30-H 可存 742 字节，L 可存 1732 字节）。
//
// 参考 ISO/IEC 18004。步骤：
//   1. 选版本：找能装下数据的最小版本
//   2. 数据编码：mode + 长度 + 数据 + 终止符 + 填充
//   3. 分块 + Reed-Solomon 纠错
//   4. 交织数据块与纠错块
//   5. 在矩阵里按之字形放置
//   6. 选最优掩码，写格式信息

import {
  ALIGNMENT,
  EC_BLOCKS,
  EC_FORMAT_BITS,
  MAX_VERSION,
  TOTAL_CODEWORDS,
  type ECLevel,
} from "./qr-tables";
import { rsEncode } from "./qr-rs";

export type { ECLevel };

// ---------------------------------------------------------------------------
// 数据编码
// ---------------------------------------------------------------------------

function utf8(text: string): Uint8Array {
  return new TextEncoder().encode(text);
}

/** 版本 1–9 用 8 位字符计数，10 起用 16 位。 */
function charCountBits(version: number): number {
  return version <= 9 ? 8 : 16;
}

/** 某版本某纠错等级下可用的数据码字数。 */
function dataCodewords(version: number, level: ECLevel): number {
  const spec = EC_BLOCKS[level][version];
  if (!spec) return 0;
  const [ecPerBlock, blocks1, blocks2] = spec;
  const total = TOTAL_CODEWORDS[version] ?? 0;
  const totalBlocks = blocks1 + blocks2;
  return total - ecPerBlock * totalBlocks;
}

/** 位流写入器。 */
class BitBuffer {
  private readonly bits: number[] = [];

  put(value: number, length: number): void {
    for (let i = length - 1; i >= 0; i -= 1) this.bits.push((value >>> i) & 1);
  }

  get length(): number {
    return this.bits.length;
  }

  toBytes(): Uint8Array {
    const out = new Uint8Array(Math.ceil(this.bits.length / 8));
    this.bits.forEach((bit, index) => {
      if (bit) out[index >> 3] = (out[index >> 3] ?? 0) | (0x80 >> (index & 7));
    });
    return out;
  }
}

/** 某纠错等级下 byte 模式能装的最大字节数。UI 用它显示剩余空间。 */
export function maxPayloadBytes(level: ECLevel): number {
  const headerBytes = Math.ceil((4 + charCountBits(MAX_VERSION)) / 8);
  return dataCodewords(MAX_VERSION, level) - headerBytes;
}

/** 估算容纳 payload 所需的最小版本。 */
function chooseVersion(byteLen: number, level: ECLevel): number {
  for (let v = 1; v <= MAX_VERSION; v += 1) {
    const capacityBits = dataCodewords(v, level) * 8;
    const needed = 4 + charCountBits(v) + byteLen * 8;
    if (needed <= capacityBits) return v;
  }
  throw new Error(
    `内容太长：${level} 级最多 ${maxPayloadBytes(level)} 字节，请缩短文本或降低纠错等级`,
  );
}

/** 编码成最终的数据码字序列（含填充）。 */
function encodeData(payload: Uint8Array, version: number, level: ECLevel): Uint8Array {
  const capacity = dataCodewords(version, level);
  const buffer = new BitBuffer();

  buffer.put(0b0100, 4); // byte mode
  buffer.put(payload.length, charCountBits(version));
  for (const byte of payload) buffer.put(byte, 8);

  // 终止符：最多 4 个 0
  const remaining = capacity * 8 - buffer.length;
  buffer.put(0, Math.min(4, remaining));

  // 补齐到字节边界
  while (buffer.length % 8 !== 0) buffer.put(0, 1);

  const bytes = Array.from(buffer.toBytes());
  // 交替填充 0xEC / 0x11（标准规定的填充字节）
  const padBytes = [0xec, 0x11];
  let padIndex = 0;
  while (bytes.length < capacity) {
    bytes.push(padBytes[padIndex % 2] ?? 0xec);
    padIndex += 1;
  }
  return new Uint8Array(bytes);
}

/**
 * 分块并交织。
 *
 * QR 的纠错是按块独立的：一个块损坏不会影响其他块。
 * 交织（交错排列）让同一块的码字在矩阵里分散开，
 * 这样一片污损不会打掉一整块的全部码字。
 */
function interleave(data: Uint8Array, version: number, level: ECLevel): Uint8Array {
  const spec = EC_BLOCKS[level][version];
  if (!spec) throw new Error("版本超出支持范围");
  const [ecPerBlock, blocks1, blocks2] = spec;
  const totalBlocks = blocks1 + blocks2;

  const totalData = dataCodewords(version, level);
  const shortBlockLen = Math.floor(totalData / totalBlocks);
  const numLong = totalData % totalBlocks;

  const dataBlocks: Uint8Array[] = [];
  const ecBlocks: Uint8Array[] = [];
  let offset = 0;

  for (let i = 0; i < totalBlocks; i += 1) {
    const len = shortBlockLen + (i >= totalBlocks - numLong ? 1 : 0);
    const block = data.subarray(offset, offset + len);
    offset += len;
    dataBlocks.push(block);
    ecBlocks.push(rsEncode(block, ecPerBlock));
  }

  const maxDataLen = shortBlockLen + (numLong > 0 ? 1 : 0);
  const out: number[] = [];

  for (let i = 0; i < maxDataLen; i += 1) {
    for (const block of dataBlocks) {
      if (i < block.length) out.push(block[i] ?? 0);
    }
  }
  for (let i = 0; i < ecPerBlock; i += 1) {
    for (const block of ecBlocks) {
      out.push(block[i] ?? 0);
    }
  }

  return new Uint8Array(out);
}

// ---------------------------------------------------------------------------
// 矩阵构建
// ---------------------------------------------------------------------------

export interface QRMatrix {
  size: number;
  /** modules[row][col] === true 表示深色。 */
  modules: boolean[][];
}

function createMatrix(size: number): (boolean | null)[][] {
  return Array.from({ length: size }, () => Array.from({ length: size }, () => null));
}

/** 画定位图案（三个角上的 7×7 回字）。 */
function placeFinder(matrix: (boolean | null)[][], row: number, col: number): void {
  for (let r = -1; r <= 7; r += 1) {
    for (let c = -1; c <= 7; c += 1) {
      const rr = row + r;
      const cc = col + c;
      if (rr < 0 || rr >= matrix.length || cc < 0 || cc >= matrix.length) continue;
      const isDark =
        (r >= 0 && r <= 6 && (c === 0 || c === 6)) ||
        (c >= 0 && c <= 6 && (r === 0 || r === 6)) ||
        (r >= 2 && r <= 4 && c >= 2 && c <= 4);
      matrix[rr]![cc] = isDark;
    }
  }
}

/** 画对齐图案（5×5 同心方）。 */
function placeAlignment(matrix: (boolean | null)[][], row: number, col: number): void {
  for (let r = -2; r <= 2; r += 1) {
    for (let c = -2; c <= 2; c += 1) {
      const rr = row + r;
      const cc = col + c;
      if (rr < 0 || rr >= matrix.length || cc < 0 || cc >= matrix.length) continue;
      if (matrix[rr]![cc] !== null) continue; // 与定位图案重叠时不覆盖
      const isDark = Math.max(Math.abs(r), Math.abs(c)) !== 1;
      matrix[rr]![cc] = isDark;
    }
  }
}

/** 画定时图案（第 6 行的水平与第 6 列的垂直黑白交替）。 */
function placeTiming(matrix: (boolean | null)[][]): void {
  const size = matrix.length;
  for (let i = 8; i < size - 8; i += 1) {
    const dark = i % 2 === 0;
    if (matrix[6]![i] === null) matrix[6]![i] = dark;
    if (matrix[i]![6] === null) matrix[i]![6] = dark;
  }
}

/** 保留格式信息区（稍后写入）。 */
function reserveFormatAreas(matrix: (boolean | null)[][]): void {
  const size = matrix.length;
  for (let i = 0; i < 9; i += 1) {
    if (matrix[8]![i] === null) matrix[8]![i] = false;
    if (matrix[i]![8] === null) matrix[i]![8] = false;
  }
  for (let i = 0; i < 8; i += 1) {
    if (matrix[8]![size - 1 - i] === null) matrix[8]![size - 1 - i] = false;
    if (matrix[size - 1 - i]![8] === null) matrix[size - 1 - i]![8] = false;
  }
  // 固定深色模块
  matrix[size - 8]![8] = true;
}

/**
 * 版本信息：版本号经 BCH(18,6) 编码，生成多项式 0x1F25。
 *
 * 版本 ≥ 7 必须写这两块 18 位信息。少了它，扫描器只能从矩阵尺寸
 * 猜版本——尺寸能唯一映射到版本，所以不少解码器会"侥幸读到"，
 * 但这是脱离标准的行为，容错余量被白白吃掉一半。
 */
function versionInfoBits(version: number): number {
  let rem = version;
  for (let i = 0; i < 12; i += 1) {
    rem = (rem << 1) ^ ((rem >> 11) * 0x1f25);
  }
  return ((version << 12) | rem) >>> 0;
}

/** 保留版本信息区，确保数据位与掩码都不碰它们。 */
function reserveVersionAreas(matrix: (boolean | null)[][], version: number): void {
  if (version < 7) return;
  const size = matrix.length;
  for (let i = 0; i < 6; i += 1) {
    for (let j = 0; j < 3; j += 1) {
      matrix[i]![size - 11 + j] = false;
      matrix[size - 11 + j]![i] = false;
    }
  }
}

/** 写入版本信息。位序从最高位开始，两块内容相同、方向不同。 */
function writeVersion(matrix: boolean[][], version: number): void {
  if (version < 7) return;
  const size = matrix.length;
  const bits = versionInfoBits(version);
  let index = 0;
  for (let i = 0; i < 6; i += 1) {
    for (let j = 0; j < 3; j += 1) {
      const bit = ((bits >> (17 - index)) & 1) === 1;
      index += 1;
      matrix[i]![size - 11 + j] = bit;
      matrix[size - 11 + j]![i] = bit;
    }
  }
}

/** 按之字形填充数据位。 */
function placeData(
  matrix: (boolean | null)[][],
  data: Uint8Array,
  reserved: boolean[][],
): void {
  const size = matrix.length;
  let bitIndex = 0;
  const totalBits = data.length * 8;

  const bitAt = (index: number): boolean => {
    if (index >= totalBits) return false; // 剩余位填 0
    const byte = data[index >> 3] ?? 0;
    return ((byte >> (7 - (index & 7))) & 1) === 1;
  };

  let upward = true;
  for (let col = size - 1; col > 0; col -= 2) {
    // 第 6 列是定时图案，跳过
    if (col === 6) col -= 1;
    for (let step = 0; step < size; step += 1) {
      const row = upward ? size - 1 - step : step;
      for (const c of [col, col - 1]) {
        if (c < 0) continue;
        if (reserved[row]![c]) continue;
        matrix[row]![c] = bitAt(bitIndex);
        bitIndex += 1;
      }
    }
    upward = !upward;
  }
}

/** 8 种掩码。标准里用这些公式逐模块决定是否翻转。 */
const MASKS: ReadonlyArray<(row: number, col: number) => boolean> = [
  (r, c) => (r + c) % 2 === 0,
  (r) => r % 2 === 0,
  (_r, c) => c % 3 === 0,
  (r, c) => (r + c) % 3 === 0,
  (r, c) => (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0,
  (r, c) => ((r * c) % 2) + ((r * c) % 3) === 0,
  (r, c) => (((r * c) % 2) + ((r * c) % 3)) % 2 === 0,
  (r, c) => (((r + c) % 2) + ((r * c) % 3)) % 2 === 0,
];

function applyMask(
  matrix: boolean[][],
  reserved: boolean[][],
  maskIndex: number,
): boolean[][] {
  const mask = MASKS[maskIndex];
  if (!mask) return matrix;
  const size = matrix.length;
  const out = matrix.map((row) => [...row]);
  for (let r = 0; r < size; r += 1) {
    for (let c = 0; c < size; c += 1) {
      if (reserved[r]![c]) continue;
      if (mask(r, c)) out[r]![c] = !out[r]![c];
    }
  }
  return out;
}

/** 格式信息：纠错等级 + 掩码，经 BCH(15,5) 编码后异或 0x5412。 */
function formatBits(level: ECLevel, maskIndex: number): number {
  const data = (EC_FORMAT_BITS[level] << 3) | maskIndex;
  let bits = data << 10;
  for (let i = 4; i >= 0; i -= 1) {
    if ((bits >> (i + 10)) & 1) bits ^= 0b10100110111 << i;
  }
  return ((data << 10) | bits) ^ 0b101010000010010;
}

function writeFormat(matrix: boolean[][], level: ECLevel, maskIndex: number): void {
  const size = matrix.length;
  const bits = formatBits(level, maskIndex);
  const bit = (i: number): boolean => ((bits >> i) & 1) === 1;

  // 两份副本，位序完全相同但布局不同。
  //
  // 布局容易搞错的地方：竖排写在 (row, 8)，横排写在 (8, col)。
  // 把这两者互换会得到一个「数据区全对、格式位全错」的码——
  // 扫描器能定位图案但读不出掩码，最终解不出内容。
  for (let i = 0; i < 15; i += 1) {
    const mod = bit(i);

    // 竖排（第 8 列）
    if (i < 6) matrix[i]![8] = mod;
    else if (i < 8) matrix[i + 1]![8] = mod;
    else matrix[size - 15 + i]![8] = mod;

    // 横排（第 8 行）
    if (i < 8) matrix[8]![size - i - 1] = mod;
    else if (i === 8) matrix[8]![7] = mod;
    else matrix[8]![15 - i - 1] = mod;
  }

  // 固定深色模块：右下角那个孤立的黑点。
  matrix[size - 8]![8] = true;
}

/** 掩码评分：罚分越低越好。四条规则见标准第 8.8.2 节。 */
function penalty(matrix: boolean[][]): number {
  const size = matrix.length;
  let score = 0;

  // 规则 1：同色连续 5 个以上
  const scanLine = (getter: (i: number, j: number) => boolean): void => {
    for (let i = 0; i < size; i += 1) {
      let run = 1;
      for (let j = 1; j < size; j += 1) {
        if (getter(i, j) === getter(i, j - 1)) {
          run += 1;
        } else {
          if (run >= 5) score += run - 2;
          run = 1;
        }
      }
      if (run >= 5) score += run - 2;
    }
  };
  scanLine((i, j) => matrix[i]![j] ?? false);
  scanLine((i, j) => matrix[j]![i] ?? false);

  // 规则 2：2×2 同色块
  for (let r = 0; r < size - 1; r += 1) {
    for (let c = 0; c < size - 1; c += 1) {
      const v = matrix[r]![c];
      if (
        v === matrix[r]![c + 1] &&
        v === matrix[r + 1]![c] &&
        v === matrix[r + 1]![c + 1]
      ) {
        score += 3;
      }
    }
  }

  // 规则 3：类似定位图案的 1:1:3:1:1 序列
  const pattern = "10111010000";
  const reverse = "00001011101";
  for (let i = 0; i < size; i += 1) {
    let rowStr = "";
    let colStr = "";
    for (let j = 0; j < size; j += 1) {
      rowStr += matrix[i]![j] ? "1" : "0";
      colStr += matrix[j]![i] ? "1" : "0";
    }
    for (let j = 0; j <= size - 11; j += 1) {
      const slice = rowStr.slice(j, j + 11);
      if (slice === pattern || slice === reverse) score += 40;
      const colSlice = colStr.slice(j, j + 11);
      if (colSlice === pattern || colSlice === reverse) score += 40;
    }
  }

  // 规则 4：深色比例偏离 50%。
  //
  // 用 ceil 而不是 floor：两者在「恰好落在 5% 边界」时会选出不同掩码。
  // 标准原文只要求「以 5% 为步长的倍数」，未规定取整方向；
  // 这里跟随主流实现（qrcode.js）的写法，因为它生成的码已被大量验证。
  let dark = 0;
  for (const row of matrix) for (const cell of row) if (cell) dark += 1;
  const percent = (dark * 100) / (size * size);
  const k = Math.abs(Math.ceil(percent / 5) - 10);
  score += k * 10;

  return score;
}

// ---------------------------------------------------------------------------
// 对外入口
// ---------------------------------------------------------------------------

export function encodeQR(text: string, level: ECLevel = "M"): QRMatrix {
  const payload = utf8(text);
  const version = chooseVersion(payload.length, level);
  const size = version * 4 + 17;

  // 1. 数据码字
  const data = encodeData(payload, version, level);
  const interleaved = interleave(data, version, level);

  // 2. 功能图案 + 保留区
  const nullable = createMatrix(size);
  placeFinder(nullable, 0, 0);
  placeFinder(nullable, 0, size - 7);
  placeFinder(nullable, size - 7, 0);
  placeTiming(nullable);
  const alignment = ALIGNMENT[version] ?? [];
  for (const row of alignment) {
    for (const col of alignment) {
      // 跳过与三个定位图案重叠的位置
      const nearFinder =
        (row <= 7 && col <= 7) ||
        (row <= 7 && col >= size - 8) ||
        (row >= size - 8 && col <= 7);
      if (nearFinder) continue;
      placeAlignment(nullable, row, col);
    }
  }
  reserveFormatAreas(nullable);
  reserveVersionAreas(nullable, version);

  const reserved = nullable.map((row) => row.map((v) => v !== null));

  // 3. 放数据
  placeData(nullable, interleaved, reserved);
  const filled = nullable.map((row) => row.map((v) => v === true));

  // 4. 选最优掩码
  let best = { index: 0, score: Infinity, matrix: filled };
  for (let maskIndex = 0; maskIndex < 8; maskIndex += 1) {
    const masked = applyMask(filled, reserved, maskIndex);
    writeFormat(masked, level, maskIndex);
    const score = penalty(masked);
    if (score < best.score) best = { index: maskIndex, score, matrix: masked };
  }

  // 5. 用最优掩码重画一次（上面循环里最后写入的格式位可能属于别的掩码）
  const finalMatrix = applyMask(filled, reserved, best.index);
  writeFormat(finalMatrix, level, best.index);
  writeVersion(finalMatrix, version);

  return { size, modules: finalMatrix };
}
