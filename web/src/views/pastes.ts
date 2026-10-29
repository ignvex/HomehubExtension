import { deletePaste, listPastes } from "../api/pastes";
import type { PasteSummary } from "../api/types";
import { pageHeader } from "../components/page";
import { emptyState, errorState, skeletonRows } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { pasteUrl } from "../lib/device";
import { formatBytes, formatDateTime, formatRelative, parseTime } from "../lib/format";
import { describeError, visibilityLabel } from "../lib/strings";
import type { ViewFactory } from "../router";

const PAGE_SIZE = 30;

function pasteRow(
  paste: PasteSummary,
  ctx: Parameters<ViewFactory>[0],
  onDeleted: () => void,
  timers: Set<number>,
): HTMLElement {
  const title = paste.title.trim() === "" ? paste.slug : paste.title;
  const expired = paste.expires_at !== null && (parseTime(paste.expires_at)?.getTime() ?? Infinity) < Date.now();

  const flags = h(
    "div",
    { class: "flag-row" },
    h("span", { class: `badge badge--${paste.visibility === "public" ? "accent" : "muted"}` }, visibilityLabel(paste.visibility)),
    h("span", { class: "badge badge--mono" }, paste.language || "text"),
    h("span", { class: "badge badge--muted" }, formatBytes(paste.size_bytes)),
    paste.has_password ? h("span", { class: "badge badge--muted" }, icon("lock"), "口令") : null,
    paste.burn_after_read ? h("span", { class: "badge badge--warning" }, "一次性") : null,
    expired ? h("span", { class: "badge badge--danger" }, "已过期") : null,
  );

  const copy = h(
    "button",
    { class: "icon-button", attrs: { type: "button", title: "复制公开链接", "aria-label": "复制公开链接" } },
    icon("copy"),
  );
  copy.addEventListener("click", () => {
    const url = pasteUrl(paste.slug);
    void navigator.clipboard
      .writeText(url)
      .then(() => ctx.toast.show("已复制公开链接", "success"))
      .catch(() => ctx.toast.show(`复制失败，公开链接：${url}`, "error"));
  });

  const remove = h(
    "button",
    {
      class: "icon-button icon-button--danger",
      attrs: { type: "button", title: "删除片段", "aria-label": "删除片段" },
    },
    icon("trash"),
  );
  let armed = false;
  let timer: number | null = null;
  const reset = (): void => {
    armed = false;
    remove.classList.remove("is-armed");
    remove.replaceChildren(icon("trash"));
    remove.setAttribute("aria-label", "删除片段");
    if (timer !== null) {
      timers.delete(timer);
      timer = null;
    }
  };

  remove.addEventListener("click", () => {
    if (!armed) {
      armed = true;
      remove.classList.add("is-armed");
      remove.replaceChildren(icon("alert"));
      remove.setAttribute("aria-label", "再次点击确认删除");
      timer = window.setTimeout(reset, 4000);
      timers.add(timer);
      return;
    }
    reset();
    remove.disabled = true;
    void deletePaste(paste.slug, ctx.signal)
      .then(() => {
        if (ctx.signal.aborted) return;
        ctx.toast.show("片段已删除", "success");
        onDeleted();
      })
      .catch((error: unknown) => {
        if (ctx.signal.aborted) return;
        ctx.toast.show(describeError(error), "error");
        remove.disabled = false;
      });
  });

  return h(
    "article",
    { class: `paste-row${expired ? " is-expired" : ""}` },
    h(
      "div",
      { class: "paste-main" },
      h("h2", { class: "paste-title" }, title),
      h(
        "p",
        { class: "paste-meta" },
        h("span", { class: "mono" }, paste.slug),
        h("span", { attrs: { "aria-hidden": "true" } }, "·"),
        h("span", {}, `${formatRelative(paste.created_at)}创建`),
        paste.expires_at
          ? h("span", { class: "paste-expiry" }, `· ${formatDateTime(paste.expires_at)} 过期`)
          : h("span", {}, "· 永久"),
      ),
      flags,
    ),
    h("div", { class: "row-actions" }, copy, remove),
  );
}

export const pastesView: ViewFactory = (ctx) => {
  const list = h("div", { class: "paste-list" });
  const footer = h("div", { class: "pagination" });
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );
  const createLink = h(
    "a",
    { class: "btn btn--primary", attrs: { href: "/pastes/new" } },
    icon("plus"),
    "新建片段",
  );
  const element = h(
    "div",
    { class: "page" },
    pageHeader("片段", "短文本、配置片段与一次性分享。公开链接不带登录态。", [refresh, createLink]),
    h("div", { class: "stack-lg" }, list, footer),
  );

  const timers = new Set<number>();
  let cursor: string | null = null;
  let loading = false;
  let count = 0;
  const rows = new Map<string, HTMLElement>();

  const updateFooter = (): void => {
    footer.replaceChildren();
    if (cursor === null) {
      if (count > 0) {
        footer.append(h("p", { class: "muted small" }, `已加载 ${count} 条`));
      }
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
    list.replaceChildren(
      emptyState({
        title: "还没有片段",
        description: "新建第一个片段，公开链接可以在任何设备打开。",
        icon: "paste",
        action: h("a", { class: "btn btn--primary", attrs: { href: "/pastes/new" } }, icon("plus"), "新建片段"),
      }),
    );
  };

  const load = async (reset: boolean): Promise<void> => {
    if (loading) return;
    loading = true;
    refresh.disabled = true;
    refresh.classList.add("is-busy");

    if (reset) {
      cursor = null;
      count = 0;
      rows.clear();
      list.replaceChildren(skeletonRows(4));
      footer.replaceChildren();
    }

    try {
      const response = await listPastes({
        limit: PAGE_SIZE,
        ...(reset ? {} : cursor ? { cursor } : {}),
        signal: ctx.signal,
      });
      if (ctx.signal.aborted) return;

      if (reset) list.replaceChildren();
      const fragment = document.createDocumentFragment();
      for (const paste of response.items) {
        if (rows.has(paste.slug)) continue;
        const row = pasteRow(
          paste,
          ctx,
          () => {
            rows.delete(paste.slug);
            row.remove();
            count = Math.max(0, count - 1);
            if (count === 0) {
              showEmpty();
              footer.replaceChildren();
            } else {
              updateFooter();
            }
          },
          timers,
        );
        rows.set(paste.slug, row);
        count += 1;
        fragment.append(row);
      }
      list.append(fragment);

      cursor = response.next_cursor ?? null;
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

  refresh.addEventListener("click", () => {
    void load(true);
  });
  void load(true);

  return {
    element,
    unmount: () => {
      for (const timer of timers) window.clearTimeout(timer);
      timers.clear();
    },
  };
};
