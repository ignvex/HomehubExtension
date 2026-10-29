/**
 * 服务端所有绝对时刻都是 Unix 秒。
 *
 * 只接受 number，不接受字符串：之前为了兼容混用的两种形态，
 * 这里写过 `value < 1e12 ? value * 1000 : value` 的启发式——
 * 那本质上是在猜「这是秒还是毫秒」，遇到合法但偏小的值会静默算错。
 * 现在协议统一了，猜的部分可以删掉。
 */
export type TimeValue = number | null | undefined;

const dateTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  dateStyle: "medium",
  timeStyle: "short",
});

const dateFormatter = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium" });
const numberFormatter = new Intl.NumberFormat("zh-CN");

export function parseTime(value: TimeValue): Date | null {
  // 0 在 Unix 时间里是 1970-01-01，一个真实时刻。
  // 但服务端约定 0 表示「未设置」并直接给 null，
  // 所以这里把 0 也当成空——真出现 0 只能是数据异常。
  if (value === null || value === undefined) return null;
  if (!Number.isFinite(value) || value <= 0) return null;
  // 唯一的秒 → 毫秒转换点。全前端只此一处，改了这里就全改了。
  return new Date(value * 1000);
}

/**
 * 当前时刻的 Unix 秒。服务端所有时刻都是这个单位，
 * 前端做乐观更新时也必须用它，不能用 toISOString() 拼字符串。
 */
export function nowSeconds(): number {
  return Math.floor(Date.now() / 1000);
}

/**
 * 格式化解码出来的时间字符串（如构建时间 "2026-10-29T15:00:35Z"）。
 *
 * 与 formatDateTime 分开是刻意的：那个函数只吃 Unix 秒，
 * 混着接受字符串就会重新引入「这是秒还是 ISO 串」的分支判断。
 * 这里用于构建元数据这类本来就以字符串存在的字段。
 */
export function formatInstantString(value: string | null | undefined): string {
  if (!value || value === "unknown") return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : dateTimeFormatter.format(date);
}

export function formatDateTime(value: TimeValue): string {
  const date = parseTime(value);
  return date ? dateTimeFormatter.format(date) : "—";
}

export function formatDate(value: TimeValue): string {
  const date = parseTime(value);
  return date ? dateFormatter.format(date) : "—";
}

export function formatRelative(value: TimeValue): string {
  const date = parseTime(value);
  if (!date) return "—";
  const diffSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  const abs = Math.abs(diffSeconds);

  if (abs < 45) return "刚刚";
  if (abs < 3600) {
    const minutes = Math.max(1, Math.round(abs / 60));
    return diffSeconds > 0 ? `${minutes} 分钟后` : `${minutes} 分钟前`;
  }
  if (abs < 86400) {
    const hours = Math.round(abs / 3600);
    return diffSeconds > 0 ? `${hours} 小时后` : `${hours} 小时前`;
  }
  if (abs < 86400 * 30) {
    const days = Math.round(abs / 86400);
    return diffSeconds > 0 ? `${days} 天后` : `${days} 天前`;
  }
  return formatDate(value);
}

export function formatBytes(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value) || value < 0) return "—";
  if (value < 1024) return `${Math.round(value)} B`;
  const units = ["KB", "MB", "GB", "TB"] as const;
  let amount = value / 1024;
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index += 1;
  }
  const digits = amount >= 100 ? 0 : amount >= 10 ? 1 : 2;
  return `${amount.toFixed(digits)} ${units[index] ?? "TB"}`;
}

export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds < 0) {
    return "—";
  }
  const whole = Math.floor(seconds);
  if (whole < 60) return `${whole} 秒`;

  const days = Math.floor(whole / 86400);
  const hours = Math.floor((whole % 86400) / 3600);
  const minutes = Math.floor((whole % 3600) / 60);
  const parts: string[] = [];
  if (days > 0) parts.push(`${days} 天`);
  if (hours > 0) parts.push(`${hours} 小时`);
  if (days === 0 && minutes > 0) parts.push(`${minutes} 分钟`);
  return parts.join(" ") || "不到 1 分钟";
}

export function formatCount(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return "—";
  return numberFormatter.format(value);
}

/** 把 Go 的 duration 字符串（"30m"、"168h"、"1h30m"）转成中文短标签。 */
export function formatInterval(value: string | null | undefined): string {
  if (!value) return "—";
  const pattern = /(\d+(?:\.\d+)?)(ns|µs|us|ms|s|m|h)/gu;
  const factors: Record<string, number> = {
    ns: 1e-9,
    µs: 1e-6,
    us: 1e-6,
    ms: 1e-3,
    s: 1,
    m: 60,
    h: 3600,
  };
  let total = 0;
  let matched = false;
  for (const match of value.matchAll(pattern)) {
    const amount = Number.parseFloat(match[1] ?? "0");
    const unit = match[2] ?? "";
    total += amount * (factors[unit] ?? 0);
    matched = true;
  }
  return matched ? formatDuration(total) : value;
}
