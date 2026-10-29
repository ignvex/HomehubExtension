import { icon } from "../components/icons";
import { h } from "../lib/dom";
import type { ViewFactory } from "../router";

export const notFoundView: ViewFactory = (ctx) => {
  const element = h(
    "div",
    { class: "page page--center" },
    h(
      "div",
      { class: "not-found" },
      h("p", { class: "not-found-code" }, "404"),
      h("h1", { class: "not-found-title" }, "页面不存在"),
      h(
        "p",
        { class: "not-found-text" },
        `没有找到 ${ctx.router.currentPath()}。链接可能已经失效，或者页面已经迁移。`,
      ),
      h(
        "div",
        { class: "not-found-actions" },
        h("a", { class: "btn btn--primary", attrs: { href: "/" } }, icon("home"), "回到工具台"),
        h("a", { class: "btn", attrs: { href: "/entries" } }, icon("inbox"), "查看条目"),
        h(
          "button",
          {
            class: "btn btn--ghost",
            attrs: { type: "button" },
            on: { click: () => history.back() },
          },
          icon("arrow-left"),
          "返回上一页",
        ),
      ),
    ),
  );
  return { element };
};
