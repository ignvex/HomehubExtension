import { api } from "./client";
import type {
  AppsResponse,
  JobRun,
  JobsResponse,
  SystemInfo,
  SystemStats,
} from "./types";

export function getApps(signal?: AbortSignal): Promise<AppsResponse> {
  return api<AppsResponse>("/apps", signal ? { signal } : {});
}

export function getSystemInfo(signal?: AbortSignal): Promise<SystemInfo> {
  return api<SystemInfo>("/system/info", signal ? { signal } : {});
}

export function getStats(signal?: AbortSignal): Promise<SystemStats> {
  return api<SystemStats>("/system/stats", signal ? { signal } : {});
}

export function getJobs(signal?: AbortSignal): Promise<JobRun[]> {
  return api<JobsResponse>("/system/jobs", signal ? { signal } : {}).then((res) => res.jobs);
}
