# 前端设计

## 1. 定位与取舍

前端是一个**阅读器 + 管理台**，不是营销页。两条硬约束决定了技术选择：

1. 页面里的正文来自不可信来源，任何时刻都不能发起第三方请求——所以不引 CDN、不引网络字体、不引外部图标库。
2. 部署在 1C2G 的机器上，前端产物体积直接对应首屏时间——所以不引 UI 框架，产物目标 < 150 KB gzip。

结论：pnpm + Vite + TypeScript（`strict`）+ 原生 DOM + 手写 CSS3。

放弃框架的代价是视图挂载与状态订阅要自己写，大约 150 行基础设施。收益是构建产物小一个数量级、没有 hydration 概念、CSS 完全可控。**触发引入框架的阈值**：视图超过 8 个且出现三处以上复杂表单联动，或需要虚拟滚动承载上千条目。到那时再评估，而不是现在预支复杂度。

## 2. 工程结构

```text
web/
├── index.html
├── package.json
├── pnpm-lock.yaml
├── tsconfig.json
├── vite.config.ts
├── public/
│   └── favicon.svg
└── src/
    ├── main.ts               # 挂载点，注册路由
    ├── app.ts                # 应用外壳与路由表构造
    ├── router.ts             # History API 路由 + 鉴权守卫
    ├── api/
    │   ├── client.ts         # 请求封装、401 刷新、错误映射
    │   ├── http.ts           # 底层 fetch 与超时
    │   ├── errors.ts         # 错误解析
    │   ├── types.ts          # 与 Go DTO 对齐的类型
    │   ├── auth.ts
    │   ├── pastes.ts
    │   ├── feeds.ts
    │   ├── entries.ts
    │   └── system.ts
    ├── auth/
    │   ├── keys.ts           # WebCrypto 生成/签名
    │   ├── storage.ts        # IndexedDB 存不可导出 CryptoKey
    │   ├── session.ts        # 登录状态、内存中的 access token
    │   └── owner-token.ts    # 匿名片段的 owner token（localStorage）
    ├── state/
    │   ├── store.ts          # 极简订阅式 store
    │   └── app-state.ts      # 全局切片：me、feeds、unread
    ├── lib/
    │   ├── dom.ts            # h() 创建元素、clamp、debounce
    │   ├── sse.ts
    │   ├── device.ts
    │   ├── theme.ts
    │   ├── format.ts         # 时间、字节、相对时间
    │   └── strings.ts        # 全部面向用户的文案
    ├── components/
    │   ├── nav.ts
    │   ├── page.ts           # 页头
    │   ├── toast.ts
    │   ├── states.ts         # 骨架屏、空状态、错误态
    │   └── icons.ts          # 内联 SVG sprite
    ├── tools/                # 15 个开发工具，见 3.0.1
    ├── views/
    │   ├── home.ts
    │   ├── pair.ts
    │   ├── pastes.ts
    │   ├── paste-new.ts
    │   ├── paste-detail.ts
    │   ├── feeds.ts
    │   ├── entries.ts
    │   ├── reader.ts
    │   ├── settings.ts       # 含清理任务一节
    │   ├── tools.ts
    │   ├── tool-runner.ts
    │   └── not-found.ts
    └── styles/
        ├── main.css          # 汇总 @layer
        ├── reset.css
        ├── tokens.css
        ├── base.css
        ├── layout.css
        ├── components.css
        ├── utilities.css
        └── reader.css
```

`package.json` 只有三个运行时依赖为零的开发依赖：`vite`、`typescript`、`@types/node`。不引入任何 UI、状态、请求、日期库。

## 3. 路由与视图

### 3.0 工具台首页

首页是应用网格，不是条目列表。卡片由 `GET /api/v1/apps` 驱动：

