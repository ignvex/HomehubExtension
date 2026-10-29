package httpapi

// 公开分享页与口令页的样式。
//
// 这两个页面刻意用内联样式：它们必须在不依赖前端 bundle 的情况下可用
// ——分享链接要能被 curl、爬虫、以及 JS 加载失败时的浏览器正常读取。
// 所以它们的 CSP 里带 style-src 'unsafe-inline'，而主站没有。
//
// 抽成常量而不是散在函数里，是为了让「这两个页面共享同一套视觉基线」
// 这件事在代码里可见：改配色只需要改一处。
const publicPageCSS = `
*{box-sizing:border-box}
body{margin:0;background:#fbfbfc;color:#1c1c1f;
     font:15px/1.6 ui-sans-serif,system-ui,"Noto Sans SC",sans-serif}
main{max-width:80ch;margin:0 auto;padding:2rem 1.25rem 4rem}
header{display:flex;flex-wrap:wrap;gap:.75rem;align-items:baseline;
       justify-content:space-between;margin-bottom:1.25rem}
h1{font-size:1.15rem;margin:0;font-weight:600}
.meta{color:#6b7280;font-size:.8125rem}
pre{margin:0;padding:1rem;background:#f4f4f6;border:1px solid #e4e4e8;
    border-radius:8px;overflow-x:auto}
code{font:13px/1.55 ui-monospace,"SF Mono",Consolas,monospace;white-space:pre}
footer{margin-top:1.5rem;color:#6b7280;font-size:.8125rem}
@media (prefers-color-scheme:dark){
  body{background:#131316;color:#e8e8ec}
  .meta,footer{color:#9ca3af}
  pre{background:#1b1b20;border-color:#2a2a30}
}
`

// centerPageCSS 用于口令页与错误页：内容居中，不需要正文排版。
const centerPageCSS = `
*{box-sizing:border-box}
body{margin:0;display:grid;place-items:center;min-height:100dvh;
     background:#fbfbfc;color:#1c1c1f;
     font:15px/1.6 system-ui,sans-serif}
main{text-align:center;padding:1.5rem}
h1{font-size:1.1rem;margin:0 0 .5rem}
p{color:#6b7280;margin:0}
form{display:grid;gap:.75rem;padding:1.5rem;min-width:18rem;
     border:1px solid #e4e4e8;border-radius:12px;background:#fff}
input{padding:.5rem .625rem;border:1px solid #d4d4d8;border-radius:6px;font:inherit}
button{padding:.5rem;border:0;border-radius:6px;background:#2563eb;
       color:#fff;font:inherit;cursor:pointer}
.err{color:#b91c1c;font-size:.8125rem}
@media (prefers-color-scheme:dark){
  body{background:#131316;color:#e8e8ec}
  p{color:#9ca3af}
  form{background:#1b1b20;border-color:#2a2a30}
  input{background:#131316;color:#e8e8ec;border-color:#3a3a42}
}
`

// htmlHead 生成两个页面共用的文档头。
func htmlHead(title, css string) string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + title + `</title><style>` + css + `</style></head><body>`
}
