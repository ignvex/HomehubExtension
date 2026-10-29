import { api } from "./client";
import type { EntryDetail, EntryListResponse, EntryPatch } from "./types";

export interface ListEntriesOptions {
  feedId?: number | null;
  unread?: boolean;
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
