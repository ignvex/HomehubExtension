import { enrollThisDevice, forgetDevice, getSession, hasLocalDeviceKey, loginWithDevice } from "../auth/session";
import { webCryptoAvailable } from "../auth/keys";
import type { SessionSnapshot } from "../auth/session";
import { loadingBlock } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { suggestDeviceName } from "../lib/device";
import { APP_NAME, describeError } from "../lib/strings";
import type { ViewFactory } from "../router";

function safeReturnTo(value: string | null): string {
  if (!value || !value.startsWith("/") || value.startsWith("//")) return "/";
  return value;
}

/**
 * 配对码规范化：去掉分隔符、转大写，补回 HH- 前缀并每 4 位分组。
 * 输入不足 12 位时原样保留，避免用户打字过程中光标与内容被强行改写。
 */
export function normalizePairingToken(raw: string): string {
  const compact = raw.toUpperCase().replace(/[^0-9A-Z]/gu, "");
  let body = compact;
  if (compact.length >= 14 && compact.startsWith("HH")) {
    body = compact.slice(2, 14);
  } else if (compact.length >= 12) {
    body = compact.slice(0, 12);
  } else {
    return raw.trim().toUpperCase();
  }
  const groups = body.match(/.{1,4}/gu)?.join("-") ?? body;
  return `HH-${groups}`;
}

function brand(): HTMLElement {
  return h(
    "div",
    { class: "auth-brand" },
    h("span", { class: "brand-mark", attrs: { "aria-hidden": "true" } }, icon("key")),
    h("div", {}, h("p", { class: "auth-eyebrow" }, APP_NAME), h("h1", { class: "auth-title" }, "设备配对")),
  );
}