```json
{
  "apps": [
    {"id":"pastebin","name":"Pastebin","description":"…","icon":"paste",
     "path":"/pastes","enabled":true,"badge":{"count":12,"label":"条"}},
    {"id":"rss","name":"RSS 阅读","description":"…","icon":"rss",
     "path":"/entries","enabled":true,"badge":{"count":3,"label":"未读"}}
  ]
}
```

前端**不硬编码应用列表**：卡片全部来自 `GET /api/v1/apps`，`home.ts` 只负责渲染。新增一个模块是两处改动——后端 `handleApps` 追加一项，前端 `app.ts` 的路由表加一条；只改后端的话卡片会指向 404。把列表放在服务端的好处是卡片能跟着配置走，前端不必重新发布。

这里有两种不同行为，别混：

- `pastebin.enabled` / `rss.enabled` 为 `false` 时，`handleApps` **直接不 append** 那张卡——它不会出现在数组里。
- `feeds` 卡片始终下发，但 `enabled` 跟着 `rss.enabled` 走。前端遇到 `enabled: false` **不隐藏**，而是渲染成不可点击的 `app-card--disabled` 加一枚「未启用」角标——用户能看到功能存在、只是被运维关掉了。

角标数据（未读数、片段数）也由后端算好，前端不做聚合；`badge` 为 `null` 或 `count` 为 `0` 时都不渲染（避免出现「0 条」这种噪音）。

### 3.0.1 开发工具台

`/tools` 是 15 个纯前端工具的入口，`/tools/:id` 是单工具容器。数据结构在 `src/tools/`：

- `types.ts` 定义 `ToolMeta`（静态元数据）与 `ToolFactory`（实现）。
- `registry.ts` 是唯一的注册点，只 import 元数据、不 import 实现——每个工具的 `load` 是 `() => import("./tools-xxx")`，Vite 据此拆成独立 chunk。首屏 JS（入口 + `shared`，gzip 后 27.5 KB）包含路由、外壳与 registry，工具实现一个也不进主 bundle：15 个 `tools-*` chunk 加一个图片工具共用的 `img-shared`，合计 27.3 KB gzip，只在打开对应工具时下载。
- 列表页 `views/tools.ts` 只读元数据：按 `CATEGORIES` 分组、带搜索框，打开这一页的代价是零。搜索时平铺不分组。
- 容器 `views/tool-runner.ts` 负责三个边界：未知 id 给建议列表而不是白屏；chunk 加载失败（部署后旧 chunk 404）提示刷新；卸载时调 `unmount`。

工具拿到的 `ToolContext` 只有两样东西：

```ts
export interface ToolContext {
  signal: AbortSignal;   // 路由切换时 abort，长任务应据此放弃
  toast: ToastApi;
}
```

约定是 **`unmount` 必须清掉所有定时器、AbortController、对象 URL 与事件监听**——工具页面的生命周期比普通视图长得多，漏掉一个定时器就是一个永久泄漏。所有计算在本地完成：不上传、不联网、不追踪。

### 3.1 路由表

| 路径 | 视图 | 鉴权 |
| --- | --- | --- |
| `/pair` | 设备配对 | 否 |
| `/` | 工具台首页：应用卡片网格 | **否**（入口页） |
| `/tools` | 开发工具列表（分类 + 搜索） | **否** |
| `/tools/:id` | 单工具（按需加载 chunk） | **否** |
| `/entries` | 全部条目（可按订阅源筛选） | 是 |
| `/entries/:id` | 阅读视图 | 是 |
| `/feeds` | 订阅源管理（新建/编辑/删除） | 是 |
| `/pastes` | 片段列表 | **否**（凭 owner token 看自己的） |
| `/pastes/new` | 新建片段 | **否** |
| `/pastes/:slug` | 片段详情（匿名凭 owner token 管理自己的） | **否** |
| `/settings` | 设置与设备管理；清理任务记录是页内一节，不是独立路由 | 是 |
| `*` | 404 | — |

