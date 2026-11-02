# 变更日志

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

### 变更

- **宿主公钥改为内联内容（破坏性变更）**：`auth.hub.public_key_path` 替换为
  `auth.hub.public_key`，值直接是 PKIX PEM 或裸 base64，不再读取文件。
  旧配置里的路径字段会让启动失败，需要把公钥内容粘贴进来；填一个路径
  也会明确报错，而不是被当成路径再读一次。
- **宿主协议文档降为指针**：`docs/hub-integration.md` 不再维护全文副本。
  `/api/v1/hub/*` 的契约由 Homehub 仓库的 `docs/hub-integration.md` 唯一定义；
  两侧各存一份已经漂移过——批量推送端点只写在了 Extension 侧。
- **部署布局改为 `hhe`**：守护单元改名 `hhe.service`，二进制装到 `/usr/bin/homehub-extension`，
  配置与宿主公钥放到 `/etc/hhe/`，数据目录 `StateDirectory=hhe` 展开为 `/var/lib/hhe`。
  备份单元同步改名为 `hhe-backup.service` / `hhe-backup.timer`，备份输出 `/var/backups/hhe`。
- **发布产物暂存区改为 `dist/`**：`make build` 输出 `dist/homehub-extension`，不再压缩、
  不入库（`.gitignore` 忽略 `/dist/`）。与前端 `web/dist`（`go:embed` 输入）互不影响。
- 默认配置搜索路径从 `/etc/homehub-extension/config.json` 改为 `/etc/hhe/config.json`，
  `homehub-extension --help` 与 `config.example.json` 同步。
- Dockerfile 镜像内路径同步为 `/usr/bin`、`/etc/hhe`、`/var/lib/hhe`。

### 新增

- **OPML 导入导出（M10）**：`GET /api/v1/feeds/export` 导出全部订阅，
  `POST /api/v1/feeds/import` 批量导入。导入按递归收集分组嵌套的
  `<outline>`（多数阅读器导出会套两层文件夹），单条失败不中断整批并
  逐条回报原因。前端在「订阅源」页加入导入/导出按钮。
- **宿主批量推送**：`POST /api/v1/hub/subscriptions/batch`。启动对账
  补推几百条订阅时不必逐条签名往返，上限 500 条。
- **前端条目搜索与标星筛选**：`/entries` 增加搜索框（300 ms 防抖、
  回车立即搜）与「只看标星」。两个参数后端本就支持，此前前端没有入口。

### 移除

- **`GET /api/v1/hub/entries` 与 `GET /api/v1/hub/stats`**。
  这两个端点是为「宿主自己渲染阅读界面」设计的，但宿主侧的
  `hubclient` 与前端 `extensionHubApi` 从未调用过——它的
  `fetch_by=extension` 语义是「对端抓取，去 Extension 阅读」，
  阅读界面本来就在本服务这边。保留无人使用的接口只会让契约
  面变宽、增加维护与攻击面，因此移除。
  `POST /api/v1/hub/subscriptions/batch` 保留。

### 性能与宿主通道

- **出站客户端改为进程级复用**：之前每次 feed 抓取都新建 `Transport`，
  连接池永远只有一条连接，每次抓取都要重做 TCP+TLS 握手。
  实测连续抓 10 次建 10 条连接，现在 1 条。
- **`rss.fetch_concurrency` 现在是全局上限**：之前只约束定时轮询，
  宿主批量补推订阅时 HTTP 层触发的抓取完全不受限。实测 20 个订阅
  并发推送会产生 20 个并发对外请求；现在受 `fetch_concurrency` 约束。
- **宿主推送的 `url` 入口校验**：与网页端新建订阅同一套规则，
  内网字面地址与非白名单端口在入库前即返回 400。
- **去重命中不再误判为超配额**：`blob.Put` 的配额检查原本排在
  去重检查之前，接近上限时同一张被多篇文章引用的图片会被判成
  `quota_exceeded`——纯由检查顺序造成的失败。现在先看内容是否已在库，
  已在库则直接返回，不占新空间也就不查配额。

### 安全

- **argon2 参数校验**：`VerifyPassword` 从 PHC 字符串解析 `m=`/`t=`/`p=`
  后直接交给 `argon2.IDKey`，而它按 `m`（KiB）分配内存。被篡改的 hash
  写成 `m=4294967295` 就是 4 TiB 分配，一次读请求即可 OOM。
  现在参数必须落在合理区间，超范围一律当作验证失败。
