import { listFeeds } from "../api/feeds";
import type { Feed } from "../api/types";
import { pageHeader } from "../components/page";
import { emptyState, errorState } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { formatCount, formatDateTime, formatInterval, formatRelative } from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewFactory } from "../router";

function feedCard(feed: Feed): HTMLElement {
  const title = feed.title.trim() !== "" ? feed.title : feed.url;
  const ok = feed.enabled && !feed.last_error && feed.last_status >= 200 && feed.last_status < 400;
  const status = !feed.enabled
    ? "已停用"
    : feed.last_status > 0
      ? `HTTP ${feed.last_status}`
      : "尚未抓取";

  const siteLink = feed.site_url.trim()
    ? h(
        "a",
        {
          class: "feed-site",
          attrs: { href: feed.site_url, target: "_blank", rel: "noopener noreferrer" },
        },
        icon("external"),
        "站点",
      )
    : null;

  return h(
    "article",
    { class: `feed-card card card--pad${feed.enabled ? "" : " is-disabled"}` },
    h(
      "div",
      { class: "feed-head" },
      h(
        "div",
        { class: "feed-heading" },
        h("h2", { class: "feed-title" }, title),
        h("p", { class: "feed-url mono" }, feed.url),
      ),
      h(
        "span",
        { class: `badge badge--${!feed.enabled ? "muted" : ok ? "success" : "danger"}` },
        !feed.enabled ? icon("circle") : ok ? icon("check") : icon("alert"),
        status,
      ),
    ),
    h(
      "div",
      { class: "feed-stats" },
      h("span", { class: "feed-stat" }, h("strong", {}, formatCount(feed.unread_count)), "未读"),
      feed.total_count !== undefined
        ? h("span", { class: "feed-stat" }, h("strong", {}, formatCount(feed.total_count)), "条目")
        : null,
      h("span", { class: "feed-stat" }, h("strong", {}, formatInterval(feed.fetch_interval)), "抓取间隔"),
    ),
    h(
      "dl",
      { class: "feed-details" },
      h("div", {}, h("dt", {}, "上次抓取"), h("dd", {}, feed.last_fetch_at ? formatDateTime(feed.last_fetch_at) : "—")),
      h("div", {}, h("dt", {}, "下次抓取"), h("dd", {}, feed.next_fetch_at ? formatRelative(feed.next_fetch_at) : "—")),
      feed.error_count !== undefined && feed.error_count > 0
        ? h("div", {}, h("dt", {}, "连续失败"), h("dd", {}, `${feed.error_count} 次`))
        : null,
    ),
    feed.last_error.trim() !== ""
      ? h(
          "p",
          { class: "feed-error", attrs: { role: "status" } },
          icon("alert"),
          h("span", {}, feed.last_error),
        )
      : null,
    h(
      "div",
      { class: "feed-actions" },
      h(
        "a",
        { class: "btn btn--sm", attrs: { href: `/entries?feed_id=${feed.id}` } },
        icon("inbox"),
        "阅读条目",
      ),
      siteLink,
    ),
  );
}

function feedSkeletons(): HTMLElement {
  const grid = h("div", { class: "feed-grid", attrs: { "aria-hidden": "true" } });
  for (let index = 0; index < 4; index += 1) {
    grid.append(
      h(
        "div",
        { class: "feed-card card card--pad" },
        h("div", { class: "skeleton skeleton--title" }),
        h("div", { class: "skeleton skeleton--line" }),
        h("div", { class: "skeleton skeleton--short" }),
      ),
    );
  }
  return grid;
}

export const feedsView: ViewFactory = (ctx) => {
  const host = h("div", { class: "feed-host" }, feedSkeletons());
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );
  const element = h(
    "div",
    { class: "page" },
    pageHeader("订阅源", "订阅由 Homehub 推送；Extension 只负责抓取、清洗与媒体本地化。", [refresh]),
    host,
  );

  const load = async (): Promise<void> => {
    refresh.disabled = true;
    refresh.classList.add("is-busy");
    host.replaceChildren(feedSkeletons());
    try {
      const response = await listFeeds(ctx.signal);
      if (ctx.signal.aborted) return;
      if (response.items.length === 0) {
        host.replaceChildren(
          emptyState({
            title: "还没有订阅源",
            description: "订阅通过 Homehub 宿主推送，推送后会在下一次抓取时出现在这里。",
            icon: "rss",
          }),
        );
        return;
      }
      const grid = h("div", { class: "feed-grid" });
      for (const feed of response.items) grid.append(feedCard(feed));
      host.replaceChildren(grid);
    } catch (error) {
      if (ctx.signal.aborted) return;
      host.replaceChildren(
        errorState(describeError(error), () => {
          void load();
        }),
      );
    } finally {
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
