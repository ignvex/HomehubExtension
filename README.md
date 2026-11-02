# HomehubExtension

Homehub 的扩展服务：一个 Go 单二进制后端 + TypeScript 前端，为 Homehub 宿主叠加若干自托管功能。

功能模块：

- **工具台首页** —— 应用卡片网格，由 `GET /api/v1/apps` 驱动。**未登录也能看**：它是入口页，不该是登录墙。卡片列表由后端 `handleApps` 组装，前端不硬编码：新增一个模块要动两处——后端追加一张卡片，前端注册对应路由。
- **Pastebin** —— **不登录也能创建**。支持过期时间、语言标记、口令保护、一次性读取、私有访问与无需 JS 的公开分享页。匿名创建时服务端返回一枚 owner token，凭它管理自己的片段。
- **RSS 阅读器（资源本地化）** —— 订阅可由 **Homehub 宿主主动推送**，也可在网页端手动添加（需登录）。抓取后把正文涉及的所有媒体下载到本地，无法本地化、但会让浏览器对外发起请求的元素一律抹除。验收标准是可执行的：**读完一篇文章，浏览器没有向第三方发出任何请求**。
- **开发工具台（15 个）** —— 全部在浏览器里算，不上传、不联网、不追踪。按路由懒加载，首屏只下载元数据，单个工具的实现不进主 bundle。分五类：文本与校验（JSON、正则、程序员计算器）、编码与哈希（Base64、URL、哈希）、配置格式（YAML / JSON / INI 互转）、图片处理（格式转换、压缩、Base64）、时间与随机（时间戳、UUID、二维码、颜色、字体）。

> 状态：M0–M10 已全部落地。当前版本 `1.0.0-alpha`，变更见 [CHANGELOG.md](CHANGELOG.md)。`make test`、`make lint`、`make units`、`pnpm --dir web check` 全绿，`go build` 与 `pnpm --dir web build` 都能产出可部署产物。

## 设计约束

整套设计由部署环境倒推：

| 约束 | 取值 | 影响 |
| --- | --- | --- |
| CPU | 1 vCPU amd64 | 并发靠限流而非加核；不在服务器上编译 |
| 内存 | 2 GB | 进程硬上限 512 MB，`GOMEMLIMIT=256MiB` |
| 磁盘 | 20 GB | blob 配额 5 GB；事件/条目保留期 + 定期 GC |
| 入口 | Caddy 反代，自动 HTTPS | Go 只监听 `127.0.0.1:8080`，不终止 TLS |
| 交付 | 单二进制 + systemd | 前端构建产物 `go:embed` 进可执行文件 |

时间表示全局统一：**Unix 秒**（SQLite `INTEGER` → Go `int64` → JSON number → JS number），只在 `lib/format.ts` 一处乘 1000。未设置是 `null` 而不是 `0`——`0` 在 Unix 时间里是 1970-01-01。时长（`"30m"`）与时刻不混用。

## 架构

```text
                         ┌──────────────────────────────┐
   浏览器 / API 客户端 ──► │  Caddy :443                  │
                         │  自动证书 · gzip/zstd · 安全头 │
                         └──────────────┬───────────────┘
                                        │ reverse_proxy（flush_interval -1）
                         ┌──────────────▼───────────────┐
                         │  HomehubExtension :8080      │
                         │                              │
                         │  gin 路由                    │
                         │   ├ /api/v1/*  REST          │
                         │   ├ /media/*   本地化资源     │
                         │   ├ /p/{slug}  公开粘贴页     │
                         │   └ /*         embed 前端 SPA │
                         │                              │
                         │  auth   挑战响应 + 入站签名   │
                         │  paste  pastebin 模块        │
                         │  rss    抓取→清洗→本地化→入库  │
                         │  cron   清理任务调度          │
                         │  hub    接收 Homehub 推送     │
                         └───────┬──────────────┬───────┘
                                 │              │
                    ┌────────────▼───┐   ┌──────▼─────────────┐
                    │ SQLite (WAL)   │   │ blobs/ 内容寻址文件  │
                    │ 元数据 + 索引   │   │ sha256 分片目录      │
                    └────────────────┘   └────────────────────┘
                                 │
                    ┌────────────▼───────────────────────┐
                    │ Homehub 宿主 HTTP API（外部）        │
                    │ 限流 · 超时 · 缓存 · singleflight    │
                    └────────────────────────────────────┘
```

设计要点：

