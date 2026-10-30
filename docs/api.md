# HTTP API 与鉴权

## 1. 通用约定

| 项 | 约定 |
| --- | --- |
| 基址 | `https://<public_url>/api/v1` |
| 编码 | 请求与响应均为 JSON，`Content-Type: application/json; charset=utf-8` |
| 时刻 | Unix 秒，JSON 整数，如 `1793286035`；未设置为 `null` |
| 时长 | 字符串，如 `"30m"`、`"168h"`（人可读，需要解析） |
| 标识 | 字符串；Paste 用 `slug`，其他资源用 ULID |
| 分页 | 游标式，`?limit=&cursor=`，响应带 `next_cursor`；没有下一页时是 `null` |
| 认证 | `Authorization: Bearer <token>`；SSE 用会话 cookie；paste 可完全匿名 |
| 追踪 | 每个响应带 `X-Request-ID`，与服务端日志一致 |
| 压缩 | Caddy 负责；API 不自行压缩 |

### 时间字段

所有**绝对时刻**统一为 Unix 秒整数：`created_at`、`updated_at`、`expires_at`、`published_at`、`fetched_at`、`read_at`、`starred_at`、`last_fetch_at`、`next_fetch_at`、`started_at`、`finished_at`。

不用 RFC3339 字符串的理由：

- 与存储一致。SQLite 里存的就是 `INTEGER` 秒，输出字符串等于每个请求都要格式化一次、客户端再解析回来。
- 只有一种形态。字符串会出现 `Z` 与 `+08:00`、有无小数秒的差异；一旦某处序列化方式漂移，客户端就得写兼容分支。
- 排序、比较、过期判断直接比大小。

**未设置一律是 `null`，不是零值。** 这条规则对两类字段都成立：

- 时刻：`0` 在 Unix 时间里是 `1970-01-01`，是一个真实时刻；用它表示"没有"会让客户端显示成 1970 年。
- 集合与游标：空集合是 `[]` 而不是 `null`（客户端对 `null` 调 `.map()` 会抛 TypeError），`next_cursor` 没有下一页时是 `null` 而不是 `""`。

**时长不是时刻。** `fetch_interval`、`expires_in`（配置口径）用字符串 `"30m"`；`expires_in`（API 响应里的剩余秒数）是整数秒，因为它是计数值。前端换算只有一处：`lib/format.ts` 的 `parseTime` 里 `sec * 1000`。

唯一的时刻例外是 `SystemInfo.build_time`：它是 `-ldflags` 注入的构建元数据，不是运行时事实，保留人可读字符串。

### 1.1 鉴权边界

不是所有端点都要求登录。边界由「这个操作会不会替用户访问外部世界」与「页面是不是入口」决定：

| 端点 | 鉴权 | 理由 |
| --- | --- | --- |
| `GET /apps` | 无 | 工具台是入口页，未登录也该看到能做什么 |
| `GET /system/info`、`GET /system/stats` | 无 | 首页卡片角标依赖它们；只返回版本与聚合计数 |
| `/pastes/*` | **可选** | 匿名可创建与管理自己的片段，见 3.1 |
| `GET /p/*` | 无 | 分享链接要能被 curl 与爬虫读取 |
| `/feeds/*`、`/entries/*` | **必须** | 抓取会主动访问外部地址，匿名开放等于开放 SSRF |
| `/system/jobs`、`/system/config` | **必须** | 运维信息 |
| `/auth/*` | 无 | 它们本身就是获取凭证的入口，有独立限流 |

`/pastes/*` 用「可选鉴权」：有 Bearer 就按设备归属，有 `X-Owner-Token` 就按凭证归属，都没有则视为匿名——但**匿名请求的归属过滤不因此放宽**，见 3.1。

统一错误结构：

```json
{
  "error": {
    "code": "invalid_request",
    "message": "expires_in exceeds the configured maximum",
    "details": [{ "field": "expires_in", "reason": "must be <= 8760h" }],
    "request_id": "01JC8Z3K9V2Q7X4B6N0M"
  }
}
```