`/p/{slug}` **不经过前端路由**，由 Go 服务端渲染。原因：分享链接必须在不加载 JS bundle 的情况下可用，且要能被爬虫与 `curl` 正常读取。

### 3.2 路由实现

```ts
interface RouteDefinition {
  path: string;                 // "/entries/:id"
  title: string;
  auth: boolean;
  view: ViewFactory;
}

interface ViewContext {
  router: Router;
  params: Record<string, string>;
  query: URLSearchParams;
  signal: AbortSignal;          // 路由切换时 abort，用于取消进行中的请求
  toast: ToastApi;
  events: (listener: (event: ServerEvent) => void) => () => void;
}

interface ViewHandle {
  element: HTMLElement;
  unmount?: () => void;
}
```

- 用 `history.pushState` + `popstate`，不做 hash 路由。
- 匹配器是手写的逐段字符串比较，**不生成正则**：先按 `/` 分段（`splitSegments`），段数不等直接跳过，`:` 开头的段取参数并 `decodeURIComponent`，其余逐字比较，按注册顺序取第一个命中。
- 切换时先调用上一个视图的 `unmount()`，再 abort `signal`，再清空容器，再 `mount()`。
- 所有内部链接拦截 `click` 做客户端跳转；带 `target="_blank"`、外域、`download` 属性、修饰键点击一律放行给浏览器。
- 路由层负责鉴权：`auth: true` 且未登录 → `replaceState` 到 `/pair`，并记住 `returnTo`。
- 滚动位置按 `history.state` 恢复，返回列表时不回到顶部。

### 3.3 渲染方式

视图直接用 DOM API 构建，不拼 `innerHTML` 字符串（除正文）。`lib/dom.ts` 提供：

```ts
h("div", { class: "card", onclick: handler }, child1, child2)
```

属性名支持 `class`、`dataset`、`on*`、`aria-*`；文本子节点自动转义（用 `textContent`）。唯一使用 `innerHTML` 的地方是渲染已清洗的 `content_html`——这是服务端的职责，前端不复核。

列表超过 200 项时用简单的分页（游标），不做无限滚动。虚拟滚动留到条目数确实过千再考虑。

## 4. 状态与数据

### 4.1 Store

```ts
function createStore<T>(initial: T) {
  let value = initial;
  const listeners = new Set<(v: T) => void>();
  return {
    get: () => value,
    set(next: T | ((prev: T) => T)) {
      value = typeof next === "function" ? (next as any)(value) : next;
      listeners.forEach((fn) => fn(value));
    },
    subscribe(fn: (v: T) => void) {
      listeners.add(fn);
      fn(value);
      return () => listeners.delete(fn);
    },
  };
}
```

全局只放三样东西：`me`（当前设备）、`unreadCount`、`toast`。其余状态属于视图，随 `unmount` 一起丢弃。视图订阅时把返回的取消函数收集到本地数组，`unmount` 时统一执行——这是避免内存泄漏的关键约定。

### 4.2 请求层

```ts
async function request<T>(path: string, init?: RequestInit): Promise<T>
```

行为：

- 自动加 `X-CSRF-Token`（写请求）与 `X-Request-ID`（前端生成，便于与服务端日志对账）。
- `401` → 尝试一次 `POST /auth/refresh` → 成功则重放原请求 → 再次 401 则清空会话并跳 `/pair`。
- 并发 401 只触发一次刷新：用一个共享的 `Promise`，其余请求 `await` 同一个。
- 响应非 2xx 时解析统一错误结构，抛 `ApiError { code, message, details, requestId }`，由视图决定展示方式。
- `AbortSignal` 透传。
- 不缓存 GET 响应（数据变化频繁，缓存会带来不一致），列表刷新靠 SSE + 局部更新。

### 4.2.1 时间字段

服务端所有绝对时刻都是 Unix 秒（数字），未设置为 `null`。前端只在 `lib/format.ts` 的 `parseTime` 里做一次 `sec * 1000`，其余地方一律用 `TimeValue = number | null | undefined`。