- **gosec 安全门禁**：CI 新增 `make sec`，severity ≥ medium 必须清零。
  既有 8 处告警逐个审查后在行内用 `#nosec Gxxx -- 理由` 标注；
  只排除 G104（清理操作的未处理错误）。
- **依赖与工具链升级**：`govulncheck` 报出 14 个可达漏洞，
  其中两个命中清洗路径（`html.Render` 的命名空间检查、文本合并的
  二次复杂度）。`x/net` → v0.62.0、`quic-go` → v0.59.1、
  Go 指令 → 1.27.3。复查为 0 个可达漏洞。
- **SSRF：拆开 IPv6 内嵌的 IPv4**：NAT64（`64:ff9b::/96`）、6to4
  （`2002::/16`）、IPv4-compatible（`::/96`）在 IPv6 表面看是公网
  地址，实际可路由到内网。`http://[64:ff9b::a9fe:a9fe]/` 曾能直通
  云元数据。Teredo 整段封禁。
- **请求体上限下沉到读取层**：原先只看 `Content-Length`，chunked
  编码下该值为 -1，检查被绕过。实测 1 MiB 上限下 20 MiB 的 body
  被完整读入。现用 `http.MaxBytesReader` 封顶。
- **登出 cookie 的 Secure 属性**：反代部署下（TLS 在 Caddy 终止）
  登出下发的是不带 Secure 的同名 cookie，浏览器拒绝用它覆盖已存在
  的 Secure cookie，refresh token 会留在浏览器里。
- **paste 归属检查 fail-closed**：迁移前的历史片段 `owner_hash` 为空，
  私有片段曾对匿名开放（两边都空被判为匹配），且任何人知道 slug
  就能改写或删除。现在要求归属非空且匹配。
- **blob 路径加固**：`Store.Path` 自己对扩展名消毒，不再依赖调用方。

### 修复

- **statfs 溢出让大容量卷不可写**：`int64(blocks) * int64(bsize)`
  在极端值下溢出成负数，`checkSpace` 因此把卷误判成空间不足。
  改为饱和转换，非正值取 0、超上限取 `MaxInt64`。
- **搜索里的 `%` 与 `_` 搜不到东西**：`escapeLike` 把它们转成了 `\%`、`\_`，
  但 SQL 缺 `ESCAPE '\\'` 子句，SQLite 默认不把反斜杠当转义符，
  于是模式变成「含反斜杠」这种永远匹配不到的形状。搜 `50%` 或
  `snake_case` 都是 0 命中。
- **媒体重试的记账与类型**：重试下载命中去重时不再累加配额用量
  （否则用量会随重试次数单调上漂，最终把还有余量的存储判成超配额）；
  重试成功的资源按真实 MIME 记 `kind`，不再一律写成 `image`；
  `TotalSize` 查询失败不再被静默当成 0（那等于关掉配额）。

- **RSS 抓取把压缩字节当成正文**：请求头声明了 `gzip, br, zstd`，但只注册了 gzip 解码器；
  且手写 `Accept-Encoding` 会让 `net/http` 认为压缩由调用方处理、跳过透明解压。
  CDN 返回 Brotli 时（如 dmhy），`gofeed` 拿到二进制并报 `failed to detect feed type`。
  改为交给标准库协商（只请求 gzip），并在服务端无视协商时返回明确的
  `unexpected content-encoding` 错误，不再把排查引向解析器。
- **媒体下载不再把编码问题误报成类型问题**：响应带非 `identity` 的 `Content-Encoding` 时，
  在类型嗅探之前判 `failed` 并记下编码名——压缩字节会被嗅探成 `unsupported`，
  掩盖真正的传输层原因。

## [1.0.0-alpha] - 2026-10-31

首个对外版本。功能面覆盖路线图 M0–M9；OPML 导入导出（M10）尚未实现。

### 新增

