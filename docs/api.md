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
sudo -u homehub /usr/bin/homehub-extension enroll \
  -config /etc/hhe/config.json \
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

`access_token` 是签过名的紧凑 JWS，15 分钟有效。这个时长固定，不提供配置项：它限制单个令牌的暴露窗口，并强制客户端周期性使用 refresh 轮换。

**校验是「验签 + 查 client」。** 每次带 Bearer 的请求都会验签，再按 `client_id` 读一次 `auth_client` 并确认 `revoked_at` 为空。多出这一次查询是刻意的：不查库的话，撤销设备或撤销访问密钥要等令牌自然过期才生效，而「撤销」在运维动作里必须立刻算数。代价是每个认证请求多一次主键查询（SQLite 上可忽略）。

`refresh_token` 是不透明随机串，库里只存 `sha256`，有效期 `auth.session_ttl`（默认 7 天），每次使用即旋转。

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

**撤销立即生效。** 令牌本身是自包含的，但每次校验都会回查 client，所以 `DELETE /api/v1/clients/{id}` 之后，该设备的 access token 在下一次请求就被拒绝，不必等 15 分钟。`refresh_token` 一并在同一事务里删除。

### 2.7 机器对机器：请求签名

目前只有一个调用方用到它：**Homehub 宿主**。粘贴创建、订阅管理这些端点都不接受签名，它们走 Bearer 或 owner token。

**身份不是一个字段，而是「能签出这个签名」这件事本身。** Extension 里没有宿主账号，没有为它签发的 API key，也没有访问令牌——只有部署时放进去的一把公钥。确认过程就四步：

1. **部署时**：宿主生成 Ed25519 密钥对，把公钥**内容**（PKIX PEM 或裸 base64）填进 `auth.hub.public_key`。私钥始终在宿主侧。
2. **每次请求**：宿主用私钥签一条固定格式的消息。
3. **Extension 验签**：用存着的公钥验。
4. **验签通过 = 请求方持有对应私钥 = 它是宿主**。

没有「先认证换 token」的交换。每条请求都自证，服务端不存会话；只留一张 nonce 去重表防重放，那张表不参与身份判定。

请求头只有三个：

```http
X-HH-Timestamp: 1793251200
X-HH-Nonce: 9mK2pQ…
X-HH-Signature: <base64>
```

待签消息七行，`\n` 连接，无尾随换行：

```text
homehubext/req/v1
<METHOD>
<PATH>
<QUERY>
<hex(sha256(body))>
<timestamp>
<nonce>
```

几个容易签错的地方：

- `PATH` 是 `URL.EscapedPath()`，百分号编码后的形式。
- `QUERY` 是 `RawQuery` 原串，不排序。
- 空 body 也要算摘要（空串的 sha256 是 `e3b0c442…`），不是空字符串。
- 时间戳是十进制 Unix 秒，只接受整数。

校验顺序（`internal/auth/signature.go`）：

1. 三个头缺任一 → 401
2. `|now - ts| > clock_skew`（默认 60s）→ 401
3. `ed25519.Verify` 不通过 → 401
4. nonce 在窗口内出现过 → 401

第 4 步放在验签之后是有意的：先登记的话，任何人都能用攒造的请求把去重表撑满（容量 10 万），把合法请求挤成「已重放」。nonce 表存在内存里，每 `ttl/4`（默认 30 秒）清扫一次，不落库。

公钥没配置时返回 **503 `unavailable`** 而不是 401——那是部署缺失，不是请求方的问题。

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

### 2.10 访问密钥：宿主签发，用户直接登录

原来只有一条拿凭证的路：SSH 到服务器跑 `enroll` 生成配对码。宿主要给某个用户发一把「你自己就能进」的凭证，隔着这层做不到。

访问密钥补上了这条路。宿主用签名通道签发，用户拿着它去 `/pair` 页面输入，或者直接调 `/auth/key`。

#### 签发

```http
POST /api/v1/hub/keys
X-HH-Timestamp: 1793251200
X-HH-Nonce: 9mK2pQ…
X-HH-Signature: <base64>

{"name": "alice", "expires_in": "720h"}
```

```json
{
  "id": "01JC…",
  "name": "alice",
  "key": "HHE-7QK4-2M9X-P3TB-8HVN-5RCW",
  "client_id": "01JC…",
  "created_at": 1793251200,
  "expires_at": 1795843200
}
```

`expires_in` 省略表示永不过期。"0" 或负数被拒绝——用「省略」表达永久，不用 `0`，避免 1970 年那种歧义。

**`key` 只在这一个响应里出现。** 库里存的是 `sha256`，之后任何接口都拿不回明文，也拿不回本体。丢了只能重新签发。

#### 兑换

```http
POST /api/v1/auth/key

{"key": "HHE-7QK4-2M9X-P3TB-8HVN-5RCW"}
```