不要写 `value < 1e12 ? value * 1000 : value` 这类启发式。项目早期时间字段混用（部分 RFC3339 字符串、部分裸数字），前端靠这个魔数去猜单位——遇到合法但偏小的值会静默算错，而且没人能说清 `1e12` 是怎么定的。协议统一后这个分支已经删掉，改成 `TimeValue` 只接受数字，让类型系统来兜底。

乐观更新时用 `nowSeconds()`，不要用 `new Date().toISOString()`——后者产出的字符串和 `TimeValue` 不兼容，编译器会当场拦住。

`build_time` 是例外：它是构建元数据，保留字符串，用 `formatInstantString` 格式化。

### 4.3 类型对齐

`api/types.ts` 手写与 Go DTO 一致的类型。约束：

- Go 侧用 struct tag 固定 JSON 字段名（`json:"published_at"`），不依赖字段名推导。
- 每个 handler 有一组 golden test，比对序列化结果与 `web/src/api/types.ts` 中的样例注释。
- 类型不做「可选字段遍地」的宽松定义；确实可能为空的字段用 `| null` 显式表达，避免 `undefined` 与 `null` 混用。

## 5. 鉴权前端

### 5.1 密钥生成

```ts
const keyPair = await crypto.subtle.generateKey(
  { name: "Ed25519" },
  false,                    // extractable = false，私钥字节永远不可导出
  ["sign", "verify"]
);
```

- `extractable=false` 是硬要求。私钥以 `CryptoKey` 对象形式存在，JavaScript 读不到字节，XSS 也无法把它带走。
- 存储用 IndexedDB 直接存 `CryptoKey`（structured clone 支持），键名 `hh.device.key`。
- `client_id` 存 `localStorage`，它不是秘密，只是索引。
- 不支持 Ed25519 的环境（旧 Safari/WebView）降级为 ECDSA P-256：

  ```ts
  { name: "ECDSA", namedCurve: "P-256" }   // 签名时 hash: "SHA-256"
  ```

  服务端按 `algorithm` 字段验签。降级是兼容手段，不是首选。

### 5.2 配对流程

```text
/pair
  → 用户输入 HH-XXXX-XXXX-XXXX
  → 生成密钥对（若已存在则提示"本设备已配对"）
  → POST /auth/enroll { token, name, algorithm, public_key }
  → 保存 client_id
  → 立即走一次 challenge → token
  → 跳转 returnTo 或 /
```

配对码输入框做自动分组与大小写归一，粘贴 `HH-7QK4-2M9X-P3TB` 或 `7qk42m9xp3tb` 都能成功。

### 5.3 会话

- `access_token` 只存内存，不写 `localStorage`——XSS 拿不到长期凭证。
- `refresh_token` 由服务端写在 `HttpOnly` cookie 里，**响应体里也不返回**，所以 JS 既拿不到也不需要。这是前后端一致的前提：如果哪一端想读它，就是设计跑偏了。
- `csrf_token` 在两处：响应体（存内存）与可读 cookie，写请求时读 cookie 并放到 `X-CSRF-Token`。
- 页面刷新后：内存中的 token 丢失 → 先 `POST /auth/refresh`（refresh cookie 自动携带）拿新的 access token 与 `client` 对象，不再额外调 `/auth/me`。没有有效 cookie 则跳 `/pair`。这一步应当无感。
- refresh 成功后服务端会同时旋转 `hh_session` 与 `hh_csrf` cookie，前端每次都要重新读取 `hh_csrf`，不能缓存旧值。
- `csrf_token` 存在内存 + 可读 cookie 里，写请求时读取并放到 `X-CSRF-Token`。

### 5.4 签名消息

必须与服务端逐字节一致：

```ts
const message = `homehubext/auth/v1\n${clientId}\n${nonce}`;
const sig = await crypto.subtle.sign({ name: "Ed25519" }, privateKey, new TextEncoder().encode(message));
```

