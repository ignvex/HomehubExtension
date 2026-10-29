import { api } from "./client";
import type { CreatePasteInput, PasteDetail, PasteListResponse } from "./types";

export interface ListPastesOptions {
  limit?: number;
  cursor?: string | null;
  signal?: AbortSignal;
}

export function listPastes(options: ListPastesOptions = {}): Promise<PasteListResponse> {
  const params = new URLSearchParams();
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  if (options.cursor) params.set("cursor", options.cursor);
  const query = params.toString();
  return api<PasteListResponse>(`/pastes${query ? `?${query}` : ""}`, {
    ...(options.signal ? { signal: options.signal } : {}),
  });
}

export function createPaste(input: CreatePasteInput, signal?: AbortSignal): Promise<PasteDetail> {
  return api<PasteDetail>("/pastes", {
    method: "POST",
    body: input,
    ...(signal ? { signal } : {}),
  });
}

export function getPaste(slug: string, signal?: AbortSignal): Promise<PasteDetail> {
  return api<PasteDetail>(`/pastes/${encodeURIComponent(slug)}`, {
    ...(signal ? { signal } : {}),
  });
}

export function deletePaste(slug: string, signal?: AbortSignal): Promise<void> {
  return api<void>(`/pastes/${encodeURIComponent(slug)}`, {
    method: "DELETE",
    ...(signal ? { signal } : {}),
  });
}
