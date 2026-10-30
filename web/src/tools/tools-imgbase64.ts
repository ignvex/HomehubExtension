// 图片 ↔ Base64 Data URL。
//
// 两个方向：
//   图片 → Base64：读文件转 Data URL，可只取裸 Base64（用于内联 CSS）
//   Base64 → 图片：粘贴 Data URL 或裸 Base64，预览并下载
//
// 一个常见坑：用户粘贴的 Base64 可能带 data:image/png;base64, 前缀，
// 也可能只有内容。两种都要接受，并按魔数判断真实类型——
// 前缀里的 MIME 是文本，可能是错的。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  bytes as fmtBytes,
  button,
  copyButton,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textarea,
} from "./shared";

/** 从魔数判断图片类型。返回 [mime, ext]。 */
function sniffImage(bytes: Uint8Array): [string, string] | null {
  const b = bytes;
  if (b.length >= 8 && b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47) {
    return ["image/png", "png"];
  }
  if (b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) {
    return ["image/jpeg", "jpg"];
  }
  if (b.length >= 6 && b[0] === 0x47 && b[1] === 0x49 && b[2] === 0x46) {
    return ["image/gif", "gif"];
  }
  if (
    b.length >= 12 &&
    b[0] === 0x52 && b[1] === 0x49 && b[2] === 0x46 && b[3] === 0x46 &&
    b[8] === 0x57 && b[9] === 0x45 && b[10] === 0x42 && b[11] === 0x50
  ) {
    return ["image/webp", "webp"];
  }
  if (b.length >= 2 && b[0] === 0x42 && b[1] === 0x4d) {
    return ["image/bmp", "bmp"];
  }
  return null;
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

function base64ToBytes(b64: string): Uint8Array {
  const binary = atob(b64);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) out[i] = binary.charCodeAt(i);
  return out;
}

export const factory: ToolFactory = (ctx) => {
  const mode = select([
    ["toBase64", "图片 → Base64"],
    ["toImage", "Base64 → 图片"],
  ]);

  const fileInput = h("input", {
    class: "input",
    attrs: { type: "file", accept: "image/*", "aria-label": "选择图片" },
  }) as HTMLInputElement;

  const includePrefix = h("input", {
    attrs: { type: "checkbox", checked: "" },
  }) as HTMLInputElement;

  const output = textarea("结果会显示在这里", { rows: 10, readonly: true });
  const inputArea = textarea("粘贴 Data URL 或裸 Base64", { rows: 10 });
  const preview = h("div", { class: "img-preview" });
  const error = errorLine();
  const statLine = stats();

  let previewUrl: string | null = null;
  let lastResult: { blob: Blob; ext: string } | null = null;

  const cleanup = (): void => {
    if (previewUrl) {
      URL.revokeObjectURL(previewUrl);
      previewUrl = null;
    }
  };

  const showImage = (bytes: Uint8Array, mime: string, ext: string): void => {
    cleanup();
    const blob = new Blob([bytes as unknown as BlobPart], { type: mime });
    lastResult = { blob, ext };
    previewUrl = URL.createObjectURL(blob);
    preview.replaceChildren(
      h("img", { class: "img-preview-img", attrs: { src: previewUrl, alt: "预览" } }),
    );
  };

  const fromFile = (file: File): void => {
    const reader = new FileReader();
    reader.onload = () => {
      try {
        const bytes = new Uint8Array(reader.result as ArrayBuffer);
        const sniffed = sniffImage(bytes);
        if (!sniffed) {
          showError(error, "无法识别这个文件是不是图片（检查文件头失败）");
          return;
        }
        const [mime, ext] = sniffed;
        const b64 = bytesToBase64(bytes);
        const dataUrl = `data:${mime};base64,${b64}`;
        output.value = includePrefix.checked ? dataUrl : b64;

        showError(error, "");
        showImage(bytes, mime, ext);
        setStats(statLine, [
          ["文件", file.name],
          ["原始", fmtBytes(bytes.length)],
          ["Base64", fmtBytes(dataUrl.length)],
          ["膨胀", `${Math.round((dataUrl.length / bytes.length) * 100 - 100)}%`],
        ]);
      } catch (e) {
        showError(error, e instanceof Error ? e.message : String(e));
      }
    };
    reader.onerror = () => {
      showError(error, "读取文件失败");
    };
    reader.readAsArrayBuffer(file);
  };

  const fromBase64 = (): void => {
    const raw = inputArea.value.trim();
    if (raw === "") {
      preview.replaceChildren();
      lastResult = null;
      showError(error, "");
      setStats(statLine, []);
      return;
    }

    // 去掉可能存在的 data URL 前缀；前缀里的 MIME 不可信，下面按魔数重判。
    const comma = raw.indexOf(",");
    const looksDataUrl = raw.startsWith("data:") && comma > 0;
    const b64 = (looksDataUrl ? raw.slice(comma + 1) : raw).replace(/\s+/gu, "");

    try {
      const bytes = base64ToBytes(b64);
      if (bytes.length === 0) {
        showError(error, "Base64 解码后为空");
        return;
      }
      const sniffed = sniffImage(bytes);
      if (!sniffed) {
        showError(error, "解码成功，但内容不是可识别的图片格式");
        preview.replaceChildren();
        lastResult = null;
        setStats(statLine, []);
        return;
      }
      const [mime, ext] = sniffed;
      showError(error, "");
      showImage(bytes, mime, ext);
      setStats(statLine, [
        ["Base64", fmtBytes(b64.length)],
        ["解码后", fmtBytes(bytes.length)],
        ["类型", mime],
      ]);
    } catch {
      showError(error, "不是合法的 Base64");
      preview.replaceChildren();
      lastResult = null;
      setStats(statLine, []);
    }
  };

  fileInput.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    if (file) fromFile(file);
  });
  includePrefix.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    if (file) fromFile(file);
  });
  inputArea.addEventListener("input", fromBase64);
  mode.addEventListener("change", () => {
    const toB64 = mode.value === "toBase64";
    fileInput.parentElement?.toggleAttribute("hidden", !toB64);
    output.parentElement?.toggleAttribute("hidden", !toB64);
    inputArea.parentElement?.toggleAttribute("hidden", toB64);
    cleanup();
    preview.replaceChildren();
    lastResult = null;
    showError(error, "");
    setStats(statLine, []);
  });

  const downloadBtn = button("下载图片", () => {
    if (!lastResult) {
      ctx.toast.show("还没有可下载的图片", "error");
      return;
    }
    const url = URL.createObjectURL(lastResult.blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `image.${lastResult.ext}`;
    a.click();
    window.setTimeout(() => {
      URL.revokeObjectURL(url);
    }, 10_000);
  });

  const element = h(
    "div",
    { class: "tool-body" },
    field("方向", mode),
    field("选择图片", fileInput),
    h("label", { class: "tool-check" }, includePrefix, h("span", {}, "输出包含 data:image/... 前缀")),
    field("Base64 输入", inputArea),
    actions(
      copyButton(() => output.value, ctx.toast),
      downloadBtn,
    ),
    error,
    field("Base64 输出", output),
    preview,
    statLine,
  );

  // 初始状态：图片 → Base64。
  inputArea.parentElement?.setAttribute("hidden", "");
  preview.replaceChildren(h("p", { class: "muted small" }, "选一张图片，或切到「Base64 → 图片」粘贴内容。"));

  return {
    element,
    unmount: cleanup,
  };
};

export default factory;
