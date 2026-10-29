// 与 Go 端 DTO 对齐的类型。
//
// 时间约定（全项目统一）：
//   * 绝对时刻一律是 Unix 秒（number），未设置时为 null。
//     不用字符串：字符串会有 "Z" 与 "+08:00"、有无小数秒的差异，
//     一旦某处序列化方式漂移，客户端就得写兼容分支。
//   * 时长一律是字符串（"30m"、"168h"），因为它是人写的、需要可读。
//     例外：API 的 expires_in 是整数秒，那是计数值不是配置。
//   * 唯一的时刻例外是 SystemInfo.build_time，它是构建元数据
//     （-ldflags 注入），不是运行时事实，保留人可读字符串。
export type AuthAlgorithm = "ed25519" | "p256";
export type PasteVisibility = "public" | "unlisted" | "private";

export interface AuthClient {
  id: string;
  name: string;
}

export interface ChallengeResponse {
  nonce: string;
  /** Unix 秒。 */
  expires_at: number;
  /** 后端回显的待签消息，仅用于排查拼装错误。 */
  message?: string;
}

export interface AuthSessionResponse {
  access_token: string;
  token_type: string;
  /** 剩余有效秒数（计数值，不是时刻）。 */
  expires_in: number;
  /** Unix 秒。 */
  expires_at: number;
  csrf_token: string;
  client: AuthClient;
}

export interface EnrollResponse {
  client_id: string;
  name: string;
  server_key: string;
  kid: string;
  algorithm: AuthAlgorithm;
}

export interface AppBadge {
  count: number;
  label: string;
}

export interface AppInfo {
  id: string;
  name: string;
  description: string;
  icon: string;
  path: string;
  enabled: boolean;
  badge: AppBadge | null;
}

export interface AppsResponse {
  apps: AppInfo[];
}

export interface SystemInfo {
  version: string;
  commit: string;
  build_time: string;
  go_version: string;
  schema_version: number;
  uptime_seconds: number;
  public_url?: string;
}

export interface SystemStats {
  pastes: number;
  feeds: number;
  entries: number;
  unread: number;
  blobs: number;
  blob_bytes: number;
  pending_media: number;
  disk_total?: number;
  disk_free?: number;
}

export interface JobRun {
  id: number;
  job: string;
  started_at: number;
  finished_at: number | null;
  status: string;
  detail: string;
}

export interface JobsResponse {
  jobs: JobRun[];
}

export interface PasteSummary {
  slug: string;
  title: string;
  language: string;
  visibility: PasteVisibility;
  size_bytes: number;
  expires_at: number | null;
  burn_after_read: boolean;
  has_password: boolean;
  created_at: number;
  updated_at?: number | null;
  url?: string;
}

export interface PasteDetail extends PasteSummary {
  body: string;
}

export interface PasteListResponse {
  items: PasteSummary[];
  next_cursor: string | null;
}

export interface CreatePasteInput {
  title: string;
  body: string;
  language: string;
  visibility: PasteVisibility;
  expires_in: string;
  password: string;
  burn_after_read: boolean;
}

export interface Feed {
  id: number;
  url: string;
  title: string;
  site_url: string;
  enabled: boolean;
  fetch_interval: string;
  next_fetch_at: number | null;
  last_fetch_at?: number | null;
  last_status: number;
  last_error: string;
  error_count?: number;
  unread_count: number;
  total_count?: number;
}

export interface FeedListResponse {
  items: Feed[];
}

export type EntryId = number;

export interface EntrySummary {
  id: EntryId;
  feed_id: number;
  title: string;
  url: string;
  author: string;
  summary: string;
  published_at: number | null;
  fetched_at: number;
  read_at: number | null;
  starred_at: number | null;
}

export interface EntryDetail extends EntrySummary {
  content_html: string;
  content_text: string;
}

export interface EntryListResponse {
  items: EntrySummary[];
  next_cursor: string | null;
}

export interface EntryPatch {
  read?: boolean;
  starred?: boolean;
}
