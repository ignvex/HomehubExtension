# RSS 抓取与资源本地化

本模块的唯一验收标准：

> 打开一篇已抓取的文章，浏览器开发者工具的 Network 面板里，除了本站域名，**没有任何第三方请求**。

所有设计都服务于这条标准。做不到本地化的元素，宁可不显示，也不让浏览器去访问原站。

## 0. 订阅从哪来

订阅由 **Homehub 宿主推送**，不是用户在本站添加的。宿主推送地址、更新间隔与启停状态，扩展负责抓取、本地化与呈现。协议见 [api.md](api.md) 的 4.0 节。

这带来两个约束：

- **不能假设用户会手动触发抓取**。首推必须立即抓一次，否则宿主那边点了订阅、首页却空着。
- **同一 URL 的重复推送必须是幂等的**。宿主可能因为重启、重连、用户改设置而反复推送同一个源，重复建订阅会让同一个源被抓几十遍。

## 1. 总览

```text
fetch ──► parse ──► sanitize ──► collect ──► download ──► rewrite ──► store
  │         │          │            │           │            │          │
条件请求   gofeed   白名单清洗   抽取资源URL   流式落盘    改成本地URL   entry
ETag/304  统一模型  剥离脚本     解析 srcset   校验+哈希    失败转换占位   entry_blob
```

四个阶段严格串行，只有 `download` 内部并发。这样任何一步失败都能定位到具体条目与具体 URL。

## 2. 抓取

### 2.1 请求

```http
GET <feed url> HTTP/1.1
User-Agent: HomehubExtension/0.1 (+https://homehub.example.com)
Accept: application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, */*;q=0.5
Accept-Encoding: gzip, br, zstd
If-None-Match: <etag>
If-Modified-Since: <last-modified>
```

- 超时：连接 5s、TLS 10s、整体 `rss.request_timeout`（默认 20s）。
- 重定向：最多 5 跳，**每一跳都重新做 SSRF 检查**。
- 响应体：`io.LimitReader(max_feed_bytes + 1)`，超出即判失败，不截断解析（截断的 XML 会给出误导性错误）。
- `304 Not Modified`：只更新 `next_fetch_at` 与 `last_status`，不碰条目。
- 非 2xx：记录 `last_status` 与响应体前 512 字节（去掉控制字符），`error_count++`。
- 压缩由 `net/http` 自动处理，但 `br`/`zstd` 需要显式注册解码器；首版只接受 `gzip`，减少依赖。

### 2.2 退避

```text
interval(n) = clamp(min_interval * 2^(error_count-1), min_interval, max_interval)
next_fetch_at = now + interval(n) * jitter(0.9, 1.1)
```

- `error_count` 只在**连续**失败时累加，成功一次清零。
- 连续 10 次失败：`enabled = 0`，写 `system.alert`，前端在订阅源列表显式提示「已停止」，需要手动重新启用。
- 加入 ±10% 抖动，避免 50 个源在同一秒醒来。

### 2.3 调度

一个独立的后台循环（`internal/app/poller.go`）每 30 秒扫一次 `feed`：

```sql
SELECT id FROM feed
WHERE enabled = 1 AND next_fetch_at <= ?
ORDER BY next_fetch_at
LIMIT 20;
```

为什么是独立循环而不是 cron 任务：

- **精度**：feed 的最小抓取间隔是 10 分钟，30 秒的扫描精度足够；而 5 段 cron 表达式最小只到分钟。
- **日志**：它每分钟都要跑，塞进 `job_run` 会瞬间淹没真正的清理记录。

单轮最多取 20 个，交给 `fetch_concurrency` 个 worker。上一轮没跑完时本轮直接跳过——排队没有意义，下一轮会重新取。

同一 feed 的并发抓取用一个进程内的 `inflight` 集合去重：手动刷新撞上定时任务时，定时那一侧跳过。抓取本身是幂等的（`UNIQUE(feed_id, guid)`），所以即使真的重复也只是多一次 HTTP 请求，不会产生重复条目。

**这个循环是 RSS 的心跳。** 没有它，`next_fetch_at` 每次抓取后都会被正确计算并写库，却永远没人消费——订阅会停在首次推送时的内容上，看起来「RSS 不更新了」，而所有单元测试仍然通过。

