import { createPaste } from "../api/pastes";
import type { PasteVisibility } from "../api/types";
import { pageHeader } from "../components/page";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { formatCount } from "../lib/format";
import { describeError } from "../lib/strings";
import type { ViewFactory } from "../router";

const LANGUAGES: ReadonlyArray<readonly [string, string]> = [
  ["text", "纯文本"],
  ["bash", "Bash"],
  ["shell", "Shell"],
  ["caddyfile", "Caddyfile"],
  ["css", "CSS"],
  ["go", "Go"],
  ["html", "HTML"],
  ["javascript", "JavaScript"],
  ["json", "JSON"],
  ["markdown", "Markdown"],
  ["nginx", "Nginx"],
  ["python", "Python"],
  ["sql", "SQL"],
  ["toml", "TOML"],
  ["typescript", "TypeScript"],
  ["yaml", "YAML"],
  ["xml", "XML"],
];

const EXPIRY_OPTIONS: ReadonlyArray<readonly [string, string]> = [
  ["", "服务端默认"],
  ["1h", "1 小时"],
  ["24h", "1 天"],
  ["168h", "7 天"],
  ["720h", "30 天"],
  ["0", "永久"],
];

export const pasteNewView: ViewFactory = (ctx) => {
  const idPrefix = "paste";

  const titleInput = h("input", {
    class: "input",
    attrs: {
      id: `${idPrefix}-title`,
      name: "title",
      type: "text",
      maxlength: "200",
      autocomplete: "off",
      placeholder: "可选，例如：Caddy 反代片段",
    },
  });

  const bodyInput = h("textarea", {
    class: "input textarea textarea--code",
    attrs: {
      id: `${idPrefix}-body`,
      name: "body",
      rows: "18",
      required: true,
      spellcheck: "false",
      placeholder: "在这里粘贴或输入内容…",
      "aria-describedby": `${idPrefix}-body-help`,
    },
  });

  const counter = h("span", { class: "field-counter" }, "0 字符");
  bodyInput.addEventListener("input", () => {
    counter.textContent = `${formatCount(bodyInput.value.length)} 字符`;
  });

  const language = h(
    "select",
    { class: "input", attrs: { id: `${idPrefix}-language`, name: "language" } },
    ...LANGUAGES.map(([value, label]) =>
      h("option", { attrs: { value, ...(value === "text" ? { selected: true } : {}) } }, label),
    ),
  );

  const visibility = h(
    "select",
    { class: "input", attrs: { id: `${idPrefix}-visibility`, name: "visibility" } },
    h("option", { attrs: { value: "unlisted", selected: true } }, "不公开（知道链接即可访问）"),
    h("option", { attrs: { value: "public" } }, "公开（出现在公开列表）"),
    h("option", { attrs: { value: "private" } }, "私有（仅创建者）"),
  );

  const expiry = h(
    "select",
    { class: "input", attrs: { id: `${idPrefix}-expiry`, name: "expires_in" } },
    ...EXPIRY_OPTIONS.map(([value, label]) =>
      h("option", { attrs: { value, ...(value === "" ? { selected: true } : {}) } }, label),
    ),
  );

  const password = h("input", {
    class: "input",
    attrs: {
      id: `${idPrefix}-password`,
      name: "password",
      type: "password",
      autocomplete: "new-password",
      placeholder: "可选",
      "aria-describedby": `${idPrefix}-password-help`,
    },
  });

  const burn = h("input", {
    attrs: { id: `${idPrefix}-burn`, name: "burn_after_read", type: "checkbox" },
  });

  const error = h(
    "p",
    { class: "form-error", attrs: { role: "alert", hidden: true } },
  );
  const setError = (message: string): void => {
    error.textContent = message;
    error.hidden = message === "";
  };

  const submit = h(
    "button",
    { class: "btn btn--primary", attrs: { type: "submit" } },
    icon("paste"),
    "创建片段",
  );

  const fieldset = h(
    "fieldset",
    { class: "fieldset" },
    error,
    h(
      "div",
      { class: "editor-layout" },
      h(
        "div",
        { class: "editor-main stack" },
        h(
          "div",
          { class: "field" },
          h("label", { class: "label", attrs: { for: `${idPrefix}-title` } }, "标题"),
          titleInput,
        ),
        h(
          "div",
          { class: "field" },
          h(
            "div",
            { class: "field-head" },
            h("label", { class: "label", attrs: { for: `${idPrefix}-body` } }, "正文"),
            counter,
          ),
          bodyInput,
          h("p", { class: "field-help", attrs: { id: `${idPrefix}-body-help` } }, "正文不能为空。创建后公开链接为 /p/{slug}。"),
        ),
      ),
      h(
        "aside",
        { class: "editor-side card card--pad" },
        h(
          "div",
          { class: "field" },
          h("label", { class: "label", attrs: { for: `${idPrefix}-language` } }, "语言标记"),
          language,
        ),
        h(
          "div",
          { class: "field" },
          h("label", { class: "label", attrs: { for: `${idPrefix}-visibility` } }, "可见性"),
          visibility,
          h("p", { class: "field-help" }, "公开只影响是否出现在公开列表，不改变链接访问权限。"),
        ),
        h(
          "div",
          { class: "field" },
          h("label", { class: "label", attrs: { for: `${idPrefix}-expiry` } }, "有效期"),
          expiry,
          h("p", { class: "field-help" }, "上限由服务端 pastebin.max_expiry 控制。"),
        ),
        h(
          "div",
          { class: "field" },
          h("label", { class: "label", attrs: { for: `${idPrefix}-password` } }, "访问口令"),
          password,
          h("p", { class: "field-help", attrs: { id: `${idPrefix}-password-help` } }, "留空表示无口令。"),
        ),
        h(
          "label",
          { class: "checkbox-row", attrs: { for: `${idPrefix}-burn` } },
          burn,
          h("span", {}, h("span", { class: "checkbox-title" }, "一次性读取"), h("span", { class: "checkbox-help" }, "首次读取后立即失效。")),
        ),
      ),
    ),
    h(
      "div",
      { class: "form-actions form-actions--end" },
      h("a", { class: "btn btn--ghost", attrs: { href: "/pastes" } }, "取消"),
      submit,
    ),
  );

  const form = h(
    "form",
    { class: "stack-lg", attrs: { novalidate: true } },
    fieldset,
  );

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    setError("");

    const body = bodyInput.value;
    if (body.trim() === "") {
      setError("正文不能为空");
      bodyInput.focus();
      return;
    }

    const visibilityValue = visibility.value;
    const visibilityTyped: PasteVisibility =
      visibilityValue === "public" || visibilityValue === "private" ? visibilityValue : "unlisted";

    fieldset.disabled = true;
    submit.textContent = "正在创建…";

    void (async () => {
      try {
        await createPaste(
          {
            title: titleInput.value.trim().slice(0, 200),
            body,
            language: language.value,
            visibility: visibilityTyped,
            expires_in: expiry.value,
            password: password.value,
            burn_after_read: burn.checked,
          },
          ctx.signal,
        );
        if (ctx.signal.aborted) return;
        ctx.toast.show("片段已创建", "success");
        ctx.router.navigate("/pastes");
      } catch (reason) {
        if (ctx.signal.aborted) return;
        setError(describeError(reason));
      } finally {
        fieldset.disabled = false;
        submit.replaceChildren(icon("paste"), "创建片段");
      }
    })();
  });

  const element = h(
    "div",
    { class: "page" },
    pageHeader("新建片段", "内容只保存在你的服务器上。", [
      h("a", { class: "btn", attrs: { href: "/pastes" } }, icon("arrow-left"), "返回列表"),
    ]),
    form,
  );

  return { element };
};
