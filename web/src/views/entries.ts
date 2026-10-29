import { listEntries, patchEntry } from "../api/entries";
import { listFeeds } from "../api/feeds";
import type { EntrySummary, Feed } from "../api/types";
import { pageHeader } from "../components/page";
import { emptyState, errorState, skeletonRows } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import {
  formatDateTime,
  formatRelative,
  nowSeconds,
} from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewContext, ViewFactory } from "../router";
import { refreshUnreadCount } from "../state/app-state";

const PAGE_SIZE = 50;

function buildPath(feedId: number | null, unread: boolean): string {
  const params = new URLSearchParams();
  if (feedId !== null) params.set("feed_id", String(feedId));
  if (unread) params.set("unread", "true");
  const query = params.toString();
  return `/entries${query ? `?${query}` : ""}`;
}

function feedName(feeds: Map<number, Feed>, feedId: number): string {
  const feed = feeds.get(feedId);
  if (!feed) return `订阅源 #${feedId}`;
  return feed.title.trim() !== "" ? feed.title : feed.url;
}

interface EntryRowHandlers {
  onStar(entry: EntrySummary, button: HTMLButtonElement): void;
  onRead(entry: EntrySummary, button: HTMLButtonElement, row: HTMLElement): void;
}

function renderStars(button: HTMLButtonElement, starred: boolean): void {
  button.replaceChildren(icon(starred ? "star-filled" : "star"));
  button.classList.toggle("is-active", starred);
  button.setAttribute("aria-pressed", String(starred));
  button.setAttribute("aria-label", starred ? "取消标星" : "标星");
  button.setAttribute("title", starred ? "取消标星" : "标星");
}

function entryRow(
  entry: EntrySummary,
  feeds: Map<number, Feed>,
  handlers: EntryRowHandlers,
): HTMLElement {
  const title = entry.title.trim() === "" ? "（无标题）" : entry.title;
  const read = entry.read_at !== null;
  const starred = entry.starred_at !== null;

  const starButton = h(
    "button",
    { class: "icon-button entry-action", attrs: { type: "button" } },
    icon(starred ? "star-filled" : "star"),
  );
  renderStars(starButton, starred);
  starButton.addEventListener("click", () => handlers.onStar(entry, starButton));

  const readButton = h(
    "button",
    {
      class: "icon-button entry-action",
      attrs: {
        type: "button",
        title: read ? "标为未读" : "标为已读",
        "aria-label": read ? "标为未读" : "标为已读",
      },
    },
    icon(read ? "circle" : "check"),
  );

  const row = h(
    "article",
    { class: `entry-row${read ? "" : " is-unread"}` },
    h(
      "div",
      { class: "entry-main" },
      h("a", { class: "entry-title", attrs: { href: `/entries/${entry.id}` } }, title),
      h(
        "p",
        { class: "entry-meta" },
        h("span", {}, feedName(feeds, entry.feed_id)),
        h("span", { attrs: { "aria-hidden": "true" } }, "·"),
        h("time", { attrs: { datetime: entry.published_at ?? entry.fetched_at, title: formatDateTime(entry.published_at ?? entry.fetched_at) } }, formatRelative(entry.published_at ?? entry.fetched_at)),
        entry.author.trim() !== "" ? h("span", { class: "entry-author" }, `· ${entry.author}`) : null,
      ),
      entry.summary.trim() !== ""
        ? h("p", { class: "entry-summary" }, entry.summary)
        : null,
    ),
    h("div", { class: "entry-actions" }, starButton, readButton),
  );

  readButton.addEventListener("click", () => handlers.onRead(entry, readButton, row));
  return row;
}