## 3. 解析

用 `mmcdole/gofeed`，统一 RSS 0.9x/1.0/2.0、Atom、JSON Feed。字段映射：

| 统一字段 | RSS | Atom | 缺失时 |
| --- | --- | --- | --- |
| `guid` | `<guid>` | `<id>` | 见下 |
| `link` | `<link>` | `<link rel=alternate>` | 空字符串 |
| `title` | `<title>` | `<title>` | `"(无标题)"` |
| `published` | `<pubDate>` | `<published>` / `<updated>` | `fetched_at` |
| `content` | `<content:encoded>` | `<content>` | `<description>` / `<summary>` |
| `summary` | `<description>` | `<summary>` | 从 content 提取前 280 字 |

**去重键**按顺序取第一个非空值：

1. `guid`（去除首尾空白后原样使用）
2. `link` 规范化后的 URL（去掉 fragment、utm_* 参数、统一小写 scheme/host）
3. `sha256(title + "\n" + published_at + "\n" + content)` 的前 16 字节

唯一约束 `UNIQUE(feed_id, guid)`。重复条目只更新可变字段（标题、正文若有变化），不重置 `read_at`/`starred_at`。

**相对链接**：清洗前先确定 base URL（`<base href>` > feed 的 `site_url` > entry 的 `link`），把所有相对地址解析成绝对地址。必须在清洗之前做，否则 `src="/a.png"` 会被误判为站内资源。

## 4. HTML 清洗

输入是任意来源的 HTML，按**不可信内容**处理。用 `golang.org/x/net/html` 建树后遍历，输出新的规范化文档片段。

### 4.1 允许保留的元素

```text
p div span br hr
a img picture source figure figcaption
h1 h2 h3 h4 h5 h6
ul ol li dl dt dd
blockquote q cite pre code kbd samp var
table thead tbody tfoot tr th td caption colgroup col
strong em b i u s del ins mark small sub sup abbr
details summary
video audio track
```

### 4.2 一律移除的元素

| 元素 | 原因 |
| --- | --- |
| `script`、`noscript` | 执行代码；noscript 内常含追踪像素 |
| `style` | 可含 `url()` 外部请求、`@import` |
| `link` | 远程样式表、preconnect、prefetch |
| `iframe`、`frame`、`frameset`、`object`、`embed`、`applet` | 无法本地化，会发起第三方请求 |
| `form`、`input`、`button`、`select`、`textarea`、`label` | 交互与提交，无意义且可被滥用 |
| `meta`、`base`、`title`（正文内） | `http-equiv=refresh` 可跳转 |
| `svg`（内联元素） | 可含 script、foreignObject、外部引用；首版不深度清洗。注意：作为 `<img src>` 引用的外部 SVG 文件不在此列，它会被下载并本地化。 |
| `math` | 同 SVG，首版移除 |
| `template` | 内容不可见但会保留原始结构 |
| 所有注释 | 可能含条件注释与 SSI 指令 |

**`<iframe>` 的例外**：如果 `src` 匹配已知视频域名（`youtube.com/embed/`、`youtu.be`、`player.vimeo.com`、`bilibili.com/player`），不静默丢弃，而是替换为：

```html
<p class="embed-fallback">
  <a href="https://www.youtube.com/watch?v=…" rel="noopener noreferrer nofollow" target="_blank">
    ▶ 在原站观看视频
  </a>
</p>
```

这里只放链接。浏览器不会自动请求，用户点击才会离开本站——满足"零自动外发请求"。

### 4.3 属性白名单

| 元素 | 保留属性 |
| --- | --- |
| `a` | `href`、`title` |
| `img` | `src`、`srcset`、`alt`、`width`、`height`、`loading`、`decoding` |
| `source` | `src`、`srcset`、`type`、`media` |
| `video` | `src`、`poster`、`controls`、`width`、`height`、`preload` |
| `audio` | `src`、`controls`、`preload` |
| `track` | `src`、`kind`、`srclang`、`label`、`default` |
| `td`、`th` | `colspan`、`rowspan`、`scope` |
| `ol` | `start`、`reversed` |
| `details` | `open` |
| 全局 | `id`（用于锚点跳转）、`lang`、`dir` |

