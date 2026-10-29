import type { ToastApi } from "./components/toast";
import type { ServerEvent } from "./lib/sse";
import { errorState } from "./components/states";
import { isAuthenticated } from "./auth/session";

export interface ViewContext {
  router: Router;
  params: Record<string, string>;
  query: URLSearchParams;
  signal: AbortSignal;
  toast: ToastApi;
  /**
   * 订阅服务端推送。返回取消函数，必须并入 unmount，
   * 否则切页后旧视图仍会被调用、持有已脱离文档的 DOM。
   */
  events: (listener: (event: ServerEvent) => void) => () => void;
}

export interface ViewHandle {
  element: HTMLElement;
  unmount?: () => void;
}

export type ViewFactory = (ctx: ViewContext) => ViewHandle;

export interface RouteDefinition {
  path: string;
  title: string;
  auth: boolean;
  view: ViewFactory;
}

export interface RouteInfo {
  path: string;
  title: string;
  auth: boolean;
}

export interface RouterOptions {
  routes: readonly RouteDefinition[];
  notFound: { title: string; view: ViewFactory };
  root: HTMLElement;
  toast: ToastApi;
  onRoute: (info: RouteInfo) => void;
  /** 全局 SSE 订阅入口，透传给每个视图。 */
  events: (listener: (event: ServerEvent) => void) => () => void;
}

interface Match {
  route: RouteDefinition;
  params: Record<string, string>;
}

const NOT_FOUND_TITLE = "页面不存在";

function normalizePath(pathname: string): string {
  if (pathname.length <= 1) return "/";
  return pathname.replace(/\/+$/u, "");
}

function splitSegments(pathname: string): string[] {
  return normalizePath(pathname)
    .split("/")
    .filter((segment) => segment !== "");
}

function matchRoute(routes: readonly RouteDefinition[], pathname: string): Match | null {
  const parts = splitSegments(pathname);

  for (const route of routes) {
    const patternParts = splitSegments(route.path);
    if (patternParts.length !== parts.length) continue;

    const params: Record<string, string> = {};
    let matched = true;
    for (let index = 0; index < patternParts.length; index += 1) {
      const pattern = patternParts[index] ?? "";
      const actual = parts[index] ?? "";
      if (pattern.startsWith(":")) {
        try {
          params[pattern.slice(1)] = decodeURIComponent(actual);
        } catch {
          matched = false;
          break;
        }
      } else if (pattern !== actual) {
        matched = false;
        break;
      }
    }
    if (matched) return { route, params };
  }
  return null;
}

export class Router {
  private readonly options: RouterOptions;
  private controller: AbortController | null = null;
  private unmountActive: (() => void) | null = null;
  private lastInfo: RouteInfo | null = null;
  private readonly onPopState = (event: PopStateEvent): void => {
    const state = (event.state ?? {}) as { scrollY?: unknown };
    const scrollY = typeof state.scrollY === "number" ? state.scrollY : null;
    this.render(`${window.location.pathname}${window.location.search}${window.location.hash}`, {
      restoreScroll: scrollY,
    });
  };
  private readonly onClick = (event: MouseEvent): void => {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;

    const target = event.target;
    if (!(target instanceof Element)) return;
    const anchor = target.closest("a");
    if (!(anchor instanceof HTMLAnchorElement)) return;
    if (anchor.hasAttribute("download") || anchor.hasAttribute("data-native")) return;
    if (anchor.target !== "" && anchor.target !== "_self") return;

    const url = new URL(anchor.href, window.location.href);
    if (url.origin !== window.location.origin) return;
    if (
      url.pathname.startsWith("/api/") ||
      url.pathname.startsWith("/media/") ||
      url.pathname.startsWith("/p/") ||
      url.pathname === "/healthz" ||
      url.pathname === "/readyz"
    ) {
      return;
    }
    // 文章内的锚点跳转交给浏览器。
    if (
      url.hash !== "" &&
      url.pathname === window.location.pathname &&
      url.search === window.location.search
    ) {
      return;
    }

    event.preventDefault();
    if (url.pathname === window.location.pathname && url.search === window.location.search) {
      if (this.lastInfo) this.options.onRoute(this.lastInfo);
      return;
    }
    this.navigate(`${url.pathname}${url.search}${url.hash}`);
  };

  constructor(options: RouterOptions) {
    this.options = options;
  }

  start(): void {
    history.scrollRestoration = "manual";
    window.addEventListener("popstate", this.onPopState);
    document.addEventListener("click", this.onClick);
    this.render(`${window.location.pathname}${window.location.search}${window.location.hash}`, {
      replaceHistory: true,
    });
  }

  navigate(to: string, options: { replace?: boolean } = {}): void {
    if (!to.startsWith("/") || to.startsWith("//")) return;
    const current = `${window.location.pathname}${window.location.search}${window.location.hash}`;
    if (to === current) return;

    const state = (history.state ?? {}) as Record<string, unknown>;
    history.replaceState({ ...state, scrollY: window.scrollY }, "");

    if (options.replace) {
      history.replaceState({ scrollY: 0 }, "", to);
    } else {
      history.pushState({ scrollY: 0 }, "", to);
    }
    this.render(to);
  }

  currentPath(): string {
    return `${window.location.pathname}${window.location.search}`;
  }

  private render(
    pathWithQuery: string,
    options: { replaceHistory?: boolean; restoreScroll?: number | null } = {},
  ): void {
    const url = new URL(pathWithQuery, window.location.origin);
    const pathname = normalizePath(url.pathname);
    const match = matchRoute(this.options.routes, pathname);
    const route = match?.route ?? null;
    const params = match?.params ?? {};
    const isNotFound = route === null;
    const title = route?.title ?? this.options.notFound.title;
    const auth = route?.auth ?? false;

    if (auth && !isAuthenticated()) {
      const returnTo = `${pathname}${url.search}`;
      const pairPath = `/pair?returnTo=${encodeURIComponent(returnTo)}`;
      history.replaceState({ scrollY: 0 }, "", pairPath);
      this.render(pairPath, { replaceHistory: true });
      return;
    }

    if (options.replaceHistory) {
      history.replaceState({ ...(history.state ?? {}), scrollY: window.scrollY }, "", pathWithQuery);
    }

    this.unmountActive?.();
    this.unmountActive = null;
    this.controller?.abort();
    const controller = new AbortController();
    this.controller = controller;

    const context: ViewContext = {
      router: this,
      params,
      query: url.searchParams,
      signal: controller.signal,
      toast: this.options.toast,
      events: this.options.events,
    };

    const info: RouteInfo = { path: pathname, title, auth };
    this.lastInfo = info;
    this.options.onRoute(info);

    this.options.root.replaceChildren();
    try {
      const handle = isNotFound
        ? this.options.notFound.view(context)
        : (route as RouteDefinition).view(context);
      this.options.root.append(handle.element);
      this.unmountActive = handle.unmount ?? null;
    } catch (error) {
      this.options.root.append(
        errorState(error instanceof Error ? error.message : "页面渲染失败"),
      );
    }

    document.title = isNotFound ? `${NOT_FOUND_TITLE} · Homehub 扩展` : `${title} · Homehub 扩展`;
    this.options.root.focus({ preventScroll: true });

    if (typeof options.restoreScroll === "number") {
      const top = options.restoreScroll;
      requestAnimationFrame(() => window.scrollTo(0, top));
    } else {
      window.scrollTo(0, 0);
    }
  }
}
