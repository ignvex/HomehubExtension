import { h } from "../lib/dom";

export function pageHeader(
  title: string,
  description?: string,
  actions: readonly Node[] = [],
): HTMLElement {
  const heading = h("div", { class: "page-heading" }, h("h1", { class: "page-title" }, title));
  if (description) heading.append(h("p", { class: "page-description" }, description));
  return h(
    "header",
    { class: "page-head" },
    heading,
    actions.length > 0 ? h("div", { class: "page-actions" }, ...actions) : null,
  );
}

export function section(
  title: string,
  description: string | undefined,
  body: Node,
  actions?: Node,
): HTMLElement {
  const head = h(
    "div",
    { class: "section-head" },
    h("div", {}, h("h2", { class: "section-title" }, title), description
      ? h("p", { class: "section-description" }, description)
      : null),
    actions ?? null,
  );
  return h("section", { class: "section" }, head, body);
}

export function refreshButton(label = "刷新", onClick?: () => void): HTMLButtonElement {
  return h(
    "button",
    {
      class: "btn",
      attrs: { type: "button", ...(onClick ? {} : { disabled: true }) },
      ...(onClick ? { on: { click: onClick } } : {}),
    },
    h("span", { class: "btn-icon" }, "↻"),
    label,
  );
}
