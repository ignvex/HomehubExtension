// 图片格式转换：PNG / JPG / WebP / BMP / ICO 互转。
//
// 关键在于「不要相信 canvas.toBlob 一定成功」：
// 浏览器对 ICO / BMP 的支持参差不齐，不支持时 toBlob 可能返回 null
// 或静默回退成 PNG。两种都要处理，否则用户会拿到一个扩展名
// 与内容不符的文件。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  bytes as fmtBytes,
  button,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textInput,
} from "./shared";
import {
  FORMATS,
  computeSize,
  download,
  encode,
  formatByMime,
  loadImage,
  replaceExt,
  supportsFormat,
} from "./img-shared";

export const factory: ToolFactory = (ctx) => {
  const fileInput = h("input", {
    class: "input",
    attrs: { type: "file", accept: "image/*", "aria-label": "选择图片" },
  }) as HTMLInputElement;

  const formatSelect = select(FORMATS.map((f) => [f.mime, f.label] as const));
  formatSelect.value = "image/webp";

  const qualityInput = h("input", {
    class: "input",
    attrs: { type: "range", min: "10", max: "100", step: "5", value: "90" },
  }) as HTMLInputElement;
  const qualityValue = h("span", { class: "mono" }, "90");

  const widthInput = textInput("留空保持原宽", { type: "number", mono: true });
  widthInput.type = "number";
  widthInput.min = "1";
  const heightInput = textInput("留空保持原高", { type: "number", mono: true });
  heightInput.type = "number";
  heightInput.min = "1";

  const keepAspect = h("input", {
    attrs: { type: "checkbox", checked: "" },
  }) as HTMLInputElement;

  const preview = h("div", { class: "img-preview" });
  const error = errorLine();
  const statLine = stats();

  let bitmap: ImageBitmap | null = null;
  let sourceFile: File | null = null;
  let resultBlob: Blob | null = null;
  let previewUrl: string | null = null;

  const cleanupPreview = (): void => {
    if (previewUrl) {
      URL.revokeObjectURL(previewUrl);
      previewUrl = null;
    }
  };

  const selectedFormat = () => formatByMime(formatSelect.value) ?? FORMATS[0];

  const convert = async (): Promise<void> => {
    if (!bitmap || !sourceFile) {
      preview.replaceChildren(h("p", { class: "muted small" }, "先选一张图片。"));
      return;
    }

    const format = selectedFormat();
    if (!format) return;

    try {
      const natural = { width: bitmap.width, height: bitmap.height };
      const width = Number(widthInput.value) || 0;
      const height = Number(heightInput.value) || 0;
      const size = computeSize(natural, { width, height, keepAspect: keepAspect.checked });

      const quality = Number(qualityInput.value) / 100;
      const blob = await encode(bitmap, size, format, quality);
      resultBlob = blob;

      cleanupPreview();
      previewUrl = URL.createObjectURL(blob);
      const img = h("img", {
        class: "img-preview-img",
        attrs: { src: previewUrl, alt: "转换结果预览" },
      });
      preview.replaceChildren(img);

      showError(error, "");
      const ratio = Math.round((blob.size / sourceFile.size) * 100);
      setStats(statLine, [
        ["原始", `${fmtBytes(sourceFile.size)} ${natural.width}×${natural.height}`],
        ["结果", `${fmtBytes(blob.size)} ${size.width}×${size.height}`],
        ["体积", `${ratio}%`],
        ["格式", format.label],
      ]);
    } catch (e) {
      showError(error, e instanceof Error ? e.message : String(e));
      preview.replaceChildren();
      resultBlob = null;
      setStats(statLine, []);
    }
  };

  fileInput.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    if (!file) return;
    sourceFile = file;
    void (async () => {
      try {
        bitmap?.close();
        bitmap = await loadImage(file);
        widthInput.placeholder = `原宽 ${bitmap.width}`;
        heightInput.placeholder = `原高 ${bitmap.height}`;
        await convert();
      } catch (e) {
        showError(error, `无法读取这张图片：${e instanceof Error ? e.message : String(e)}`);
      }
    })();
  });

  formatSelect.addEventListener("change", () => {
    const format = selectedFormat();
    qualityInput.disabled = !format?.lossy;
    void convert();
  });

  qualityInput.addEventListener("input", () => {
    qualityValue.textContent = qualityInput.value;
    void convert();
  });

  for (const input of [widthInput, heightInput, keepAspect]) {
    input.addEventListener("change", () => {
      void convert();
    });
  }

  const downloadBtn = button(
    "下载",
    () => {
      if (!resultBlob || !sourceFile) {
        ctx.toast.show("还没有可下载的结果", "error");
        return;
      }
      const format = selectedFormat();
      download(resultBlob, replaceExt(sourceFile.name, format?.ext ?? "bin"));
    },
    "primary",
  );

  const element = h(
    "div",
    { class: "tool-body" },
    field("选择图片", fileInput, "支持 PNG / JPEG / WebP / GIF / BMP"),
    h(
      "div",
      { class: "tool-grid-3" },
      field("目标格式", formatSelect),
      field("质量", h("div", { class: "tool-inline" }, qualityInput, qualityValue)),
      field("保持宽高比", keepAspect),
    ),
    h(
      "div",
      { class: "tool-grid-2" },
      field("宽度（px）", widthInput),
      field("高度（px）", heightInput),
    ),
    actions(downloadBtn),
    error,
    preview,
    statLine,
  );

  // 启动时探测格式支持，把不可用的选项置灰。
  void (async () => {
    for (const format of FORMATS) {
      if (!format.maybeUnsupported) continue;
      const ok = await supportsFormat(format);
      if (ok) continue;
      const option = [...formatSelect.options].find((o) => o.value === format.mime);
      if (option) {
        option.disabled = true;
        option.textContent = `${format.label}（当前浏览器不支持）`;
      }
    }
  })();

  preview.append(h("p", { class: "muted small" }, "先选一张图片。"));

  return {
    element,
    unmount: () => {
      cleanupPreview();
      bitmap?.close();
      bitmap = null;
    },
  };
};

export default factory;
