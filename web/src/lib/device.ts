export function suggestDeviceName(): string {
  const ua = navigator.userAgent;
  let browser = "浏览器";
  if (/Edg\//u.test(ua)) browser = "Edge";
  else if (/Firefox\//u.test(ua)) browser = "Firefox";
  else if (/Chrome\//u.test(ua)) browser = "Chrome";
  else if (/Safari\//u.test(ua)) browser = "Safari";

  let system = "";
  if (/Windows/u.test(ua)) system = "Windows";
  else if (/Mac OS X/u.test(ua)) system = "macOS";
  else if (/Android/u.test(ua)) system = "Android";
  else if (/iPhone|iPad/u.test(ua)) system = "iOS";
  else if (/Linux/u.test(ua)) system = "Linux";

  return system ? `${browser} · ${system}` : browser;
}

/** 粘贴链接使用服务端公开页 /p/{slug}；没有 public_url 时以当前源拼接。 */
export function pasteUrl(slug: string): string {
  return `${window.location.origin}/p/${encodeURIComponent(slug)}`;
}
