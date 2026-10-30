// 单工具容器：按 id 动态加载实现。
//
// 懒加载在这里发生。registry 里的 load() 返回动态 import，
// Vite 会把它拆成独立 chunk——首屏不会下载任何工具的实现。
//
// 三个必须处理的边界：
//   - 未知 id：给一个明确的错误页，不要白屏
//   - chunk 加载失败（离线、部署后旧 chunk 404）：提示刷新
//   - 卸载：调用工具的 unmount，否则定时器与对象 URL 会泄漏

import { pageHeader } from "../components/page";
import { errorState, loadingBlock } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import { describeError } from "../lib/strings";
import { findTool, TOOLS } from "../tools/registry";
import type { ToolFactory } from "../tools/types";
import type { ViewFactory } from "../router";

export const toolView: ViewFactory = (ctx) => {
  const host = h("div", { class: "tool-host" }, loadingBlock());
  const id = ctx.params.id ?? "";
  const meta = findTool(id);

  const back = h(
    "a",
    { class: "btn", attrs: { href: "/tools" } },
    icon("arrow-left"),
    "全部工具",
  );

  const element = h(
    "div",
    { class: "page tool-page" },
    pageHeader(meta?.name ?? "工具", meta?.description, [back]),
    host,
  );

  if (!meta) {
    // 列出全部工具，比单纯报错有用——用户可能是手输了一个旧链接。
    const suggestions = h("div", { class: "tool-suggestions" });
    for (const tool of TOOLS.slice(0, 6)) {
      suggestions.append(
        h(
          "a",
          { class: "btn btn--sm", attrs: { href: `/tools/${encodeURIComponent(tool.id)}` } },
          tool.name,
        ),
      );
    }
    host.replaceChildren(
      h(
        "div",
        {},
        errorState(`没有名为「${id}」的工具`, () => ctx.router.navigate("/tools")),
        suggestions,
      ),
    );
    return { element };
  }

  let unmountTool: (() => void) | undefined;
  let disposed = false;

  const mount = async (): Promise<void> => {
    host.replaceChildren(loadingBlock());
    try {
      const mod = await meta.load();
      if (disposed || ctx.signal.aborted) return;

      const factory: ToolFactory = mod.default;
      const handle = factory({ signal: ctx.signal, toast: ctx.toast });
      unmountTool = handle.unmount;

      host.replaceChildren(handle.element);
      document.title = `${meta.name} · Homehub 扩展`;
    } catch (error) {
      if (disposed || ctx.signal.aborted) return;
      // chunk 404 常见于「部署了新版本但页面还是旧的」。
      // 这种情况刷新一次就能解决，提示里说清楚。
      host.replaceChildren(
        errorState(`${describeError(error)}（页面可能已过期，刷新一次试试）`, () => {
          void mount();
        }),
      );
    }
  };

  void mount();

  return {
    element,
    unmount: () => {
      disposed = true;
      unmountTool?.();
    },
  };
};