不做 JSON 序列化当作签名载荷——JSON 的键序、空格、Unicode 转义在不同实现里不一致，是签名校验失败最常见的原因。

## 6. CSS 设计

### 6.1 分层

```css
/* main.css */
@layer reset, tokens, base, layout, components, utilities;
```

| 层 | 内容 |
| --- | --- |
| `reset` | 盒模型、`margin: 0`、`img { display: block }`、`button` 继承字体 |
| `tokens` | 颜色、字号、间距、圆角、阴影、层级、动效时长 |
| `base` | `html`/`body` 排版、链接、`::selection`、`focus-visible` |
| `layout` | 应用外壳 Grid、侧栏、顶栏、内容区、响应式断点 |
| `components` | 导航、卡片、表单、按钮、标签、对话框、Toast、骨架屏 |
| `utilities` | 极少量：`.visually-hidden`、`.truncate`、`.stack` |

分层的实际收益：组件样式永远覆盖不了布局，改布局不会意外改到按钮。

### 6.2 Token

```css
:root {
  color-scheme: light dark;

  --bg: oklch(98.5% 0.003 250);
  --surface: oklch(100% 0 0);
  --border: oklch(90% 0.004 250);
  --text: oklch(22% 0.01 250);
  --text-muted: oklch(52% 0.012 250);
  --accent: oklch(58% 0.16 255);
  --danger: oklch(55% 0.19 25);

  --space-1: 0.25rem;
  --space-2: 0.5rem;
  --space-3: 0.75rem;
  --space-4: 1rem;
  --space-6: 1.5rem;
  --space-8: 2rem;

  --text-xs: 0.75rem;
  --text-sm: 0.875rem;
  --text-base: 1rem;
  --text-lg: 1.125rem;
  --text-xl: 1.375rem;

  --radius-sm: 4px;
  --radius-md: 8px;
  --radius-lg: 12px;

  --measure: 68ch;
  --sidebar-w: 15rem;
  --header-h: 3.25rem;

  --ease: cubic-bezier(0.2, 0, 0, 1);
  --dur: 160ms;
}

[data-theme="dark"] { /* 覆盖颜色 token */ }

@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) { /* 同上 */ }
}
```

- 颜色用 OKLCH：明度与色度可独立调节，暗色主题不用凭感觉微调。2026 年的浏览器支持已无顾虑。
- **主题初始化零内联脚本**：偏好存在 `hh_theme` cookie（`Path=/; Max-Age=31536000; SameSite=Lax`，不设 `HttpOnly`——前端要能写）。Go 返回 `index.html` 时读取该 cookie，直接把 `data-theme` 写进 `<html>` 标签，因此首帧就是正确的主题，不会闪白。

  实现方式：`index.html` 里写 `<html lang="zh-CN" data-theme="__THEME__">`，Go 用一次 `strings.Replace` 替换占位符；命中不到占位符时记 warn 并原样返回，避免构建产物变更导致静默失效。`index.html` 本来就带 `Cache-Control: no-cache`，替换不影响缓存语义。

  好处：CSP 保持 `script-src 'self'`，不需要 `'unsafe-inline'`，也不需要维护构建时的 sha256 哈希——哈希方案在改一行内联代码后就失效，是一个很容易忘记维护的隐式依赖。
- 主题三态：跟随系统 / 亮 / 暗。`data-theme` 缺失或为 `auto` 时走 `@media (prefers-color-scheme: dark)`；显式 `light`/`dark` 时用属性选择器覆盖。前端切换时同时写 cookie 与 `documentElement.dataset.theme`。
- 间距基数 4px，只用 token，不写裸数值。

### 6.3 布局

```css
.app {
  display: grid;
  grid-template-columns: var(--sidebar-w) minmax(0, 1fr);
  grid-template-rows: var(--header-h) minmax(0, 1fr);
  grid-template-areas: "sidebar header" "sidebar main";
  min-height: 100dvh;
}
```