- **单二进制**：前端 `pnpm build` 产物通过 `//go:embed all:web/dist` 打进程序。仓库里只提交 `web/dist/.gitkeep`，让目录存在且非空，保证全新 clone 直接 `go build` 成功。
- **SQLite 用 `modernc.org/sqlite`**（纯 Go、无 CGO），`WAL` + 单写连接，`_pragma` 在 DSN 里固定，避免 `SQLITE_BUSY`。
- **内容寻址存储**：媒体文件按 `sha256` 存 `blobs/<前2位>/<hash>.<ext>`，同一图片被多篇文章引用只占一份空间；DB 里用引用计数决定何时回收。
- **双向公私钥，两个方向各管一件事**：
  - 浏览器 → 服务端：Ed25519（或 P-256）挑战-响应。私钥以不可导出方式生成并存在 IndexedDB，服务端只存公钥。
  - Homehub → 服务端：每个入站请求自带签名，覆盖 method/path/query/body 摘要/时间戳/nonce。Extension 只存宿主公钥，一条合法请求无法被挪到别的端点重放。
  详见 [docs/api.md](docs/api.md)。
- **清理策略只有一处定义**：所有保留期、GC 都在 `config.json` 的 `cleanup.jobs` 里，用 cron 表达式描述，运行时可在「系统 → 任务」页面看到每次执行结果。详见 [docs/config.md](docs/config.md)。

## 技术栈

Go module 路径：`github.com/ignvex/HomehubExtension`（与仓库地址一致，保留大小写）。

- 后端：Go 1.27、gin（`gin.New()`，非 `Default()`）、`log/slog`、`modernc.org/sqlite`、`robfig/cron/v3`、`mmcdole/gofeed`、`golang.org/x/net/html`
- 前端：TypeScript（`strict`）、Vite、原生 DOM + 轻量路由、手写 CSS3（无 UI 框架）、pnpm
- 部署：Caddy（反代 + 证书）、systemd、GitHub Actions 交叉编译 `linux/amd64`

## 目录结构

```text
HomehubExtension/
├── main.go                  # 入口：子命令、日志、信号处理
├── enroll.go                # enroll 子命令的最小装配
├── assets.go                # go:embed all:web/dist
├── Makefile
├── Dockerfile               # 多阶段：前端 → Go → alpine 运行镜像
├── go.mod
├── config.example.json      # 配置模板（字段与 docs/config.md 一致）
├── internal/
│   ├── app/                 # 依赖装配、生命周期、清理任务实现
│   ├── config/              # 加载、${VAR} 展开、聚合校验
│   ├── httpapi/             # gin 路由、中间件、handler、SPA fallback、SSE
│   ├── auth/                # 挑战响应、会话、入站请求签名
│   ├── store/               # 仓储接口与值对象
│   │   └── sqlite/          # SQLite 实现 + embed 迁移
│   ├── blob/                # 内容寻址文件存储（流式写入）
│   ├── paste/               # pastebin 领域逻辑
│   ├── rss/                 # 清洗、抓取、媒体本地化、失败重试
│   ├── scheduler/           # cron 调度与任务执行记录
│   ├── systemd/             # sd_notify：READY / WATCHDOG / STOPPING
│   └── events/              # 进程内事件总线（SSE 广播）
├── web/                     # 前端工程（pnpm + Vite + TS）
│   ├── src/tools/           # 15 个开发工具，每个一个模块，registry 懒加载
│   └── dist/.gitkeep        # 让 go:embed 在全新 clone 上也能编译（目录不得为空）
├── docs/                    # 设计文档
├── deploy/                  # systemd unit/timer、Caddyfile、env 示例、备份脚本
└── .github/workflows/ci.yml
```

## 快速开始（开发）

前置：Go ≥ 1.27、Node ≥ 22、pnpm ≥ 10。

```bash
git clone https://github.com/ignvex/HomehubExtension.git
cd HomehubExtension

cp config.example.json config.json
# 至少改 server.public_url、database.path、storage.blob_dir
# 密钥用 ${VAR} 引用，写在环境变量或 .env 里，不要进 config.json

go run . serve -config config.json    # 后端 :8080
pnpm --dir web install
pnpm --dir web dev                    # 前端 :5173，/api 与 /media 代理到 :8080
```

`serve` 启动时会自动执行迁移。要把「改库结构」从「重启服务」里拆出来，就单独跑一次——失败时停在启动前，不必等 `Type=notify` 超时：

```bash
go run . migrate -config config.json   # 只应用待执行的迁移，打印 schema_version 后退出
```

首次使用需要在服务端生成配对码，再在浏览器 `/pair` 页面注册设备公钥：

```bash
go run . enroll -config config.json -name "macbook-chrome"
```

构建发布产物：

```bash
make build     # 等价的裸命令：
pnpm --dir web build
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/homehub-extension .
```

