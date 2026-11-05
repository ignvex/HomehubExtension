// 图片压缩：调质量与尺寸，实时显示压缩率。
//
// 与「格式转换」的区别在于关注点：
//   转换关心「变成什么格式」；压缩关心「小了多少」。
// 所以这里默认 WebP、突出前后体积对比，并提供「最长边」这种
// 更贴合实际需求的缩放方式（而不是让用户算宽高比）。

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
} from "./shared";
import {
  computeSize,
  download,
  encode,
  formatByMime,
  loadImage,
  replaceExt,
} from "./img-shared";

const TARGETS: ReadonlyArray<readonly [string, string]> = [
  ["image/webp", "WebP（推荐，同质量下最小）"],
  ["image/jpeg", "JPEG（兼容性最好）"],
  ["image/png", "PNG（无损，通常更大）"],
];

const MAX_EDGES: ReadonlyArray<readonly [string, string]> = [
  ["0", "保持原尺寸"],
  ["1920", "最长边 1920"],
  ["1280", "最长边 1280"],
  ["800", "最长边 800"],
  ["400", "最长边 400"],
];

export const factory: ToolFactory = (ctx) => {
  const fileInput = h("input", {
    class: "input",
    attrs: { type: "file", accept: "image/*", "aria-label": "选择图片" },
  }) as HTMLInputElement;

  const formatSelect = select(TARGETS);
  const edgeSelect = select(MAX_EDGES);
  edgeSelect.value = "1920";

  const qualityInput = h("input", {
    class: "input",
    attrs: { type: "range", min: "20", max: "100", step: "5", value: "80" },
  }) as HTMLInputElement;
  const qualityLabel = h("span", { class: "mono" }, "80");

  const preview = h("div", { class: "img-preview" });
  const error = errorLine();
  const statLine = stats();

  let bitmap: ImageBitmap | null = null;
  let sourceFile: File | null = null;
  let resultBlob: Blob | null = null;
  let previewUrl: string | null = null;

  // 每次编码自增；await 回来时不是最新的一次就丢弃结果（不创建 object URL）。
  let generation = 0;
  // 每次选图自增；旧图晚到的 bitmap 直接关闭。
  let loadGeneration = 0;
  // 卸载后仍在跑的 encode 回来时不能再 createObjectURL，否则 URL 永远不会被 revoke。
  let disposed = false;

  const cleanupPreview = (): void => {
    if (previewUrl) {
      URL.revokeObjectURL(previewUrl);
      previewUrl = null;
    }
  };

  const compress = async (): Promise<void> => {
    generation += 1;
    const current = generation;
    const isStale = (): boolean => disposed || current !== generation;
    if (!bitmap || !sourceFile) {
      preview.replaceChildren(h("p", { class: "muted small" }, "先选一张图片。"));
      return;
    }
    const format = formatByMime(formatSelect.value);
    if (!format) return;

    const image = bitmap;
    try {
      const longest = Number(edgeSelect.value) || 0;
      // 「最长边」比宽高更贴合实际：照片横竖不一，用户只关心多大。
      const size =
        longest > 0
          ? image.width >= image.height
            ? computeSize(image, { width: longest, height: 0, keepAspect: true })
            : computeSize(image, { width: 0, height: longest, keepAspect: true })
          : { width: image.width, height: image.height };

      const quality = Number(qualityInput.value) / 100;
      const source = sourceFile;
      const blob = await encode(image, size, format, quality);
      if (isStale()) return;
      resultBlob = blob;

      cleanupPreview();
      previewUrl = URL.createObjectURL(blob);
      preview.replaceChildren(
        h("img", {
          class: "img-preview-img",
          attrs: { src: previewUrl, alt: "压缩结果预览" },
        }),
      );

      showError(error, "");
      const saved = source.size - blob.size;
      const ratio = Math.round((blob.size / source.size) * 100);
      setStats(statLine, [
        ["原始", `${fmtBytes(source.size)} · ${image.width}×${image.height}`],
        ["压缩后", `${fmtBytes(blob.size)} · ${size.width}×${size.height}`],
        ["比例", `${ratio}%`],
        ["节省", saved > 0 ? fmtBytes(saved) : "反而变大"],
      ]);
    } catch (e) {
      if (isStale()) return;
      showError(error, e instanceof Error ? e.message : String(e));
      resultBlob = null;
      preview.replaceChildren();
      setStats(statLine, []);
    }
  };

  fileInput.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    if (!file) return;
    loadGeneration += 1;
    const current = loadGeneration;
    void (async () => {
      try {
        // 先成功解码新图再关闭旧图：解码失败时旧图与旧结果仍然可用。
        const next = await loadImage(file);
        if (disposed || current !== loadGeneration) {
          next.close();
          return;
        }
        bitmap?.close();
        bitmap = next;
        sourceFile = file;
        await compress();
      } catch (e) {
        if (disposed || current !== loadGeneration) return;
        showError(error, `无法读取这张图片：${e instanceof Error ? e.message : String(e)}`);
      }
    })();
  });

  formatSelect.addEventListener("change", () => {
    qualityInput.disabled = !formatByMime(formatSelect.value)?.lossy;
    void compress();
  });
  edgeSelect.addEventListener("change", () => {
    void compress();
  });
  qualityInput.addEventListener("input", () => {
    qualityLabel.textContent = qualityInput.value;
    void compress();
  });

  const downloadBtn = button(
    "下载",
    () => {
      if (!resultBlob || !sourceFile) {
        ctx.toast.show("还没有可下载的结果", "error");
        return;
      }
      // 扩展名按结果 blob 的实际类型取，而不是按当前下拉框：
      // 新一轮编码还没完成时下拉框已变，浏览器回退格式时类型也会不同。
      const ext = formatByMime(resultBlob.type)?.ext ?? "bin";
      download(resultBlob, replaceExt(sourceFile.name, ext));
    },
    "primary",
  );

  const element = h(
    "div",
    { class: "tool-body" },
    field("选择图片", fileInput),
    h(
      "div",
      { class: "tool-grid-3" },
      field("目标格式", formatSelect),
      field("尺寸", edgeSelect),
      field("质量", h("div", { class: "tool-inline" }, qualityInput, qualityLabel)),
    ),
    actions(downloadBtn),
    error,
    preview,
    statLine,
  );

  preview.append(h("p", { class: "muted small" }, "先选一张图片。"));

  return {
    element,
    unmount: () => {
      disposed = true;
      generation += 1;
      cleanupPreview();
      bitmap?.close();
      bitmap = null;
    },
  };
};

export default factory;
