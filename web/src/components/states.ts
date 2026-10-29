import { h } from "../lib/dom";
import { COMMON } from "../lib/strings";
import type { IconName } from "./icons";
import { icon } from "./icons";

export interface EmptyStateOptions {
  title: string;
  description?: string;
  icon?: IconName;
  action?: Node;
}

export function emptyState(options: EmptyStateOptions): HTMLElement {
  const container = h(
    "div",
    { class: "state" },
    h("div", { class: "state-icon" }, icon(options.icon ?? "inbox")),
    h("h2", { class: "state-title" }, options.title),
  );
  if (options.description) {
    container.append(h("p", { class: "state-text" }, options.description));
  }
  if (options.action) container.append(h("div", { class: "state-action" }, options.action));
  return container;
}

export function errorState(message: string, onRetry?: () => void): HTMLElement {
  const container = h(
    "div",
    { class: "state state--error", attrs: { role: "alert" } },
    h("div", { class: "state-icon" }, icon("alert")),
    h("h2", { class: "state-title" }, "加载失败"),
    h("p", { class: "state-text" }, message),
  );
  if (onRetry) {
    container.append(
      h(
        "div",
        { class: "state-action" },
        h(
          "button",
          {
            class: "btn btn--sm",
            attrs: { type: "button" },
            on: { click: onRetry },
          },
          icon("refresh"),
          COMMON.retry,
        ),
      ),
    );
  }
  return container;
}

export function skeletonRows(count = 4): HTMLElement {
  const list = h("div", { class: "skeleton-list", attrs: { "aria-hidden": "true" } });
  for (let index = 0; index < count; index += 1) {
    list.append(
      h(
        "div",
        { class: "skeleton-row" },
        h("div", { class: "skeleton skeleton--title" }),
        h("div", { class: "skeleton skeleton--line" }),
        h("div", { class: "skeleton skeleton--short" }),
      ),
    );
  }
  list.append(h("span", { class: "visually-hidden" }, COMMON.loading));
  return list;
}

export function loadingBlock(label: string = COMMON.loading): HTMLElement {
  return h(
    "div",
    { class: "loading-block", attrs: { role: "status" } },
    h("span", { class: "spinner", attrs: { "aria-hidden": "true" } }),
    label,
  );
}