- 用 `dvh` 而不是 `vh`，避免移动端地址栏导致的跳动。
- `minmax(0, 1fr)` 防止长 URL、宽表格撑破容器。
- 宽度 < 900px：侧栏变成抽屉，顶栏出现汉堡按钮，遮罩层 + `inert` 处理焦点陷阱。
- 宽度 < 600px：单列，操作按钮收进溢出菜单。
- 用 `@container` 做卡片内部的自适应（标题换行、元信息折行），不依赖全局断点。
- 移动端底部安全区：`padding-bottom: env(safe-area-inset-bottom)`。

### 6.4 可访问性

- 语义标签：`<nav>`、`<main>`、`<article>`、`<button>` 而不是 `<div onclick>`。
- 每个页面一个 `<h1>`，标题层级不跳级。
- 跳转链接 `<a class="skip-link" href="#main">跳到内容</a>`，聚焦时可见。
- 焦点样式用 `:focus-visible` + `outline: 2px solid var(--accent); outline-offset: 2px`，从不 `outline: none`。
- Toast 用 `role="status" aria-live="polite"`；错误用 `role="alert"`。
- 对话框用原生 `<dialog>` + `showModal()`，焦点陷阱与 Esc 由浏览器负责。
- `prefers-reduced-motion: reduce` 时把所有 `transition`/`animation` 降到 `0.01ms`。
- 对比度按 WCAG AA（正文 4.5:1，大字 3:1），用 OKLCH 常量保证。
- 键盘：`j`/`k` 上下条，`Enter` 打开，`u` 切换已读，`s` 标星，`?` 显示快捷键。输入框聚焦时全部禁用。

### 6.5 无网络字体

字体栈固定为系统字体：

```css
--font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans SC",
             "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
--font-mono: ui-monospace, "SF Mono", "Cascadia Code", "JetBrains Mono",
             "Noto Sans Mono", Consolas, monospace;
```

不加载网络字体：既避免第三方请求，也省掉首屏的字体阻塞。中文字体文件动辄几 MB，本地加载不现实。

## 7. 阅读视图

阅读体验是产品核心，单独设计。

```css
.reader {
  max-width: var(--measure);
  margin-inline: auto;
  padding-inline: var(--space-4);
  font-size: var(--reader-font-size, 1.0625rem);
  line-height: 1.75;
  text-wrap: pretty;
  hanging-punctuation: allow-end;
}

.reader img,
.reader video {
  max-width: 100%;
  height: auto;
  border-radius: var(--radius-md);
}

.reader pre {
  overflow-x: auto;
  padding: var(--space-4);
  border-radius: var(--radius-md);
  background: var(--surface-alt);
  font-family: var(--font-mono);
  font-size: 0.875em;
  line-height: 1.6;
}

.reader table {
  display: block;
  max-width: 100%;
  overflow-x: auto;
  border-collapse: collapse;
}

.reader :where(h2, h3) {
  margin-block: 2em 0.75em;
  line-height: 1.3;
}

.reader blockquote {
  margin-inline: 0;
  padding-inline-start: var(--space-4);
  border-inline-start: 3px solid var(--border-strong);
  color: var(--text-muted);
}
```

细节：

