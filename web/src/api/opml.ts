import { api } from "./client";
import { ApiError } from "./errors";

// OPML 的请求体与导出响应是 XML 文本，但仍走统一请求层 api()：
// 这样 Bearer、CSRF、X-Owner-Token、X-Request-ID、401 刷新重放与
// 服务端 JSON 错误信封的解析都与其它接口一致，不再各写一份。

export interface OPMLFailure {
  url: string;
  reason: string;
}

export interface OPMLImportResult {
  created: number;
  updated: number;
  total: number;
  failed: OPMLFailure[];
}

function isCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function invalidResponse(): ApiError {
  return new ApiError(0, "invalid_response", "服务端返回的导入结果格式不正确", null, null);
}

/** 服务端返回值的形状校验：不对就当成协议错误，而不是让视图读到 undefined。 */
function parseImportResult(payload: unknown): OPMLImportResult {
  const record = typeof payload === "object" && payload !== null ? (payload as Record<string, unknown>) : null;
  if (!record || !isCount(record.created) || !isCount(record.updated) || !isCount(record.total)) {
    throw invalidResponse();
  }
  const failedRaw = record.failed ?? [];
  if (!Array.isArray(failedRaw)) throw invalidResponse();
  const failed: OPMLFailure[] = [];
  for (const item of failedRaw as unknown[]) {
    const entry = typeof item === "object" && item !== null ? (item as Record<string, unknown>) : null;
    if (!entry || typeof entry.url !== "string") throw invalidResponse();
    failed.push({ url: entry.url, reason: typeof entry.reason === "string" ? entry.reason : "" });
  }
  return { created: record.created, updated: record.updated, total: record.total, failed };
}

/** 导出全部订阅为 OPML 文本。失败时抛出带服务端错误码与消息的 ApiError。 */
export async function exportOPML(signal?: AbortSignal): Promise<string> {
  return api<string>("/feeds/export", {
    accept: "text/x-opml, application/json",
    responseType: "text",
    ...(signal ? { signal } : {}),
  });
}

/** 导入 OPML 文本，返回逐条结果。 */
export async function importOPML(text: string, signal?: AbortSignal): Promise<OPMLImportResult> {
  const payload = await api<unknown>("/feeds/import", {
    method: "POST",
    rawBody: text,
    contentType: "text/x-opml; charset=utf-8",
    ...(signal ? { signal } : {}),
  });
  return parseImportResult(payload);
}
