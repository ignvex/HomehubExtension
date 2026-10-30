// 工具注册表。
//
// 只放元数据与加载函数，不 import 任何具体实现——
// 一旦在这里静态 import 工具，它们就会被打进首屏 bundle，
// 懒加载就失效了。
//
// 新增工具的步骤：写一个导 default factory 的模块，
// 然后在 TOOLS 里加一行。列表页与详情页会自动出现它。

import type { CategoryId, ToolMeta } from "./types";

export const TOOLS: readonly ToolMeta[] = [
  // ---- 文本与校验 ----
  {
    id: "json",
    name: "JSON 格式化",
    description: "格式化、压缩与校验，语法错误带行列定位。",
    category: "text",
    icon: "braces",
    keywords: ["json", "format", "格式化", "校验", "压缩"],
    load: () => import("./tools-json"),
  },
  {
    id: "regex",
    name: "正则测试",
    description: "实时高亮匹配，支持 g / i / m / s 修饰符。",
    category: "text",
    icon: "regex",
    keywords: ["regex", "正则", "匹配", "pattern"],
    load: () => import("./tools-regex"),
  },
  {
    id: "calc",
    name: "程序员计算器",
    description: "HEX / DEC / OCT / BIN 同步显示，位运算与 64 位模式。",
    category: "text",
    icon: "calculator",
    keywords: ["calc", "计算器", "进制", "位运算", "hex"],
    load: () => import("./tools-calc"),
  },

  // ---- 编码与哈希 ----
  {
    id: "base64",
    name: "Base64 编解码",
    description: "完整支持中文等 UTF-8 字符，也可转换文件。",
    category: "encode",
    icon: "binary",
    keywords: ["base64", "编码", "解码", "btoa"],
    load: () => import("./tools-base64"),
  },
  {
    id: "urlcode",
    name: "URL 编解码",
    description: "encodeURIComponent 与 decodeURIComponent 互转。",
    category: "encode",
    icon: "link",
    keywords: ["url", "encode", "decode", "转义"],
    load: () => import("./tools-urlcode"),
  },
  {
    id: "hash",
    name: "哈希计算",
    description: "MD5 / SHA-1 / SHA-256 / SHA-384 / SHA-512，支持文本与文件。",
    category: "encode",
    icon: "hash",
    keywords: ["hash", "md5", "sha", "摘要", "指纹"],
    load: () => import("./tools-hash"),
  },

  // ---- 配置格式 ----
  {
    id: "yaml",
    name: "配置格式转换",
    description: "YAML / JSON / INI 互转与格式化，自动识别输入格式。",
    category: "config",
    icon: "file-code",
    keywords: ["yaml", "yml", "ini", "json", "转换"],
    load: () => import("./tools-yaml"),
  },

  // ---- 图片处理 ----
  {
    id: "imgconvert",
    name: "图片格式转换",
    description: "PNG / JPG / WebP / BMP / ICO 互转，可调质量与尺寸。",
    category: "image",
    icon: "image",
    keywords: ["image", "png", "jpg", "webp", "转换"],
    load: () => import("./tools-imgconvert"),
  },
  {
    id: "imgcompress",
    name: "图片压缩",
    description: "调节质量与尺寸压缩图片，实时显示压缩率。",
    category: "image",
    icon: "minimize",
    keywords: ["compress", "压缩", "webp", "体积"],
    load: () => import("./tools-imgcompress"),
  },
  {
    id: "imgbase64",
    name: "图片 Base64",
    description: "图片转 Data URL，或把 Base64 还原成图片。",
    category: "image",
    icon: "image-plus",
    keywords: ["image", "base64", "dataurl", "内联"],
    load: () => import("./tools-imgbase64"),
  },

  // ---- 时间与随机 ----
  {
    id: "timestamp",
    name: "时间戳转换",
    description: "Unix 时间戳与日期互转，秒 / 毫秒精度与本地时区。",
    category: "time",
    icon: "clock",
    keywords: ["timestamp", "时间戳", "unix", "日期"],
    load: () => import("./tools-timestamp"),
  },
  {
    id: "uuid",
    name: "UUID 生成",
    description: "v4 与 v7，可批量生成并一键复制。",
    category: "time",
    icon: "fingerprint",
    keywords: ["uuid", "guid", "v4", "v7", "随机"],
    load: () => import("./tools-uuid"),
  },
  {
    id: "qrcode",
    name: "二维码生成",
    description: "文本或链接转二维码，L / M / Q / H 四档纠错，可下载 PNG。",
    category: "time",
    icon: "qr",
    keywords: ["qrcode", "二维码", "qr", "扫码"],
    load: () => import("./tools-qrcode"),
  },
  {
    id: "color",
    name: "颜色转换",
    description: "HEX、RGB、HSL 互转，附取色器与预览。",
    category: "time",
    icon: "palette",
    keywords: ["color", "颜色", "hex", "rgb", "hsl"],
    load: () => import("./tools-color"),
  },
  {
    id: "font",
    name: "字体预览",
    description: "对比常用系统字体的渲染效果，可调字号与字重。",
    category: "time",
    icon: "type",
    keywords: ["font", "字体", "预览", "排版"],
    load: () => import("./tools-font"),
  },
];

const BY_ID = new Map(TOOLS.map((tool) => [tool.id, tool]));

export function findTool(id: string): ToolMeta | null {
  return BY_ID.get(id) ?? null;
}

/** 按分类分组，保持 CATEGORIES 的顺序。空分类会被保留但不渲染。 */
export function groupByCategory(): Map<CategoryId, ToolMeta[]> {
  const groups = new Map<CategoryId, ToolMeta[]>();
  for (const tool of TOOLS) {
    const list = groups.get(tool.category) ?? [];
    list.push(tool);
    groups.set(tool.category, list);
  }
  return groups;
}

/** 简单的关键词过滤。空查询返回全部。 */
export function searchTools(query: string): ToolMeta[] {
  const q = query.trim().toLowerCase();
  if (q === "") return [...TOOLS];
  return TOOLS.filter((tool) => {
    if (tool.name.toLowerCase().includes(q)) return true;
    if (tool.description.toLowerCase().includes(q)) return true;
    return tool.keywords.some((k) => k.toLowerCase().includes(q));
  });
}
