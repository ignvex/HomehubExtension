// 清理上一次构建的产物，但保留 .gitkeep。
//
// 为什么需要这个：Vite 默认在构建前清空整个 outDir，会把版本库里的
// .gitkeep 一起删掉，导致 git 显示删除、且全新 clone 后 go build 失败。
// 所以关掉 emptyOutDir，改由这里精确清理。
import { rm } from "node:fs/promises";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const dist = join(resolve(dirname(fileURLToPath(import.meta.url)), ".."), "dist");

for (const name of ["_app", "assets", "index.html", "favicon.svg"]) {
  await rm(join(dist, name), { recursive: true, force: true });
}