| HTTP | `code` | 含义 |
| --- | --- | --- |
| 400 | `invalid_request` | 请求体格式或字段非法 |
| 401 | `unauthenticated` | 缺少或无效凭证 |
| 403 | `forbidden` | 已认证但无权访问该资源 |
| 404 | `not_found` | 资源不存在 |
| 409 | `conflict` | 唯一约束冲突（slug、feed URL） |
| 410 | `gone` | 已过期或已被一次性读取 |
| 413 | `payload_too_large` | 超过配置上限 |
| 422 | `unprocessable` | 语义错误（如 feed 无法解析） |
| 429 | `rate_limited` | 触发限流，带 `Retry-After` |
| 500 | `internal` | 未预期错误，不泄露内部细节 |
| 502 | `upstream_error` | Homehub 宿主或目标站点失败 |
| 503 | `unavailable` | 启动中、迁移中或只读降级 |

所有 `/api/` 下的 404 都返回 JSON，不会落到 SPA fallback。

## 2. 鉴权：双向公私钥

### 2.1 为什么是挑战-响应

密码在服务端需要可逆或可哈希存储，泄露即失守；mTLS 的客户端证书在浏览器里分发和轮换都很麻烦。Ed25519 挑战-响应把这两点都绕开：

- 服务端只存公钥，库泄露也无法冒充客户端。
- 私钥由客户端生成，永不出设备，服务端从未见过。
- 不需要 CA、不需要证书链、不需要处理浏览器证书选择弹窗。

「双向」指两侧各有密钥对、互相验证：

| 方向 | 证明什么 | 机制 |
| --- | --- | --- |
| 客户端 → 服务端 | 请求方持有已登记私钥 | 对服务端下发的 nonce 签名 |
| 服务端 → 客户端 | 令牌由本服务签发且未被篡改 | 服务端 Ed25519 签发紧凑 JWS，公钥可公开获取 |

### 2.2 密钥与算法

- 算法：Ed25519（`alg: "EdDSA"`）。浏览器 WebCrypto 不支持时降级为 ECDSA P-256（`alg: "ES256"`），服务端两种都接受，算法记录在 `auth_client.algorithm`。
- 客户端私钥：**必须**以不可导出方式生成（WebCrypto `extractable=false`），存储在 IndexedDB 的 `CryptoKey` 对象里。JavaScript 无法读取私钥字节，XSS 也只能在页面上下文中滥用而不能带走密钥。
- 服务端私钥：`auth.server_key_path`，PKCS#8 PEM，权限 `0600`，首次启动自动生成。公钥通过 `GET /api/v1/auth/server-key` 暴露。
- 服务端密钥 ID `kid` = `base64url(sha256(公钥)[:8])`。

### 2.3 签名消息格式

所有签名都在**原始字节**上做 Ed25519，不做哈希包装（Ed25519 内部已含 SHA-512）。消息是 UTF-8 文本，字段以 `\n` 分隔，首行是领域前缀，防止签名被搬到其他协议里复用：

**认证挑战**

```text
homehubext/auth/v1
<client_id>
<nonce>
```

**请求签名（机器轨，见 2.7）**

```text
homehubext/req/v1
<METHOD>
<PATH>
<QUERY>
<hex(sha256(body))>
<timestamp>
<nonce>
```

### 2.4 设备配对

配对码由运维在服务器上生成，不经过网络：

```bash
sudo -u homehub /usr/local/bin/homehub-extension enroll \
  -config /etc/homehub-extension/config.json \
  -name "macbook-chrome"
```

输出形如 `HH-7QK4-2M9X-P3TB`，有效期 `auth.enrollment_ttl`（默认 15 分钟），服务端只存哈希。

浏览器流程：

```text
GET  /pair                      配对页
POST /api/v1/auth/enroll
     { "token": "HH-…", "name": "macbook-chrome",
       "algorithm": "ed25519", "public_key": "<base64>" }
  ←  { "client_id": "01JC…", "server_key": "<base64>", "kid": "k3Jd…" }
```

服务端校验：token 未使用、未过期、来源未被限流。成功后创建 `auth_client` 并在同一事务里标记 token 已用。

`/api/v1/auth/enroll` 是唯一不需要凭证的写接口，因此有三重约束：一次性 token、短有效期、按 IP 限流。

### 2.5 登录

