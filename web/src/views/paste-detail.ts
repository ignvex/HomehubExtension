import { deletePaste, getPaste } from "../api/pastes";
import type { PasteDetail } from "../api/types";
import { pageHeader } from "../components/page";
import { errorState, loadingBlock } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { pasteUrl } from "../lib/device";
import { formatBytes, formatDateTime, formatRelative, parseTime } from "../lib/format";
import { describeError, visibilityLabel } from "../lib/strings";
import type { ViewFactory } from "../router";

/** 详情页的正文区。等宽显示，保留原始换行与空白。 */
function contentBlock(body: string): HTMLElement {
  const pre = h("pre", { class: "paste-content" });
  const code = h("code", {});
  // textContent 而不是 innerHTML：片段内容是用户输入的原文，
  // 不是 HTML。用 innerHTML 会把 <script> 当标签解释。
  code.textContent = body;
  pre.append(code);
  return pre;
}

function metaRow(label: string, value: string): HTMLElement {
  return h("div", {}, h("dt", {}, label), h("dd", {}, value));
}

export const pasteDetailView: ViewFactory = (ctx) => {
  const host = h("div", { class: "paste-detail-host" }, loadingBlock());
  const back = h(
    "a",
    { class: "btn", attrs: { href: "/pastes" } },
    icon("arrow-left"),
    "返回列表",
  );

  const element = h(
    "div",
    { class: "page" },
    pageHeader("片段详情", "公开链接不带登录态，任何人都能打开。", [back]),
    host,
  );

  const slug = ctx.params.slug ?? "";
  if (slug === "") {
    host.replaceChildren(errorState("缺少片段标识", () => ctx.router.navigate("/pastes")));
    return { element };
  }

  const render = (paste: PasteDetail): void => {
    const title = paste.title.trim() === "" ? paste.slug : paste.title;
    const expired =
      paste.expires_at !== null && (parseTime(paste.expires_at)?.getTime() ?? Infinity) < Date.now();

    const copyLink = h(
      "button",
      { class: "btn btn--sm", attrs: { type: "button" } },
      icon("copy"),
      "复制公开链接",
    );
    copyLink.addEventListener("click", () => {
      const url = pasteUrl(paste.slug);
      void navigator.clipboard
        .writeText(url)
        .then(() => ctx.toast.show("已复制公开链接", "success"))
        .catch(() => ctx.toast.show(`复制失败：${url}`, "error"));
    });

    const openLink = h(
      "a",
      {
        class: "btn btn--sm",
        attrs: { href: `/p/${encodeURIComponent(paste.slug)}`, target: "_blank", rel: "noopener" },
      },
      icon("external"),
      "打开公开页",
    );

    // 删除沿用列表页的两段式确认：第一次点击进入待确认，四秒后自动复位。
    // 用 confirm() 也行，但列表页已经是这个交互，保持一致。
    const remove = h(
      "button",
      { class: "btn btn--sm btn--danger", attrs: { type: "button" } },
      icon("trash"),
      "删除",
    );
    let armed = false;
    let timer: number | null = null;
    const reset = (): void => {
      armed = false;
      remove.classList.remove("is-armed");
      remove.replaceChildren(icon("trash"), "删除");
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    };
    remove.addEventListener("click", () => {
      if (!armed) {
        armed = true;
        remove.classList.add("is-armed");
        remove.replaceChildren(icon("alert"), "确认删除");
        timer = window.setTimeout(reset, 4000);
        return;
      }
      reset();
      remove.disabled = true;
      void deletePaste(paste.slug, ctx.signal)
        .then(() => {
          if (ctx.signal.aborted) return;
          ctx.toast.show("片段已删除", "success");
          ctx.router.navigate("/pastes");
        })
        .catch((error: unknown) => {
          if (ctx.signal.aborted) return;
          ctx.toast.show(describeError(error), "error");
          remove.disabled = false;
        });
    });

    const flags = h(
      "div",
      { class: "flag-row" },
      h(
        "span",
        { class: `badge badge--${paste.visibility === "public" ? "accent" : "muted"}` },
        visibilityLabel(paste.visibility),
      ),
      h("span", { class: "badge badge--mono" }, paste.language || "text"),
      h("span", { class: "badge badge--muted" }, formatBytes(paste.size_bytes)),
      paste.has_password ? h("span", { class: "badge badge--muted" }, icon("lock"), "口令") : null,
      paste.burn_after_read ? h("span", { class: "badge badge--warning" }, "一次性") : null,
      expired ? h("span", { class: "badge badge--danger" }, "已过期") : null,
    );

    const details = h(
      "dl",
      { class: "paste-detail-meta" },
      metaRow("短链", paste.slug),
      metaRow("创建", `${formatDateTime(paste.created_at)}（${formatRelative(paste.created_at)}）`),
      metaRow(
        "到期",
        paste.expires_at ? formatDateTime(paste.expires_at) : "永久",
      ),
    );

    const article = h(
      "article",
      { class: "paste-detail card card--pad" },
      h(
        "header",
        { class: "paste-detail-head" },
        h("h2", { class: "paste-detail-title" }, title),
        h("div", { class: "row-actions" }, copyLink, openLink, remove),
      ),
      flags,
      details,
      contentBlock(paste.body),
    );

    document.title = `${title} · Homehub 扩展`;
    host.replaceChildren(article);
  };

  const load = async (): Promise<void> => {
    host.replaceChildren(loadingBlock());
    try {
      const paste = await getPaste(slug, ctx.signal);
      if (ctx.signal.aborted) return;
      render(paste);
    } catch (error) {
      if (ctx.signal.aborted) return;
      // 404 有两种可能：片段不存在，或者它存在但属于别人（private）。
      // 界面上不必区分——区分反而等于告诉对方「这个 slug 是真的」。
      host.replaceChildren(
        errorState(describeError(error), () => {
          void load();
        }),
      );
    }
  };

  void load();
  return { element };
};
