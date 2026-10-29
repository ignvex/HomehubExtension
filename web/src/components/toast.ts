import { h } from "../lib/dom";
import type { IconName } from "./icons";
import { icon } from "./icons";

export type ToastTone = "info" | "success" | "error";

export interface ToastApi {
  show(message: string, tone?: ToastTone): void;
}

export interface ToastController {
  api: ToastApi;
  element: HTMLElement;
}

const TONE_ICON: Record<ToastTone, IconName> = {
  info: "circle",
  success: "check",
  error: "alert",
};

export function createToast(): ToastController {
  const region = h("div", {
    class: "toast-region",
    attrs: {
      role: "status",
      "aria-live": "polite",
      "aria-atomic": "false",
    },
  });

  const api: ToastApi = {
    show(message, tone = "info") {
      const node = h(
        "div",
        {
          class: `toast toast--${tone}`,
          attrs: { role: tone === "error" ? "alert" : "status" },
        },
        icon(TONE_ICON[tone], "icon toast-icon"),
        h("span", { class: "toast-text" }, message),
      );
      region.append(node);
      while (region.children.length > 4) {
        region.firstElementChild?.remove();
      }
      window.setTimeout(() => node.remove(), tone === "error" ? 6000 : 4000);
    },
  };

  return { api, element: region };
}
