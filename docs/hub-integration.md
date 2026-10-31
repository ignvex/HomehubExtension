# Homehub 宿主对接指南

这份文档写给 **Homehub 宿主的实现者**：你在宿主里推送订阅、查询抓取状态、为用户签发登录密钥。它不解释 Extension 内部怎么实现，只说明你必须遵守的协议，以及出错时该怎么判断。

浏览器端与 REST API 的完整约定见 [api.md](api.md)；本文只覆盖 `/api/v1/hub/*` 这条机器通道。

## 1. 通道概览

宿主是主动方。Extension 不轮询宿主，也不持有任何宿主账号或 API key。全部调用都走请求签名。

| 能力 | 方法与路径 | 成功状态 |
| --- | --- | --- |
| 推送/更新订阅 | `POST /api/v1/hub/subscriptions` | 201 新建 / 200 更新 |
| 对账订阅列表 | `GET /api/v1/hub/subscriptions` | 200 |
| 移除订阅 | `DELETE /api/v1/hub/subscriptions/{id}` | 204 |
| 立即抓取 | `POST /api/v1/hub/subscriptions/{id}/refresh` | 200 |
| 签发访问密钥 | `POST /api/v1/hub/keys` | 201 |
| 列出访问密钥 | `GET /api/v1/hub/keys` | 200 |
| 撤销访问密钥 | `DELETE /api/v1/hub/keys/{id}` | 204 |

基址是 `https://<server.public_url>/api/v1`。`/hub/*` **不接受** `Authorization: Bearer`，只认签名头——带 Bearer 不会加分，缺签名照样 401。

## 2. 身份：一把宿主持有的私钥

Extension 里没有「宿主账号」。它只保存一把公钥，每次请求都用这把公钥验签。验签通过就等于「请求方持有对应私钥」——身份不是某个字段，而是签名本身。

### 2.1 生成密钥对

算法固定为 Ed25519。宿主侧生成，私钥永远不离开宿主：

```bash
openssl genpkey -algorithm ed25519 -out homehub.key
openssl pkey -in homehub.key -pubout -out homehub.pub
chmod 600 homehub.key
```

公钥文件是 PKIX PEM（`-----BEGIN PUBLIC KEY-----`）。Extension 也接受裸 base64、raw Ed25519 PEM，但部署脚本统一用 PKIX 最省事。

### 2.2 部署公钥

把 `homehub.pub` 放到 Extension 主机的：

```text
/etc/homehub-extension/homehub.pub
```

并在 `config.json` 里确认：

```json
{
  "auth": {
    "hub": {
      "enabled": true,
      "public_key_path": "/etc/homehub-extension/homehub.pub",
      "clock_skew": "60s",
      "nonce_ttl": "2m"
    }
  }
}
```

**公钥在进程启动时读取一次。** 换公钥要重启 Extension。文件缺失或解析失败时进程仍会启动，但 `/hub/*` 全部返回 `503 unavailable`，日志里有一条 `hub signature channel disabled`——这是部署问题，不是你的签名写错了。

文件权限建议 `0644`，属主 `root`。Extension 以 `homehub` 用户运行，只要可读即可。

### 2.3 密钥轮换

1. 生成新密钥对，把新公钥放到另一个路径。
2. 改配置指向新公钥，重启 Extension。
3. 新旧公钥不能同时生效——首版只支持一把。所以轮换期间会有一个短暂窗口，旧私钥的请求全部 401。选在宿主不推送的时间做。

## 3. 请求签名

### 3.1 三个头

```http
X-HH-Timestamp: 1793251200
X-HH-Nonce: 9mK2pQ7vX1
X-HH-Signature: <base64>
```

| 头 | 要求 |
| --- | --- |
| `X-HH-Timestamp` | 十进制 Unix 秒，只接受整数。与服务器时间差不得超过 `clock_skew`（默认 60 秒） |
| `X-HH-Nonce` | 每次请求唯一。重复使用会被拒。建议 16 字节随机数的 hex 或 UUID |
| `X-HH-Signature` | Ed25519 签名的标准 base64（也接受 base64url） |

