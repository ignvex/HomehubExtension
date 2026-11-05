import { getEntry, patchEntry } from "../api/entries";
import { listFeeds } from "../api/feeds";
import type { EntryDetail, Feed } from "../api/types";
import { errorState } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import {
  formatDateTime,
  formatRelative,
  nowSeconds,
  parseTime,
} from "../lib/format";
import { describeError } from "../lib/strings";
import { safeHref } from "../lib/url";
import type { ViewFactory } from "../router";
import { refreshUnreadCount } from "../state/app-state";

function readerSkeleton(): HTMLElement {
  return h(
    "div",
    { class: "reader-loading", attrs: { "aria-hidden": "true" } },
    h("div", { class: "skeleton skeleton--line" }),
    h("div", { class: "skeleton skeleton--title-lg" }),
    h("div", { class: "skeleton skeleton--line" }),
    h("div", { class: "skeleton skeleton--line" }),
    h("div", { class: "skeleton skeleton--short" }),
    h("div", { class: "skeleton skeleton--title" }),
    h("div", { class: "skeleton skeleton--line" }),
    h("div", { class: "skeleton skeleton--line" }),
    h("div", { class: "skeleton skeleton--short" }),
  );
}

export const readerView: ViewFactory = (ctx) => {
  const host = h("div", { class: "reader-host" }, readerSkeleton());
  const element = h("div", { class: "page page--reader" }, host);
  const rawId = ctx.params.id ?? "";
  const id = /^\d+$/u.test(rawId) ? Number(rawId) : Number.NaN;

  const backLink = (): HTMLElement =>
    h(
      "a",
      { class: "btn btn--ghost btn--sm", attrs: { href: "/entries" } },
      icon("arrow-left"),
      "返回条目",
    );

  if (!Number.isSafeInteger(id) || id <= 0) {
    host.replaceChildren(
      errorState("条目 ID 无效", () => {
        ctx.router.navigate("/entries");
      }),
      h("div", { class: "reader-back" }, backLink()),
    );
    return { element };
  }

  const render = (entry: EntryDetail, feed: Feed | null): void => {
    const title = entry.title.trim() === "" ? "（无标题）" : entry.title;
    const feedLabel = feed ? (feed.title.trim() !== "" ? feed.title : feed.url) : `订阅源 #${entry.feed_id}`;

    const starButton = h("button", {
      class: "btn btn--sm",
      attrs: { type: "button", "aria-pressed": String(entry.starred_at !== null) },
    });
    const paintStar = (): void => {
      const starred = entry.starred_at !== null;
      starButton.replaceChildren(icon(starred ? "star-filled" : "star"), starred ? "取消标星" : "标星");
      starButton.setAttribute("aria-pressed", String(starred));
    };
    paintStar();
    starButton.addEventListener("click", () => {
      if (starButton.disabled) return;
      const next = entry.starred_at === null;
      starButton.disabled = true;
      void patchEntry(id, { starred: next }, ctx.signal)
        .then(() => {
          entry.starred_at = next ? nowSeconds() : null;
          paintStar();
        })
        .catch((error: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(error), "error");
        })
        .finally(() => {
          starButton.disabled = false;
        });
    });

    const readButton = h("button", {
      class: "btn btn--sm",
      attrs: { type: "button" },
    });
    const paintRead = (): void => {
      const currentlyRead = entry.read_at !== null;
      readButton.replaceChildren(icon(currentlyRead ? "circle" : "check"), currentlyRead ? "标为未读" : "标为已读");
    };
    paintRead();
    readButton.addEventListener("click", () => {
      if (readButton.disabled) return;
      const next = entry.read_at === null;
      readButton.disabled = true;
      void patchEntry(id, { read: next }, ctx.signal)
        .then(() => {
          entry.read_at = next ? nowSeconds() : null;
          paintRead();
          void refreshUnreadCount();
        })
        .catch((error: unknown) => {
          if (!ctx.signal.aborted) ctx.toast.show(describeError(error), "error");
        })
        .finally(() => {
          readButton.disabled = false;
        });
    });

    const originalHref = safeHref(entry.url);
    const originalLink = originalHref
      ? h(
          "a",
          {
            class: "btn btn--sm",
            attrs: { href: originalHref, target: "_blank", rel: "noopener noreferrer" },
          },
          icon("external"),
          "原文",
        )
      : null;

    const body = h("div", { class: "reader-body" });
    const html = entry.content_html.trim();
    if (html !== "") {
      // 唯一允许使用 innerHTML 的位置：content_html 已由服务端按白名单清洗。
      body.innerHTML = entry.content_html;
    } else if (entry.content_text.trim() !== "") {
      body.append(
        h("p", { class: "notice notice--muted" }, "服务端没有提供可渲染的 HTML，以下为纯文本正文。"),
        h("pre", { class: "reader-plaintext" }, entry.content_text),
      );
    } else {
      body.append(h("p", { class: "muted" }, "正文为空。"));
    }

    // 正文链接一律在新标签打开，避免读者离开阅读视图；服务端已完成域名白名单清洗。
    for (const anchor of body.querySelectorAll<HTMLAnchorElement>("a[href]")) {
      const href = anchor.getAttribute("href") ?? "";
      try {
        const url = new URL(href, window.location.href);
        if (url.origin !== window.location.origin) {
          anchor.target = "_blank";
          anchor.rel = "noopener noreferrer";
        }
      } catch {
        anchor.removeAttribute("href");
      }
    }
    for (const image of body.querySelectorAll<HTMLImageElement>("img")) {
      if (!image.hasAttribute("loading")) image.setAttribute("loading", "lazy");
      if (!image.hasAttribute("decoding")) image.setAttribute("decoding", "async");
    }

    // GET /entries/{id} 已在服务端标记已读；响应里的 read_at 是写入前的快照，这里对齐。
    if (entry.read_at === null) {
      entry.read_at = nowSeconds();
      paintRead();
      void refreshUnreadCount();
    }

    const article = h(
      "article",
      { class: "reader" },
      h(
        "header",
        { class: "reader-head" },
        h(
          "div",
          { class: "reader-top" },
          backLink(),
          h("div", { class: "reader-top-actions" }, originalLink, starButton, readButton),
        ),
        h("p", { class: "reader-feed" }, feedLabel),
        h("h1", { class: "reader-title" }, title),
        h(
          "p",
          { class: "reader-meta" },
          entry.author.trim() !== "" ? h("span", {}, entry.author) : null,
          h(
            "time",
            {
              attrs: {
                // datetime 要求 ISO 8601，不能直接放 Unix 秒。
                datetime: parseTime(entry.published_at ?? entry.fetched_at)?.toISOString(),
                title: formatDateTime(entry.published_at ?? entry.fetched_at),
              },
            },
            `发布于 ${formatRelative(entry.published_at ?? entry.fetched_at)}`,
          ),
        ),
      ),
      body,
    );

    document.title = `${title} · Homehub 扩展`;
    host.replaceChildren(article);
  };

  const load = async (): Promise<void> => {
    host.replaceChildren(readerSkeleton());
    try {
      const [entry, feeds] = await Promise.all([
        getEntry(id, ctx.signal),
        listFeeds(ctx.signal).catch(() => null),
      ]);
      if (ctx.signal.aborted) return;
      const feed = feeds?.items.find((item) => item.id === entry.feed_id) ?? null;
      render(entry, feed);
    } catch (error) {
      if (ctx.signal.aborted) return;
      host.replaceChildren(
        errorState(describeError(error), () => {
          void load();
        }),
        h("div", { class: "reader-back" }, backLink()),
      );
    }
  };

  void load();
  return { element };
};
