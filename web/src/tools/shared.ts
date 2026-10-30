// 工具共用的 UI 构件。
//
// 15 个工具的界面结构高度相似（输入区 + 选项 + 输出区 + 复制按钮），
// 把这些模式抽出来，单个工具的代码就只剩下真正的逻辑。

import { h } from "../lib/dom";
import { icon } from "../components/icons";
import type { ToastApi } from "../components/toast";

/** 带标签的字段容器。 */
export function field(label: string, control: Node, hint?: string): HTMLElement {
  return h(
    "label",
    { class: "tool-field" },
    h("span", { class: "tool-field-label" }, label),
    control,
    hint ? h("span", { class: "tool-field-hint" }, hint) : null,
  );
}

export function textarea(
  placeholder: string,
  options: { rows?: number; readonly?: boolean; mono?: boolean; spellcheck?: boolean } = {},
): HTMLTextAreaElement {
  return h("textarea", {
    class: `input tool-input${options.mono === false ? "" : " mono"}`,
    attrs: {
      rows: String(options.rows ?? 8),
      placeholder,
      spellcheck: String(options.spellcheck ?? false),
      ...(options.readonly ? { readonly: "readonly" } : {}),
    },
  }) as HTMLTextAreaElement;
}

export function textInput(
  placeholder: string,
  options: { type?: string; mono?: boolean; value?: string } = {},
): HTMLInputElement {
  return h("input", {
    class: `input${options.mono ? " mono" : ""}`,
    attrs: {
      type: options.type ?? "text",
      placeholder,
      autocomplete: "off",
      spellcheck: "false",
      ...(options.value !== undefined ? { value: options.value } : {}),
    },
  }) as HTMLInputElement;
}

export function select(options: ReadonlyArray<readonly [string, string]>): HTMLSelectElement {
  const el = h("select", { class: "input" }) as HTMLSelectElement;
  for (const [value, label] of options) {
    el.append(h("option", { attrs: { value } }, label));
  }
  return el;
}

export function checkbox(label: string, checked = false): HTMLLabelElement {
  const input = h("input", { attrs: { type: "checkbox", ...(checked ? { checked: "" } : {}) } });
  return h("label", { class: "tool-check" }, input, h("span", {}, label));
}

export function button(label: string, onClick: () => void, variant?: "primary" | "danger"): HTMLButtonElement {
  const cls = variant === "primary" ? "btn btn--primary" : variant === "danger" ? "btn btn--danger" : "btn";
  const el = h("button", { class: cls, attrs: { type: "button" } }, label);
  el.addEventListener("click", onClick);
  return el;
}

/**
 * 复制到剪贴板的按钮。
 *
 * 失败时不是静默，而是把内容打给用户看——剪贴板 API 在
 * 非 HTTPS 或权限被拒时会失败，那时候用户还是得拿到结果。
 */
export function copyButton(
  getText: () => string,
  toast: ToastApi,
  label = "复制",
): HTMLButtonElement {
  const el = h(
    "button",
    { class: "btn btn--sm", attrs: { type: "button" } },
    icon("copy"),
    label,
  ) as HTMLButtonElement;
  el.addEventListener("click", () => {
    const text = getText();
    if (text === "") {
      toast.show("没有可复制的内容", "error");
      return;
    }
    void navigator.clipboard
      .writeText(text)
      .then(() => toast.show("已复制", "success"))
      .catch(() => toast.show("复制失败，请手动选中文本", "error"));
  });
  return el;
}

/** 一行操作按钮。 */
export function actions(...children: (Node | null)[]): HTMLElement {
  return h("div", { class: "tool-actions" }, ...children.filter((c): c is Node => c !== null));
}

/** 结果统计条，用于显示字节数、耗时、压缩率等。 */
export function stats(): HTMLElement {
  return h("div", { class: "tool-stats", attrs: { role: "status", "aria-live": "polite" } });
}

export function setStats(el: HTMLElement, parts: ReadonlyArray<readonly [string, string]>): void {
  el.replaceChildren();
  for (const [label, value] of parts) {
    el.append(
      h("span", { class: "tool-stat" }, h("span", { class: "tool-stat-label" }, label), h("strong", {}, value)),
    );
  }
}

/** 错误提示。默认隐藏，由工具按需填充。 */
export function errorLine(): HTMLElement {
  const el = h("p", { class: "form-error", attrs: { role: "alert" } });
  el.hidden = true;
  return el;
}

export function showError(el: HTMLElement, message: string): void {
  el.textContent = message;
  el.hidden = message === "";
}

/** 格式化字节数。与 lib/format 的 formatBytes 不同：这里给工具用，精度更高。 */
export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB"];
  let value = n / 1024;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i += 1;
  }
  return `${value.toFixed(value < 10 ? 2 : 1)} ${units[i]}`;
}

/**
 * 把一个可能抛错的同步操作包起来，统一转成错误提示。
 * 工具里大量「解析失败要显示在哪」的场景都用它。
 */
export function guard<T>(errorEl: HTMLElement, fn: () => T): T | undefined {
  try {
    const result = fn();
    showError(errorEl, "");
    return result;
  } catch (error) {
    showError(errorEl, error instanceof Error ? error.message : String(error));
    return undefined;
  }
}
