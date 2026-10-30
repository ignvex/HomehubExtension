import { loadOwnerToken } from "../auth/owner-token";
import { ApiError } from "./errors";

const API_BASE = "/api/v1";

export interface HttpInit {
  method?: string;
  body?: unknown;
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
  headers.set("Accept", "application/json");
  headers.set("X-Request-ID", requestId());

  // 匿名所有权凭证。没有登录态时也带上，服务端据此识别"自己的"片段。
  // 未设置时为空字符串，服务端按匿名处理。
  const ownerToken = loadOwnerToken();
  if (ownerToken) headers.set("X-Owner-Token", ownerToken);

  let body: string | undefined;
  if (init.body !== undefined) {
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