- 行宽 `68ch` 是长文阅读的常见最优值；字号三档（0.95 / 1.06 / 1.2 rem），存 `localStorage`。
- 图片加载前用 `width`/`height` 属性占位：浏览器会用这两个属性自动推导 `aspect-ratio`，因此只需 `img { height: auto; max-width: 100% }` 就能避免布局抖动，不需要 `attr()` 表达式（`attr()` 返回数值类型是新特性，兼容面还不够）。
- 表格用 `display: block` + `overflow-x: auto`，比包一层 `<div>` 更简单，且不破坏语义（`<table>` 的 role 仍保留）。
- 代码块不做语法高亮。原因：`highlight.js` 完整包 1 MB、`shiki` 需要加载语法文件，两者都会让首屏变重，而 1C2G 的服务端也不适合做高亮。保留 `<pre><code class="language-x">`，未来按需懒加载一个轻量高亮库。
- 正文里的 `id` 锚点全部保留，脚注跳转可用；`scroll-margin-top: var(--header-h)` 防止标题被固定顶栏遮住。
- 图片 `loading="lazy"` 与 `decoding="async"` 已在服务端写入，前端不再重复处理。
- 阅读进度：用 `IntersectionObserver` 观察最后一段，记录 `entry.read_at`；不做滚动百分比条（收益低、开销高）。
- 打印：`@media print` 隐藏导航与操作栏，`a[href]::after { content: " (" attr(href) ")" }`。

## 8. 实时更新

后端通过 `/api/v1/events/stream` 推送 `entry.created`、`feed.updated`、`job.finished`、`system.alert`、`system.resync`。视图通过 `ctx.events(listener)` 订阅，返回的取消函数必须并入 `unmount`——忘了这一步，切页后旧视图仍会被调用并持有已脱离文档的 DOM。

```ts
const es = new EventSource("/api/v1/events/stream", { withCredentials: true });

es.addEventListener("entry.created", (e) => {
  const data = JSON.parse((e as MessageEvent).data);
  appState.unreadCount.set((n) => n + 1);
  // 若当前在条目列表，插入到顶部
});
```

- **SSE 用 `hh_session` cookie 认证，不用 Bearer**。原因：`EventSource` 无法设置自定义请求头，而 `access_token` 只存内存、没有 cookie 形式。服务端对 `/api/v1/events/stream` 接受 refresh cookie 直接验证（与 `/auth/refresh` 同一凭证）。只读端点、同源、HTTPS，风险可控。这是整个 API 里唯一用 cookie 认证的读接口，需要显式写进中间件豁免表；同时它也必须豁免 CSRF 检查（GET 不产生副作用）。
- 单例连接，由 `lib/sse.ts` 统一管理，所有视图通过订阅接收事件。
- 浏览器自动重连（默认 3 秒），配合服务端的 `Last-Event-ID` 补发。
- 页面隐藏超过 5 分钟后主动 `close()`，`visibilitychange` 恢复时重建连接——后台标签页不该长期占着一个连接。
- 收到 `system.resync` 时清空本地列表并重新拉取。
- 连接失败不弹错误提示，只在顶栏显示一个不打扰的小圆点，恢复后自动消失。

## 9. 构建与嵌入

### 9.1 Vite 配置

```ts
export default defineConfig({
  base: "/",
  build: {
    outDir: "dist",
    assetsDir: "_app",
    target: "es2022",
    cssCodeSplit: true,
    sourcemap: false,
    // 关闭 modulepreload polyfill：它默认以内联 script 注入，
    // 会撞上 CSP 的 script-src 'self' 而被拦截（控制台报 CSP 违规，
    // 而页面看起来正常，属于很难发现的隐性故障）。
    // target 是 es2022，浏览器原生支持 modulepreload，不需要 polyfill。
    modulePreload: { polyfill: false },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/media": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/p": { target: "http://127.0.0.1:8080", changeOrigin: false },
    },
  },
});
```

- 产物文件名带内容哈希（Vite 默认），配合 `immutable` 长缓存。
- `sourcemap: false`：生产不带源码。排查问题靠 `X-Request-ID` 与服务端日志。
- **构建产物不允许出现内联脚本**。除了 `modulePreload.polyfill: false`，还要确认 Vite 没有开启内联 CSS（`build.cssCodeSplit` 保持 `true`，样式走外链文件）；CSP 里 `style-src 'self'` 同样没有留 `'unsafe-inline'` 的余地。CI 里加一条断言：`grep -c '<script>' web/dist/index.html` 必须为 0。
- 目标 `es2022`：2026 年的浏览器都支持，不需要为 IE 留任何降级。

