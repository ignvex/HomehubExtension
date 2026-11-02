import { api } from "./client";
import type { EntryDetail, EntryListResponse, EntryPatch } from "./types";

export interface ListEntriesOptions {
  feedId?: number | null;
  unread?: boolean;
  starred?: boolean;
  search?: string;
  limit?: number;
  cursor?: string | null;
  signal?: AbortSignal;
}

export function listEntries(options: ListEntriesOptions = {}): Promise<EntryListResponse> {
  const params = new URLSearchParams();
  if (options.feedId !== undefined && options.feedId !== null) {
    params.set("feed_id", String(options.feedId));
  }
  if (options.unread) params.set("unread", "true");
  if (options.starred) params.set("starred", "true");
  // 关键词原样交给服务端：它负责 LIKE 转义与 ESCAPE 子句。
  // 前端不要自己拼 % —— 那样会绕过转义，搜 _ 会变成通配符。
  const q = options.search?.trim();
  if (q) params.set("q", q);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  if (options.cursor) params.set("cursor", options.cursor);
  const query = params.toString();
  return api<EntryListResponse>(`/entries${query ? `?${query}` : ""}`, {
    ...(options.signal ? { signal: options.signal } : {}),
  });
}

export function getEntry(id: number, signal?: AbortSignal): Promise<EntryDetail> {
  return api<EntryDetail>(`/entries/${id}`, signal ? { signal } : {});
}

export function patchEntry(id: number, patch: EntryPatch, signal?: AbortSignal): Promise<void> {
  return api<void>(`/entries/${id}`, {
    method: "PATCH",
    body: patch,
    ...(signal ? { signal } : {}),
  });
}
