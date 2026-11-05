import { loadOwnerToken } from "../auth/owner-token";
import { ApiError } from "./errors";

const API_BASE = "/api/v1";

export interface HttpInit {
  method?: string;
  /** JSON 请求体；与 rawBody 互斥。 */
  body?: unknown;
  /** 原样发送的文本请求体（例如 OPML）；需同时给 contentType。 */
  rawBody?: string;
  contentType?: string;
  /** 覆盖默认的 Accept: application/json。 */
  accept?: string;
  /**
   * 成功响应的解析方式。默认 "json"（解析失败时返回原文）；
   * "text" 原样返回响应文本。错误响应无论如何都按 JSON 错误信封解析。
   */
  responseType?: "json" | "text";
  signal?: AbortSignal;
  headers?: Record<string, string>;
}

interface ErrorEnvelope {
  error?: {
    code?: unknown;
    message?: unknown;
    details?: unknown;
    request_id?: unknown;
  };
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : null;
}

function requestId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

function toApiError(status: number, payload: unknown, headerRequestId: string | null): ApiError {
  const envelope = asRecord(payload) as ErrorEnvelope | null;
  const detail = asRecord(envelope?.error);
  const code = typeof detail?.code === "string" && detail.code ? detail.code : "http_error";
  const message =
    typeof detail?.message === "string" && detail.message
      ? detail.message
      : `请求失败（HTTP ${status}）`;
  const requestIdValue =
    typeof detail?.request_id === "string" && detail.request_id ? detail.request_id : headerRequestId;
  return new ApiError(status, code, message, detail?.details ?? null, requestIdValue);
}

export async function send<T>(path: string, init: HttpInit = {}): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  headers.set("Accept", init.accept ?? "application/json");
  headers.set("X-Request-ID", requestId());

  // 匿名所有权凭证。没有登录态时也带上，服务端据此识别"自己的"片段。
  // 未设置时为空字符串，服务端按匿名处理。
  const ownerToken = loadOwnerToken();
  if (ownerToken) headers.set("X-Owner-Token", ownerToken);

  let body: string | undefined;
  if (init.rawBody !== undefined) {
    headers.set("Content-Type", init.contentType ?? "text/plain; charset=utf-8");
    body = init.rawBody;
  } else if (init.body !== undefined) {
    headers.set("Content-Type", "application/json; charset=utf-8");
    body = JSON.stringify(init.body);
  }

  const request: RequestInit = {
    method,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  };
  if (body !== undefined) request.body = body;
  if (init.signal) request.signal = init.signal;

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, request);
  } catch (error) {
    if (init.signal?.aborted) throw error;
    throw new ApiError(0, "network_error", "网络请求失败，请检查连接", null, null);
  }

  const responseRequestId = response.headers.get("X-Request-ID");
  const text = await response.text();
  if (response.ok && init.responseType === "text") {
    return text as T;
  }
  let payload: unknown = null;
  if (text !== "") {
    try {
      payload = JSON.parse(text) as unknown;
    } catch {
      payload = text;
    }
  }

  if (!response.ok) {
    throw toApiError(response.status, payload, responseRequestId);
  }
  if (response.status === 204 || text === "") {
    return undefined as T;
  }
  return payload as T;
}
