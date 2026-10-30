// 正则表达式测试：实时高亮匹配结果。
//
// 高亮的实现要避开一个陷阱：不能用 innerHTML 拼 <mark>，
// 否则匹配内容里的 HTML 会被当成标签解释（XSS 与显示错乱）。
// 这里用 Range + 独立的 <mark> 节点逐段拼接。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  errorLine,
  field,
  setStats,
  showError,
  stats,
  textInput,
  textarea,
} from "./shared";

const FLAGS: ReadonlyArray<readonly [string, string]> = [
  ["g", "g 全局"],
  ["i", "i 忽略大小写"],
  ["m", "m 多行"],
  ["s", "s . 匹配换行"],
  ["u", "u Unicode"],
  ["y", "y 粘连"],
];

const MAX_HIGHLIGHTS = 5000;

export const factory: ToolFactory = (ctx) => {
  const patternInput = textInput("例如 \\b\\w+@\\w+\\.\\w+\\b", { mono: true });
  const subject = textarea("在这里粘贴要匹配的文本", { rows: 12 });
  const flagsHost = h("div", { class: "tool-flags" });
  const highlight = h("div", { class: "regex-highlight mono" });
  const matchesHost = h("div", { class: "regex-matches" });
  const error = errorLine();
  const statLine = stats();

  // 默认开 g 与 i：绝大多数场景都需要全局匹配。
  const flagInputs = new Map<string, HTMLInputElement>();
  for (const [flag, label] of FLAGS) {
    const input = h("input", {
      attrs: { type: "checkbox", ...(flag === "g" || flag === "i" ? { checked: "" } : {}) },
    }) as HTMLInputElement;
    flagInputs.set(flag, input);
    flagsHost.append(h("label", { class: "tool-check mono" }, input, h("span", {}, label)));
  }

  const activeFlags = (): string => {
    let out = "";
    for (const [flag, input] of flagInputs) if (input.checked) out += flag;
    return out;
  };

  const run = (): void => {
    // 每次改输入都重建高亮节点，比尝试增量更新简单且不易出错。
    highlight.replaceChildren();
    matchesHost.replaceChildren();

    const pattern = patternInput.value;
    const text = subject.value;

    if (pattern === "") {
      showError(error, "");
      setStats(statLine, []);
      highlight.append(...textNodes(text));
      return;
    }

    let re: RegExp;
    try {
      re = new RegExp(pattern, activeFlags());
    } catch (e) {
      // 语法错误时仍然把原文显示出来，用户能看到自己在编辑什么。
      highlight.append(...textNodes(text));
      setStats(statLine, []);
      showError(error, e instanceof Error ? e.message : String(e));
      return;
    }

    showError(error, "");

    const segments: Node[] = [];
    const rows: HTMLElement[] = [];
    let last = 0;
    let count = 0;
    let truncated = false;

    // 零宽匹配（如 ^ 或 \b）会让 lastIndex 不前进，导致死循环。
    // 这种情况下手动推进一格。
    const guard = (prev: number, next: number): number => (next === prev ? prev + 1 : next);

    for (;;) {
      const m = re.exec(text);
      if (m === null) break;
      count += 1;

      if (count > MAX_HIGHLIGHTS) {
        truncated = true;
        break;
      }

      if (m.index > last) segments.push(...textNodes(text.slice(last, m.index)));
      segments.push(h("mark", { class: "regex-hit" }, m[0] === "" ? "∅" : m[0]));

      if (rows.length < 200) {
        rows.push(
          h(
            "div",
            { class: "regex-match" },
            h("span", { class: "regex-match-index" }, String(count)),
            h("span", { class: "regex-match-text mono" }, m[0] === "" ? "(空匹配)" : m[0]),
            h("span", { class: "regex-match-pos" }, `@${m.index}`),
            m.length > 1
              ? h(
                  "span",
                  { class: "regex-groups" },
                  ...m.slice(1).map((g, i) =>
                    h("span", { class: "regex-group" }, `$${i + 1}=${g ?? "undefined"}`),
                  ),
                )
              : null,
          ),
        );
      }

      last = m.index + m[0].length;
      if (!re.global) break;
      const next = guard(m.index, re.lastIndex);
      if (next > text.length) break;
      re.lastIndex = next;
      if (m[0] === "" && next > last) last = next;
    }

    if (last < text.length) segments.push(...textNodes(text.slice(last)));
    highlight.append(...segments);

    matchesHost.append(...rows);
    if (rows.length === 0) {
      matchesHost.append(h("p", { class: "muted small" }, "没有匹配。"));
    } else if (count > rows.length) {
      matchesHost.append(
        h("p", { class: "muted small" }, `仅列出前 ${rows.length} 条，共 ${count} 条。`),
      );
    }

    setStats(statLine, [
      ["匹配数", truncated ? `${count}+` : String(count)],
      ...(truncated ? ([["提示", "已截断，可能过于宽泛"]] as const) : []),
    ]);
  };

  /** 把纯文本切成文本节点，保证不会解释 HTML。 */
  function textNodes(text: string): Node[] {
    if (text === "") return [];
    // 保留换行为 <br>，否则高亮区里多行内容会挤成一行。
    const parts = text.split("\n");
    const out: Node[] = [];
    parts.forEach((part, index) => {
      if (index > 0) out.push(h("br"));
      if (part !== "") out.push(document.createTextNode(part));
    });
    return out;
  }

  patternInput.addEventListener("input", run);
  subject.addEventListener("input", run);
  for (const input of flagInputs.values()) input.addEventListener("change", run);

  const copyAll = button("复制全部匹配", () => {
    const pattern = patternInput.value;
    if (pattern === "") {
      ctx.toast.show("还没有正则", "error");
      return;
    }
    try {
      const re = new RegExp(pattern, activeFlags().includes("g") ? activeFlags() : activeFlags() + "g");
      const found = [...subject.value.matchAll(re)].map((m) => m[0]);
      if (found.length === 0) {
        ctx.toast.show("没有匹配", "error");
        return;
      }
      void navigator.clipboard
        .writeText(found.join("\n"))
        .then(() => ctx.toast.show(`已复制 ${found.length} 条`, "success"))
        .catch(() => ctx.toast.show("复制失败", "error"));
    } catch {
      ctx.toast.show("正则无效", "error");
    }
  });

  const element = h(
    "div",
    { class: "tool-body" },
    field("正则表达式", patternInput, "不要写前后的 / 分隔符"),
    h("div", { class: "tool-row" }, flagsHost, actions(copyAll)),
    h(
      "div",
      { class: "tool-grid-2" },
      field("测试文本", subject),
      field("匹配结果", highlight),
    ),
    error,
    statLine,
    matchesHost,
  );

  run();
  return { element };
};

export default factory;
