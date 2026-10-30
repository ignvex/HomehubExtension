import { hasLocalClientId, restoreSession, subscribeSession } from "./auth/session";
import { Nav } from "./components/nav";
import { createToast } from "./components/toast";
import { loadingBlock } from "./components/states";
import { icon, type IconName } from "./components/icons";
import { h } from "./lib/dom";
import { APP_NAME } from "./lib/strings";
import { initEvents, subscribe as subscribeEvents } from "./lib/sse";
import { applyTheme, currentTheme, initTheme, nextTheme, THEME_LABELS, type Theme } from "./lib/theme";
import { Router, type RouteDefinition } from "./router";
import { refreshUnreadCount, unreadCount } from "./state/app-state";
import { entriesView } from "./views/entries";
import { feedsView } from "./views/feeds";
import { homeView } from "./views/home";
import { notFoundView } from "./views/not-found";
import { pairView } from "./views/pair";
import { pasteNewView } from "./views/paste-new";
import { pastesView } from "./views/pastes";
import { readerView } from "./views/reader";
import { settingsView } from "./views/settings";

// 路由与鉴权边界。
//
// auth: false 的页面在未登录时也能打开：
//   工具台  —— 用户看到的第一个页面，不该是登录墙
//   paste   —— 匿名可创建与管理自己的片段（凭 owner token）
//
// auth: true 的页面必须配对：
//   RSS 阅读、订阅源管理 —— 抓取会主动访问外部地址
//   设置 —— 含运维信息
const ROUTES: readonly RouteDefinition[] = [
  { path: "/", view: homeView, auth: false, title: "工具台" },
  { path: "/pair", view: pairView, auth: false, title: "设备配对" },
  { path: "/pastes", view: pastesView, auth: false, title: "片段" },
  { path: "/pastes/new", view: pasteNewView, auth: false, title: "新建片段" },
  { path: "/entries", view: entriesView, auth: true, title: "条目" },
  { path: "/entries/:id", view: readerView, auth: true, title: "阅读" },
  { path: "/feeds", view: feedsView, auth: true, title: "订阅源" },
  { path: "/settings", view: settingsView, auth: true, title: "设置" },
];

const THEME_ICONS: Record<Theme, IconName> = {
  auto: "monitor",
  light: "sun",
  dark: "moon",
};