**鉴权与凭证**
- 设备配对：一次性配对码（`HH-XXXX-XXXX-XXXX`，15 分钟），配对码只存哈希。
- 挑战-响应登录：Ed25519 / ECDSA P-256，客户端私钥不可导出并存在 IndexedDB。
- 会话：15 分钟 access token + 7 天 refresh token（HttpOnly cookie、每次使用即旋转）。
- CSRF 双提交；`/auth/refresh` 与 SSE 两处显式豁免。
- 设备管理：列表与撤销；撤销立即生效（每次校验都回查 client）。
- 宿主入站签名通道：覆盖 method/path/query/body 摘要/时间戳/nonce，
  60 秒时钟窗口、内存防重放表。
- 访问密钥：宿主通过签名通道签发 `HHE-…`，用户在前端兑换登录；
  明文只返回一次，可重复使用，撤销即连带撤销会话。
- 实例级归属：配对设备与访问密钥共享 `owner_id`，私有内容跨入口可见、
  轮换密钥不丢数据；匿名内容仍按 owner token 隔离。

**Pastebin**
- 匿名创建（返回 owner token）与登录创建。
- 过期、语言标记、argon2id 口令、一次性读取、public/unlisted/private。
- 无 JS 公开分享页 `/p/{slug}` 与 `?raw=1`，CSP 为 `default-src 'none'`。

**RSS**
- 宿主推送订阅（幂等、省略字段保留、间隔夹紧、首推即抓）。
- 网页端手动添加与管理订阅。
- 条件请求（ETag / Last-Modified）、指数退避、连续 10 次失败自动停用、
  显式恢复时清零失败计数。
- HTML 白名单清洗；媒体全量下载到内容寻址 blob 并重写为 `/media/<sha256>`。
- 已读/标星、批量操作、按订阅/未读/搜索分页。
- SSE 实时事件：`entry.created`、`entry.updated`、`feed.updated`、
  `job.finished`、`system.alert`，支持 `Last-Event-ID` 补发。

**工具台**
- 15 个纯浏览器工具，覆盖文本与校验、编码与哈希、配置格式、图片处理、
  时间与随机；按路由懒加载，实现不进首屏 bundle。
- 工具台首页卡片由 `GET /api/v1/apps` 驱动，未登录可见。

**运维与交付**
- `/healthz`、`/readyz`；`/api/v1/system/{info,stats,jobs,config}`。
- cron 清理任务：过期片段、会话、媒体重试、条目保留、blob GC、VACUUM、
  任务日志保留；每次执行写入 `job_run`，可手动触发。
- `enroll`、`migrate`、`backup` 子命令。
- systemd：`Type=notify` + watchdog 心跳、StateDirectory、启动前迁移、
  备份 timer（每日 3:00，`Persistent=true`）、加固评分 1.5。
- 单二进制：前端构建产物 `go:embed`；Caddyfile、Dockerfile、
  GitHub Actions（测试、race、lint、systemd 单元、前端产物规则）。
- 宿主对接指南 `docs/hub-integration.md`：签名参考实现与固定测试向量。

### 变更

- `fetch_interval` 统一输出紧凑时长（`"45m"`、`"1h30m"`），
  不再出现 `"45m0s"`；输出可直接作为下一次推送的 `interval`。
- `POST /api/v1/hub/subscriptions` 的响应带真实 `unread_count`
  （此前恒为 0，与列表接口不一致）。
- RSS 模块关闭时，四个订阅端点统一返回 404 `not_found`。
- 宿主 `refresh` 失败改用统一错误结构：上游问题 502、订阅不存在 404。
- 默认 User-Agent 更新为 `HomehubExtension/1.0`。

### 修复

- 服务端在端口绑定成功之前发送 `READY=1`，导致端口被占时
  `systemctl start` 返回成功但无监听。
- 宿主只改标题的推送会把已暂停订阅重新启用、把间隔重置回默认。
- 重复推送同一间隔会把下一次抓取不断推后。
- 间隔变更后 `next_fetch_at` 未重排，要等满一个旧周期才生效。
- 撤销设备/密钥后 access token 仍可用至过期；现在立即失效。
- 匿名列表在归属为空时未过滤（fail-closed 修复）。

### 已知限制

- OPML 导入导出（M10）未实现。
- 宿主公钥只支持一把，轮换需要重启并接受一个短暂 401 窗口。
- 真实外网站点抓取、真实浏览器 WebCrypto 配对未纳入自动化测试。
- 许可协议尚未选择，默认保留所有权利。

[1.0.0-alpha]: https://github.com/ignvex/HomehubExtension/releases/tag/v1.0.0-alpha
