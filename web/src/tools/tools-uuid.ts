// UUID 生成：v4 与 v7。
//
// v4：128 位随机，不含时间信息，不可排序。
// v7：前 48 位是毫秒时间戳，按字典序即按生成时间排序——
//     数据库主键用它比 v4 友好得多（B-tree 插入不会随机跳页）。
//
// 不用 crypto.randomUUID()：它只给 v4，没有 v7；而且非安全上下文
// （局域网 http 访问）下不可用，而这正是本项目的典型部署方式。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";

const HEX: readonly string[] = Array.from({ length: 256 }, (_, i) =>
  i.toString(16).padStart(2, "0"),
);

function randomBytes(n: number): Uint8Array {
  const bytes = new Uint8Array(n);
  crypto.getRandomValues(bytes);
  return bytes;
}

/** 把 16 字节格式化成 8-4-4-4-12。 */
function format(bytes: Uint8Array): string {
  const hex: string[] = [];
  for (const b of bytes) hex.push(HEX[b] ?? "00");
  const s = hex.join("");
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
}

function uuidV4(): string {
  const bytes = randomBytes(16);
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40; // version 4
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80; // variant RFC 4122
  return format(bytes);
}

// 同一毫秒内生成多个 v7 时，用 12 位计数器保证有序且不重复。
let lastMs = 0;
let counter = 0;

function uuidV7(): string {
  const now = Date.now();
  if (now === lastMs) {
    counter += 1;
    // 计数器溢出说明同一毫秒内生成超过 4096 个——退回 v4，
    // 宁可短暂失序也不要产生重复值。
    if (counter > 0x0fff) return uuidV4();
  } else {
    lastMs = now;
    counter = 0;
  }

  const bytes = randomBytes(16);

  // 48 位毫秒时间戳（大端）
  bytes[0] = Math.floor(now / 2 ** 40) & 0xff;
  bytes[1] = Math.floor(now / 2 ** 32) & 0xff;
  bytes[2] = Math.floor(now / 2 ** 24) & 0xff;
  bytes[3] = Math.floor(now / 2 ** 16) & 0xff;
  bytes[4] = Math.floor(now / 2 ** 8) & 0xff;
  bytes[5] = now & 0xff;

  bytes[6] = 0x70 | ((counter >> 8) & 0x0f); // version 7 + rand_a 高位
  bytes[7] = counter & 0xff;
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80; // variant

  return format(bytes);
}

export const factory: ToolFactory = (ctx) => {
  const countInput = h("input", {
    class: "input tool-count",
    attrs: { type: "number", min: "1", max: "100", value: "5" },
  }) as HTMLInputElement;

  const version = h("select", { class: "input" }) as HTMLSelectElement;
  for (const [value, label] of [
    ["v7", "v7（时间有序，推荐）"],
    ["v4", "v4（纯随机）"],
    ["both", "同时生成 v4 与 v7"],
  ] as const) {
    version.append(h("option", { attrs: { value } }, label));
  }

  const upper = h("input", { attrs: { type: "checkbox" } }) as HTMLInputElement;
  const noDash = h("input", { attrs: { type: "checkbox" } }) as HTMLInputElement;

  const output = h("textarea", {
    class: "input mono",
    attrs: {
      rows: "14",
      readonly: "readonly",
      placeholder: "点「生成」",
      spellcheck: "false",
    },
  }) as HTMLTextAreaElement;

  const generate = (): void => {
    const raw = Number(countInput.value);
    const n = Number.isFinite(raw) ? Math.min(Math.max(Math.trunc(raw), 1), 100) : 5;
    countInput.value = String(n);

    const lines: string[] = [];
    for (let i = 0; i < n; i += 1) {
      if (version.value === "v4") lines.push(uuidV4());
      else if (version.value === "v7") lines.push(uuidV7());
      else {
        lines.push(uuidV4());
        lines.push(uuidV7());
      }
    }

    let text = lines.join("\n");
    if (noDash.checked) text = text.replace(/-/gu, "");
    if (upper.checked) text = text.toUpperCase();
    output.value = text;
  };

  const generateBtn = h(
    "button",
    { class: "btn btn--primary", attrs: { type: "button" } },
    "生成",
  );
  generateBtn.addEventListener("click", generate);

  for (const el of [version, countInput, upper, noDash]) {
    el.addEventListener("change", generate);
  }

  const copyBtn = h("button", { class: "btn", attrs: { type: "button" } }, "复制全部");
  copyBtn.addEventListener("click", () => {
    if (output.value === "") {
      ctx.toast.show("还没有生成内容", "error");
      return;
    }
    void navigator.clipboard
      .writeText(output.value)
      .then(() => ctx.toast.show("已复制", "success"))
      .catch(() => ctx.toast.show("复制失败，请手动选中", "error"));
  });

  const element = h(
    "div",
    { class: "tool-body" },
    h(
      "div",
      { class: "tool-row" },
      h("label", { class: "tool-inline" }, h("span", {}, "数量"), countInput),
      h("label", { class: "tool-inline" }, h("span", {}, "版本"), version),
      h("label", { class: "tool-check" }, noDash, h("span", {}, "去连字符")),
      h("label", { class: "tool-check" }, upper, h("span", {}, "大写")),
      generateBtn,
      copyBtn,
    ),
    output,
  );

  generate();
  return { element };
};

export default factory;