```text
POST /api/v1/auth/challenge
     { "client_id": "01JC…" }
  ←  { "nonce": "<base64 32B>", "expires_at": 1793287235 }

POST /api/v1/auth/token
     { "client_id": "01JC…", "nonce": "<base64>",
       "signature": "<base64>",          ← 对 2.3 的消息签名
       "client_kind": "browser" }        ← 省略时默认 browser
  ←  { "access_token": "<JWS>", "token_type": "Bearer",
       "expires_in": 900, "expires_at": 1793288135,
       "csrf_token": "<opaque>", "client": { "id": "01JC…", "name": "…" } }
     Set-Cookie: hh_session=<refresh_token>; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=604800
     Set-Cookie: hh_csrf=<csrf_token>; Secure; SameSite=Lax; Path=/
```

**`refresh_token` 不出现在响应体里。** 这是刻意的：它只在 `hh_session` cookie 中（`HttpOnly`），JavaScript 读不到。如果同时放进 JSON，XSS 只需一个 `fetch` 就能把长期凭证带走，HttpOnly 的全部意义就没了。

需要长期凭证的非浏览器客户端显式传 `"client_kind": "machine"`，此时才在响应体里返回 `refresh_token`，且不下发 cookie。两种客户端走不同分支，不靠 UA 猜测。

nonce 一次性：校验通过后立即写 `consumed_at`，重复提交返回 401。nonce 过期、client 已撤销、签名不匹配都返回同一个 401，不区分原因——避免给出枚举线索。

`access_token` 是无状态 JWS，15 分钟有效，服务端校验只需验签。这个时长固定，不提供配置项——它就是「撤销一台设备」的最坏延迟。`refresh_token` 是不透明随机串，数据库只存 `sha256`，7 天有效，可即时撤销。

`refresh` 的响应形状与 `/auth/token` 一致（含 `client` 对象），因此前端刷新页面时只调 `/auth/refresh` 一次就能同时拿到会话与身份，不需要再跟一个 `/auth/me`。旧 refresh token 在使用成功后立即失效（旋转），旧 token 重用视为泄露，服务端撤销该 client 的全部会话。

### 2.6 刷新、登出与撤销

| 端点 | 说明 |
| --- | --- |
| `POST /api/v1/auth/refresh` | 用 refresh cookie 换新的 access token，旋转 refresh 与 csrf；响应内含 `client`，前端页面加载只此一请求即可完成会话恢复 |
| `POST /api/v1/auth/logout` | 撤销当前 refresh token，清除 cookie |
| `GET /api/v1/auth/me` | 返回当前 client 与令牌剩余时间 |
| `GET /api/v1/auth/server-key` | 公开。返回 `{ "kid": "…", "public_key": "…", "algorithm": "ed25519" }` |
| `GET /api/v1/clients` | 已登记设备列表（不含公钥明细） |
| `DELETE /api/v1/clients/{id}` | 撤销设备：`revoked_at` 置位，同时删该 client 的全部 session 与 challenge |

撤销的设备在 access token 过期前（≤ 15 分钟）仍能通过验签。这是无状态令牌的固有取舍，15 分钟窗口可接受。

### 2.7 机器对机器：请求签名

适合服务端脚本、CI、Homehub 宿主回调。不使用 cookie 与 Bearer，而是每个请求自带签名：

```http
POST /api/v1/pastes HTTP/1.1
X-HH-Client: 01JC8Z3K9V2Q7X4B6N0M
X-HH-Timestamp: 1793251200
X-HH-Nonce: 9mK2pQ…
X-HH-Signature: <base64>
```

服务端校验顺序：时间戳偏差 ≤ 60 秒 → nonce 未在窗口内出现过 → 签名匹配 → client 未撤销。nonce 存在内存 LRU（容量 10 万，TTL 120 秒），不落库。

**已实现**，并且是 Homehub 推送订阅的唯一认证方式（见 4.0）。

### 2.8 CSRF

Cookie 认证的写请求必须带 `X-CSRF-Token`，值与 `hh_csrf` cookie 一致（双提交）。`SameSite=Lax` 已经阻断跨站表单，双提交是第二道防线。Bearer 认证的请求豁免，因为浏览器不会自动附加该头。

缺少或不匹配返回 403 `forbidden`，不区分具体原因。

**两个显式豁免**，在实现时必须写进中间件的豁免表：

| 端点 | 为什么徽免 | 补偿措施 |
| --- | --- | --- |
| `POST /api/v1/auth/refresh` | 页面加载时就要调它换 token，而 `csrf_token` 本就由它下发，存在鸡生蛋依赖 | `SameSite=Lax` 阻断跨站 POST；refresh token 每次使用即旋转；跨域请求读不到响应，「拿到」本身就不可能 |
| `GET /api/v1/events/stream` | `EventSource` 无法设自定义头；只读、无副作用 | 只接收 GET；按 client 限制并发连接数 |

