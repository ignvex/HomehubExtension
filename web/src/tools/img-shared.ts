// 图片处理的共享逻辑。
//
// 转换与压缩两个工具做的是同一件事的两面（改格式 / 改质量），
// Canvas 导出、尺寸计算、体积统计都一样，抽出来避免重复。
//
// 一个必须处理的边界：canvas.toBlob 对某些格式组合会返回 null
// （浏览器不支持该格式，或尺寸超出上限）。这不能当成「成功但为空」。

export type OutputFormat = "image/png" | "image/jpeg" | "image/webp" | "image/bmp" | "image/x-icon";

export interface FormatOption {
  mime: OutputFormat;
  label: string;
  ext: string;
  /** 是否支持质量参数（PNG 与 BMP 不支持）。 */
  lossy: boolean;
  /** 浏览器是否可能不支持导出该格式，需要探测。 */
  maybeUnsupported?: boolean;
}

export const FORMATS: readonly FormatOption[] = [
  { mime: "image/png", label: "PNG（无损）", ext: "png", lossy: false },
  { mime: "image/jpeg", label: "JPEG", ext: "jpg", lossy: true },
  { mime: "image/webp", label: "WebP", ext: "webp", lossy: true },
  { mime: "image/bmp", label: "BMP", ext: "bmp", lossy: false },
  { mime: "image/x-icon", label: "ICO", ext: "ico", lossy: false, maybeUnsupported: true },
];

export function formatByMime(mime: string): FormatOption | null {
  return FORMATS.find((f) => f.mime === mime) ?? null;
}

/** 读文件为 ImageBitmap。用 createImageBitmap 而不是 <img>：
 *  它不会把图片插入 DOM，也不会因为宽高为 0 而静默失败。 */
export async function loadImage(file: File): Promise<ImageBitmap> {
  return createImageBitmap(file);
}

export interface ResizeOptions {
  /** 目标宽度；0 表示按高度等比缩放。 */
  width: number;
  /** 目标高度；0 表示按宽度等比缩放。 */
  height: number;
  /** 是否保持宽高比。 */
  keepAspect: boolean;
}

/** 计算最终绘制尺寸。 */
export function computeSize(
  natural: { width: number; height: number },
  options: ResizeOptions,
): { width: number; height: number } {
  const { width, height, keepAspect } = options;

  if (width <= 0 && height <= 0) return natural;
  if (!keepAspect) {
    return {
      width: width > 0 ? width : natural.width,
      height: height > 0 ? height : natural.height,
    };
  }
  if (width > 0 && height > 0) {
    // 两边都给时按比例缩到框内，不拉伸变形。
    const scale = Math.min(width / natural.width, height / natural.height);
    return {
      width: Math.max(1, Math.round(natural.width * scale)),
      height: Math.max(1, Math.round(natural.height * scale)),
    };
  }
  if (width > 0) {
    const scale = width / natural.width;
    return { width, height: Math.max(1, Math.round(natural.height * scale)) };
  }
  const scale = height / natural.height;
  return { width: Math.max(1, Math.round(natural.width * scale)), height };
}

/**
 * 把图片绘制到 canvas 并导出为指定格式。
 *
 * 背景色的处理：JPEG 不支持透明，透明区域会被填成黑色。
 * 用户期待的通常是白色，所以导出 JPEG 时先铺一层白底。
 */
export async function encode(
  bitmap: ImageBitmap,
  size: { width: number; height: number },
  format: FormatOption,
  quality: number,
): Promise<Blob> {
  const canvas = document.createElement("canvas");
  canvas.width = size.width;
  canvas.height = size.height;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("浏览器不支持 Canvas 2D，无法处理图片");

  if (format.mime === "image/jpeg") {
    ctx.fillStyle = "#ffffff";
    ctx.fillRect(0, 0, size.width, size.height);
  }

  // 缩放时用高质量插值，否则缩小后的图会明显锯齿。
  ctx.imageSmoothingEnabled = true;
  ctx.imageSmoothingQuality = "high";
  ctx.drawImage(bitmap, 0, 0, size.width, size.height);

  const blob = await new Promise<Blob | null>((resolve) => {
    canvas.toBlob(
      (b) => {
        resolve(b);
      },
      format.mime,
      format.lossy ? quality : undefined,
    );
  });

  if (!blob) {
    throw new Error(`浏览器不支持导出 ${format.label}`);
  }
  // 有些浏览器对不支持的格式会回退成 PNG 而不是返回 null，
  // 那样扩展名与内容就不一致了。
  if (!format.maybeUnsupported && blob.type !== format.mime && format.mime !== "image/bmp") {
    throw new Error(`导出格式回退成了 ${blob.type}，当前浏览器不支持 ${format.label}`);
  }
  return blob;
}

/** 探测浏览器是否支持导出某格式。 */
export async function supportsFormat(format: FormatOption): Promise<boolean> {
  if (!format.maybeUnsupported) return true;
  const canvas = document.createElement("canvas");
  canvas.width = 1;
  canvas.height = 1;
  const blob = await new Promise<Blob | null>((resolve) => {
    canvas.toBlob((b) => {
      resolve(b);
    }, format.mime);
  });
  return blob !== null && blob.type === format.mime;
}

/** 触发下载。对象 URL 必须在点击后释放，否则整页生命周期内一直占内存。 */
export function download(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // 延迟释放：立刻 revoke 在某些浏览器上会让下载拿不到数据。
  window.setTimeout(() => {
    URL.revokeObjectURL(url);
  }, 10_000);
}

/** 把文件名换成新扩展名。 */
export function replaceExt(name: string, ext: string): string {
  const dot = name.lastIndexOf(".");
  const base = dot > 0 ? name.slice(0, dot) : name;
  return `${base}.${ext}`;
}
