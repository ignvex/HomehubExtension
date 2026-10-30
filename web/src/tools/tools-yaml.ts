// 配置格式转换的界面。
//
// 编解码在 config-codec.ts；这里只负责输入、错误展示与格式选择。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import { detectFormat, parseAny, serialize, type Format } from "./config-codec";
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

// ---------------------------------------------------------------------------

const SOURCE_FORMATS: ReadonlyArray<readonly [string, string]> = [
  ["auto", "自动识别"],
  ["json", "JSON"],
  ["yaml", "YAML"],
  ["ini", "INI"],
];

const TARGET_FORMATS: ReadonlyArray<readonly [string, string]> = [
  ["yaml", "YAML"],
  ["json", "JSON"],
  ["ini", "INI"],
];

export const factory: ToolFactory = (ctx) => {
  const sourceFormat = select(SOURCE_FORMATS);
  const targetFormat = select(TARGET_FORMATS);
  targetFormat.value = "json";

  const input = textarea('name: my-app\nversion: 1.0.0\nfeatures:\n  - pastebin\n  - rss', {
    rows: 14,
  });
  input.value = "name: my-app\nversion: 1.0.0\nfeatures:\n  - pastebin\n  - rss\nlimits:\n  maxBody: 262144\n  slugLength: 6";

  const output = textarea("结果会显示在这里", { rows: 14, readonly: true });
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

    try {
      const detected: Format =
        sourceFormat.value === "auto" ? detectFormat(text) : (sourceFormat.value as Format);
      const value = parseAny(text, detected);
      const result = serialize(value, targetFormat.value as Format);
      output.value = result;
      showError(error, "");
      setStats(statLine, [
        ["识别为", detected.toUpperCase()],
        ["输出", targetFormat.value.toUpperCase()],
        ["字符", String(result.length)],
      ]);
    } catch (e) {
      output.value = "";
      setStats(statLine, []);
      showError(error, e instanceof Error ? e.message : String(e));
    }
  };

  input.addEventListener("input", run);
  sourceFormat.addEventListener("change", run);
  targetFormat.addEventListener("change", run);

  // 交换：把结果当新输入，方向反过来。
  // 自动识别在这里没有意义——我们已知刚才产出的格式，直接写死更可靠。
  const swapBtn = button("交换", () => {
    const from = sourceFormat.value === "auto" ? detectFormat(input.value) : sourceFormat.value;
    const to = targetFormat.value;
    input.value = output.value;
    sourceFormat.value = to;
    targetFormat.value = from;
    run();
  });

  const element = h(
    "div",
    { class: "tool-body" },
    h(
      "div",
      { class: "tool-grid-2" },
      field("输入格式", sourceFormat),
      field("输出格式", targetFormat),
    ),
    h(
      "div",
      { class: "tool-grid-2" },
      field("输入", input),
      field("输出", output),
    ),
    actions(
      swapBtn,
      button("清空", () => {
        input.value = "";
        output.value = "";
        showError(error, "");
        setStats(statLine, []);
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
