import { ApiError } from "./errors";
import { getAccessToken, getCsrfToken } from "../auth/session";

// OPML 走的是 XML/二进制，与 JSON 的 send() 路径不同，
// 所以单独一个小模块而不是硬塞进 http.ts。
// 认证与 CSRF 的规则保持一致：Bearer 在头里、写操作带 X-CSRF-Token。

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

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = {};
  const token = getAccessToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

/** 导出全部订阅为 OPML 文本。 */
export async function exportOPML(signal?: AbortSignal): Promise<string> {
  const init: RequestInit = {
    method: "GET",
    headers: { ...authHeaders(), Accept: "text/x-opml" },
    credentials: "same-origin",
    cache: "no-store",
  };
  if (signal) init.signal = signal;

  const response = await fetch("/api/v1/feeds/export", init);
  if (!response.ok) {
    throw new ApiError(
      response.status,
      "export_failed",
      `导出失败（HTTP ${response.status}）`,
      null,
      response.headers.get("X-Request-ID"),
    );
  }
  return response.text();
}

/** 导入 OPML 文本，返回逐条结果。 */
export async function importOPML(text: string, signal?: AbortSignal): Promise<OPMLImportResult> {
  const headers: Record<string, string> = {
    ...authHeaders(),
    "Content-Type": "text/x-opml; charset=utf-8",
    Accept: "application/json",
  };
  // 与 client.ts 同一规则：写操作带 CSRF。
  const csrf = getCsrfToken();
  if (csrf) headers["X-CSRF-Token"] = csrf;

  const init: RequestInit = {
    method: "POST",
    headers,
    body: text,
    credentials: "same-origin",
    cache: "no-store",
  };
  if (signal) init.signal = signal;

  const response = await fetch("/api/v1/feeds/import", init);
  const raw = await response.text();

  let payload: unknown = null;
  if (raw !== "") {
    try {
      payload = JSON.parse(raw) as unknown;
    } catch {
      payload = raw;
    }
  }

  if (!response.ok) {
    const record = typeof payload === "object" && payload !== null ? (payload as Record<string, unknown>) : null;
    const detail =
      typeof record?.error === "object" && record.error !== null
        ? (record.error as Record<string, unknown>)
        : null;
    throw new ApiError(
      response.status,
      typeof detail?.code === "string" ? detail.code : "import_failed",
      typeof detail?.message === "string" ? detail.message : `导入失败（HTTP ${response.status}）`,
      detail?.details ?? null,
      response.headers.get("X-Request-ID"),
    );
  }

  return payload as OPMLImportResult;
}
