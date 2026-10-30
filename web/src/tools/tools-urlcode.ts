// URL 编解码。
//
// 两种模式的区别必须讲清楚，否则用户会奇怪为什么 "a b" 编码成
// "a%20b" 还是 "a+b"：
//
//   encodeURIComponent — 用于拼接 URL 的一部分（查询参数值、路径段），
//                        会转义 & / ? # 等分隔符。本工具用这个。
//   encodeURI          — 用于整个 URL，保留分隔符。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  copyButton,
  errorLine,
  field,
  select,
  showError,
  textarea,
} from "./shared";

export const factory: ToolFactory = (ctx) => {
  const input = textarea("要编码的文本，或要解码的 URL 片段", { rows: 10 });
  const output = textarea("结果会显示在这里", { rows: 10, readonly: true });
  const mode = select([
    ["component", "encodeURIComponent / decodeURIComponent"],
    ["uri", "encodeURI / decodeURI"],
  ]);
  const error = errorLine();

  const run = (): void => {
    const text = input.value;
    if (text === "") {
      output.value = "";
      showError(error, "");
      return;
    }

    try {
      if (mode.value === "component") {
        // 解码方向靠一个启发式判断：出现 %XX 就认为是要解码。
        // 更严谨的做法是让用户显式选方向，但两个按钮比一个下拉更笨重。
        const looksEncoded = /%[0-9a-f]{2}/iu.test(text);
        output.value = looksEncoded ? decodeURIComponent(text) : encodeURIComponent(text);
      } else {
        const looksEncoded = /%[0-9a-f]{2}/iu.test(text);
        output.value = looksEncoded ? decodeURI(text) : encodeURI(text);
      }
      showError(error, "");
    } catch (e) {
      output.value = "";
      if (e instanceof URIError) {
        showError(error, "不是合法的百分号编码：存在不完整的 %XX 序列（如 %ZZ、%4）");
      } else {
        showError(error, e instanceof Error ? e.message : String(e));
      }
    }
  };

  input.addEventListener("input", run);
  mode.addEventListener("change", run);

  const encodeBtn = button(
    "编码",
    () => {
      try {
        output.value =
          mode.value === "component" ? encodeURIComponent(input.value) : encodeURI(input.value);
        showError(error, "");
      } catch (e) {
        showError(error, e instanceof Error ? e.message : String(e));
      }
    },
    "primary",
  );

  const decodeBtn = button("解码", () => {
    try {
      output.value =
        mode.value === "component" ? decodeURIComponent(input.value) : decodeURI(input.value);
      showError(error, "");
    } catch (e) {
      output.value = "";
      showError(error, e instanceof URIError ? "存在不完整的 %XX 序列" : String(e));
    }
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
      encodeBtn,
      decodeBtn,
      button("清空", () => {
        input.value = "";
        output.value = "";
        showError(error, "");
        input.focus();
      }),
      copyButton(() => output.value, ctx.toast),
    ),
    error,
  );

  run();
  return { element };
};

export default factory;
