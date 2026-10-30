// Base64 编解码。
//
// 不能直接用 btoa/atob 处理中文：它们只接受 Latin-1，遇到 UTF-8
// 多字节字符会抛 InvalidCharacterError。正确做法是先用 TextEncoder
// 转成字节，再逐字节拼成二进制串。
//
// 解码同理：atob 得到的是字节串，必须用 TextDecoder 还原成文本，
// 否则中文会变成乱码。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  bytes,
  copyButton,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textarea,
} from "./shared";

/** UTF-8 文本 → Base64。 */
function encodeBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = "";
  // 分块拼串：String.fromCharCode 的参数个数有上限，
  // 大文件一次性展开会 RangeError: Maximum call stack size exceeded。
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

/** Base64 → UTF-8 文本。 */
function decodeBase64(input: string): string {
  const binary = atob(input);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
}

/** 把 Base64 折成每行 76 字符，与常见工具的输出一致。 */
function wrap(text: string): string {
  if (text.length <= 76) return text;
  const lines: string[] = [];
  for (let i = 0; i < text.length; i += 76) lines.push(text.slice(i, i + 76));
  return lines.join("\n");
}

export const factory: ToolFactory = (ctx) => {
  const input = textarea("要编码的文本，或要解码的 Base64", { rows: 10 });
  const output = textarea("结果会显示在这里", { rows: 10, readonly: true });
  const mode = select([
    ["encode", "编码（文本 → Base64）"],
    ["decode", "解码（Base64 → 文本）"],
  ]);
  const error = errorLine();
  const statLine = stats();

  const run = (): void => {
    const text = input.value;
    if (text === "") {
      output.value = "";
      showError(error, "");
      setStats(statLine, []);
      return;
    }

    try {
      if (mode.value === "encode") {
        const result = wrap(encodeBase64(text));
        output.value = result;
        showError(error, "");
        setStats(statLine, [
          ["输入", `${bytes(new TextEncoder().encode(text).length)}`],
          ["Base64", bytes(result.length)],
        ]);
      } else {
        // 用户常从别处粘贴带换行的 Base64，先去掉所有空白。
        const cleaned = text.replace(/\s+/gu, "");
        const result = decodeBase64(cleaned);
        output.value = result;
        showError(error, "");
        setStats(statLine, [
          ["Base64", bytes(cleaned.length)],
          ["解码后", bytes(new TextEncoder().encode(result).length)],
        ]);
      }
    } catch (e) {
      output.value = "";
      setStats(statLine, []);
      if (e instanceof DOMException && e.name === "InvalidCharacterError") {
        showError(error, "不是合法的 Base64：包含非法字符或长度不正确");
      } else if (e instanceof TypeError) {
        showError(error, "解码结果不是合法的 UTF-8 文本（可能原本是二进制数据）");
      } else {
        showError(error, e instanceof Error ? e.message : String(e));
      }
    }
  };

  input.addEventListener("input", run);
  mode.addEventListener("change", run);

  const swapBtn = button("交换", () => {
    const previous = output.value;
    mode.value = mode.value === "encode" ? "decode" : "encode";
    input.value = previous;
    run();
    if (input.value !== "") input.focus();
  });

  const element = h(
    "div",
    { class: "tool-body" },
    h(
      "div",
      { class: "tool-grid-2" },
      field("输入", input),
      field("输出", output),
    ),
    actions(
      mode,
      swapBtn,
      button("清空", () => {
        input.value = "";
        output.value = "";
        run();
        input.focus();
      }),
      copyButton(() => output.value, ctx.toast),
    ),
    error,
    statLine,
  );

  run();
  return { element };
};

export default factory;