其他属性一律删除。特别是：

- 所有 `on*` 事件属性。
- `style` 属性：正文排版交给站点 CSS，避免不可控的绝对定位、隐藏文本、`background:url()`。这是有意的取舍——保留 `style` 会让"零外部请求"难以保证。
- `class`：删除。原站的类名对本站无意义，保留只会与本站 CSS 冲突。
- `data-*`：删除。部分站点把原文 URL 藏在这里，前端脚本会用它补请求。

### 4.4 属性值协议校验

| 属性 | 允许的 scheme |
| --- | --- |
| `href` | `http`、`https`、`mailto`、`tel`、以 `/` 或 `#` 开头的相对地址 |
| `src`、`poster` | `http`、`https`、`data:image/*`（≤ 64 KB） |
| `srcset` | 同上，逐项校验 |

拒绝 `javascript:`、`vbscript:`、`file:`、`blob:`、`filesystem:`，以及所有含控制字符或前后空白的变体（`java\tscript:` 这类绕过）。校验前先做实体解码与 trim。

### 4.5 外链处理

所有保留下来的 `<a>` 统一加：

```html
rel="noopener noreferrer nofollow" target="_blank"
```

`noreferrer` 保证点击跳转时也不把本站地址泄露给目标站。

### 4.6 追踪像素

在清洗阶段用启发式识别并移除 `img`：

- `width` 与 `height` 同时 ≤ 2（属性值或 CSS 内联均无，只看属性）
- URL 命中已知追踪域列表（`google-analytics.com`、`doubleclick.net`、`facebook.com/tr`、`pixel.wp.com` 等），列表随代码发布，不做远程更新
- URL 路径以 `/pixel`、`/beacon`、`/track`、`/1x1` 结尾且无扩展名

无法确定的一律按普通图片处理（下载并本地化），因为误删内容比多下载一个文件更糟。

实现位置：`Document.isPixel`，在 `copyAttrs` 之前调用——此时原始 `src` 与 `width`/`height` 还在，`copyAttrs` 会把地址解析成绝对地址并丢掉 `class`/`data-*`，之后就不好判断了。判定为像素的 `img` 整棵丢弃，**不进入下载队列**：下载它等于替追踪器完成一次上报。

## 5. 媒体收集与下载

### 5.1 收集

清洗后的文档里扫描这些位置，产出 `(element, attr, url)` 三元组：

```text
img[src]              img[srcset]
video[poster]         video[src]
audio[src]
track[src]
picture > source[srcset]
a[href] 且（有 download 属性 或 扩展名属于媒体/文档白名单）
```

`srcset` 按 HTML 规范解析：逗号分隔候选，但 URL 内部的逗号必须正确处理——解析器先取到空白前的连续非空白字符作为 URL，再解析描述符。不用 `strings.Split(",")`，那会破坏 CDN 的 `?w=100,200` 形式参数。

同一个 URL 在一次抓取内只下载一次（`map[url]result`）。

### 5.2 下载

每个资源：

1. **URL 校验**：scheme 必须是 http/https，长度 ≤ 2048。
2. **SSRF 检查**（第 7 节），每一跳重定向都检查。
3. `GET`，带 `Referer`（按 `rss.referer` 配置）、与 feed 相同的 UA、`Accept-Encoding: identity`（避免解压炸弹）。
4. 先看 `Content-Length`：超过 `max_blob_bytes` 直接记 `too_large`，不读 body。
5. 流式读 `io.LimitReader(max_blob_bytes+1)` 到 `tmp/<random>`，边读边算 sha256，同时累计配额。
6. 读完后若长度超限，删除临时文件，记 `too_large`。
7. 用前 512 字节做 `http.DetectContentType`，与响应头 `Content-Type` 交叉校验；**以嗅探结果为准**，响应头只作参考。不一致时记一条 debug 日志。
8. 按白名单判定 `kind` 与扩展名（第 5.3 节）。不在白名单 → 记 `unsupported`，删除临时文件。
9. `rename(tmp, blobs/<xx>/<sha256>.<ext>)`。目标已存在说明内容重复，直接删临时文件。
10. 事务内写 `blob`（`INSERT … ON CONFLICT DO NOTHING`）与 `entry_blob`（`ON CONFLICT DO NOTHING`）。