响应与 `POST /auth/token` 完全一致（`access_token`、`expires_in`、`csrf_token`、`client`），并且同样用 `Set-Cookie` 下发 `hh_session`。拿到令牌之后，两条登录路径再无区别——同一个 `issue()`、同一个 owner_hash、同一个权限模型。

#### 管理

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/hub/keys` | 签发，201，明文只返回一次 |
| `GET` | `/api/v1/hub/keys` | 列表，**不含明文也不含哈希**，带 `live` 布尔供对账 |
| `DELETE` | `/api/v1/hub/keys/{id}` | 撤销密钥，并连带撤销它换出去的会话 |

#### 语义

- **可重复使用**：同一串密钥可以反复兑换，换浏览器、换设备都用它。这是它与配对码的核心区别——配对码 15 分钟、用一次即废。
- **撤销即生效**：`DELETE` 会同时撤销绑定客户端和它的全部 refresh 会话。access token 虽然是自包含 JWS，但每次校验都会确认 client 未被撤销，所以旧令牌在下一次请求就失效，没有 15 分钟残留窗口。
- **输入容错**：大小写、空格、分隔符、是否带 `HHE-` 前缀都不影响结果。规范化只按「前缀 + 主体」的总长度判定，不会误裁主体本身（字母表含 H 和 E，主体可能恰好以 `HHE` 开头）。
- **熵**：20 位 31 进制，约 99 bit，离线穷举不现实。兑换端点仍然过 IP 限流。

#### 安全边界

宿主签发时，Extension 会生成一对 Ed25519 密钥、把公钥登记为一台「无主客户端」、**私钥当场丢弃**。所以：

- 这台客户端永远无法用挑战-响应登录——没人签得出那个消息；
- 它只能靠密钥兑换；
- `auth_client` 的表结构一个都不用改，不需要在 SQLite 里重建表。

密钥本身是长期共享凭证，一旦泄露等于账号泄露。它不像设备私钥那样「不可导出」，所以不要写进 URL、不要贴进公开渠道。需要作废时调 `DELETE /api/v1/hub/keys/{id}`。

#### 授权边界

**Extension 不做授权决策。** 谁能拿到密钥、拿几把、什么时候收回，全部由宿主的权限系统判断；Extension 只验证「这条请求确实来自持有宿主私钥的一方」。因此签名通过 = 无条件下发密钥，没有任何白名单、配额或审批环节。

这是有意的取舍：Extension 里没有用户表，也没有身份数据，它无法独立判断「这个人在宿主里还在不在职」。把授权放在宿主是唯一说得通的位置——撤销权限时宿主停止签发并调 `DELETE`，比在 Extension 里复刻一套权限模型可靠得多。

代价要说清楚：**宿主私钥一旦泄露，攻击者可以给自己签发任意数量的永久密钥**，Extension 没有任何手段阻止。所以宿主私钥的保护等级必须等同于 Extension 实例本身。

#### 数据归属

所有能登录的凭证——配对设备、宿主签发的访问密钥——都属于同一个主人，`owner_id` 填同一个实例级标识 `primary`，`owner_hash` 按它推导。于是：

- **跨入口可见**：用密钥建的私有片段，配对的浏览器设备看得到；反过来也一样。同一个人不该因为换了认证方式就看不到自己的东西。
- **密钥轮换无损**：签发新密钥 → 确认能登录 → 撤销旧的。新密钥读得到旧密钥名下的全部私有片段，撤销只让旧密钥自己的会话失效。
- **匿名片段仍然隔离**：匿名创建没有身份，`owner_hash` 按 owner token 算。登录身份读不到别人的匿名私有片段，只有持有 owner token 的一方可以。
- **RSS 订阅全局共享**：`feed` 表按 `url` 唯一、没有 owner 列，所有凭证看到同一批订阅。订阅是实例级资源，同一份 feed 抓一次就够了。

现有部署升级到 0005 时会有一次数据搬迁：`auth_client.owner_id` 补齐，同时全部 `paste.owner_hash` 从 `c:<clientID>` 重算为 `o:primary`。两步在同一个事务里完成，中途失败一起回滚。搬迁之后旧哈希不再匹配任何查询，也不保留兼容分支。

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

- **幂等，且省略就是保留**：同一个 URL 重复推送不重复建订阅。省略的字段分三类处理——`title` / `site_url` 为空表示「没说」，保留已学到的值（`etag` / `last_modified` 同理，永远不会被推送清空）；`interval` 省略沿用已存的间隔，新建时用 `rss.default_interval`；`enabled` 省略沿用已存状态，新建时默认 `true`。

  后两条不是细节：如果省略就回落默认，宿主一次「只改标题」的推送会把用户显式暂停的订阅重新启用，并把间隔重置回 30 分钟。
- **间隔夹紧**：`interval` 落在 `[rss.min_interval, rss.max_interval]` 之间，超出即夹到边界。下限保护目标站点，上限防止一个源被无限期冷落。
- **重排规则**：`next_fetch_at` 只在间隔真的变了才重算（`now + interval`）。重复推同样的间隔不会把下一次抓取推迟，否则宿主频繁对账推送会把抓取永远推后。
- **首推即抓**：`fetch_now` 缺省为 `true`，异步触发一次抓取（独立 5 分钟 context，请求立即返回），宿主不必等一个完整周期。
- **恢复会清零失败计数**：`enabled` 从 `false` 显式改回 `true` 时，`error_count` 与 `last_error` 一并清零。否则一个已因连续失败被停用的源，恢复后下一次失败就重新越线，演成「刚恢复就又停了」。
- **鉴权**：整个 `/api/v1/hub/*` 分组走 2.7 的请求签名，不接受 Bearer。一条合法请求无法被挪到另一个路径或另一个 body 上重放。
- **模块关闭时四个端点一致**：`rss.enabled = false` 时，push / list / delete / refresh 都返回 `404 not_found` + `"RSS 模块未启用"`。注意这是签名之后的回答：未签名的请求仍然先得到 401，不会因为模块关闭而泄露它的存在。宿主可以用同一个判断决定要不要展示 RSS 功能。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/hub/subscriptions` | 推送（201 新建 / 200 更新） |
| `GET` | `/api/v1/hub/subscriptions` | 对账列表，含 `last_status`、`last_error`、`error_count` |
| `DELETE` | `/api/v1/hub/subscriptions/{id}` | 移除订阅，级联清理条目与 blob 引用 |
| `POST` | `/api/v1/hub/subscriptions/{id}/refresh` | 立即抓取，同步返回本轮统计 |

失败退避：连续失败按指数退避至 `max_interval`，连续 10 次把 `enabled` 置 0 并在响应里带出 `last_error`，等宿主侧决定是否重新启用。已抓取的条目不受影响。

宿主想恢复一个被自动停用的源，只要再推一次 `{"url": "...", "enabled": true}`：`error_count` 会被清零，`next_fetch_at` 按当前间隔重排。如果源已经彻底不可用，它会在再失败 10 次之后重新被停用。

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

## 5. 工具台与系统

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/v1/apps` | 工具台卡片清单（见下） |
| `GET` | `/healthz` | 进程存活，恒 200，不查依赖 |
| `GET` | `/readyz` | 依赖就绪：DB 可读、迁移完成、blob 目录可写、磁盘余量；任一失败 503 |
| `GET` | `/api/v1/system/info` | 版本、构建时间、commit、schema 版本、运行时长、go 版本 |
| `GET` | `/api/v1/system/stats` | 计数：paste、feed、entry、blob 占用、待重试媒体 |
| `GET` | `/api/v1/system/jobs` | 最近 50 条 `job_run` |
| `POST` | `/api/v1/system/jobs/{name}/run` | 手动触发一次清理，同步返回结果摘要 |
| `GET` | `/api/v1/system/config` | 当前生效配置的**脱敏**视图（密钥、token 显示为 `***`） |

`/healthz` 与 `/readyz` 不进鉴权，也不记访问日志（避免探针刷日志）。`/readyz` 的失败原因放在响应体里，便于 curl 排查。

### 5.1 工具台卡片

`GET /api/v1/apps` 返回卡片数组，前端只负责渲染：

```json
{
  "apps": [
    {"id":"pastebin","name":"Pastebin","icon":"paste","description":"…",
     "path":"/pastes","enabled":true,"badge":{"count":12,"label":"条"}},
    {"id":"tools","name":"开发工具","icon":"tools","description":"…",
     "path":"/tools","enabled":true,"badge":null}
  ]
}
```

第二张卡的 `badge` 写成 `null` 而不是省略：实际响应里它一定存在，值可以是对象或 `null`。

- 卡片列表在 `handleApps` 里逐项追加，顺序固定：pastebin → rss → tools → system → feeds。
- 关闭配置时有两种行为，不要混为一谈：`pastebin.enabled` / `rss.enabled` 为 `false` 时，对应卡片**根本不 append**，数组里看不到它；而 `feeds` 卡片始终下发，它的 `enabled` 跟着 `rss.enabled` 走——RSS 关闭时它仍在数组里，但是 `false`。前端对 `enabled: false` 的卡片不隐藏，而是渲染成不可点击的 `app-card--disabled` 加一枚「未启用」角标。
- `badge` 是 `null` 而不是被省略（字段没有 `omitempty`）——前端判断 `if (app.badge && app.badge.count > 0)`，`null` 与 `0` 都不渲染。角标口径必须与列表一致：片段数按 `owner` 过滤，未读数在匿名请求下强制为 `0`——否则会出现「首页说 2 条、列表是空的」。
- 空数组是 `[]`，不是 `null`。

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

- 服务端密钥轮换（多 `kid` 并存）首版不做。密钥文件持久化在 `auth.server_key_path`，重启复用，不会换新；只有运维主动替换文件才会让已签发的 access token 立即失效（校验时签名对不上）。

## 10. 相关文档

- 面向 Homehub 宿主的对接指南：[hub-integration.md](hub-integration.md)——部署准备、签名实现、端点清单、重试与对账语义。
