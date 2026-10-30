// 颜色转换：HEX / RGB / HSL 互转。
//
// 三者的关系要说清楚，否则用户会奇怪为什么改了 HEX 而 HSL 跟着动：
//   HEX 与 RGB 是一一对应的（同一个值的两种写法）。
//   HSL 是 RGB 的另一种参数化，多个 HSL 可以对应同一个 RGB
//   （色相在灰色上无意义），所以从 RGB 转 HSL 后回不到原始 HSL。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import { actions, button, copyButton, errorLine, field, showError, textInput } from "./shared";

interface RGB {
  r: number;
  g: number;
  b: number;
}
interface HSL {
  h: number;
  s: number;
  l: number;
}

const HEX6 = /^#?([0-9a-f]{6})$/iu;
const HEX3 = /^#?([0-9a-f]{3})$/iu;
const RGB_RE = /^rgba?\(\s*(\d{1,3})\s*[, ]\s*(\d{1,3})\s*[, ]\s*(\d{1,3})/iu;
const HSL_RE = /^hsla?\(\s*([\d.]+)(?:deg)?\s*[, ]\s*([\d.]+)%\s*[, ]\s*([\d.]+)%/iu;

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(Math.max(n, lo), hi);
}

function hexToRgb(input: string): RGB | null {
  const s = input.trim();
  const m6 = HEX6.exec(s);
  if (m6) {
    const v = parseInt(m6[1] ?? "", 16);
    return { r: (v >> 16) & 0xff, g: (v >> 8) & 0xff, b: v & 0xff };
  }
  const m3 = HEX3.exec(s);
  if (m3) {
    const chars = (m3[1] ?? "").split("");
    const expand = (c: string): number => parseInt(c + c, 16);
    return { r: expand(chars[0] ?? "0"), g: expand(chars[1] ?? "0"), b: expand(chars[2] ?? "0") };
  }
  const mr = RGB_RE.exec(s);
  if (mr) {
    return {
      r: clamp(Number(mr[1]), 0, 255),
      g: clamp(Number(mr[2]), 0, 255),
      b: clamp(Number(mr[3]), 0, 255),
    };
  }
  return null;
}

function rgbToHex({ r, g, b }: RGB): string {
  const hex = (n: number): string => Math.round(n).toString(16).padStart(2, "0");
  return `#${hex(r)}${hex(g)}${hex(b)}`;
}

function rgbToHsl({ r, g, b }: RGB): HSL {
  const rn = r / 255;
  const gn = g / 255;
  const bn = b / 255;
  const max = Math.max(rn, gn, bn);
  const min = Math.min(rn, gn, bn);
  const l = (max + min) / 2;
  const d = max - min;

  if (d === 0) return { h: 0, s: 0, l: l * 100 };

  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  let h: number;
  if (max === rn) h = ((gn - bn) / d) % 6;
  else if (max === gn) h = (bn - rn) / d + 2;
  else h = (rn - gn) / d + 4;
  h *= 60;
  if (h < 0) h += 360;

  return { h, s: s * 100, l: l * 100 };
}

function hslToRgb({ h: hDeg, s: sPct, l: lPct }: HSL): RGB {
  const h = ((hDeg % 360) + 360) % 360;
  const s = clamp(sPct, 0, 100) / 100;
  const l = clamp(lPct, 0, 100) / 100;

  const c = (1 - Math.abs(2 * l - 1)) * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = l - c / 2;

  let rp = 0;
  let gp = 0;
  let bp = 0;
  if (h < 60) [rp, gp, bp] = [c, x, 0];
  else if (h < 120) [rp, gp, bp] = [x, c, 0];
  else if (h < 180) [rp, gp, bp] = [0, c, x];
  else if (h < 240) [rp, gp, bp] = [0, x, c];
  else if (h < 300) [rp, gp, bp] = [x, 0, c];
  else [rp, gp, bp] = [c, 0, x];

  return {
    r: Math.round((rp + m) * 255),
    g: Math.round((gp + m) * 255),
    b: Math.round((bp + m) * 255),
  };
}

/** 按 WCAG 计算相对亮度，用于判断深色背景上该用黑字还是白字。 */
function luminance({ r, g, b }: RGB): number {
  const channel = (v: number): number => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

export const factory: ToolFactory = (ctx) => {
  const hexInput = textInput("#3b82f6", { mono: true });
  const rgbInput = textInput("rgb(59, 130, 246)", { mono: true });
  const hslInput = textInput("hsl(217, 91%, 60%)", { mono: true });
  const picker = h("input", {
    attrs: { type: "color", value: "#3b82f6", "aria-label": "取色器" },
  }) as HTMLInputElement;
  const swatch = h("div", { class: "color-swatch", attrs: { "aria-hidden": "true" } });
  const contrastNote = h("p", { class: "color-contrast" });
  const error = errorLine();

  // 防止三个输入框互相触发形成死循环。
  let syncing = false;

  const paint = (rgb: RGB): void => {
    const hex = rgbToHex(rgb);
    swatch.style.setProperty("--swatch", hex);
    picker.value = hex;

    const lum = luminance(rgb);
    // WCAG 对比度：与白色、黑色各算一次，取高的那个。
    const withWhite = 1.05 / (lum + 0.05);
    const withBlack = (lum + 0.05) / 0.05;
    const best = withWhite >= withBlack ? "白" : "黑";
    const ratio = Math.max(withWhite, withBlack);
    contrastNote.textContent =
      `相对亮度 ${lum.toFixed(3)} · 建议文字用${best}色（对比度 ${ratio.toFixed(2)}:1）` +
      (ratio >= 4.5 ? " · 达 AA" : ratio >= 3 ? " · 达 AA 大字" : " · 不足 AA");

    syncing = true;
    hexInput.value = hex;
    const { r, g, b } = rgb;
    rgbInput.value = `rgb(${r}, ${g}, ${b})`;
    const hsl = rgbToHsl(rgb);
    hslInput.value = `hsl(${Math.round(hsl.h)}, ${Math.round(hsl.s)}%, ${Math.round(hsl.l)}%)`;
    syncing = false;
    showError(error, "");
  };

  const syncFromHex = (): void => {
    if (syncing) return;
    const rgb = hexToRgb(hexInput.value);
    if (!rgb) {
      showError(error, "无法识别：支持 #RGB、#RRGGBB 或 rgb(r,g,b)");
      return;
    }
    paint(rgb);
  };

  const syncFromRgb = (): void => {
    if (syncing) return;
    const rgb = hexToRgb(rgbInput.value);
    if (!rgb) {
      showError(error, "无法识别：格式应为 rgb(255, 0, 0)");
      return;
    }
    paint(rgb);
  };

  const syncFromHsl = (): void => {
    if (syncing) return;
    const m = HSL_RE.exec(hslInput.value.trim());
    if (!m) {
      showError(error, "无法识别：格式应为 hsl(217, 91%, 60%)");
      return;
    }
    paint(hslToRgb({ h: Number(m[1]), s: Number(m[2]), l: Number(m[3]) }));
  };

  hexInput.addEventListener("input", syncFromHex);
  rgbInput.addEventListener("input", syncFromRgb);
  hslInput.addEventListener("input", syncFromHsl);
  picker.addEventListener("input", () => {
    const rgb = hexToRgb(picker.value);
    if (rgb) paint(rgb);
  });

  const randomBtn = button("随机", () => {
    const bytes = new Uint8Array(3);
    crypto.getRandomValues(bytes);
    paint({ r: bytes[0] ?? 0, g: bytes[1] ?? 0, b: bytes[2] ?? 0 });
  });

  const element = h(
    "div",
    { class: "tool-body" },
    h(
      "div",
      { class: "color-top" },
      swatch,
      h(
        "div",
        { class: "color-top-side" },
        picker,
        contrastNote,
        actions(
          randomBtn,
          copyButton(() => hexInput.value, ctx.toast, "复制 HEX"),
          copyButton(() => rgbInput.value, ctx.toast, "复制 RGB"),
        ),
      ),
    ),
    h(
      "div",
      { class: "tool-grid-3" },
      field("HEX", hexInput),
      field("RGB", rgbInput),
      field("HSL", hslInput),
    ),
    error,
  );

  syncFromHex();
  return { element };
};

export default factory;