并发控制：全局 `media_concurrency`（默认 3）的信号量。同一 feed 内串行，不同 feed 之间可并行。每条目再限制下载资源数（默认 60），防止一篇"图片墙"文章把配额吃光。

### 5.3 类型白名单

| `kind` | MIME | 扩展名 |
| --- | --- | --- |
| `image` | `image/jpeg`、`image/png`、`image/gif`、`image/webp`、`image/avif`、`image/svg+xml` | `.jpg` `.png` `.gif` `.webp` `.avif` `.svg` |
| `audio` | `audio/mpeg`、`audio/mp4`、`audio/ogg`、`audio/opus`、`audio/wav` | `.mp3` `.m4a` `.ogg` `.opus` `.wav` |
| `video` | `video/mp4`、`video/webm`、`video/ogg` | `.mp4` `.webm` `.ogv` |
| `font` | `font/woff2`、`font/woff`、`font/ttf`、`font/otf` | `.woff2` `.woff` `.ttf` `.otf` |
| `attachment` | `application/pdf`、`application/epub+zip` | `.pdf` `.epub` |

明确拒绝：`text/html`、`application/xhtml+xml`、`application/javascript`、`application/zip`、`application/octet-stream`（除非能嗅探出上面的类型）。

SVG 特殊处理：允许下载，因为 `<img>` 上下文里的 SVG 不执行脚本也不加载外部资源；但 `/media` 返回 SVG 时必须加 `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`，防止用户直接打开该地址时以顶级文档执行脚本。

**流式媒体（HLS/DASH）不支持**：`application/vnd.apple.mpegurl`、`.m3u8`、`.mpd` 及其分片不在白名单内。原因是一篇文章可能引用数千个分片，下载它们会瞬间打满配额和带宽，而把这些分片重新封装成可播放格式又超出了本项目的范围。处置：`<video>` 元素连同其 `src` 一起移除，不给链接——原站播放器本身就意味着第三方请求。

### 5.4 防盗链

部分图床校验 `Referer`。按顺序尝试，全部失败才放弃：

1. `rss.referer = auto`：用 entry 的链接
2. 失败且配置为 `auto`：改用 feed 的 `site_url` 重试一次
3. 仍 403/404：记 `failed`，正文用占位

不能无限重试——每一次重试都是对目标站的额外请求。上限 2 次。

### 5.5 失败处理

| 状态 | 触发 | 正文中的表现 |
| --- | --- | --- |
| `ok` | 下载并入库 | `/media/<sha256>.<ext>` |
| `too_large` | 超单文件或配额 | 占位块，`<img>` 保留 `alt` |
| `blocked` | SSRF 检查拒绝 | 直接移除该元素 |
| `unsupported` | 类型不在白名单 | 直接移除该元素 |
| `failed` | 网络错误、4xx/5xx | 占位块，由 `media-retry` 重试 |
| `quota_exceeded` | 全局配额或磁盘不足 | 占位块，写 `system.alert` |

占位块是一个内联 SVG data URI（灰色方块 + 图标），**不含原始 URL**。原 URL 只存在于 `media_fetch.source_url`，用于重试与排查；不渲染到页面，避免用户误点或前端脚本自动加载。

`<video>`/`<audio>` 下载失败时直接移除元素——静态占位对音视频没有意义。

## 6. URL 重写

下载成功的资源替换为：

```html
<img src="/media/3f9a…c1.webp"
     srcset="/media/3f9a…c1.webp 1x, /media/7b2e…44.webp 2x"
     alt="…" loading="lazy" decoding="async" width="800" height="600">
```

规则：