产物落在 `dist/`，**不压缩、不入库**（`.gitignore` 忽略 `/dist/`）。与前端 `web/dist` 是两回事：前者是待分发的二进制暂存区，后者是 `go:embed` 的输入。

## 测试

```bash
make lint    # 规范检查：gofmt、vet、deadcode、行宽、函数长度、import 分组
make test    # go test ./...
make race    # 并发路径（一次性读取、SSRF 开关、调度器互斥、轮询器）
make units   # systemd 单元语法 + 加固评分
make build   # 前端构建 + 交叉编译 linux/amd64 → dist/
```

`make lint` 落实的是项目采用的 Go 编码规范（阿里规范 + Uber Style Guide 常见实践）。几项阈值写成了脚本，CI 里同样跑：

- 单行 ≤ 120 字符，单文件 ≤ 500 行
- 单函数 ≤ 80 行，参数 ≤ 5 个（超出用 struct 收口）
- import 分三组：标准库 → 内部包 → 第三方库
- `deadcode` 零输出——不能有写了却没人调用的代码

CI 与本地一致：`.github/workflows/ci.yml` 分四个 job（Go、前端、交叉编译、镜像）。

编译 job 必须等前端产物就位——`go:embed` 在编译期读取 `web/dist`，顺序错了会产出一个只有 `.gitkeep` 的二进制：体积正常、能启动，但访问任何页面只看到一张「前端未构建」提示页。CI 里用 `grep -qa "_app/"` 断言前端确实嵌进去了。

测试覆盖的重点不是行数，而是几条容易出错的语义：

- 一次性片段在 8 个并发读取下只有 1 个成功
- 签名不能被搬到另一个路径或另一个 body 上使用
- 同一 nonce 重放必须被拒
- 外键级联删除后 `blob.ref_count` 必须归零
- 清洗后的 HTML 里除 `<a href>` 外没有任何绝对外链
- API 的 404 必须是 JSON，不能被 SPA fallback 吞掉

## 鉴权边界

不是所有页面都要求登录：

| 页面 / 端点 | 鉴权 | 理由 |
| --- | --- | --- |
| 工具台 `/`、`/tools/*`、`/apps`、`/system/info`、`/system/stats` | 无 | 入口页；只返回聚合计数，不含用户内容；工具全在浏览器本地算 |
| 片段 `/pastes/*`、`/p/*` | 可选 | 匿名可创建与管理自己的片段 |
| RSS 阅读 `/entries/*` | 必须 | 含抓取的全文内容 |
| 订阅源 `/feeds/*` | 必须 | 抓取会访问外部地址，匿名开放等于开放 SSRF |
| 设置 `/settings`、`/system/jobs`、`/system/config` | 必须 | 运维信息 |

匿名创建片段后，响应里的 `owner_token` 是管理凭证：只出现一次，服务端只存哈希。存进浏览器 `localStorage`，之后的所有请求带 `X-Owner-Token`。**丢了就找不回**——这是不设账号的固有代价。

## 与 Homehub 宿主对接

宿主推送订阅，Extension 负责抓取与本地化。请求必须签名：

```bash
# 签名消息（逐字节，\n 是真实换行）
homehubext/req/v1
<METHOD>
<PATH>
<QUERY>
<hex(sha256(body))>
<timestamp>
<nonce>
```

请求头带 `X-HH-Timestamp`、`X-HH-Nonce`、`X-HH-Signature`（base64）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/hub/subscriptions` | 推送订阅：`{url, title, interval, enabled, fetch_now}`。同 URL 幂等更新 |
| `GET` | `/api/v1/hub/subscriptions` | 对账列表 |
| `DELETE` | `/api/v1/hub/subscriptions/{id}` | 移除订阅 |
| `POST` | `/api/v1/hub/subscriptions/{id}/refresh` | 立即抓取 |
| `POST` | `/api/v1/hub/keys` | 签发访问密钥：`{name, expires_in}`。明文只返回一次 |
| `GET` | `/api/v1/hub/keys` | 密钥对账，不含明文 |
| `DELETE` | `/api/v1/hub/keys/{id}` | 撤销密钥，连带踢掉它换出的会话 |

`interval` 会被夹到 `rss.min_interval` 与 `rss.max_interval` 之间。首推默认立即抓一次。

推送是幂等的，且**省略就是保留**：不带 `interval` / `enabled` 时沿用已存的全局值，不是回落默认。否则一次「只改标题」的推送会把用户显式暂停的订阅重新启用。显式把 `enabled` 从 `false` 改回 `true` 会同时清零 `error_count`——否则一个被自动停用的源恢复后，下一次失败就重新越线。

### 访问密钥：宿主发凭证，用户自己登录

订阅通道之外，宿主还能给用户签发一把**长期登录凭证**，省掉「SSH 上服务器跑 enroll」这一步：

```bash
curl -X POST https://homehub.example.com/api/v1/hub/keys \
  -H 'X-HH-Timestamp: …' -H 'X-HH-Nonce: …' -H 'X-HH-Signature: …' \
  -d '{"name":"alice","expires_in":"720h"}'