export async function mountApp(root: HTMLElement): Promise<void> {
  initTheme();

  const nav = new Nav();
  const toast = createToast();
  const main = h("main", { class: "main", id: "main", attrs: { tabindex: "-1" } });
  const topbarTitle = h("p", { class: "topbar-title" }, APP_NAME);

  const menuButton = h(
    "button",
    {
      class: "icon-button menu-button",
      attrs: {
        type: "button",
        "aria-label": "打开导航",
        "aria-controls": "sidebar",
        "aria-expanded": "false",
      },
    },
    icon("menu"),
  );

  const themeButton = h(
    "button",
    { class: "icon-button theme-button", attrs: { type: "button" } },
    icon("monitor"),
    h("span", { class: "theme-button-label" }, "跟随系统"),
  );

  const paintThemeButton = (): void => {
    const theme = currentTheme();
    themeButton.replaceChildren(
      icon(THEME_ICONS[theme]),
      h("span", { class: "theme-button-label" }, THEME_LABELS[theme]),
    );
    themeButton.setAttribute("aria-label", `切换主题，当前：${THEME_LABELS[theme]}`);
    themeButton.title = `切换主题，当前：${THEME_LABELS[theme]}`;
  };
  paintThemeButton();

  themeButton.addEventListener("click", () => {
    const theme = nextTheme(currentTheme());
    applyTheme(theme);
    paintThemeButton();
    window.dispatchEvent(new Event("hh:theme"));
    toast.api.show(`主题：${THEME_LABELS[theme]}`, "success");
  });
  window.addEventListener("hh:theme", paintThemeButton);

  const sessionHost = h("div", { class: "sidebar-session-host" });
  let wasAuthenticated = false;
  subscribeSession((snapshot) => {
    if (snapshot.authenticated && !wasAuthenticated) void refreshUnreadCount();
    if (!snapshot.authenticated) unreadCount.set(0);
    wasAuthenticated = snapshot.authenticated;


    const name = snapshot.client?.name?.trim() ? snapshot.client.name : "未配对设备";
    const href = snapshot.authenticated ? "/settings" : "/pair";
    sessionHost.replaceChildren(
      h(
        "a",
        { class: "sidebar-session", attrs: { href } },
        h(
          "span",
          {
            class: "status-dot",
            attrs: { "aria-hidden": "true", ...(snapshot.authenticated ? { "data-tone": "ok" } : {}) },
          },
        ),
        h(
          "span",
          { class: "sidebar-session-text" },
          h("strong", {}, name),
          h("span", {}, snapshot.authenticated ? "已登录" : "点击配对"),
        ),
      ),
    );
  });

  const brand = h(
    "a",
    { class: "brand", attrs: { href: "/" } },
    h("span", { class: "brand-mark", attrs: { "aria-hidden": "true" } }, icon("home")),
    h(
      "span",
      { class: "brand-text" },
      h("span", { class: "brand-name" }, "Homehub"),
      h("span", { class: "brand-sub" }, "扩展"),
    ),
  );

  const sidebar = h(
    "aside",
    { class: "sidebar", id: "sidebar" },
    brand,
    nav.element,
    h("div", { class: "sidebar-footer" }, sessionHost),
  );

  const scrim = h("button", {
    class: "scrim",
    attrs: { type: "button", "aria-label": "关闭导航", tabindex: "-1" },
  });

  const topbar = h(
    "header",
    { class: "topbar" },
    menuButton,
    topbarTitle,
    h("div", { class: "topbar-actions" }, themeButton),
  );

  const skipLink = h("a", { class: "skip-link", attrs: { href: "#main" } }, "跳到内容");
  const app = h("div", { class: "app" }, skipLink, sidebar, topbar, main, scrim, toast.element);
  root.replaceChildren(app);

  const mobileQuery = window.matchMedia("(max-width: 899px)");
  let drawerOpen = false;

  const setDrawer = (open: boolean, restoreFocus = false): void => {
    if (open && !mobileQuery.matches) return;
    const wasOpen = drawerOpen;
    drawerOpen = open && mobileQuery.matches;
    document.body.classList.toggle("nav-open", drawerOpen);
    menuButton.setAttribute("aria-expanded", String(drawerOpen));
    scrim.hidden = !drawerOpen;
    sidebar.inert = mobileQuery.matches && !drawerOpen;
    main.inert = mobileQuery.matches && drawerOpen;

    if (drawerOpen) {
      sidebar.querySelector<HTMLElement>("a, button")?.focus();
    } else if (restoreFocus && wasOpen && mobileQuery.matches) {
      menuButton.focus();
    }
  };

  setDrawer(false);

  menuButton.addEventListener("click", () => {
    setDrawer(!drawerOpen, true);
  });
  scrim.addEventListener("click", () => {
    setDrawer(false, true);
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && drawerOpen) setDrawer(false, true);
  });
  mobileQuery.addEventListener("change", () => {
    drawerOpen = false;
    document.body.classList.remove("nav-open");
    menuButton.setAttribute("aria-expanded", "false");
    scrim.hidden = true;
    sidebar.inert = mobileQuery.matches;
    main.inert = false;
  });

  // 全局接线：新条目让未读徽标 +1，重连后全量重取（断线期间的事件收不到）。
  // 各视图通过 ctx.events 订阅各自关心的事件。
  initEvents();
  subscribeEvents((event) => {
    if (event.type === "entry.created") {
      unreadCount.set(unreadCount.get() + 1);
    } else if (event.type === "system.resync") {
      void refreshUnreadCount();
    }
  });

  const router = new Router({
    routes: ROUTES,
    notFound: { title: "页面不存在", view: notFoundView },
    root: main,
    toast: toast.api,
    events: (listener) => subscribeEvents(listener),
    onRoute: (info) => {
      nav.setActive(info.path);
      topbarTitle.textContent = info.title;
      setDrawer(false);
    },
  });

  window.addEventListener("hh:unauthorized", () => {
    const current = `${window.location.pathname}${window.location.search}`;
    if (current.startsWith("/pair")) return;
    router.navigate(`/pair?returnTo=${encodeURIComponent(current)}`, { replace: true });
  });

  main.append(loadingBlock("正在恢复会话…"));
  if (hasLocalClientId()) {
    await restoreSession();
  }
  if (!root.isConnected) return;
  router.start();
}
