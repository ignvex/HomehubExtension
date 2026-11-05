/**
 * 把来自服务端的地址收窄成可以放进 href 的形式。
 *
 * 只放行 http: 与 https: 的绝对 URL；javascript:、data:、相对路径、
 * 解析失败一律返回 null，调用方据此不渲染链接。
 */
export function safeHref(raw: string): string | null {
  const value = raw.trim();
  if (value === "") return null;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return null;
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") return null;
  return url.href;
}

/**
 * 站内路径：以单个 / 开头、不含反斜杠（浏览器会把 \ 当 /，"/\evil" 等价于 "//evil"）。
 */
export function isSameOriginPath(value: string): boolean {
  return value.startsWith("/") && !value.startsWith("//") && !value.includes("\\");
}
