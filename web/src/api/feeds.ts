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

export interface CreateFeedInput {
  url: string;
  title?: string;
  interval?: string;
  enabled?: boolean;
}

export function createFeed(input: CreateFeedInput, signal?: AbortSignal): Promise<Feed> {
  return api<Feed>("/feeds", {
    method: "POST",
    body: input,
    ...(signal ? { signal } : {}),
  });
}

export interface UpdateFeedInput {
  title?: string;
  interval?: string;
  enabled?: boolean;
}

export function updateFeed(
  id: number,
  input: UpdateFeedInput,
  signal?: AbortSignal,
): Promise<Feed> {
  return api<Feed>(`/feeds/${id}`, {
    method: "PATCH",
    body: input,
    ...(signal ? { signal } : {}),
  });
}

export function deleteFeed(id: number, signal?: AbortSignal): Promise<void> {
  return api<void>(`/feeds/${id}`, {
    method: "DELETE",
    ...(signal ? { signal } : {}),
  });
}
