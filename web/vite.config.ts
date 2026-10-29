import { defineConfig } from "vite";

// 构建产物必须能被 CSP `script-src 'self'; style-src 'self'` 放行：
// - modulePreload polyfill 会以 <script>…</script> 内联注入，必须关闭；
// - 样式保持独立文件，不内联进 HTML。
export default defineConfig({
  base: "/",
  build: {
    outDir: "dist",
    // 由 scripts/clean-dist.mjs 精确清理，保留版本库里的 .gitkeep。
    // 开着 emptyOutDir 会把 .gitkeep 一起删掉，导致全新 clone 无法 go build。
    emptyOutDir: false,
    assetsDir: "_app",
    target: "es2022",
    cssCodeSplit: true,
    sourcemap: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/media": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/p": { target: "http://127.0.0.1:8080", changeOrigin: false },
    },
  },
});
