import { ApiError, isAbortError } from "../api/errors";
import type { PasteVisibility } from "../api/types";

export const APP_NAME = "Homehub 扩展";

export const NAV_LABELS = {
  home: "工具台",
  entries: "条目",
  feeds: "订阅源",
  pastes: "片段",
  pair: "配对",
  settings: "设置",
} as const;

export const COMMON = {
  loading: "加载中…",
  retry: "重试",
  refresh: "刷新",
  cancel: "取消",
  save: "保存",
  copy: "复制",
  copied: "已复制",
  delete: "删除",
  confirmDelete: "确认删除",
  back: "返回",
  unknown: "—",
  empty: "暂无内容",
} as const;

export function describeError(error: unknown): string {
  if (isAbortError(error)) return "请求已取消";
  if (error instanceof ApiError) {
    if (error.status === 0) return error.message;
    const suffix = error.requestId ? `（请求 ${error.requestId}）` : "";
    return `${error.message}${suffix}`;
  }
  if (error instanceof Error) return error.message;
  return "发生未知错误";
}

export function visibilityLabel(visibility: PasteVisibility | string): string {
  switch (visibility) {
    case "public":
      return "公开";
    case "private":
      return "私有";
    case "unlisted":
      return "不公开";
    default:
      return visibility || COMMON.unknown;
  }
}

export function jobStatusLabel(status: string): string {
  switch (status) {
    case "ok":
    case "success":
    case "done":
      return "成功";
    case "error":
    case "failed":
    case "fail":
      return "失败";
    case "running":
      return "运行中";
    case "skipped":
      return "已跳过";
    default:
      return status || COMMON.unknown;
  }
}

export function jobStatusTone(status: string): "ok" | "error" | "running" | "neutral" {
  switch (status) {
    case "ok":
    case "success":
    case "done":
      return "ok";
    case "error":
    case "failed":
    case "fail":
      return "error";
    case "running":
      return "running";
    default:
      return "neutral";
  }
}
