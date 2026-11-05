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
  parseTime,
} from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewContext, ViewFactory } from "../router";
import { refreshUnreadCount } from "../state/app-state";

const PAGE_SIZE = 50;

type EntryFilter = {
  feedId: number | null;
  unread: boolean;
  starred: boolean;
  search: string;
};

function buildPath(filter: EntryFilter): string {
  const params = new URLSearchParams();
  if (filter.feedId !== null) params.set("feed_id", String(filter.feedId));
  if (filter.unread) params.set("unread", "true");
  if (filter.starred) params.set("starred", "true");
  const q = filter.search.trim();
  if (q !== "") params.set("q", q);
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
  const when = entry.published_at ?? entry.fetched_at;

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
        // datetime 属性要求 ISO 8601；直接塞 Unix 秒是无效值。换算走 parseTime（唯一的 ×1000 处）。
        h("time", { attrs: { datetime: parseTime(when)?.toISOString(), title: formatDateTime(when) } }, formatRelative(when)),
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
  let starredOnly = ctx.query.get("starred") === "true" || ctx.query.get("starred") === "1";
  let searchTerm = ctx.query.get("q") ?? "";

  const feedSelect = h("select", {
    class: "input input--compact",
    attrs: { id: "entry-feed-filter", "aria-label": "按订阅源筛选" },
  });
  const unreadCheckbox = h("input", {
    attrs: { id: "entry-unread-filter", type: "checkbox", ...(unreadOnly ? { checked: true } : {}) },
  });
  const starredCheckbox = h("input", {
    attrs: { id: "entry-starred-filter", type: "checkbox", ...(starredOnly ? { checked: true } : {}) },
  });
  const searchInput = h("input", {
    class: "input input--compact",
    attrs: {
      id: "entry-search",
      type: "search",
      placeholder: "搜索标题与正文",
      "aria-label": "搜索条目",
      autocomplete: "off",
      value: searchTerm,
    },
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
      h("label", { class: "toolbar-label", attrs: { for: "entry-search" } }, icon("search"), "搜索"),
      searchInput,
    ),
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
    h(
      "label",
      { class: "checkbox-row checkbox-row--inline", attrs: { for: "entry-starred-filter" } },
      starredCheckbox,
      h("span", {}, "只看标星"),
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
  // 每次重置加载自增。搜索词变化或 SSE 触发的重置不能因为上一轮还在途就被丢掉，
  // 否则列表停在旧搜索词的结果上；改为让新一轮接管、旧一轮回来时丢弃。
  let generation = 0;

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
    const filtered = feedFilter !== null || unreadOnly || starredOnly || searchTerm.trim() !== "";
    // 搜索/标星为空时的文案与「没有未读」不同：前者是筛选条件太窄，
    // 后者是正经地全读完了。引导语给错会让人以为功能坏了。
    let title = "还没有条目";
    let description = "订阅由 Homehub 推送，抓取完成后会出现在这里。";
    if (searchTerm.trim() !== "") {
      title = "没有匹配的条目";
      description = `没有标题或正文包含「${searchTerm.trim()}」的条目。`;
    } else if (starredOnly) {
      title = "还没有标星的条目";
      description = "在列表里点星标图标，条目会出现在这里。";
    } else if (unreadOnly) {
      title = "没有未读条目";
      description = "所有条目都已读完，或者这个筛选条件暂时没有匹配项。";
    } else if (filtered) {
      description = "这个订阅源还没有抓取到条目。";
    }

    list.replaceChildren(
      emptyState({
        title,
        description,
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

  const removeRow = (entry: EntrySummary, row: HTMLElement): void => {
    seen.delete(entry.id);
    row.remove();
    count = Math.max(0, count - 1);
    if (count === 0) {
      showEmpty();
      footer.replaceChildren();
    } else {
      updateFooter();
    }
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
          // 在「只看标星」列表里取消标星，该条已不满足筛选条件。
          // 不移除的话列表会显示一条与筛选条件矛盾的内容。
          if (starredOnly && !next) {
            const row = button.closest<HTMLElement>(".entry-row");
            if (row) removeRow(entry, row);
          }
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
          if (unreadOnly && next) removeRow(entry, row);
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
    if (loading && !reset) return;
    if (reset) generation += 1;
    const current = generation;
    const isStale = (): boolean => ctx.signal.aborted || current !== generation;
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
      starred: starredOnly,
      search: searchTerm,
      limit: PAGE_SIZE,
      ...(reset || !cursor ? {} : { cursor }),
      signal: ctx.signal,
    });

    try {
      const [feedResponse, entryResponse] = await Promise.all([feedsPromise, entriesPromise]);
      if (isStale()) return;

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
      const suffix = searchTerm.trim() !== "" ? "（搜索）" : unreadOnly ? "未读" : starredOnly ? "标星" : "";
      summary.textContent = `${count} 条${suffix}`;
      if (count === 0) showEmpty();
      updateFooter();
    } catch (error) {
      if (isStale()) return;
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
      // 过期的一轮不能清掉接管它的那一轮的 loading 状态。
      if (current === generation) {
        loading = false;
        refresh.disabled = false;
        refresh.classList.remove("is-busy");
      }
    }
  };

  feedSelect.addEventListener("change", () => {
    feedFilter = feedSelect.value === "" ? null : Number(feedSelect.value);
    ctx.router.navigate(buildPath({ feedId: feedFilter, unread: unreadOnly, starred: starredOnly, search: searchTerm }));
  });
  unreadCheckbox.addEventListener("change", () => {
    unreadOnly = unreadCheckbox.checked;
    ctx.router.navigate(buildPath({ feedId: feedFilter, unread: unreadOnly, starred: starredOnly, search: searchTerm }));
  });
  starredCheckbox.addEventListener("change", () => {
    starredOnly = starredCheckbox.checked;
    ctx.router.navigate(buildPath({ feedId: feedFilter, unread: unreadOnly, starred: starredOnly, search: searchTerm }));
  });

  // 搜索不能走 router.navigate：那会重新挂载整个视图，输入框丢焦点、
  // 光标跳回开头——边打字边被重置。这里改成「原地重载 + replaceState」，
  // 既能刷新列表又能保持输入。用 replace 而不是 push，
  // 否则每敲一个字都会往历史里压一条，返回键要按十几下。
  let searchTimer: number | null = null;
  const applySearch = (): void => {
    const next = searchInput.value;
    if (next === searchTerm) return;
    searchTerm = next;
    const target = buildPath({ feedId: feedFilter, unread: unreadOnly, starred: starredOnly, search: searchTerm });
    history.replaceState({ scrollY: 0 }, "", target);
    void load(true);
  };

  searchInput.addEventListener("input", () => {
    if (searchTimer !== null) window.clearTimeout(searchTimer);
    // 300ms：中文输入法组字期间不该发请求，输入法上屏后手也差不多停了。
    searchTimer = window.setTimeout(() => {
      searchTimer = null;
      applySearch();
    }, 300);
  });
  // 回车立即搜，不等 debounce。
  searchInput.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    if (searchTimer !== null) {
      window.clearTimeout(searchTimer);
      searchTimer = null;
    }
    applySearch();
  });
  // 搜索框的清除按钮（type=search 自带）也会触发 input，
  // 但某些浏览器只发 search 事件。两个都接，靠 applySearch 去重。
  searchInput.addEventListener("search", applySearch);

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

  return {
    element,
    unmount: () => {
      if (searchTimer !== null) window.clearTimeout(searchTimer);
      unsubscribe();
    },
  };
};