### 9.2 Go 侧嵌入

```go
//go:embed all:web/dist
var webFS embed.FS
```

- `web/dist/` 目录里只提交一个 `.gitkeep`，保证全新 clone 直接 `go build` 成功（`go:embed all:web/dist` 要求目录存在且非空）。
- `.gitignore` 用 `web/dist/*` + `!web/dist/.gitkeep`：目录里只提交 `.gitkeep`，构建产出的 `index.html`、`_app/` 一律不入库。
- 构建顺序必须是「先 `pnpm build`，再 `go build`」。CI 与 Dockerfile 都要显式保证；`make build` 把这个顺序固化。
- 静态资源服务：`_app/` 用 `http.FileServerFS` + 长缓存；其余路径回退 `index.html` 并 `no-cache`。
- `/api/`、`/media/`、`/p/`、`/healthz`、`/readyz` 不参与 fallback，否则会把 404 变成 HTML。

### 9.3 体积预算

闸门写在 `web/scripts/check-dist.mjs` 里，`pnpm check:dist` 超限即失败。分两档统计，因为懒加载让「总量大」和「首屏慢」脱钩了：

| 项 | 预算 | 当前 |
| --- | --- | --- |
| 首屏 JS（gzip，入口 + shared） | ≤ 40 KB | 27.5 KB（2 个 chunk） |
| 按需 JS（gzip） | ≤ 80 KB | 27.3 KB（16 个 chunk） |
| CSS（gzip） | ≤ 20 KB | 8.4 KB |
| dist 未压缩总量 | ≤ 400 KB | 180.7 KB |
| 按需 chunk 个数 | ≤ 40 | 16 |

只卡总量会逼着人删功能，只卡首屏则总量可能失控，所以两档都设上限。另有无网络字体（`index.html` 不引任何 `@font-face` 远程资源）、无外链脚本/样式（`check-dist.mjs` 逐条校验 `index.html` 的引用目标真实存在）。

## 10. 开发约定

- `tsconfig.json`：`strict`、`noUncheckedIndexedAccess`、`exactOptionalPropertyTypes`、`noImplicitOverride`。
- 只用 `pnpm`，仓库提交 `pnpm-lock.yaml`。`package.json` 里 `packageManager` 字段锁定版本，`corepack` 自动切换。
- 脚本：`dev`、`build`、`preview`、`check`（`tsc --noEmit`）、`check:dist`（产物规则）、`test:qr`、`test:crypto`、`verify`（依次跑 check → test:qr → build → check:dist → test:crypto）。
- 不引 ESLint，也不引 Prettier：`tsc --strict` 已经覆盖大部分问题，格式靠下面几条人工约定。没有格式化脚本，也没有 `.prettierrc`/`.editorconfig`。
- 代码风格：两空格缩进、双引号、语句末尾分号、`const` 优先。没有工具强制，靠 review 守。
- 组件不写 `style` 属性：HTML 里的 `style="..."` 会被 CSP 拦截。运行时通过 CSSOM 设置（`el.style.setProperty("--progress", "42%")`）不受 CSP 限制，但仍要求动态值只走自定义属性，不直接写颜色或尺寸——样式规则的归属保持在 CSS 文件里。
- 中文字符串直接写在代码里，首版不引 i18n。所有面向用户的文案集中在 `lib/strings.ts`，为将来抽取做准备。

## 11. 待定

- 语法高亮的按需加载（`highlight.js` 的 core + 常用语言约 30 KB）。
- 条目列表是否需要虚拟滚动（取决于实际订阅量）。
- 移动端是否提供 PWA（离线读已抓取文章）。这是很自然的延伸：所有资源已本地化，加一个 Service Worker 就能离线。但要处理缓存版本与后台更新，首版不做。
