// 工具列表页：按分类分组的网格 + 搜索框。
//
// 只读 registry 的元数据，不加载任何工具实现——
// 打开这一页的代价是零。

import { pageHeader } from "../components/page";
import { emptyState } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { groupByCategory, searchTools } from "../tools/registry";
import { CATEGORIES } from "../tools/types";
import type { ToolMeta } from "../tools/types";
import type { ViewFactory } from "../router";

function toolCard(tool: ToolMeta): HTMLElement {
  return h(
    "a",
    { class: "tool-card card", attrs: { href: `/tools/${encodeURIComponent(tool.id)}` } },
    h("span", { class: "tool-icon", attrs: { "aria-hidden": "true" } }, icon(tool.icon as never)),
    h(
      "span",
      { class: "tool-card-text" },
      h("span", { class: "tool-card-title" }, tool.name),
      h("span", { class: "tool-card-desc" }, tool.description),
    ),
  );
}

export const toolsView: ViewFactory = (ctx) => {
  const gridHost = h("div", {});
  const searchInput = h("input", {
    class: "input tool-search",
    attrs: {
      type: "search",
      placeholder: "搜索工具…",
      "aria-label": "搜索工具",
      autocomplete: "off",
    },
  });

  const render = (query: string): void => {
    const matched = searchTools(query);
    gridHost.replaceChildren();

    if (matched.length === 0) {
      gridHost.append(
        emptyState({
          title: "没有匹配的工具",
          description: `没有找到与「${query.trim()}」相关的工具。换个关键词试试。`,
          icon: "search",
        }),
      );
      return;
    }

    if (query.trim() !== "") {
      // 搜索时平铺，不分组——分组会让少量结果之间的联系变得难以扫视。
      const grid = h("div", { class: "tool-grid" });
      for (const tool of matched) grid.append(toolCard(tool));
      gridHost.append(grid);
      return;
    }

    const groups = groupByCategory();
    for (const category of CATEGORIES) {
      const list = groups.get(category.id);
      if (!list || list.length === 0) continue;
      const grid = h("div", { class: "tool-grid" });
      for (const tool of list) grid.append(toolCard(tool));
      gridHost.append(
        h(
          "section",
          { class: "tool-section" },
          h(
            "header",
            { class: "tool-section-head" },
            h("h2", { class: "tool-section-title" }, category.name),
            h("p", { class: "tool-section-desc" }, category.description),
          ),
          grid,
        ),
      );
    }
  };

  searchInput.addEventListener("input", () => {
    render(searchInput.value);
  });

  render("");

  const element = h(
    "div",
    { class: "page" },
    pageHeader("开发工具", "全部在浏览器本地运行，不上传、不联网、不追踪。", [
      h("a", { class: "btn", attrs: { href: "/" } }, icon("arrow-left"), "返回工具台"),
    ]),
    h("div", { class: "tool-toolbar" }, searchInput),
    gridHost,
  );

  // 搜索结果为空时把焦点留在搜索框，方便继续改关键词。
  ctx.signal.addEventListener("abort", () => {}, { once: true });

  return { element };
};
