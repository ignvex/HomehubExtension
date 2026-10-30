// 哈希计算：MD5 / SHA-1 / SHA-256 / SHA-384 / SHA-512。
//
// SHA 系列用 WebCrypto，MD5 必须自己实现——WebCrypto 的
// digest() 不支持 MD5（它被当作已弃用的算法排除在规范外），
// 但「算一下文件的 MD5」是极常见的需求，不能没有。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  bytes as fmtBytes,
  copyButton,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textarea,
} from "./shared";

// ---------------------------------------------------------------------------
// MD5（RFC 1321）
// ---------------------------------------------------------------------------

/** 每轮左移位数。 */
const MD5_SHIFTS = [
  7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
  5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
  6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
] as const;

/** K[i] = floor(2^32 × abs(sin(i + 1)))，标准常量表。 */
const MD5_K = Array.from({ length: 64 }, (_, i) =>
  Math.floor(Math.abs(Math.sin(i + 1)) * 2 ** 32) >>> 0,
);

function md5(bytes: Uint8Array): string {
  const originalBits = bytes.length * 8;

  // 补齐：0x80 + 若干 0，使长度 ≡ 56 (mod 64)，再接 8 字节小端长度。
  const paddedLength = (((bytes.length + 8) >>> 6) + 1) << 6;
  const buffer = new Uint8Array(paddedLength);
  buffer.set(bytes);
  buffer[bytes.length] = 0x80;

  // 长度是 64 位小端；JS 的位运算只有 32 位，所以拆成低/高两个字。
  const view = new DataView(buffer.buffer);
  view.setUint32(paddedLength - 8, originalBits >>> 0, true);
  view.setUint32(paddedLength - 4, Math.floor(originalBits / 2 ** 32), true);

  let a0 = 0x67452301;
  let b0 = 0xefcdab89;
  let c0 = 0x98badcfe;
  let d0 = 0x10325476;

  const m = new Uint32Array(16);

  for (let offset = 0; offset < paddedLength; offset += 64) {
    for (let i = 0; i < 16; i += 1) m[i] = view.getUint32(offset + i * 4, true);

    let a = a0;
    let b = b0;
    let c = c0;
    let d = d0;

    for (let i = 0; i < 64; i += 1) {
      let f: number;
      let g: number;
      if (i < 16) {
        f = (b & c) | (~b & d);
        g = i;
      } else if (i < 32) {
        f = (d & b) | (~d & c);
        g = (5 * i + 1) % 16;
      } else if (i < 48) {
        f = b ^ c ^ d;
        g = (3 * i + 5) % 16;
      } else {
        f = c ^ (b | ~d);
        g = (7 * i) % 16;
      }

      const shift = MD5_SHIFTS[i] ?? 0;
      const k = MD5_K[i] ?? 0;
      const sum = (a + f + k + (m[g] ?? 0)) >>> 0;
      const rotated = ((sum << shift) | (sum >>> (32 - shift))) >>> 0;

      a = d;
      d = c;
      c = b;
      b = (b + rotated) >>> 0;
    }

    a0 = (a0 + a) >>> 0;
    b0 = (b0 + b) >>> 0;
    c0 = (c0 + c) >>> 0;
    d0 = (d0 + d) >>> 0;
  }

  // 输出小端字节序的十六进制。
  const out = new Uint8Array(16);
  const outView = new DataView(out.buffer);
  outView.setUint32(0, a0, true);
  outView.setUint32(4, b0, true);
  outView.setUint32(8, c0, true);
  outView.setUint32(12, d0, true);
  return [...out].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// ---------------------------------------------------------------------------

const ALGORITHMS: ReadonlyArray<readonly [string, string]> = [
  ["md5", "MD5"],
  ["SHA-1", "SHA-1"],
  ["SHA-256", "SHA-256"],
  ["SHA-384", "SHA-384"],
  ["SHA-512", "SHA-512"],
];

async function digest(algorithm: string, data: Uint8Array): Promise<string> {
  if (algorithm === "md5") return md5(data);
  const buf = await crypto.subtle.digest(algorithm, data as unknown as ArrayBuffer);
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export const factory: ToolFactory = (ctx) => {
  const mode = select([
    ["text", "文本"],
    ["file", "文件"],
  ]);
  const textInput = textarea("要计算摘要的文本", { rows: 8 });
  const fileInput = h("input", {
    class: "input",
    attrs: { type: "file", "aria-label": "选择文件" },
  }) as HTMLInputElement;
  const output = textarea("结果会显示在这里", { rows: 10, readonly: true });
  const error = errorLine();
  const statLine = stats();

  let fileBytes: Uint8Array | null = null;
  let fileName = "";
  let running = false;

  const run = async (): Promise<void> => {
    if (running) return;
    running = true;
    try {
      let data: Uint8Array;
      if (mode.value === "file") {
        if (!fileBytes) {
          output.value = "";
          showError(error, "");
          setStats(statLine, []);
          return;
        }
        data = fileBytes;
      } else {
        data = new TextEncoder().encode(textInput.value);
        if (data.length === 0) {
          output.value = "";
          showError(error, "");
          setStats(statLine, []);
          return;
        }
      }

      showError(error, "");
      const lines: string[] = [];
      for (const [algo, label] of ALGORITHMS) {
        const hex = await digest(algo, data);
        lines.push(`${label.padEnd(8)} ${hex}`);
      }
      output.value = lines.join("\n");

      setStats(statLine, [
        ["输入", `${fmtBytes(data.length)}`],
        ...(fileName ? ([["文件", fileName]] as const) : []),
      ]);
    } catch (e) {
      showError(error, e instanceof Error ? e.message : String(e));
    } finally {
      running = false;
    }
  };

  textInput.addEventListener("input", () => {
    void run();
  });
  mode.addEventListener("change", () => {
    const isFile = mode.value === "file";
    textInput.parentElement?.toggleAttribute("hidden", isFile);
    fileInput.parentElement?.toggleAttribute("hidden", !isFile);
    void run();
  });

  fileInput.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    if (!file) {
      fileBytes = null;
      fileName = "";
      void run();
      return;
    }
    fileName = file.name;
    const reader = new FileReader();
    reader.onload = () => {
      // FileReader 的 result 是 ArrayBuffer。
      fileBytes = new Uint8Array(reader.result as ArrayBuffer);
      void run();
    };
    reader.onerror = () => {
      showError(error, "读取文件失败");
    };
    reader.readAsArrayBuffer(file);
  });

  const element = h(
    "div",
    { class: "tool-body" },
    field("输入类型", mode),
    field("文本", textInput),
    field("文件", fileInput),
    actions(
      button(
        "计算",
        () => {
          void run();
        },
        "primary",
      ),
      copyButton(() => output.value, ctx.toast, "复制全部"),
    ),
    error,
    field("摘要", output),
    statLine,
  );

  // 初始隐藏文件选择。
  fileInput.parentElement?.setAttribute("hidden", "");

  return { element };
};

export default factory;
