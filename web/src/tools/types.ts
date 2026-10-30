// 开发工具的类型契约。
//
// 每个工具是一个独立的模块，由 registry 用动态 import 加载——
// 这样首屏只会下载工具列表的元数据，具体实现按需取。
// 15 个工具全打进主 bundle 会让首屏从 23KB 涨到 150KB+。

import type { ToastApi } from "../components/toast";

export interface ToolContext {
  /** 路由切换时 abort，长任务应据此放弃。 */
  signal: AbortSignal;
  toast: ToastApi;
}

export interface ToolHandle {
  element: HTMLElement;
  /**
   * 离开工具时调用。必须清掉所有定时器、
   * AbortController、对象 URL 与事件监听。
   */
  unmount?: () => void;
}

export type ToolFactory = (ctx: ToolContext) => ToolHandle;

/** 工具的静态元数据。不要在这里 import 具体实现。 */
export interface ToolMeta {
  id: string;
  name: string;
  /** 一句话说明它做什么，显示在列表与详情页标题下方。 */
  description: string;
  /** 分类用于列表分组，顺序由 CATEGORIES 决定。 */
  category: CategoryId;
  icon: string;
  /** 关键词，用于列表页搜索。 */
  keywords: string[];
  /** 动态 import。返回 Promise 是因为 chunk 还没下载。 */
  load: () => Promise<{ default: ToolFactory }>;
}

export type CategoryId = "text" | "encode" | "image" | "time" | "config";

export interface Category {
  id: CategoryId;
  name: string;
  description: string;
}

/** 分类的展示顺序：越常用的越靠前。 */
export const CATEGORIES: readonly Category[] = [
  { id: "text", name: "文本与校验", description: "JSON、正则、计算器" },
  { id: "encode", name: "编码与哈希", description: "Base64、URL、哈希" },
  { id: "config", name: "配置格式", description: "YAML / JSON / INI 互转" },
  { id: "image", name: "图片处理", description: "转换、压缩、Base64" },
  { id: "time", name: "时间与随机", description: "时间戳、UUID、二维码" },
];