`/auth/refresh` 的残余风险是「攻击者能诱导浏览器旋转 token 造成自己的会话失效」，属于自扰式 DoS，代价低于把 token 下发前就强制 CSRF 带来的实现复杂度。

### 2.9 限流

| 范围 | 限制 |
| --- | --- |
| `/api/v1/auth/*` 按来源 IP | 10 次 / 5 分钟，超限 429 |
| 其他 `/api/v1/*` 按 client | 600 次 / 分钟 |
| `POST /pastes` 按 client | 30 次 / 小时（防止被当作公开文件床） |
| `/media/*` | 不限流；内容寻址 + 强缓存，热请求基本不落到 Go |

实现用 `golang.org/x/time/rate` 的内存桶，单实例无需共享存储。进程重启后计数清零，可接受。

## 3. Pastebin

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/pastes` | 创建 |
| `GET` | `/api/v1/pastes` | 列表（仅本人可见，游标分页） |
| `GET` | `/api/v1/pastes/{slug}` | 读取 |
| `PUT` | `/api/v1/pastes/{slug}` | 覆盖更新（需要密码则必须带） |
| `DELETE` | `/api/v1/pastes/{slug}` | 软删除 |
| `GET` | `/p/{slug}` | 公开 HTML 页（服务端渲染，无 JS） |
| `GET` | `/p/{slug}?raw=1` | 纯文本，`text/plain` |

### 3.1 归属与 owner token

创建不要求登录，但「私有片段只有创建者能看」与「管理自己创建的片段」都需要一个身份。方案是一枚 **owner token**：

- 首次匿名创建时服务端生成，只在创建响应里出现一次，字段名 `owner_token`
- 客户端必须存下来（`localStorage`），之后的请求带 `X-Owner-Token` 头
- 服务端只存它的 `sha256`，库里没有明文；丢了找不回
- 已登录创建不返回该字段，归属由 Bearer 承载

**为什么用 header 而不是 query**：query 会进访问日志，而这些日志通常会被收集到别处。

**过滤必须 fail-closed**：`GET /pastes` 始终按归属过滤，哪怕归属是空字符串。早先的实现把空字符串当"不过滤"，结果匿名调用方能看到所有人的片段——空字符串恰恰就是匿名调用方会带来的值。

`visibility` 的语义：

| 值 | 含义 |
| --- | --- |
| `public` | 出现在公开列表 |
| `unlisted` | 知道链接即可访问（默认） |
| `private` | 只有创建者能读；其他人拿到链接返回 **404**（不是 403——确认存在性本身就是泄露） |

创建请求：

```json
{
  "title": "Caddy 反代片段",
  "body": "reverse_proxy 127.0.0.1:8080 {\n  flush_interval -1\n}",
  "language": "caddyfile",
  "visibility": "unlisted",
  "expires_in": "168h",
  "password": null,
  "burn_after_read": false
}
```

- `visibility`：`public`（出现在公开列表）、`unlisted`（知道链接即可访问，默认）、`private`（仅创建者）。
- `expires_in`：`"0"` 表示永久，上限为 `pastebin.max_expiry`。
- `password`：非空时用 argon2id 哈希存储；读取需要 `X-Paste-Password` 头或 `?password=`。
- `burn_after_read`：首次成功读取后立刻标记删除，之后返回 410。

响应：

```json
{
  "id": "01JC8Z…",
  "slug": "aB3xK9",
  "url": "https://homehub.example.com/p/aB3xK9",
  "created_at": 1793286720,
  "expires_at": 1793891520
}
```

`GET /p/{slug}` 由服务端渲染极简页面：内联 CSS、等宽字体、`<pre>` 原文、右上角显示过期时间、无任何外部资源、`CSP: default-src 'none'; style-src 'unsafe-inline'`。这样即使前端 bundle 出问题，分享链接仍然可用；也给爬虫和 `curl` 一个稳定入口。

## 4. RSS

### 4.0 入口：Homehub 主动推送

订阅不是用户在前端添加的，而是 Homehub 宿主推过来的。宿主知道用户在它的界面里订阅了什么，把地址、更新间隔与启停状态一起推给 Extension；Extension 只负责抓取、本地化与呈现。

```http
POST /api/v1/hub/subscriptions
X-HH-Timestamp: 1793251200
X-HH-Nonce: 9mK2pQ…
X-HH-Signature: <base64>

