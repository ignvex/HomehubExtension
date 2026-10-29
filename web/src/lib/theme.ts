export type Theme = "auto" | "light" | "dark";

const THEME_COOKIE = "hh_theme";
const THEME_MAX_AGE = 31536000;

export const THEMES: readonly Theme[] = ["auto", "light", "dark"];

export const THEME_LABELS: Record<Theme, string> = {
  auto: "跟随系统",
  light: "亮",
  dark: "暗",
};

export function normalizeTheme(value: string | null | undefined): Theme {
  return value === "light" || value === "dark" || value === "auto" ? value : "auto";
}

export function currentTheme(): Theme {
  return normalizeTheme(document.documentElement.dataset.theme);
}

/**
 * 主题值同时写到 <html data-theme> 与 hh_theme cookie。
 * 不设 HttpOnly：前端切换时必须能写；服务端渲染 index.html 时读取它替换占位符。
 */
export function applyTheme(theme: Theme, persist = true): void {
  document.documentElement.dataset.theme = theme;
  if (!persist) return;
  document.cookie = `${THEME_COOKIE}=${theme}; Path=/; Max-Age=${THEME_MAX_AGE}; SameSite=Lax`;
}

export function initTheme(): Theme {
  const theme = normalizeTheme(document.documentElement.dataset.theme);
  document.documentElement.dataset.theme = theme;
  return theme;
}

export function nextTheme(theme: Theme): Theme {
  switch (theme) {
    case "auto":
      return "light";
    case "light":
      return "dark";
    case "dark":
      return "auto";
  }
}