export const pairView: ViewFactory = (ctx) => {
  const returnTo = safeReturnTo(ctx.query.get("returnTo"));
  const panelHost = h("div", { class: "auth-panel", attrs: { "aria-live": "polite" } });
  const element = h("div", { class: "auth-page" }, brand(), panelHost);

  const go = (): void => {
    ctx.router.navigate(returnTo, { replace: true });
  };

  const errorBox = (message = ""): HTMLElement => {
    const box = h(
      "p",
      { class: "form-error", attrs: { role: "alert", ...(message ? {} : { hidden: true }) } },
      message,
    );
    return box;
  };

  const setError = (box: HTMLElement, message: string): void => {
    box.textContent = message;
    box.hidden = message === "";
  };

  const notice = (text: string): HTMLElement => h("p", { class: "notice" }, icon("lock"), h("span", {}, text));

  const renderSignedIn = (session: SessionSnapshot, keyExists: boolean): void => {
    const client = session.client;
    const body = h(
      "div",
      { class: "auth-body" },
      h("p", { class: "auth-lead" }, "本设备已配对。访问令牌只保存在内存里，刷新页面会自动续期。"),
      h(
        "dl",
        { class: "detail-list detail-list--compact" },
        h("div", {}, h("dt", {}, "设备名称"), h("dd", {}, client?.name ?? "—")),
        h("dt", {}, "设备 ID"),
        h("dd", { class: "mono break-all" }, client?.id ?? "—"),
      ),
      keyExists
        ? null
        : h(
            "p",
            { class: "notice notice--warning" },
            icon("alert"),
            h("span", {}, "本机私钥已丢失。当前会话仍然有效，但过期后需要重新配对。"),
          ),
    );

    const actions = h("div", { class: "form-actions" });
    const enter = h(
      "button",
      { class: "btn btn--primary", attrs: { type: "button" } },
      icon("arrow-left"),
      returnTo === "/" ? "进入工具台" : "返回原页面",
    );
    enter.addEventListener("click", go);
    actions.append(enter);

    if (keyExists) {
      const relogin = h(
        "button",
        { class: "btn", attrs: { type: "button" } },
        icon("refresh"),
        "重新登录",
      );
      relogin.addEventListener("click", () => {
        relogin.disabled = true;
        void loginWithDevice(ctx.signal)
          .then(() => {
            ctx.toast.show("已重新登录", "success");
            go();
          })
          .catch((error: unknown) => ctx.toast.show(describeError(error), "error"))
          .finally(() => {
            relogin.disabled = false;
          });
      });
      actions.append(relogin);
    }

    const forget = h(
      "button",
      { class: "btn btn--danger", attrs: { type: "button" } },
      icon("trash"),
      "清除本机设备",
    );
    let armed = false;
    let timer: number | null = null;
    forget.addEventListener("click", () => {
      if (!armed) {
        armed = true;
        forget.textContent = "确认清除本机密钥";
        timer = window.setTimeout(() => {
          armed = false;
          forget.replaceChildren(icon("trash"), "清除本机设备");
        }, 4000);
        return;
      }
      if (timer !== null) window.clearTimeout(timer);
      forget.disabled = true;
      void forgetDevice()
        .then(() => {
          ctx.toast.show("已清除本机密钥", "success");
          renderEnroll();
        })
        .catch((error: unknown) => {
          ctx.toast.show(describeError(error), "error");
          forget.disabled = false;
        });
    });
    actions.append(forget);

    body.append(actions, notice("设备在服务端仍然有效；清除后需要用新的配对码重新登记密钥。"));
    panelHost.replaceChildren(
      h("div", { class: "auth-card" }, h("h2", { class: "auth-card-title" }, "已配对"), body),
    );
  };

  const renderLogin = (): void => {
    const error = errorBox();
    const login = h(
      "button",
      { class: "btn btn--primary btn--wide", attrs: { type: "button" } },
      icon("key"),
      "使用本机密钥登录",
    );
    const forget = h(
      "button",
      { class: "btn btn--ghost btn--wide", attrs: { type: "button" } },
      "清除本机密钥并重新配对",
    );

    login.addEventListener("click", () => {
      error.hidden = true;
      login.disabled = true;
      login.textContent = "正在验证…";
      void loginWithDevice(ctx.signal)
        .then(() => {
          ctx.toast.show("登录成功", "success");
          go();
        })
        .catch((reason: unknown) => {
          if (ctx.signal.aborted) return;
          setError(error, describeError(reason));
        })
        .finally(() => {
          login.disabled = false;
          login.replaceChildren(icon("key"), "使用本机密钥登录");
        });
    });

    forget.addEventListener("click", () => {
      forget.disabled = true;
      void forgetDevice()
        .then(() => renderEnroll())
        .catch((reason: unknown) => {
          setError(error, describeError(reason));
          forget.disabled = false;
        });
    });

    panelHost.replaceChildren(
      h(
        "div",
        { class: "auth-card" },
        h("h2", { class: "auth-card-title" }, "本设备已登记"),
        h(
          "p",
          { class: "auth-lead" },
          "本机保存了不可导出的私钥。点击登录，服务器会下发一次性挑战，由私钥签名后换取访问令牌。",
        ),
        error,
        h("div", { class: "form-actions form-actions--stack" }, login, forget),
        notice("私钥保存在 IndexedDB 中，JavaScript 无法读取其字节。"),
      ),
    );
  };

  const renderEnroll = (): void => {
    const error = errorBox();
    const token = h("input", {
      class: "input input--token",
      attrs: {
        id: "pair-token",
        name: "token",
        type: "text",
        placeholder: "HH-XXXX-XXXX-XXXX",
        autocomplete: "off",
        autocapitalize: "characters",
        spellcheck: "false",
        maxlength: "17",
        required: true,
      },
    });
    const name = h("input", {
      class: "input",
      attrs: {
        id: "pair-name",
        name: "name",
        type: "text",
        maxlength: "64",
        autocomplete: "off",
        value: suggestDeviceName(),
        required: true,
      },
    });

    const submit = h(
      "button",
      { class: "btn btn--primary btn--wide", attrs: { type: "submit" } },
      icon("key"),
      "登记本设备",
    );

    const fieldset = h(
      "fieldset",
      { class: "fieldset" },
      h(
        "div",
        { class: "field" },
        h("label", { class: "label", attrs: { for: "pair-token" } }, "配对码"),
        token,
        h(
          "p",
          { class: "field-help" },
          "在服务器上运行 enroll 命令生成，形如 HH-7QK4-2M9X-P3TB，15 分钟内有效。",
        ),
      ),
      h(
        "div",
        { class: "field" },
        h("label", { class: "label", attrs: { for: "pair-name" } }, "设备名称"),
        name,
        h("p", { class: "field-help" }, "用于在服务端识别这台设备，之后可以在设置里看到。"),
      ),
      error,
      h("div", { class: "form-actions" }, submit),
    );

    token.addEventListener("blur", () => {
      if (token.value.trim() !== "") token.value = normalizePairingToken(token.value);
    });

    const form = h(
      "form",
      { class: "auth-form", attrs: { novalidate: true } },
      h(
        "ol",
        { class: "steps" },
        h("li", {}, h("span", { class: "step-number" }, "1"), "生成 Ed25519 密钥（不可导出）"),
        h("li", {}, h("span", { class: "step-number" }, "2"), "登记公钥到服务器"),
        h("li", {}, h("span", { class: "step-number" }, "3"), "签名挑战并登录"),
      ),
      fieldset,
    );

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      error.hidden = true;

      const tokenValue = normalizePairingToken(token.value);
      const nameValue = name.value.trim() || suggestDeviceName();
      if (tokenValue.trim() === "") {
        setError(error, "请输入配对码");
        token.focus();
        return;
      }
      if (!webCryptoAvailable()) {
        setError(error, "当前环境不支持 WebCrypto，请使用 HTTPS 或 localhost 访问");
        return;
      }

      fieldset.disabled = true;
      submit.textContent = "正在配对…";
      void (async () => {
        try {
          if (await hasLocalDeviceKey()) {
            throw new Error("本机已有密钥。请先清除本机设备，或直接使用已有密钥登录。");
          }
          const result = await enrollThisDevice(tokenValue, nameValue, ctx.signal);
          if (ctx.signal.aborted) return;
          ctx.toast.show(`配对成功（${result.algorithm === "ed25519" ? "Ed25519" : "P-256"}）`, "success");
          go();
        } catch (reason) {
          if (ctx.signal.aborted) return;
          setError(error, describeError(reason));
        } finally {
          fieldset.disabled = false;
          submit.replaceChildren(icon("key"), "登记本设备");
        }
      })();
    });

    panelHost.replaceChildren(
      h(
        "div",
        { class: "auth-card" },
        h("h2", { class: "auth-card-title" }, "登记这台设备"),
        h(
          "p",
          { class: "auth-lead" },
          "配对码由服务器本地生成，一次性使用。浏览器只把公钥发出去，私钥永远不会离开这台设备。",
        ),
        form,
        notice("本机密钥以不可导出 CryptoKey 形式存放在 IndexedDB；清除站点数据会一并删除。"),
      ),
    );
  };

  void (async () => {
    panelHost.replaceChildren(loadingBlock("正在检查本机密钥…"));
    const session = getSession();
    let keyExists = false;
    try {
      keyExists = await hasLocalDeviceKey();
    } catch {
      keyExists = false;
    }
    if (ctx.signal.aborted) return;

    if (session.authenticated) {
      renderSignedIn(session, keyExists);
    } else if (keyExists) {
      renderLogin();
    } else {
      renderEnroll();
    }
  })();

  return { element };
};
