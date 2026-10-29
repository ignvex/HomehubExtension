import { h } from "../lib/dom";
import { NAV_LABELS } from "../lib/strings";
import { unreadCount } from "../state/app-state";
import type { IconName } from "./icons";
import { icon } from "./icons";

interface NavItem {
  path: string;
  label: string;
  icon: IconName;
  badge?: boolean;
}

const PRIMARY: readonly NavItem[] = [
  { path: "/", label: NAV_LABELS.home, icon: "home" },
  { path: "/entries", label: NAV_LABELS.entries, icon: "inbox", badge: true },
  { path: "/feeds", label: NAV_LABELS.feeds, icon: "rss" },
  { path: "/pastes", label: NAV_LABELS.pastes, icon: "paste" },
];

const SECONDARY: readonly NavItem[] = [
  { path: "/pair", label: NAV_LABELS.pair, icon: "key" },
  { path: "/settings", label: NAV_LABELS.settings, icon: "settings" },
];

export class Nav {
  readonly element: HTMLElement;
  private readonly links: Array<{ path: string; anchor: HTMLAnchorElement }> = [];
  private readonly badge: HTMLElement;
  private readonly unsubscribe: () => void;

  constructor() {
    this.badge = h("span", { class: "nav-badge", attrs: { hidden: true } });
    this.element = h("nav", { class: "nav", attrs: { "aria-label": "主导航" } });

    this.element.append(this.createGroup("浏览", PRIMARY));
    this.element.append(this.createGroup("系统", SECONDARY));
    this.unsubscribe = unreadCount.subscribe((count) => this.setUnread(count));
  }

  private createGroup(heading: string, items: readonly NavItem[]): HTMLElement {
    const list = h("ul", { class: "nav-list" });
    for (const item of items) {
      const badge = item.badge ? this.badge : null;
      const anchor = h(
        "a",
        { class: "nav-link", attrs: { href: item.path } },
        icon(item.icon, "icon nav-icon"),
        h("span", { class: "nav-label" }, item.label),
        badge,
      );
      list.append(h("li", {}, anchor));
      this.links.push({ path: item.path, anchor });
    }
    return h(
      "div",
      { class: "nav-group" },
      h("p", { class: "nav-heading" }, heading),
      list,
    );
  }

  setActive(pathname: string): void {
    for (const { path, anchor } of this.links) {
      const active = path === "/" ? pathname === "/" : pathname === path || pathname.startsWith(`${path}/`);
      if (active) {
        anchor.setAttribute("aria-current", "page");
      } else {
        anchor.removeAttribute("aria-current");
      }
    }
  }

  setUnread(count: number): void {
    const visible = count > 0;
    this.badge.hidden = !visible;
    this.badge.textContent = visible ? (count > 99 ? "99+" : String(count)) : "";
    this.badge.setAttribute("aria-label", visible ? `${count} 条未读` : "");
  }

  destroy(): void {
    this.unsubscribe();
  }
}
