import { getApps, getStats } from "../api/system";
import type { AppInfo, SystemStats } from "../api/types";
import { pageHeader } from "../components/page";
import { emptyState, errorState } from "../components/states";
import { appIcon, icon } from "../components/icons";
import { h } from "../lib/dom";
import { formatCount } from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewFactory } from "../router";
import { unreadCount } from "../state/app-state";

function skeletonCards(): HTMLElement {
  const grid = h("div", { class: "app-grid", attrs: { "aria-hidden": "true" } });
  for (let index = 0; index < 4; index += 1) {
    grid.append(
      h(
        "div",
        { class: "app-card app-card--skeleton" },
        h("div", { class: "skeleton skeleton--icon" }),
        h("div", { class: "skeleton skeleton--title" }),
        h("div", { class: "skeleton skeleton--line" }),
      ),
    );
  }
  return grid;
}

function appCard(app: AppInfo): HTMLElement {
  const iconNode = appIcon(app.icon);
  const badge =
    app.badge && app.badge.count > 0
      ? h(
          "span",
          { class: "badge badge--accent" },
          `${formatCount(app.badge.count)} ${app.badge.label}`,
        )
      : null;

  const body = [
    h(
      "div",
      { class: "app-icon", attrs: { "aria-hidden": "true" } },
      iconNode ?? h("span", { class: "app-icon-letter" }, app.name.slice(0, 1)),
    ),
    h(
      "div",
      { class: "app-card-text" },
      h("h2", { class: "app-card-title" }, app.name),
      h("p", { class: "app-card-description" }, app.description),
    ),
    h("span", { class: "app-card-arrow", attrs: { "aria-hidden": "true" } }, icon("chevron-right")),
  ];

  if (!app.enabled) {
    return h(
      "div",
      { class: "app-card app-card--disabled", attrs: { "aria-disabled": "true" } },
      ...body,
      h("span", { class: "badge badge--muted" }, "未启用"),
    );
  }

  const external = !app.path.startsWith("/");
  return h(
    "a",
    {
      class: "app-card",
      attrs: {
        href: app.path,
        ...(external ? { target: "_blank", rel: "noopener noreferrer" } : {}),
      },
    },
    ...body,
    badge,
  );
}

function statLink(label: string, value: number, href: string, iconName: "paste" | "inbox" | "rss" | "list"): HTMLElement {
  return h(
    "a",
    { class: "stat", attrs: { href } },
    h("span", { class: "stat-icon" }, icon(iconName)),
    h("span", { class: "stat-body" }, h("span", { class: "stat-value" }, formatCount(value)), h("span", { class: "stat-label" }, label)),
  );
}

function overview(stats: SystemStats): HTMLElement {
  return h(
    "div",
    { class: "overview" },
    statLink("片段", stats.pastes, "/pastes", "paste"),
    statLink("未读条目", stats.unread, "/entries?unread=true", "inbox"),
    statLink("订阅源", stats.feeds, "/feeds", "rss"),
    statLink("条目总数", stats.entries, "/entries", "list"),
  );
}

export const homeView: ViewFactory = (ctx) => {
  const gridHost = h("div", { class: "app-grid-host" }, skeletonCards());
  const overviewHost = h("div", { class: "overview-host" });
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );
  const element = h(
    "div",
    { class: "page" },
    pageHeader("工具台", "所有数据都留在你自己的服务器上。", [refresh]),
    overviewHost,
    gridHost,
  );

  let loading = false;

  const load = async (): Promise<void> => {
    if (loading) return;
    loading = true;
    refresh.disabled = true;
    refresh.classList.add("is-busy");

    try {
      const [appsResult, statsResult] = await Promise.allSettled([
        getApps(ctx.signal),
        getStats(ctx.signal),
      ]);
      if (ctx.signal.aborted) return;

      if (appsResult.status === "fulfilled") {
        gridHost.replaceChildren();
        if (appsResult.value.apps.length === 0) {
          gridHost.append(
            emptyState({
              title: "没有可用的应用",
              description: "后端没有返回任何已启用的模块。检查配置后刷新。",
              icon: "home",
            }),
          );
        } else {
          const grid = h("div", { class: "app-grid" });
          for (const app of appsResult.value.apps) grid.append(appCard(app));
          gridHost.replaceChildren(grid);
        }
      } else {
        gridHost.replaceChildren(
          errorState(describeError(appsResult.reason), () => {
            void load();
          }),
        );
      }

      if (statsResult.status === "fulfilled") {
        overviewHost.replaceChildren(overview(statsResult.value));
        unreadCount.set(statsResult.value.unread);
      } else {
        overviewHost.replaceChildren();
      }
    } finally {
      loading = false;
      refresh.disabled = false;
      refresh.classList.remove("is-busy");
    }
  };

  refresh.addEventListener("click", () => {
    void load();
  });
  void load();

  return { element };
};
