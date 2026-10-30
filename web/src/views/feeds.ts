import { createFeed, deleteFeed, listFeeds, refreshFeed, updateFeed } from "../api/feeds";
import type { Feed } from "../api/types";
import { pageHeader, section } from "../components/page";
import { emptyState, errorState } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { formatCount, formatDateTime, formatInterval, formatRelative } from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewFactory } from "../router";

interface FeedCallbacks {
  onToggle: (feed: Feed) => void;
  onRefresh: (feed: Feed) => void;
  onDelete: (feed: Feed) => void;
  onInterval: (feed: Feed) => void;
}

function feedCard(feed: Feed, cb: FeedCallbacks): HTMLElement {
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
      h(
        "button",
        {
          class: "btn btn--sm",
          attrs: { type: "button", "data-action": "refresh" },
          on: { click: () => cb.onRefresh(feed) },
        },
        icon("refresh"),
        "立即抓取",
      ),
      h(
        "button",
        {
          class: "btn btn--sm",
          attrs: { type: "button" },
          on: { click: () => cb.onInterval(feed) },
        },
        icon("clock"),
        formatInterval(feed.fetch_interval),
      ),
      h(
        "button",
        {
          class: "btn btn--sm",
          attrs: { type: "button" },
          on: { click: () => cb.onToggle(feed) },
        },
        feed.enabled ? "停用" : "启用",
      ),
      h(
        "button",
        {
          class: "btn btn--sm btn--danger",
          attrs: { type: "button" },
          on: { click: () => cb.onDelete(feed) },
        },
        "删除",
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

// INTERVALS 是抓取间隔的可选值。
//
// 后端会夹紧到 rss.min_interval 与 rss.max_interval 之间，
// 所以这里出现越界值时不会报错，只是被修正——界面上的选项
// 覆盖常见场景即可，不追求与配置完全同步。
const INTERVALS: ReadonlyArray<readonly [string, string]> = [
  ["10m", "10 分钟"],
  ["30m", "30 分钟"],
  ["1h", "1 小时"],
  ["3h", "3 小时"],
  ["6h", "6 小时"],
  ["12h", "12 小时"],
  ["24h", "1 天"],
];

export const feedsView: ViewFactory = (ctx) => {
  const host = h("div", { class: "feed-host" }, feedSkeletons());
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );

  // ---- 新建表单 ----
  const urlInput = h("input", {
    class: "input",
    attrs: {
      type: "url",
      placeholder: "https://example.com/feed.xml",
      required: "required",
      autocomplete: "off",
      spellcheck: "false",
    },
  });
  const titleInput = h("input", {
    class: "input",
    attrs: { type: "text", placeholder: "留空则使用源站标题", autocomplete: "off" },
  });
  const intervalSelect = h("select", { class: "input" });
  for (const [value, label] of INTERVALS) {
    intervalSelect.append(h("option", { attrs: { value } }, label));
  }
  intervalSelect.value = "30m";

  const createError = h("p", { class: "form-error", attrs: { role: "alert" } });
  createError.hidden = true;

  const submit = h(
    "button",
    { class: "btn btn--primary", attrs: { type: "submit" } },
    icon("plus"),
    "添加订阅",
  );

  const createForm = h(
    "form",
    { class: "feed-create" },
    h(
      "div",
      { class: "feed-create-row" },
      h("label", { class: "field" }, h("span", {}, "订阅地址"), urlInput),
      h("label", { class: "field" }, h("span", {}, "名称（可选）"), titleInput),
      h("label", { class: "field field--narrow" }, h("span", {}, "抓取间隔"), intervalSelect),
      submit,
    ),
    createError,
  );

  createForm.addEventListener("submit", (event) => {
    event.preventDefault();
    const url = urlInput.value.trim();
    if (url === "") {
      createError.textContent = "请填写订阅地址";
      createError.hidden = false;
      return;
    }
    createError.hidden = true;
    submit.disabled = true;
    submit.replaceChildren(icon("refresh"), "添加中…");

    void createFeed(
      {
        url,
        title: titleInput.value.trim(),
        interval: intervalSelect.value,
      },
      ctx.signal,
    )
      .then((feed) => {
        if (ctx.signal.aborted) return;
        ctx.toast.show("订阅已添加", "success");
        urlInput.value = "";
        titleInput.value = "";
        void load();
        // 新订阅的首次抓取由服务端异步触发，给它一点时间再刷一次。
        window.setTimeout(() => {
          if (!ctx.signal.aborted) void load();
        }, 2500);
        return feed;
      })
      .catch((reason: unknown) => {
        if (ctx.signal.aborted) return;
        createError.textContent = describeError(reason);
        createError.hidden = false;
      })
      .finally(() => {
        submit.disabled = false;
        submit.replaceChildren(icon("plus"), "添加订阅");
      });
  });

  // ---- 卡片操作 ----
  const callbacks: FeedCallbacks = {
    onToggle: (feed) => {
      void updateFeed(feed.id, { enabled: !feed.enabled }, ctx.signal)
        .then(() => {
          ctx.toast.show(feed.enabled ? "已停用" : "已启用", "success");
          void load();
        })
        .catch((reason: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(reason), "error");
        });
    },
    onRefresh: (feed) => {
      ctx.toast.show("正在抓取…", "info");
      void refreshFeed(feed.id, ctx.signal)
        .then(() => {
          ctx.toast.show("抓取完成", "success");
          void load();
        })
        .catch((reason: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(reason), "error");
        });
    },
    onInterval: (feed) => {
      const current = feed.fetch_interval;
      const next = window.prompt(
        `抓取间隔（如 30m、1h、6h）。后端会夹紧到配置的上下限。\n当前：${current}`,
        current,
      );
      if (next === null || next.trim() === "" || next.trim() === current) return;
      void updateFeed(feed.id, { interval: next.trim() }, ctx.signal)
        .then(() => {
          ctx.toast.show("间隔已更新", "success");
          void load();
        })
        .catch((reason: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(reason), "error");
        });
    },
    onDelete: (feed) => {
      const label = feed.title.trim() !== "" ? feed.title : feed.url;
      // 删除会连同条目与本地媒体一起清掉，必须明确确认。
      if (!window.confirm(`删除「${label}」？\n它的条目与已下载的媒体也会一并移除。`)) return;
      void deleteFeed(feed.id, ctx.signal)
        .then(() => {
          ctx.toast.show("已删除", "success");
          void load();
        })
        .catch((reason: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(reason), "error");
        });
    },
  };

  const element = h(
    "div",
    { class: "page" },
    pageHeader("订阅源", "在这里添加与管理订阅；RSS 阅读需要登录，抓取会访问外部地址。", [refresh]),
    section("添加订阅", undefined, createForm),
    host,
  );

  const load = async (): Promise<void> => {
    refresh.disabled = true;
    refresh.classList.add("is-busy");
    try {
      const response = await listFeeds(ctx.signal);
      if (ctx.signal.aborted) return;
      if (response.items.length === 0) {
        host.replaceChildren(
          emptyState({
            title: "还没有订阅源",
            description: "在上面的表单里填一个 feed 地址即可开始。Homehub 宿主推送的订阅也会出现在这里。",
            icon: "rss",
          }),
        );
        return;
      }
      const grid = h("div", { class: "feed-grid" });
      for (const feed of response.items) grid.append(feedCard(feed, callbacks));
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
