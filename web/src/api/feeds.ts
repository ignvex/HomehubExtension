import { api } from "./client";
import type { Feed, FeedListResponse } from "./types";

export function listFeeds(signal?: AbortSignal): Promise<FeedListResponse> {
  return api<FeedListResponse>("/feeds", signal ? { signal } : {});
}

export function getFeed(id: number, signal?: AbortSignal): Promise<Feed> {
  return api<Feed>(`/feeds/${id}`, signal ? { signal } : {});
}

export function refreshFeed(id: number, signal?: AbortSignal): Promise<unknown> {
  return api<unknown>(`/feeds/${id}/refresh`, {
    method: "POST",
    ...(signal ? { signal } : {}),
  });
}