### 3.2 待签消息

七行，用 `\n` 连接，**无尾随换行**：

```text
homehubext/req/v1
<METHOD>
<PATH>
<QUERY>
<hex(sha256(body))>
<timestamp>
<nonce>
```

- `METHOD`：大写，如 `POST`。
- `PATH`：百分号编码后的路径，即 Go 的 `URL.EscapedPath()`。不含 host、不含 query。
- `QUERY`：原始 query 串（`RawQuery`），**不排序、不重新编码**。没有 query 时是空行。
- body 摘要：请求体的原始字节做 SHA-256，小写 hex。**空 body 也要算**，空串的摘要是 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`，不是空字符串。
- 时间戳、nonce 必须与请求头里的值逐字节一致。

签名是这个消息字节串的 Ed25519 签名。Ed25519 内部已含哈希，**不要再对消息做一次 SHA-256**。

### 3.3 易错点

- **PATH 用编码后的形式。** 如果 URL 里有中文或空格，先编码再签，并且发出去的请求要使用同一份编码结果。最稳妥的做法是构造好最终 URL 对象，从它上面取 path 和 query。
- **query 不要排序。** 有些 HTTP 客户端会重排参数，签完之后请求就变了。签名前先固定 query 字符串，再原样拼进 URL。
- **body 必须与签名字节完全一致。** 先序列化 JSON 得到 `[]byte`，用这份字节签名，再原样发送。不要签完再让框架重新序列化一遍（字段顺序、空格、转义都可能变）。
- **重试要换 nonce 和时间戳。** 重放同一个 nonce 会得到 401 `请求已被处理过`。nonce 去重表容量 10 万条、TTL 默认 2 分钟。
- **时钟偏差没有自动校正。** 主机 NTP 漂移超过 60 秒后所有请求都会 401，错误信息是 `请求时间戳超出允许范围`。

### 3.4 Go 参考实现

```go
package hubclient

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type Signer struct {
	key ed25519.PrivateKey
}

func LoadSigner(path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return &Signer{key: priv}, nil
}

// Do 构造并发送一条已签名请求。body 为 nil 表示空请求体。
func (s *Signer) Do(method, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	ts := time.Now().Unix()
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes)

	sum := sha256.Sum256(body)
	msg := strings.Join([]string{
		"homehubext/req/v1",
		method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		hex.EncodeToString(sum[:]),
		fmt.Sprint(ts),
		nonce,
	}, "\n")

	req.Header.Set("X-HH-Timestamp", fmt.Sprint(ts))
	req.Header.Set("X-HH-Nonce", nonce)
	req.Header.Set("X-HH-Signature",
		base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(msg))))
	return http.DefaultClient.Do(req)
}
```

### 3.5 TypeScript 参考实现

```ts
import { createHash, createPrivateKey, randomBytes, sign } from "node:crypto";

export function signedRequest(
  privateKeyPem: string,
  method: string,
  rawUrl: string,
  body = "",
): { url: string; init: RequestInit } {
  const url = new URL(rawUrl);
  const ts = Math.floor(Date.now() / 1000).toString();
  const nonce = randomBytes(16).toString("hex");
  const digest = createHash("sha256").update(body, "utf8").digest("hex");

  const message = [
    "homehubext/req/v1",
    method.toUpperCase(),
    url.pathname,          // 已是百分号编码形式
    url.search.slice(1),   // 去掉前导 '?'
    digest,
    ts,
    nonce,
  ].join("\n");

  const signature = sign(null, Buffer.from(message, "utf8"),
    createPrivateKey(privateKeyPem)).toString("base64");

  return {
    url: url.toString(),
    init: {
      method,
      body: body === "" ? undefined : body,
      headers: {
        "Content-Type": "application/json",
        "X-HH-Timestamp": ts,
        "X-HH-Nonce": nonce,
        "X-HH-Signature": signature,
      },
    },
  };
}
```

注意 `body === ""` 与 `body === "{}"` 的区别：前者摘要按空串算，并且不发送请求体。不要让 fetch 把一个空字符串当成请求体发送——某些实现会附加 `Content-Length: 0` 之外的行为差异，而签名只认字节。

### 3.6 测试向量

用这组固定输入可以自检签名实现。私钥种子是 32 个 `0x01` 字节（**仅用于测试，绝不可用于生产**）。

```text
method:    POST
path:      /api/v1/hub/subscriptions
query:     dry=1
body:      {"url":"https://example.com/feed.xml","interval":"30m"}
timestamp: 1793251200
nonce:     test-nonce-0001

