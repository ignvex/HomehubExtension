// 二维码生成 UI。
//
// 渲染策略：先用 canvas 画，再转成 PNG 供下载。
// 也保留一个 SVG 版本供复制——SVG 在任何尺寸下都清晰，
// 而 canvas 放大后会模糊。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import { actions, button, copyButton, errorLine, field, select, setStats, showError, stats, textarea } from "./shared";
import { encodeQR, maxPayloadBytes, type ECLevel, type QRMatrix } from "./qr-encode";

/** 画到 canvas。quiet 是四周的静区，标准要求至少 4 个模块。 */
function drawToCanvas(matrix: QRMatrix, scale: number, quiet = 4): HTMLCanvasElement {
  const modules = matrix.size + quiet * 2;
  const px = modules * scale;
  const canvas = document.createElement("canvas");
  canvas.width = px;
  canvas.height = px;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("浏览器不支持 Canvas 2D");

  ctx.fillStyle = "#ffffff";
  ctx.fillRect(0, 0, px, px);
  ctx.fillStyle = "#000000";
  for (let r = 0; r < matrix.size; r += 1) {
    for (let c = 0; c < matrix.size; c += 1) {
      if (!matrix.modules[r]?.[c]) continue;
      ctx.fillRect((c + quiet) * scale, (r + quiet) * scale, scale, scale);
    }
  }
  return canvas;
}

/** 生成 SVG 字符串。 */
function toSVG(matrix: QRMatrix, quiet = 4): string {
  const modules = matrix.size + quiet * 2;
  const parts: string[] = [
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${modules} ${modules}" shape-rendering="crispEdges">`,
    `<rect width="${modules}" height="${modules}" fill="#fff"/>`,
    `<path fill="#000" d="`,
  ];
  for (let r = 0; r < matrix.size; r += 1) {
    for (let c = 0; c < matrix.size; c += 1) {
      if (matrix.modules[r]?.[c]) parts.push(`M${c + quiet} ${r + quiet}h1v1h-1z`);
    }
  }
  parts.push(`"/></svg>`);
  return parts.join("");
}

export const factory: ToolFactory = (ctx) => {
  const text = textarea("输入文本或链接", { rows: 4 });
  text.value = "https://www.ignvex.cc/";

  const level = select([
    ["L", "L — 约 7% 容错（容量最大）"],
    ["M", "M — 约 15% 容错（推荐）"],
    ["Q", "Q — 约 25% 容错"],
    ["H", "H — 约 30% 容错（最耐污损）"],
  ]);
  level.value = "M";

  const sizeSelect = select([
    ["4", "小（每模块 4px）"],
    ["8", "中（每模块 8px）"],
    ["12", "大（每模块 12px）"],
    ["16", "超大（每模块 16px）"],
  ]);
  sizeSelect.value = "8";

  const preview = h("div", { class: "qr-preview" });
  const error = errorLine();
  const statLine = stats();

  let currentMatrix: QRMatrix | null = null;

  const render = (): void => {
    const value = text.value;
    if (value.trim() === "") {
      preview.replaceChildren(h("p", { class: "muted small" }, "输入内容后这里会显示二维码。"));
      currentMatrix = null;
      setStats(statLine, []);
      showError(error, "");
      return;
    }

    try {
      const matrix = encodeQR(value, level.value as ECLevel);
      currentMatrix = matrix;

      preview.replaceChildren();
      const scale = Number(sizeSelect.value) || 8;
      const canvas = drawToCanvas(matrix, scale);
      canvas.className = "qr-canvas";
      // canvas 的像素尺寸是固定的，用 CSS 控制显示大小，
      // 这样在小屏上不会撑破布局。
      canvas.style.setProperty("max-width", "100%");
      canvas.style.setProperty("height", "auto");
      preview.append(canvas);

      showError(error, "");
      const bytes = new TextEncoder().encode(value).length;
      // 版本对应的纠错等级下可存字节数，用于提示剩余空间。
      setStats(statLine, [
        ["版本", String((matrix.size - 17) / 4)],
        ["尺寸", `${matrix.size}×${matrix.size}`],
        ["内容", `${bytes} / ${maxPayloadBytes(level.value as ECLevel)} 字节`],
        ["纠错", level.value],
      ]);
    } catch (e) {
      currentMatrix = null;
      preview.replaceChildren();
      setStats(statLine, []);
      showError(error, e instanceof Error ? e.message : String(e));
    }
  };

  text.addEventListener("input", render);
  level.addEventListener("change", render);
  sizeSelect.addEventListener("change", render);

  const downloadPng = button(
    "下载 PNG",
    () => {
      if (!currentMatrix) {
        ctx.toast.show("先生成二维码", "error");
        return;
      }
      const canvas = drawToCanvas(currentMatrix, 16);
      canvas.toBlob((blob) => {
        if (!blob) {
          ctx.toast.show("导出失败", "error");
          return;
        }
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = "qrcode.png";
        a.click();
        window.setTimeout(() => {
          URL.revokeObjectURL(url);
        }, 10_000);
      }, "image/png");
    },
    "primary",
  );

  const element = h(
    "div",
    { class: "tool-body" },
    field("内容", text, "文本或链接，支持中文"),
    h(
      "div",
      { class: "tool-grid-2" },
      field("纠错等级", level, "等级越高越耐污损，但能存的内容越少"),
      field("显示尺寸", sizeSelect),
    ),
    actions(
      downloadPng,
      copyButton(() => (currentMatrix ? toSVG(currentMatrix) : ""), ctx.toast, "复制 SVG"),
    ),
    error,
    preview,
    statLine,
  );

  render();
  return { element };
};

export default factory;
