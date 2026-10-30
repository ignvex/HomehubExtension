// JSON 格式化 / 压缩 / 校验。
//
// 与「直接 JSON.parse 再 stringify」的区别在于错误处理：
// 原生错误信息在长文本里几乎无法定位，这里算出出错的行列，
// 并在输入框下方直接标出来。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  copyButton,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textarea,
} from "./shared";

/** 从 V8 的报错里提取位置。不同引擎的措辞不同，能做的是尽力而为。 */
function locateError(text: string, error: unknown): { line: number; column: number } | null {
  const message = error instanceof Error ? error.message : String(error);
  // V8: "Unexpected token } in JSON at position 42"
  // 新版 V8: "Unexpected token '}', ...\" is not valid JSON" 不带 position
  const posMatch = /position (\d+)/u.exec(message);
  if (!posMatch) return null;
  const pos = Number(posMatch[1]);
  if (!Number.isFinite(pos) || pos < 0 || pos > text.length) return null;

  const before = text.slice(0, pos);
  const line = before.split("\n").length;
  const lastBreak = before.lastIndexOf("\n");
  const column = pos - lastBreak;
  return { line, column };
}

export const factory: ToolFactory = (ctx) => {
  const input = textarea('{"hello": "world"}', { rows: 14 });
  const output = textarea("结果会显示在这里", { rows: 14, readonly: true });
  const mode = select([
    ["format", "格式化（2 空格缩进）"],
    ["compact", "压缩（去掉空白）"],
    ["sort", "格式化并按 key 排序"],
  ]);
  const error = errorLine();
  const statLine = stats();

  const run = (): void => {
    const text = input.value;
    if (text.trim() === "") {
      output.value = "";
      showError(error, "");
      setStats(statLine, []);
      return;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch (e) {
      const where = locateError(text, e);
      const base = e instanceof Error ? e.message : String(e);
      showError(error, where ? `第 ${where.line} 行第 ${where.column} 列：${base}` : base);
      output.value = "";
      setStats(statLine, []);
      return;
    }

    showError(error, "");

    // 递归排序 key。数组顺序保持不变——数组的下标有意义。
    const sortKeys = (value: unknown): unknown => {
      if (Array.isArray(value)) return value.map(sortKeys);
      if (value !== null && typeof value === "object") {
        const src = value as Record<string, unknown>;
        const out: Record<string, unknown> = {};
        for (const key of Object.keys(src).sort()) out[key] = sortKeys(src[key]);
        return out;
      }
      return value;
    };

    const view = mode.value === "sort" ? sortKeys(parsed) : parsed;
    const result =
      mode.value === "compact" ? JSON.stringify(view) : JSON.stringify(view, null, 2);

    output.value = result;

    const bytes = new TextEncoder().encode(result).length;
    const ratio = text.length > 0 ? Math.round((result.length / text.length) * 100) : 100;
    setStats(statLine, [
      ["字符", String(result.length)],
      ["字节", String(bytes)],
      ["相对输入", `${ratio}%`],
    ]);
  };

  input.addEventListener("input", run);
  mode.addEventListener("change", run);

  const formatBtn = button(
    "格式化",
    () => {
      mode.value = "format";
      run();
    },
    "primary",
  );
  const compactBtn = button("压缩", () => {
    mode.value = "compact";
    run();
  });
  const clearBtn = button("清空", () => {
    input.value = "";
    output.value = "";
    run();
    input.focus();
  });
  const sampleBtn = button("示例", () => {
    input.value = JSON.stringify(
      {
        name: "HomehubExtension",
        version: "0.1.0",
        features: ["pastebin", "rss"],
        limits: { maxBodyBytes: 262144, slugLength: 6 },
      },
      null,
      2,
    );
    run();
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
    actions(sampleBtn, formatBtn, compactBtn, clearBtn, copyButton(() => output.value, ctx.toast)),
    mode,
    error,
    statLine,
  );

  run();
  return { element };
};

export default factory;
