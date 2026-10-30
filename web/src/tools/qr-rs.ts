// Reed-Solomon 纠错码（GF(256)，本原多项式 0x11D）。
//
// QR 的纠错按块独立计算：一个块损坏不影响其他块。
// 这里只实现编码方向——解码是扫描器的事。

const GF_EXP = new Uint8Array(512);
const GF_LOG = new Uint8Array(256);

(function initGF(): void {
  let x = 1;
  for (let i = 0; i < 255; i += 1) {
    GF_EXP[i] = x;
    GF_LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  for (let i = 255; i < 512; i += 1) GF_EXP[i] = GF_EXP[i - 255] ?? 0;
})();

function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return GF_EXP[(GF_LOG[a] ?? 0) + (GF_LOG[b] ?? 0)] ?? 0;
}

/** 生成多项式 g(x) = ∏(x - α^i)，i = 0..degree-1。 */
function rsGenerator(degree: number): Uint8Array {
  let poly = new Uint8Array([1]);
  for (let i = 0; i < degree; i += 1) {
    const next = new Uint8Array(poly.length + 1);
    const alpha = GF_EXP[i] ?? 1;
    for (let j = 0; j < poly.length; j += 1) {
      next[j] = (next[j] ?? 0) ^ (poly[j] ?? 0);
      next[j + 1] = (next[j + 1] ?? 0) ^ gfMul(poly[j] ?? 0, alpha);
    }
    poly = next;
  }
  return poly;
}

/** 计算纠错码字。 */
export function rsEncode(data: Uint8Array, ecCount: number): Uint8Array {
  const gen = rsGenerator(ecCount);
  const result = new Uint8Array(ecCount);
  for (const byte of data) {
    const factor = byte ^ (result[0] ?? 0);
    result.copyWithin(0, 1);
    result[ecCount - 1] = 0;
    if (factor !== 0) {
      for (let i = 0; i < ecCount; i += 1) {
        result[i] = (result[i] ?? 0) ^ gfMul(gen[i + 1] ?? 0, factor);
      }
    }
  }
  return result;
}