export const entriesView: ViewFactory = (ctx: ViewContext) => {
  const feedIdRaw = ctx.query.get("feed_id");
  const parsedFeedId = feedIdRaw ? Number.parseInt(feedIdRaw, 10) : Number.NaN;
  let feedFilter: number | null = Number.isSafeInteger(parsedFeedId) ? parsedFeedId : null;
  let unreadOnly = ctx.query.get("unread") === "true" || ctx.query.get("unread") === "1";

  const feedSelect = h("select", {
    class: "input input--compact",
    attrs: { id: "entry-feed-filter", "aria-label": "按订阅源筛选" },
  });
  const unreadCheckbox = h("input", {
    attrs: { id: "entry-unread-filter", type: "checkbox", ...(unreadOnly ? { checked: true } : {}) },
  });
  const summary = h("p", { class: "toolbar-summary" });
  const list = h("div", { class: "entry-list" });
  const footer = h("div", { class: "pagination" });
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );

  const toolbar = h(
    "div",
    { class: "toolbar" },
    h(
      "div",
      { class: "toolbar-field" },
      h("label", { class: "toolbar-label", attrs: { for: "entry-feed-filter" } }, icon("filter"), "订阅源"),
      feedSelect,
    ),
    h(
      "label",
      { class: "checkbox-row checkbox-row--inline", attrs: { for: "entry-unread-filter" } },
      unreadCheckbox,
      h("span", {}, "只看未读"),
    ),
    summary,
  );

  const element = h(
    "div",
    { class: "page" },
    pageHeader("条目", "正文中的图片、视频与样式均已本地化，阅读时不产生第三方请求。", [refresh]),
    toolbar,
    h("div", { class: "stack-lg" }, list, footer),
  );

  const feeds = new Map<number, Feed>();
  const seen = new Set<number>();
  let cursor: string | null = null;
  let count = 0;
  let loading = false;

  const fillFeedSelect = (): void => {
    const options: HTMLElement[] = [
      h("option", { attrs: { value: "" } }, "全部订阅源"),
    ];
    for (const feed of [...feeds.values()].sort((a, b) => a.id - b.id)) {
      const label = feed.title.trim() !== "" ? feed.title : feed.url;
      options.push(
        h(
          "option",
          {
            attrs: {
              value: String(feed.id),
              ...(feedFilter === feed.id ? { selected: true } : {}),
            },
          },
          label,
        ),
      );
    }
    feedSelect.replaceChildren(...options);
    feedSelect.value = feedFilter === null ? "" : String(feedFilter);
  };

  const updateFooter = (): void => {
    footer.replaceChildren();
    if (cursor === null) {
      if (count > 0) footer.append(h("p", { class: "muted small" }, `已加载 ${count} 条`));
      return;
    }
    const more = h(
      "button",
      { class: "btn", attrs: { type: "button" } },
      icon("chevron-right"),
      "加载更多",
    );
    more.addEventListener("click", () => {
      void load(false);
    });
    footer.append(more);
  };

  const showEmpty = (): void => {
    const filtered = feedFilter !== null || unreadOnly;
    list.replaceChildren(
      emptyState({
        title: unreadOnly ? "没有未读条目" : "还没有条目",
        description: unreadOnly
          ? "所有条目都已读完，或者这个筛选条件暂时没有匹配项。"
          : filtered
            ? "这个订阅源还没有抓取到条目。"
            : "订阅由 Homehub 推送，抓取完成后会出现在这里。",
        icon: "inbox",
        action: filtered
          ? h("a", { class: "btn", attrs: { href: "/entries" } }, "查看全部条目")
          : h(
              "button",
              {
                class: "btn",
                attrs: { type: "button" },
                on: { click: () => void load(true) },
              },
              icon("refresh"),
              "刷新",
            ),
      }),
    );
  };

  const updateReadButton = (button: HTMLButtonElement, read: boolean): void => {
    button.replaceChildren(icon(read ? "circle" : "check"));
    button.setAttribute("title", read ? "标为未读" : "标为已读");
    button.setAttribute("aria-label", read ? "标为未读" : "标为已读");
  };

  const handlers: EntryRowHandlers = {
    onStar(entry, button) {
      if (button.disabled) return;
      const next = entry.starred_at === null;
      button.disabled = true;
      void patchEntry(entry.id, { starred: next }, ctx.signal)
        .then(() => {
          entry.starred_at = next ? nowSeconds() : null;
          renderStars(button, next);
        })
        .catch((error: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(error), "error");
        })
        .finally(() => {
          button.disabled = false;
        });
    },
    onRead(entry, button, row) {
      if (button.disabled) return;
      const next = entry.read_at === null;
      button.disabled = true;
      void patchEntry(entry.id, { read: next }, ctx.signal)
        .then(() => {
          entry.read_at = next ? nowSeconds() : null;
          updateReadButton(button, next);
          row.classList.toggle("is-unread", !next);
          if (unreadOnly && next) {
            seen.delete(entry.id);
            row.remove();
            count = Math.max(0, count - 1);
            if (count === 0) {
              showEmpty();
              footer.replaceChildren();
            } else {
              updateFooter();
            }
          }
          void refreshUnreadCount();
        })
        .catch((error: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(error), "error");
        })
        .finally(() => {
          button.disabled = false;
        });
    },
  };

  const load = async (reset: boolean): Promise<void> => {
    if (loading) return;
    loading = true;
    refresh.disabled = true;
    refresh.classList.add("is-busy");

    if (reset) {
      cursor = null;
      count = 0;
      seen.clear();
      list.replaceChildren(skeletonRows(5));
      footer.replaceChildren();
      summary.textContent = "";
    }

    const feedsPromise = listFeeds(ctx.signal).catch(() => null);
    const entriesPromise = listEntries({
      feedId: feedFilter,
      unread: unreadOnly,
      limit: PAGE_SIZE,
      ...(reset || !cursor ? {} : { cursor }),
      signal: ctx.signal,
    });

    try {
      const [feedResponse, entryResponse] = await Promise.all([feedsPromise, entriesPromise]);
      if (ctx.signal.aborted) return;

      if (feedResponse) {
        feeds.clear();
        for (const feed of feedResponse.items) feeds.set(feed.id, feed);
        fillFeedSelect();
      }

      if (reset) list.replaceChildren();
      const fragment = document.createDocumentFragment();
      for (const entry of entryResponse.items) {
        if (seen.has(entry.id)) continue;
        seen.add(entry.id);
        count += 1;
        fragment.append(entryRow(entry, feeds, handlers));
      }
      list.append(fragment);

      cursor = entryResponse.next_cursor ?? null;
      summary.textContent = `${count} 条${unreadOnly ? "未读" : ""}`;
      if (count === 0) showEmpty();
      updateFooter();
    } catch (error) {
      if (ctx.signal.aborted) return;
      if (reset) {
        list.replaceChildren(
          errorState(describeError(error), () => {
            void load(true);
          }),
        );
      } else {
        ctx.toast.show(describeError(error), "error");
      }
    } finally {
      loading = false;
      refresh.disabled = false;
      refresh.classList.remove("is-busy");
    }
  };

  feedSelect.addEventListener("change", () => {
    feedFilter = feedSelect.value === "" ? null : Number(feedSelect.value);
    ctx.router.navigate(buildPath(feedFilter, unreadOnly));
  });
  unreadCheckbox.addEventListener("change", () => {
    unreadOnly = unreadCheckbox.checked;
    ctx.router.navigate(buildPath(feedFilter, unreadOnly));
  });
  refresh.addEventListener("click", () => {
    void load(true);
  });

  fillFeedSelect();
  void load(true);

  // 新条目到达时刷新列表。只在这一页可见时刷新：
  // 用户读到一半内容被插入的列表打断，比数据晚几秒更烦人。
  const unsubscribe = ctx.events((event) => {
    if (event.type !== "entry.created") return;
    const data = event.data as { feed_id?: number } | null;
    if (feedFilter !== null && data?.feed_id !== undefined && data.feed_id !== feedFilter) {
      return;
    }
    if (document.visibilityState !== "visible") return;
    void load(true);
  });

  return { element, unmount: unsubscribe };
};