{
  "url": "https://example.com/feed.xml",
  "title": "Example Blog",
  "interval": "30m",
  "enabled": true,
  "fetch_now": true
}
```

语义：

- **幂等**：同一个 URL 重复推送只更新调度参数，不重复建订阅，也不清空已经学到的 `title` / `site_url` / `etag`——空值表示"没说"，不是"清空"。
- **间隔夹紧**：`interval` 落在 `[rss.min_interval, rss.max_interval]` 之间，超出即夹到边界。下限保护目标站点，上限防止一个源被无限期冷落。
- **首推即抓**：`fetch_now` 缺省为 `true`，异步触发一次抓取，宿主不必等一个完整周期。
- **鉴权**：整个 `/api/v1/hub/*` 分组走 2.7 的请求签名，不接受 Bearer。一条合法请求无法被挪到另一个路径或另一个 body 上重放。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/hub/subscriptions` | 推送（201 新建 / 200 更新） |
| `GET` | `/api/v1/hub/subscriptions` | 对账列表，含 `last_status`、`last_error`、`error_count` |
| `DELETE` | `/api/v1/hub/subscriptions/{id}` | 移除订阅，级联清理条目与 blob 引用 |
| `POST` | `/api/v1/hub/subscriptions/{id}/refresh` | 立即抓取，同步返回本轮统计 |

失败退避：连续失败按指数退避至 `max_interval`，连续 10 次把 `enabled` 置 0 并在响应里带出 `last_error`，等宿主侧决定是否重新启用。已抓取的条目不受影响。

### 4.1 订阅源

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/hub/subscriptions` | **Homehub 推送订阅**（签名认证），见 2.7 |
| `GET` | `/api/v1/feeds` | 列表，含未读数与最近错误 |
| `GET` | `/api/v1/feeds/{id}` | 详情 |
| `PATCH` | `/api/v1/feeds/{id}` | 改标题、间隔、启用状态 |
| `DELETE` | `/api/v1/feeds/{id}` | 删除并级联清理条目引用 |
| `POST` | `/api/v1/feeds/{id}/refresh` | 立即抓取（手动刷新） |
| `GET` | `/api/v1/feeds/{id}/entries` | 该源条目，等价于 `entries?feed_id=` |

响应包含抓取状态：

```json
{
  "id": 7,
  "url": "https://example.com/feed.xml",
  "title": "Example Blog",
  "site_url": "https://example.com/",
  "enabled": true,
  "fetch_interval": "30m",
  "next_fetch_at": 1793290200,
  "last_status": 200,
  "last_error": "",
  "error_count": 0,
  "unread_count": 12,
  "total_count": 340
}
```

### 4.2 条目

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/entries` | 列表。参数：`feed_id`、`unread`、`starred`、`q`、`limit`、`cursor` |
| `GET` | `/api/v1/entries/{id}` | 详情，含 `content_html` |
| `PATCH` | `/api/v1/entries/{id}` | `{"read": true}` 或 `{"starred": true}` |
| `POST` | `/api/v1/entries/bulk` | `{"ids":[…],"read":true}`，单次上限 200 |
| `GET` | `/api/v1/entries/{id}/original` | 清洗前的原始 HTML，仅用于排查（`?download=1` 可下载） |

列表默认不含 `content_html`，只给 `summary`，避免一次拉几百篇文章。阅读时再取详情。

`content_html` 中的资源地址已经全部指向 `/media/<sha256>.<ext>`，前端直接 `innerHTML` 渲染即可，不会产生任何第三方请求。

### 4.3 媒体

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/media/{sha256}.{ext}` | 返回文件，强缓存 + `ETag` |
| `GET` | `/media/{sha256}.{ext}?download=1` | 附 `Content-Disposition: attachment` |

- `Cache-Control: public, max-age=31536000, immutable`——内容寻址，地址变了内容必然变。
- 支持 `If-None-Match` 返回 304。
- 响应带 `X-Content-Type-Options: nosniff`；`Content-Type` 取自入库时探测并校验过的 MIME，绝不回显客户端提供值。
- 支持 `Range`（视频拖动需要），单区间，返回 206。

## 5. 系统

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/healthz` | 进程存活，恒 200，不查依赖 |
| `GET` | `/readyz` | 依赖就绪：DB 可读、迁移完成、blob 目录可写、磁盘余量；任一失败 503 |
| `GET` | `/api/v1/system/info` | 版本、构建时间、commit、schema 版本、运行时长、go 版本 |
| `GET` | `/api/v1/system/stats` | 计数：paste、feed、entry、blob 占用、待重试媒体 |
| `GET` | `/api/v1/system/jobs` | 最近 50 条 `job_run` |
| `POST` | `/api/v1/system/jobs/{name}/run` | 手动触发一次清理，同步返回结果摘要 |
| `GET` | `/api/v1/system/config` | 当前生效配置的**脱敏**视图（密钥、token 显示为 `***`） |

`/healthz` 与 `/readyz` 不进鉴权，也不记访问日志（避免探针刷日志）。`/readyz` 的失败原因放在响应体里，便于 curl 排查。

## 6. 实时事件

```text
GET /api/v1/events/stream
Accept: text/event-stream
```

```text
event: entry.created
id: 12841
data: {"entry_id":12841,"feed_id":7,"title":"…"}

event: job.finished
id: 12842
data: {"job":"blob-gc","status":"ok","detail":"purged 12 blobs, 340 MB"}

: ping
```

| 事件 | 触发时机 |
| --- | --- |
| `entry.created` | 新条目入库 |
| `entry.updated` | 已读/标星变化（其他设备） |
| `feed.updated` | 抓取完成，含失败状态 |
| `job.finished` | 清理任务结束 |
| `system.alert` | 磁盘不足、WAL 膨胀、配额到顶 |

约束：

- 心跳注释行每 20 秒一次，防止中间设备按空闲断开。
- `id` 单调递增（全局事件序号，存内存 + 启动时取 `entry` 最大 id 作为基线）。客户端重连带 `Last-Event-ID`，服务端补发 `id > last` 的事件（窗口 5 分钟，超出则发 `system.resync` 让前端全量刷新）。
- 单个连接的发送缓冲 64 条，满则断开该连接而不是阻塞广播。
- 浏览器用 `EventSource`，cookie 自动携带，无需自定义头。
- **鉴权凭据是 `hh_session`（refresh token）cookie，不是 Bearer**。`EventSource` 不支持自定义请求头，`access_token` 又只存内存。服务端对 `/events/stream` 直接校验 refresh cookie 的哈希，与 `/auth/refresh` 用同一张表、同一套有效期。这是唯一的 cookie 认证读接口，不需要 CSRF（GET 无副作用），但需要按 client 限流（最多 3 个并发连接，超出关旧建新）。
- Caddy 必须 `flush_interval -1`，Go 必须 `WriteTimeout = 0`。

## 7. 缓存头

| 路径 | `Cache-Control` |
| --- | --- |
| `/media/*` | `public, max-age=31536000, immutable` |
| `/_app/*`（带 hash 的静态资源） | `public, max-age=31536000, immutable` |
| `/index.html`、SPA fallback | `no-cache` |
| `/api/*` | `no-store` |
| `/p/{slug}` | `public, max-age=60`（private 除外） |

## 8. 示例

```bash
# 登录（浏览器或脚本先完成配对）
curl -sX POST https://homehub.example.com/api/v1/auth/challenge \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"01JC8Z3K9V2Q7X4B6N0M"}'

# 用返回的 nonce 签名后换 token
curl -sX POST https://homehub.example.com/api/v1/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"01JC…","nonce":"…","signature":"…"}'

# 创建片段
curl -sX POST https://homehub.example.com/api/v1/pastes \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"hello","body":"world","visibility":"unlisted","expires_in":"24h"}'

# 添加订阅
curl -sX POST https://homehub.example.com/api/v1/feeds \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/feed.xml","fetch_interval":"30m"}'

# 读取未读条目
curl -s "https://homehub.example.com/api/v1/entries?unread=true&limit=20" \
  -H "Authorization: Bearer $TOKEN"
```

## 9. 待定

- OPML 导入导出：`POST /api/v1/feeds/import`、`GET /api/v1/feeds/export`，取决于是否需要批量迁移。
- 请求签名（2.7）是否值得实现，取决于是否真有 CI/宿主回调场景。
- 服务端密钥轮换（多 `kid` 并存）首版不做，单密钥 + 重启即换新会让所有 access token 在 15 分钟内失效，可接受。