# → {"id":"01JC…","key":"HHE-7QK4-2M9X-P3TB-8HVN-5RCW","expires_at":1795843200,…}
```

用户拿这串 `HHE-…` 到 `/pair` 页面输入即可进入，不需要本机私钥。它和配对码的区别：

| | 配对码 `HH-…` | 访问密钥 `HHE-…` |
| --- | --- | --- |
| 谁生成 | 服务端 `enroll` 命令 | 宿主签名调用 `/api/v1/hub/keys` |
| 有效期 | 15 分钟 | 由 `expires_in` 决定，可永久 |
| 可重复使用 | 否，一次即废 | 是，换浏览器换设备都用它 |
| 登录后本机存什么 | 不可导出的设备私钥 | 什么都不存，每次输入同一串 |

密钥是长期共享凭证，泄露等于账号泄露，所以要当密码对待。签发时 Extension 会为该密钥建一台「无主客户端」（公钥入库、私钥当场丢弃），所以它只能靠密钥兑换，无法走挑战-响应；`DELETE /api/v1/hub/keys/{id}` 会连带撤销这台客户端和它的全部会话。

**授权在宿主侧，Extension 不判断谁能拿密钥。** 签名通过就无条件下发——Extension 里没有用户表，也无从知道某人在宿主里是否还在职。代价是宿主私钥泄露即可批量签发永久密钥，所以它的保护等级等同于本实例。

**轮换是安全的**：签发新密钥 → 确认能登录 → 撤销旧的。所有能登录的凭证（配对设备 + 访问密钥）共享同一个实例级 `owner_id`，所以新密钥读得到旧密钥名下的全部私有片段，配对设备也看得到密钥建的内容；撤销旧密钥只让它自己的会话失效。匿名片段不受影响，仍然按 owner token 隔离。

细节见 [docs/api.md](docs/api.md#210-访问密钥宿主签发用户直接登录)。

## 配置

`config.json` 通过 `-config` 指定（systemd 用绝对路径），也可以用 `HOMEHUB_CONFIG` 环境变量。所有字段、默认值与清理任务语法见 [docs/config.md](docs/config.md)，模板见 [`config.example.json`](config.example.json)。

几条硬规则：

- 找不到配置文件时**直接退出**，不回落到内置默认值。
- 支持 `${VAR}` 与 `${VAR:-default}` 展开，密钥只出现在环境变量里。
- 校验错误一次性全部列出，而不是撞到第一个就退。

## 部署

`make build` 会把二进制、安装脚本、systemd 单元与配置模板汇总到 `dist/`。
整个目录拷到服务器后，一条命令完成安装：

```bash
make build                      # 产出 dist/（不入库）
scp -r dist user@host:/tmp/hhe  # 整个目录拷过去

