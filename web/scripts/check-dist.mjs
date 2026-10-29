import { gzipSync } from "node:zlib";
import { readFile, readdir, stat } from "node:fs/promises";
import { join, relative, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const dist = join(root, "dist");
const failures = [];

function fail(message) {
  failures.push(message);
}

async function walk(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await walk(full)));
    } else if (entry.isFile()) {
      files.push(full);
    }
  }
  return files;
}

const files = await walk(dist);
const sizes = [];
for (const file of files) {
  const bytes = await readFile(file);
  sizes.push({
    name: relative(dist, file).replaceAll("\\", "/"),
    raw: bytes.byteLength,
    gzip: gzipSync(bytes).byteLength,
  });
}

const htmlEntry = sizes.find((file) => file.name === "index.html");
if (!htmlEntry) {
  fail("dist/index.html 不存在");
}

const html = htmlEntry ? await readFile(join(dist, "index.html"), "utf8") : "";

// 1. 不允许任何内联脚本。外链 module script 的标签体内必须为空。
const scriptTags = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)];
for (const [tag, attrs = "", body = ""] of scriptTags) {
  const hasSrc = /\bsrc\s*=\s*["'][^"']+["']/i.test(attrs);
  if (!hasSrc) {
    fail(`index.html 含内联脚本：${tag.slice(0, 80)}`);
  }
  if (body.trim() !== "") {
    fail("index.html 的 <script> 标签体内有内容");
  }
  if (/\b(async|defer)\b/i.test(attrs) && !hasSrc) {
    fail("index.html 含无 src 的脚本标签");
  }
}

// 2. 不允许内联样式或内联事件处理器。
if (/<style\b/i.test(html)) fail("index.html 含 <style> 内联样式块");
if (/\sstyle\s*=/i.test(html)) fail("index.html 含 style 属性");
for (const match of html.matchAll(/\son[a-z]+\s*=/gi)) {
  fail(`index.html 含内联事件处理器：${match[0].trim()}`);
}
if (/javascript:/i.test(html)) fail("index.html 含 javascript: URL");

// 3. 主题占位符必须保留，后端负责替换成 auto/light/dark。
if (!html.includes('data-theme="__THEME__"')) {
  fail('index.html 缺少 data-theme="__THEME__" 占位');
}

// 4. 外链样式与脚本必须真实存在。
if (!/<link\b[^>]*rel=["']stylesheet["']/i.test(html)) {
  fail("index.html 没有外链样式表");
}
const refs = [...html.matchAll(/(?:src|href)=["']([^"']+)["']/gi)].map((m) => m[1] ?? "");
for (const ref of refs) {
  if (!ref.startsWith("/") || ref.startsWith("//")) continue;
  const clean = ref.split("?")[0]?.split("#")[0] ?? "";
  if (!clean || clean === "/") continue;
  try {
    const info = await stat(join(dist, clean.slice(1)));
    if (!info.isFile()) fail(`引用目标不是文件：${ref}`);
  } catch {
    fail(`index.html 引用的文件不存在：${ref}`);
  }
}

// 5. 产物里不得残留 sourcemap。
if (sizes.some((file) => file.name.endsWith(".map"))) {
  fail("dist 中不应有 sourcemap");
}

// 6. 体积闸门。数值来自 docs/frontend.md 的预算。
const totalRaw = sizes.reduce((sum, file) => sum + file.raw, 0);
const js = sizes.filter((file) => file.name.endsWith(".js"));
const css = sizes.filter((file) => file.name.endsWith(".css"));
const jsGzip = js.reduce((sum, file) => sum + file.gzip, 0);
const cssGzip = css.reduce((sum, file) => sum + file.gzip, 0);
if (totalRaw > 300 * 1024) fail(`dist 未压缩总量 ${totalRaw} 字节，超过 300 KB`);
if (jsGzip > 60 * 1024) fail(`JS gzip ${jsGzip} 字节，超过 60 KB`);
if (cssGzip > 15 * 1024) fail(`CSS gzip ${cssGzip} 字节，超过 15 KB`);

sizes.sort((a, b) => b.raw - a.raw);
for (const file of sizes) {
  console.log(
    `${file.name.padEnd(44)} ${String(file.raw).padStart(8)} B  ${String(file.gzip).padStart(7)} B gzip`,
  );
}
console.log(
  `\n合计：${sizes.length} 个文件，${totalRaw} B 未压缩 / ${sizes.reduce((s, f) => s + f.gzip, 0)} B gzip`,
);

if (failures.length > 0) {
  console.error("\n产物检查失败：");
  for (const message of failures) console.error(`- ${message}`);
  process.exit(1);
}
console.log("产物检查通过：无内联脚本/样式，外链资源存在，体积在预算内。");