body sha256:
fae5596b35f13d7a967e1ce18e29a078c2a4c25c432b4445f6d1b146db1129e5

待签消息（7 行，行尾无额外字符）:
homehubext/req/v1
POST
/api/v1/hub/subscriptions
dry=1
fae5596b35f13d7a967e1ce18e29a078c2a4c25c432b4445f6d1b146db1129e5
1793251200
test-nonce-0001

公钥（base64）:
iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=

签名（base64）:
/kzfFqg4awpnAl+6XTpVhYaX3zhMw+qkubelssgiEO8sRRDH1l2S/rrKvDohfpVzmvMFooTpufw4ulRCPRZYCA==
```

用 Go 复现：

```go
seed := bytes.Repeat([]byte{1}, ed25519.SeedSize)
priv := ed25519.NewKeyFromSeed(seed)
sig := ed25519.Sign(priv, []byte(message))
// base64.StdEncoding.EncodeToString(sig) 应等于上面的签名
```

## 4. 端点参考

### 4.1 推送订阅

```http
POST /api/v1/hub/subscriptions
```

```json
{
  "url": "https://example.com/feed.xml",
  "title": "Example Blog",
  "interval": "30m",
  "enabled": true,
  "fetch_now": true
}
```

只有 `url` 必填。**省略的字段表示「保留原值」，不是「重置为默认」**：

| 字段 | 省略时 |
| --- | --- |
| `title` | 保留已学到的标题。空字符串同义 |
| `interval` | 保留已存间隔；新建时用 `rss.default_interval` |
| `enabled` | 保留已存状态；新建时 `true` |
| `fetch_now` | 默认 `true`，异步触发一次抓取后立即返回 |

`interval` 会被夹到 `[rss.min_interval, rss.max_interval]`（默认 10 分钟到 6 小时）。超出范围不报错，直接取边界值——响应里的 `fetch_interval` 是实际生效值。

`next_fetch_at` 只在间隔真的变化时重排。重复推送同一间隔不会把抓取时间往后推。

响应里的 `fetch_interval` 是紧凑写法（`"30m"`、`"1h30m"`、`"6h"`），可以直接作为下一次推送的 `interval` 原样传回。

### 4.2 对账列表

```http
GET /api/v1/hub/subscriptions
```

```json
{
  "items": [
    {
      "id": 7,
      "url": "https://example.com/feed.xml",
      "title": "Example Blog",
      "site_url": "https://example.com/",
      "enabled": true,
      "fetch_interval": "30m",
      "next_fetch_at": 1793290200,
      "last_fetch_at": 1793288400,
      "last_status": 200,
      "last_error": "",
      "error_count": 0,
      "unread_count": 12,
      "created_at": 1793251200
    }
  ]
}
```

`items` 始终是数组，没有订阅时是 `[]` 而不是 `null`。时刻字段未设置时是 `null`。

### 4.3 删除订阅

```http
DELETE /api/v1/hub/subscriptions/7
```

返回 204，条目与 blob 引用级联清理。订阅不存在返回 404。重复删除不是幂等的 204——第二次会是 404，宿主按「已删除」处理即可。

### 4.4 立即抓取

```http
POST /api/v1/hub/subscriptions/7/refresh
```

同步执行，最长 2 分钟。响应：

```json
{
  "feed_id": 7,
  "url": "https://example.com/feed.xml",
  "not_modified": false,
  "new_entries": 3,
  "skipped": 0,
  "media_ok": 5,
  "media_fail": 0,
  "status": 200
}
```

目标站点失败返回 502 `upstream_error`，订阅不存在返回 404。抓取失败也会更新 `error_count` 与 `next_fetch_at`。

### 4.5 签发访问密钥

```http
POST /api/v1/hub/keys
```

```json
{ "name": "alice", "expires_in": "720h" }
```

```json
{
  "id": "01JC8Z…",
  "name": "alice",
  "key": "HHE-7QK4-2M9X-P3TB-8HVN-5RCW",
  "client_id": "01JC8Z…",
  "created_at": 1793251200,
  "expires_at": 1795843200
}
```

- `expires_in` 省略表示永不过期。`"0"` 或负数被拒绝。
- **`key` 只在这一个响应里出现。** 库里存的是 `sha256`，之后任何接口都拿不回明文。
- 同一个人可以有多把密钥；它们共享同一个实例级归属，所以用 A 密钥建的私有片段，B 密钥也能看到。
- 密钥兑换登录由用户在前端完成（`POST /api/v1/auth/key`），宿主不需要代劳。

**授权在宿主侧。** Extension 不判断谁能拿密钥——签名通过就无条件下发。宿主私钥泄露等于可以批量签发永久密钥，所以它的保护等级等同于 Extension 实例本身。

### 4.6 列出访问密钥

```http
GET /api/v1/hub/keys
```

```json
{
  "items": [
    {
      "id": "01JC8Z…",
      "name": "alice",
      "client_id": "01JC8Z…",
      "created_at": 1793251200,
      "expires_at": null,
      "last_used_at": 1793288400,
      "revoked": false,
      "live": true
    }
  ]
}
```

`live` 是服务端替你算好的布尔：未撤销且未过期。宿主对账时只看这一个字段，不要自己比较 `expires_at` 与本地时间。

响应**不含明文，也不含哈希**。

### 4.7 撤销访问密钥

```http
DELETE /api/v1/hub/keys/01JC8Z…
```

返回 204。同时撤销绑定客户端和它的全部 refresh 会话；已签发的 access token 在下一次请求就失效（服务端每次都会确认 client 未撤销）。

```text
注意：撤销之后，该密钥名下的私有片段仍然可被其他凭证读取。
它们属于同一个主人，撤销的是登录方式，不是数据。
```

## 5. 错误处理

统一错误结构：

```json
{
  "error": {
    "code": "unauthenticated",
    "message": "签名校验失败",
    "request_id": "511da52e177f79a6"
  }
}
```

| HTTP | code | 含义 | 宿主该怎么做 |
| --- | --- | --- | --- |
| 400 | `invalid_request` | 字段格式错误 | 修正后重试，不要原样重发 |
| 401 | `unauthenticated` | 签名/时间戳/nonce 问题 | **不要重试**。检查时钟、nonce 唯一性、PATH/QUERY 编码 |
| 404 | `not_found` | 订阅或密钥不存在；RSS 模块未启用 | 从本地状态里移除；模块关闭时停止展示 RSS |
| 413 | `payload_too_large` | 超过 `server.max_body_bytes` | 缩小请求体 |
| 429 | `rate_limited` | 触发限流 | 按 `Retry-After` 秒数退避 |
| 500 | `internal` | 未预期错误 | 退避重试，带 `request_id` 报日志 |
| 502 | `upstream_error` | 目标站点抓取失败 | 可重试；但连续失败会被 Extension 自动退避 |
| 503 | `unavailable` | 公钥未配置或启动中 | 检查部署；这是配置问题，重试无用 |

重试策略：

- 401、400、404、413、503 都**不要**盲目重试。前四类是请求或状态问题，503 是部署问题。
- 429 按 `Retry-After`。
- 500、502 用指数退避，建议起始 5 秒、封顶 5 分钟，并加抖动。
- 每次重试都重新生成 nonce 与时间戳。

`request_id` 与服务端 JSON 日志里的 `request_id` 一致，报障时带上它。

## 6. 对账与状态机

### 6.1 推荐时序

宿主不需要轮询。推荐这三个触发点：

1. **用户在宿主里订阅/取消订阅时**：调 `POST` 或 `DELETE`。
2. **启动时**：`GET /api/v1/hub/subscriptions` 与本地订阅表对账。Extension 里存在但宿主没有的，删除；宿主有但 Extension 没有的，补推。
3. **定期（如每小时）**：再对账一次，修复漏推。

`fetch_now` 默认是 `true`，所以首次推送后不必额外调 refresh。只有用户在界面上显式点「立即刷新」时才调 `/refresh`。

### 6.2 抓取状态的解读

| 字段 | 含义 |
| --- | --- |
| `last_status` | 最近一次 HTTP 状态码。0 表示还没抓过 |
| `last_error` | 最近一次失败原因，已截断到 500 字符。成功时为空 |
| `error_count` | 连续失败次数。成功一次即清零 |
| `unread_count` | 该订阅下未读条目数，用于角标 |

`last_status` 为 0 且 `last_fetch_at` 为 `null`，说明还没开始抓——通常是刚推送、异步抓取尚未完成。

### 6.3 自动停用与恢复

Extension 的退避规则：

- 第一次失败按原间隔重排；之后每次失败间隔翻倍，封顶 `rss.max_interval`。
- 连续失败 10 次后 `enabled` 置为 `false`，`last_error` 前缀变成 `停用：连续失败 10 次；…`。

恢复只需要再推一次：

```json
{ "url": "https://example.com/feed.xml", "enabled": true }
```

`error_count` 会被清零、`last_error` 清空、`next_fetch_at` 按当前间隔重排。不要只把 `enabled` 改回 `true` 就以为完事——Extension 侧必须看到这次显式推送才会清零，否则下一次失败立刻又越线。

### 6.4 模块关闭时

`rss.enabled = false` 时，四个订阅端点统一返回 404 `not_found` + `RSS 模块未启用`；密钥端点不受影响。判断发生在验签之后，所以未签名请求仍然先得到 401——模块是否启用不会泄露给未授权方。

宿主可以用同一个 404 判断决定是否在界面上展示 RSS 功能。

## 7. 版本与兼容

- 签名域前缀是 `homehubext/req/v1`。协议有不兼容变更时才会升到 `v2`，届时会提供并行支持窗口。
- 请求体里未知字段会被忽略，宿主可以提前发送未来字段。
- 响应里可能新增字段。宿主解析时应忽略未知字段，不要做严格模式校验。
- 端点只增不减；删除端点会走版本升级。

## 8. 上线自检清单

- [ ] 私钥权限 `0600`，不在镜像、日志、环境变量里
- [ ] 公钥已放到 `auth.hub.public_key_path`，Extension 日志有 `hub signature channel ready`
- [ ] 宿主与 Extension 的时钟偏差小于 30 秒（留一半余量给 `clock_skew`）
- [ ] `GET /api/v1/hub/subscriptions` 返回 200
- [ ] 故意改一个字节的签名，确认得到 401（证明验签真的在跑）
- [ ] 同一个 nonce 连发两次，第二次得到 401
- [ ] 推送一个订阅后，`GET` 列表里能看到它，且 `last_fetch_at` 在几十秒内被填上
- [ ] 密钥签发后只保存 `id` 与明文 `key` 一次，并确认 `GET /keys` 里 `live=true`
- [ ] 撤销密钥后，用它兑换登录得到 401
- [ ] 错误日志里记录了 `request_id`，能和服务端日志对上
