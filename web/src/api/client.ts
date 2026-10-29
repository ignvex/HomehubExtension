import { send, type HttpInit } from "./http";
import { ApiError } from "./errors";
import { getAccessToken, getCsrfToken, refreshSession } from "../auth/session";

export interface ApiOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
}

const WRITE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

function notifyUnauthorized(): void {
  window.dispatchEvent(new Event("hh:unauthorized"));
}

/**
 * 所有业务请求的统一入口。
 *
 * Bearer token 只存在内存；401 时共享一次 refresh 后重放原请求。
 * 重放后仍然 401 才清理会话并通知外壳跳转配对页。
 */
export async function api<T>(path: string, options: ApiOptions = {}): Promise<T> {
  const method = (options.method ?? "GET").toUpperCase();

  const attempt = (): Promise<T> => {
    const headers: Record<string, string> = {};
    const token = getAccessToken();
    if (token) headers.Authorization = `Bearer ${token}`;
    if (WRITE_METHODS.has(method)) {
      const csrf = getCsrfToken();
      if (csrf) headers["X-CSRF-Token"] = csrf;
    }

    const init: HttpInit = { method, headers };
    if (options.body !== undefined) init.body = options.body;
    if (options.signal) init.signal = options.signal;
    return send<T>(path, init);
  };

  try {
    return await attempt();
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 401) throw error;
    if (options.signal?.aborted) throw error;

    const refreshed = await refreshSession();
    if (!refreshed) {
      notifyUnauthorized();
      throw error;
    }

    try {
      return await attempt();
    } catch (retryError) {
      if (retryError instanceof ApiError && retryError.status === 401) notifyUnauthorized();
      throw retryError;
    }
  }
}
