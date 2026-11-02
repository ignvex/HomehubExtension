# 变更日志

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

### 变更

- **部署布局改为 `hhe`**：守护单元改名 `hhe.service`，二进制装到 `/usr/bin/homehub-extension`，
  配置与宿主公钥放到 `/etc/hhe/`，数据目录 `StateDirectory=hhe` 展开为 `/var/lib/hhe`。
  备份单元同步改名为 `hhe-backup.service` / `hhe-backup.timer`，备份输出 `/var/backups/hhe`。
- **发布产物暂存区改为 `dist/`**：`make build` 输出 `dist/homehub-extension`，不再压缩、
  不入库（`.gitignore` 忽略 `/dist/`）。与前端 `web/dist`（`go:embed` 输入）互不影响。
- 默认配置搜索路径从 `/etc/homehub-extension/config.json` 改为 `/etc/hhe/config.json`，
  `homehub-extension --help` 与 `config.example.json` 同步。
- Dockerfile 镜像内路径同步为 `/usr/bin`、`/etc/hhe`、`/var/lib/hhe`。

### 性能与宿主通道

- **出站客户端改为进程级复用**：之前每次 feed 抓取都新建 `Transport`，
  连接池永远只有一条连接，每次抓取都要重做 TCP+TLS 握手。
  实测连续抓 10 次建 10 条连接，现在 1 条。
- **`rss.fetch_concurrency` 现在是全局上限**：之前只约束定时轮询，
  宿主批量补推订阅时 HTTP 层触发的抓取完全不受限。实测 20 个订阅
  并发推送会产生 20 个并发对外请求；现在受 `fetch_concurrency` 约束。

### 修复

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
