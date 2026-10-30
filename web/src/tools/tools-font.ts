// 字体预览：对比常用系统字体的渲染效果。
//
// 「检测本机是否有这个字体」没有直接的 API。可行的方法是
// 用 canvas 测量同一段文本在不同字体下的宽度：如果候选字体
// 与兜底字体（monospace / serif / sans-serif）的宽度完全一致，
// 说明候选字体不存在，浏览器回退到了兜底字体。
//
// 这个方法不完美（等宽字体有时宽度恰好相同），但足以应付
// 「我装了这个字体吗」的日常判断。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import { field, select, textarea } from "./shared";

interface FontItem {
  /** CSS font-family 值。 */
  stack: string;
  /** 用于检测的单个族名；null 表示这是通用族，不需要检测。 */
  probe: string | null;
  label: string;
}

const FONTS: readonly FontItem[] = [
  { stack: "-apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif", probe: null, label: "系统默认（无衬线）" },
  { stack: "'PingFang SC', 'Microsoft YaHei', 'Noto Sans SC', sans-serif", probe: "PingFang SC", label: "苹方 / 微软雅黑（中文无衬线）" },
  { stack: "'Songti SC', SimSun, 'Noto Serif SC', serif", probe: "Songti SC", label: "宋体（中文衬线）" },
  { stack: "'Kaiti SC', KaiTi, STKaiti, serif", probe: "Kaiti SC", label: "楷体" },
  { stack: "'Heiti SC', SimHei, sans-serif", probe: "Heiti SC", label: "黑体" },
  { stack: "Georgia, 'Times New Roman', Times, serif", probe: "Georgia", label: "Georgia（英文衬线）" },
  { stack: "'Helvetica Neue', Helvetica, Arial, sans-serif", probe: "Helvetica Neue", label: "Helvetica（英文无衬线）" },
  { stack: "'SF Mono', 'Cascadia Code', 'JetBrains Mono', Consolas, 'Courier New', monospace", probe: "Cascadia Code", label: "等宽（代码）" },
  { stack: "ui-monospace, 'SF Mono', Menlo, Consolas, monospace", probe: "Menlo", label: "ui-monospace（系统等宽）" },
];

/**
 * 检测某个字体族是否可用。
 *
 * 原理：分别用「候选字体 + 兜底」与「只兜底」测量同一段文本，
 * 宽度不同说明候选字体生效了。文本特意包含宽窄差异大的字符，
 * 让不同字体的度量差异更容易暴露。
 */
function hasFont(family: string, fallback: "monospace" | "serif" | "sans-serif"): boolean {
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d");
  if (!ctx) return false;

  const probe = "mmmmmmmmmmlliWWQO0中文测试";
  const size = "72px";

  ctx.font = `${size} ${fallback}`;
  const baseline = ctx.measureText(probe).width;

  ctx.font = `${size} '${family}', ${fallback}`;
  const withCandidate = ctx.measureText(probe).width;

  return Math.abs(withCandidate - baseline) > 0.5;
}

/** 根据栈的兜底族名推断该用哪个基准。 */
function fallbackOf(stack: string): "monospace" | "serif" | "sans-serif" {
  if (stack.includes("monospace")) return "monospace";
  if (stack.includes("serif") && !stack.includes("sans-serif")) return "serif";
  return "sans-serif";
}

export const factory: ToolFactory = (_ctx) => {
  const sample = textarea(
    "中文排版 ABC abc 0123 —— “引号” 《书名》\nconst greet = (name) => `你好，${name}`;",
    { rows: 4, mono: false },
  );
  sample.value =
    "中文排版 ABC abc 0123 —— “引号” 《书名》\nconst greet = (name) => `你好，${name}`;";

  const sizeSelect = select([
    ["14", "14px"],
    ["16", "16px（正文）"],
    ["18", "18px"],
    ["24", "24px"],
    ["32", "32px"],
  ]);
  sizeSelect.value = "18";

  const weightSelect = select([
    ["300", "300 细"],
    ["400", "400 常规"],
    ["500", "500 中等"],
    ["600", "600 半粗"],
    ["700", "700 粗"],
  ]);
  weightSelect.value = "400";

  const onlyAvailable = h("input", { attrs: { type: "checkbox" } }) as HTMLInputElement;

  const listHost = h("div", { class: "font-list" });

  const render = (): void => {
    listHost.replaceChildren();
    const size = sizeSelect.value;
    const weight = weightSelect.value;

    for (const font of FONTS) {
      const available =
        font.probe === null ? true : hasFont(font.probe, fallbackOf(font.stack));
      if (onlyAvailable.checked && !available) continue;

      const sampleNode = h("p", {
        class: "font-sample",
        text: sample.value || "（输入文本以预览）",
      });
      sampleNode.style.setProperty("font-family", font.stack);
      sampleNode.style.setProperty("font-size", `${size}px`);
      sampleNode.style.setProperty("font-weight", weight);

      listHost.append(
        h(
          "section",
          { class: "font-row" },
          h(
            "header",
            { class: "font-row-head" },
            h("span", { class: "font-row-label" }, font.label),
            h(
              "span",
              {
                class: `badge badge--${available ? "success" : "muted"}`,
              },
              available ? "可用" : "未安装（回退）",
            ),
          ),
          sampleNode,
        ),
      );
    }

    if (listHost.childElementCount === 0) {
      listHost.append(h("p", { class: "muted small" }, "没有可用的字体。"));
    }
  };

  sample.addEventListener("input", render);
  sizeSelect.addEventListener("change", render);
  weightSelect.addEventListener("change", render);
  onlyAvailable.addEventListener("change", render);

  const element = h(
    "div",
    { class: "tool-body" },
    field("预览文本", sample),
    h(
      "div",
      { class: "tool-row" },
      field("字号", sizeSelect),
      field("字重", weightSelect),
      h("label", { class: "tool-check" }, onlyAvailable, h("span", {}, "只看已安装的")),
    ),
    listHost,
  );

  render();
  return { element };
};

export default factory;