- `srcset` 的每个候选独立下载、独立重写；某个候选失败则该候选从列表中剔除。**`src` 永远不能保留原始地址**：若原 `src` 下载失败但 `srcset` 中有成功候选，`src` 取该候选的本地地址；全部失败则整图降级为占位。
- `sizes` 删除——本地图片的固有尺寸已由 `width`/`height` 表达，保留 `sizes` 会让浏览器按错误条件选择。
- 保留 `width`/`height`（防止布局抖动），但做上限截断（单边 > 4096 时按比例缩到 4096），避免恶意超大尺寸撑破布局。
- `loading="lazy"` 与 `decoding="async"` 无条件加上。
- 正文里第一条 `<img>` 可以标记为 `fetchpriority="high"`（首屏），由前端 CSS 决定是否显示。
- 外链 `href` 不做任何重写，只加 `rel`/`target`。

## 7. SSRF 防护

这是本模块最容易被忽视的安全边界：服务端会按用户提供的 URL 发起请求，而它同时能访问家庭内网。

### 7.1 检查项

URL 的 host 解析出**全部** A/AAAA 记录后，逐条检查，任何一条命中就拒绝：

| 网段 | 说明 |
| --- | --- |
| `0.0.0.0/8`、`::/128` | 未指定地址 |
| `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16` | RFC1918 私有地址 |
| `127.0.0.0/8`、`::1` | 回环 |
| `169.254.0.0/16`、`fe80::/10` | 链路本地（含云元数据 `169.254.169.254`） |
| `100.64.0.0/10` | CGNAT |
| `fc00::/7` | IPv6 ULA |
| `224.0.0.0/4`、`ff00::/8` | 组播 |
| `192.0.2.0/24`、`198.51.100.0/24`、`203.0.113.0/24` | 文档用网段 |
| `198.18.0.0/15` | 基准测试网段 |
| `192.88.99.0/24` | 6to4 中继任意播 |
| `255.255.255.255/32` | 受限广播 |

另外拒绝：

- 非 `http`/`https` scheme
- 含用户信息的 URL（`http://user:pass@host/`）
- 端口不在 `{80, 443, 8080, 8443}` 之外的显式端口（可按需放宽到全端口，默认收紧）
- host 是裸 IPv6 字面量时同样走上述检查

实现提示：`net/netip` 的 `IsPrivate()` 只覆盖 RFC1918 与 ULA，**不能单独用它**。需要组合 `IsUnspecified`、`IsLoopback`、`IsLinkLocalUnicast`、`IsLinkLocalMulticast`、`IsInterfaceLocalMulticast`、`IsMulticast`、`IsPrivate`，再叠加一张针对 `100.64.0.0/10`、`192.0.2.0/24`、`198.18.0.0/15`、`198.51.100.0/24`、`203.0.113.0/24`、`192.88.99.0/24`、`255.255.255.255/32` 的 CIDR 表。另外 `4in6`（`::ffff:127.0.0.1`）必须先 `Unmap()` 再判断，否则是一个现成的绕过。

`rss.allow_private_hosts = true` 时跳过网段检查，仅保留 scheme 与端口限制。这个开关是给"订阅自家 NAS 上的 feed"准备的，默认关闭。

### 7.2 DNS rebinding

只做一次"解析→检查→再请求域名"是不够的：第二次解析可能返回内网地址。做法是自定义 `DialContext`：

```go
DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
    host, port, _ := net.SplitHostPort(addr)
    ips, err := resolver.LookupIPAddr(ctx, host)
    if err != nil { return nil, err }
    for _, ip := range ips {
        if !isAllowed(ip.IP) { return nil, errBlockedHost }
    }
    // 直接拨号到已校验的 IP，SNI/Host 头仍为原域名
    return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}
```

这样从校验到连接之间不存在第二次解析。TLS 的 SNI 与 `Host` 头由 `http.Transport` 按原 URL 设置，证书校验不受影响。

DNS 解析用带超时的 `net.Resolver`，避免恶意域名拖住 worker。

### 7.3 重定向

`CheckRedirect` 对每一跳重新执行 7.1 的检查，最多 5 跳。不满足直接返回错误，不跟随。

## 8. 存储与配额

```text
blobs/<sha256[0:2]>/<sha256>.<ext>
tmp/<random>.part
```