# 服务器上
cd /tmp/hhe && sudo ./install.sh
```

脚本会把二进制装到 `/usr/bin`、建 `/etc/hhe` 放配置、装 systemd 单元，
然后**询问是否开机自启、是否立即启动**。已有 `/etc/hhe/config.json` 会原样保留，
所以重复执行等于升级。`--yes` 全部取默认值（是），`--dry-run` 只打印动作。

目录布局：

```text
/usr/bin/homehub-extension               二进制（dist/ 构建后拷入）
/usr/bin/homehub-backup.sh                备份脚本（deploy/backup.sh）
/etc/hhe/config.json                      配置（0640 root:homehub）
/etc/hhe/env                              密钥（0600 root:homehub）
/var/lib/hhe/homehub.db                   SQLite（由 StateDirectory 创建）
/var/lib/hhe/blobs/                       媒体文件
/var/backups/hhe/                         每日备份，保留 7 份
```

Caddy 关键片段（完整版见 [`deploy/Caddyfile`](deploy/Caddyfile)）：

```caddy
homehub.example.com {
    encode zstd gzip

    # 关掉反代响应缓冲。SSE 必须，否则事件会被攒成一批发。
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }

    request_body {
        max_size 2MB
    }
}
```

`flush_interval -1` 全局开启而不是只对事件流生效：代价只是普通响应少一层攒批，换来的是不会因为路径写错而只在 SSE 上出问题。

systemd 部署的三个要点（守护进程名 `hhe`，完整单元见 [`deploy/hhe.service`](deploy/hhe.service)，细节见 [docs/config.md](docs/config.md#5-systemd-集成)）：

- **`Type=notify`**：进程在端口真正 bind 之后才发 `READY=1`，`systemctl start` 返回时服务已经可用，依赖它的 unit 和部署脚本都不必再轮询 `/healthz`。`WatchdogSec=60s` 配合每 30 秒一次的心跳，能发现「进程还在、端口还占着，但已经不干活」的假死。
- **`StateDirectory=hhe`**：`/var/lib/hhe` 由 systemd 创建并 chown，同时自动放行给 `ProtectSystem=strict`，安装时不需要手工 `mkdir`。
- **没有 `ExecReload`**：进程只处理 `SIGINT`/`SIGTERM`，SIGHUP 会直接终止它。改配置一律 `systemctl restart`——写一条 `ExecReload=/bin/kill -HUP $MAINPID` 得到的是「reload 把服务杀掉」，而不是热重载。

`GOMEMLIMIT=256MiB` 与 `MemoryMax=512M` 成对生效：前者让 Go 的 GC 提前压低堆，后者是 cgroup 硬墙。只配后者的话，触墙就是 OOM kill，没有减震。

加固程度可量化：`systemd-analyze security --offline=yes deploy/hhe.service` 当前评分为 **1.5 OK**（首版是 6.3 MEDIUM）。

备份（数据库走二进制的 `backup` 子命令，媒体用 rsync 硬链接增量，不依赖 sqlite3 CLI）：

```bash
sudo install -m 0755 deploy/backup.sh /usr/bin/homehub-backup.sh
sudo install -m 0644 deploy/hhe-backup.service deploy/hhe-backup.timer /etc/systemd/system/
sudo install -d -o homehub -g homehub -m 0750 /var/backups/hhe
sudo systemctl daemon-reload && sudo systemctl enable --now hhe-backup.timer
systemctl list-timers hhe-backup.timer
```

调度交给 systemd timer 而不是 cron：`Persistent=true` 会补跑关机期间错过的触发点，日志直接进 journal（`journalctl -u hhe-backup`），`RandomizedDelaySec` 负责错峰。

容器部署见 [`Dockerfile`](Dockerfile)。容器内需要监听 `0.0.0.0`，所以配置里要设 `server.allow_remote_listen: true`。

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | 运行拓扑、组件职责、数据流、存储布局、资源预算、技术选型理由 |
| [docs/config.md](docs/config.md) | 配置加载、全字段说明、cleanup cron 设计、systemd 集成 |
| [docs/api.md](docs/api.md) | REST 约定、Ed25519 双向鉴权协议、端点清单、错误与分页 |
| [docs/hub-integration.md](docs/hub-integration.md) | **指向 Homehub 仓库的指针**：`/api/v1/hub/*` 的协议唯一事实源在宿主侧 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更记录；本版发布说明见 [docs/release-notes/v1.0.0-alpha.md](docs/release-notes/v1.0.0-alpha.md) |
| [docs/rss.md](docs/rss.md) | 抓取与条件请求、HTML 白名单清洗、媒体下载与重写、SSRF 防护、CSP |
| [docs/frontend.md](docs/frontend.md) | pnpm 工程、路由与视图、CSS 分层、阅读器排版、构建与 embed |

## 路线图

- [x] **M0** 设计定稿：README + `docs/`
- [x] **M1** 后端骨架：config、slog、gin 中间件、SQLite 迁移、`/healthz`、`/readyz`、优雅关闭
- [x] **M2** 前端骨架：Vite + TS + CSS token 体系 + 应用外壳 + 开发代理 + 工具台首页
- [x] **M3** 交付链：embed、SPA fallback、CSP、systemd + Caddyfile
- [x] **M4** 鉴权闭环：密钥生成、设备配对、挑战登录、会话、CSRF、入站签名
- [x] **M5** Pastebin 端到端（含无 JS 公开页）
- [x] **M6** RSS：抓取 → 清洗 → 媒体本地化 → 重试 → 阅读视图
- [x] **M7** 清理调度 + 运维接口 + 测试
- [x] **M8** 首发硬化：GitHub Actions CI、Dockerfile、备份子命令
- [x] **M9** 开发工具台：15 个纯前端工具，按路由懒加载，首屏只下载元数据
- [x] **M10** OPML 导入导出：`POST /api/v1/feeds/import`、`GET /api/v1/feeds/export`，分组嵌套按递归收集

## 许可

尚未选择许可证。在 LICENSE 落地前，默认保留所有权利。