- 写入前检查：`blob 总大小 + 新文件大小 > quota_bytes` → 拒；`磁盘可用 < min_free_bytes` → 拒。
- 配额统计不做全表 `SUM(size)`（每次都扫表太贵），而是维护 `settings` 里的一行累计值，`blob-gc` 时用 `SUM` 校准。
- 同一内容被 N 篇文章引用只存一份，`blob.ref_count = N`。
- `media-retry` 每 30 分钟重试 `pending`/`failed` 且 `attempts < max_attempts`、`within` 时间窗内的记录。

## 9. 内容安全策略

前端页面由 Go 中间件统一下发：

```text
default-src 'none';
script-src 'self';
style-src 'self';
img-src 'self' data:;
media-src 'self';
font-src 'self';
connect-src 'self';
frame-src 'none';
object-src 'none';
base-uri 'none';
form-action 'self';
frame-ancestors 'none'
```

含义约束到实现：

- 正文里不能有内联 `<style>` 或 `style=` 属性——否则需要 `'unsafe-inline'`，而 `'unsafe-inline'` 一旦加上，追踪用的 `background:url()` 就有了通道。这正是 4.3 删除 `style` 属性的原因。
- 不允许外部字体。原站自定义字体一律丢弃，回落到系统字体栈。
- `frame-src 'none'` 与 4.2 的 iframe 移除互相印证：即使清洗漏了一个 iframe，CSP 也会拦住。
- 开发模式（Vite HMR）需要内联脚本，CSP 由 `web/index.html` 的开发配置单独放宽；生产构建不注入任何内联脚本。

前端渲染 `content_html` 时直接 `innerHTML`。这里的安全性由"服务端已完成清洗 + CSP 兜底"两层保证，而不是靠前端再清洗一次——前端没有 HTML 解析器，做不到正确清洗。

## 10. 全文抓取（未实现）

**当前版本不抓全文。** feed 只给摘要时就只存摘要。

这里记录设计意图，供将来实现时参考。曾经存在 `rss.fulltext.enabled` 配置项，但代码从未实现对应逻辑——配置项存在却不生效比没有它更糟，所以已经删除。

要做的语义：

1. feed 的 `content` 长度 < 500 字符且无 `<img>` 时，判定为摘要。
2. `GET` 条目链接（同样的 SSRF 与限额约束）。
3. 用 readability 类算法提取正文。
4. 结果进入同一套 sanitize → collect → download → rewrite 管线。

风险：这是对目标站的额外请求，可能触发反爬。实现时必须带：

- 每个 feed 每小时最多 20 次。
- 同一域名串行，间隔 ≥ 2 秒。
- 失败不重试，直接用摘要。
- 明确的开关，默认关闭。

## 11. 性能预算

单篇 20 张图的文章，在 1 vCPU 上：

| 阶段 | 耗时 |
| --- | --- |
| feed 下载（1 MB） | 0.2–2 s（网络） |
| 解析 + 清洗（2 MB HTML） | 60–120 ms CPU |
| 媒体下载（20 × 200 KB，3 并发） | 2–8 s（网络） |
| sha256 + 落盘 | 50 ms |
| 入库事务 | 10–20 ms |

CPU 峰值出现在 HTML 解析，单核可接受。真正的风险是慢速站点把 worker 占住——`request_timeout` 与整体 `fetch_timeout` 是硬约束。

## 12. 验收清单

实现完成后逐条验证：

- [ ] 一篇含图片、视频、iframe 的样本文章，`content_html` 中不存在任何非本站绝对 URL（除 `a[href]`）。
- [ ] 用 `curl` 检查 `/media/*` 的 `Content-Type` 与 `Cache-Control`。
- [ ] 断网后打开已抓取文章，图片与视频仍可播放。
- [ ] 构造指向 `127.0.0.1`、`169.254.169.254`、`10.0.0.1` 的图片 URL，确认全部 `blocked`。
- [ ] 构造 301 到内网的重定向，确认被拒绝。
- [ ] 构造 1 GB 图片，确认 `too_large` 且未写满磁盘。
- [ ] 删除 blob 文件后打开文章，确认显示占位而不是 500。
- [ ] `media-retry` 执行后，失败项按预期重试且不无限增长。
