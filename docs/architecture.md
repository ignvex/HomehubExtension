# 架构设计

本文档定义 HomehubExtension 的运行结构、组件边界、数据流与资源预算。所有后续实现都应能在本文找到依据；与本文冲突的实现需要先改文档。

## 1. 目标与非目标

**目标**

- 在 1 vCPU / 2 GB / 20 GB 的单机（amd64）上稳定运行，常驻内存 < 150 MB。
- 单二进制交付：前端产物嵌入可执行文件，部署不依赖 Node、不依赖系统 SQLite。
- 两个功能域：Pastebin、RSS 阅读器（含媒体本地化）。
- 所有出站请求可控、可审计、可限速——本服务是家庭网络里唯一会主动访问公网的部分之一。

**非目标**

- 不是 Homehub 的替代品，而是它的扩展；不直接操作设备，只通过宿主 HTTP API。
- 不做多租户、不做用户注册体系；鉴权以「设备」为单位。
- 不做分布式：单实例、单写连接、无跨节点同步。
- 不在服务端做重型内容处理（视频转码、图片缩放、全文索引引擎）。

## 2. 运行拓扑

```text
公网 :443 ──► Caddy ──► 127.0.0.1:8080 ──► HomehubExtension
                                              │
                                              ├─► /var/lib/homehub-extension/homehub.db
                                              ├─► /var/lib/homehub-extension/blobs/
                                              └─► Homehub 宿主 API（局域网或本机）
```

边界规则：

- Go 进程**只绑定回环地址**，不处理 TLS，不读取客户端证书。
- Caddy 负责 TLS、压缩、安全响应头、可选的访问日志与 IP 限流。
- `SetTrustedProxies` 只信任回环，`ClientIP()` 取 `X-Forwarded-For` 的最右非可信地址。
- SSE 路径必须在 Caddy 配置里关闭缓冲（`flush_interval -1`），并且 Go 侧 `WriteTimeout = 0`。

## 3. 进程内组件

依赖方向自上而下，不允许反向引用：

```text
                    ┌──────────┐
                    │  main.go │  解析子命令与 -config
                    └────┬─────┘
                    ┌────▼─────┐
                    │  app     │  装配、启动顺序、优雅关闭
                    └────┬─────┘
        ┌────────────────┼─────────────────┐
   ┌────▼────┐      ┌────▼────┐       ┌────▼─────┐
   │ httpapi │      │scheduler│       │  rss     │
   │  gin    │      │  cron   │       │ service  │
   └────┬────┘      └────┬────┘       └────┬─────┘
        │                │                 │
   ┌────▼────────────────▼─────────────────▼────┐
   │  auth · paste · blob · hub                 │  领域服务
   └────────────────────┬───────────────────────┘
                        │
              ┌─────────▼──────────┐
              │ store (interface)  │
              └─────────┬──────────┘
                        │
              ┌─────────▼──────────┐
              │ store/sqlite       │
              └────────────────────┘
```

| 包 | 职责 | 明确不做的事 |
| --- | --- | --- |
| `app` | 构造依赖、启动顺序、信号处理、关停 | 业务规则 |
| `config` | 解析、`${VAR}` 展开、聚合校验 | 读环境变量以外的 I/O |
| `httpapi` | 路由、中间件、DTO 转换、错误映射 | 直接写 SQL |
| `auth` | 密钥、挑战、会话、CSRF、中间件 | 授权策略以外的业务判断 |
| `paste` | 片段生命周期、slug、过期、一次性读取 | HTML 渲染 |
| `rss` | 抓取、解析、清洗、媒体本地化、去重 | HTTP 暴露 |
| `blob` | 内容寻址写入、读取、引用回收 | 判断内容是否该保存 |
| `scheduler` | cron 解析、任务注册、单实例执行、结果记录 | 具体清理逻辑 |
| `store` | 仓储接口与事务边界 | 生成 ID、校验业务规则 |
| `hub` | Homehub 宿主 API 客户端 | 缓存策略以外的业务 |

领域包之间只通过接口和值对象交互。`rss` 需要落库时依赖 `store` 接口，不依赖 `store/sqlite` 具体类型。

## 4. 请求路径

### 4.1 读一条已本地化的文章

```text
GET /api/v1/entries/42
  → auth 中间件：校验 cookie 或 Bearer → 注入 client_id
  → handler：store.Entry.Get(42)
  → 返回 JSON，其中 content_html 里的资源地址已被改写为 /media/<sha256>.<ext>

GET /media/3f9a…c1.webp
  → ETag 命中则 304
  → blob.Open(sha256)：先查 blob 表，再按分片路径打开文件
  → 返回 Cache-Control: public, max-age=31536000, immutable
```

全程没有第三方域名，浏览器不会向原站发请求。

### 4.2 抓取一轮订阅

```text
scheduler tick（每 30s 扫描到期 feed，带 ±10% 抖动）
  → feed.next_fetch_at <= now 且 enabled
  → hub/rss fetcher：GET url
      If-None-Match: etag, If-Modified-Since: last_modified
      304 → 更新 next_fetch_at，结束
  → 200 → 限额读取（max_feed_bytes）→ gofeed 解析
  → 对每个新 guid（UNIQUE(feed_id, guid) 去重）：
      sanitize：白名单清洗，剥离脚本/iframe/远程样式
      collect：抽取 img/srcset/source/video/audio/poster 的绝对 URL
      download：并发 ≤ media_concurrency，流式写入临时文件
                校验 Content-Type、大小上限、SSRF 检查
                成功 → blob.Put（sha256 命名，去重）→ 记录 entry_blob
                失败 → 记 media_fetch(status=failed)，HTML 中改为占位
      rewrite：把可本地化的 URL 换成 /media/…，其余按策略移除
  → 单事务写入 entry + entry_blob + feed.last_fetch_at
  → 广播 SSE created 事件
```

关键点：**媒体下载在写库之前完成**。这样落库的 `content_html` 一定是最终形态，读者打开时不会触发懒加载式的补抓，也不会出现"先显示原图地址、几秒后变成占位"的闪烁。

代价是单个 feed 的首抓可能耗时较长。因此抓取任务整体有超时（默认 90s），超时的条目按已下载部分入库，未完成部分记为 `pending`，由下一次 `media-retry` 任务补。

### 4.3 Paste 创建

```text
POST /api/v1/pastes
  → 校验 body 大小、语言白名单、过期时间上下限
  → 生成 slug（base62，6 字符，冲突重试 5 次）
  → 可选 argon2id 哈希密码
  → 写入 paste 表（deleted_at 为空）
  → 201 + Location: /p/{slug}
```

一次性读取（`burn_after_read`）的语义：**首次成功返回内容时在同一事务里写入 `deleted_at`**，之后的请求一律 410。不做"读后延迟删除"，避免并发窗口。

## 5. 存储布局

```text
/var/lib/homehub-extension/
├── homehub.db
├── homehub.db-wal
├── homehub.db-shm
├── blobs/
│   ├── 3f/
│   │   └── 3f9a…c1.webp
│   └── …
└── tmp/                 # 下载中的临时文件，启动时清空
```

- 分片目录取 sha256 前两位十六进制，最多 256 个目录，避免单目录文件过多。
- 临时文件与最终文件**必须同一文件系统**，保证 `rename` 原子替换。
- `tmp/` 在启动时无条件清空——崩溃残留的下载文件不应占用配额。
- 数据库与 blob 分开备份：数据库用 `VACUUM INTO`，blob 目录用 `rsync --link-dest` 增量。

### 5.1 SQLite 打开参数

```text
file:/var/lib/homehub-extension/homehub.db
  ?_pragma=journal_mode(WAL)
  &_pragma=synchronous(NORMAL)
  &_pragma=busy_timeout(5000)
  &_pragma=foreign_keys(1)
  &_pragma=temp_store(MEMORY)
  &_pragma=cache_size(-16000)
  &_pragma=auto_vacuum(INCREMENTAL)
```

- `foreign_keys` 是**每连接**生效，不是持久化设置；漏掉会导致级联删除静默失效。
- `auto_vacuum=INCREMENTAL` 只在建库时生效，后续无法直接改；迁移里需要 `VACUUM` 重建。
- 写连接池 `MaxOpenConns(1)`；读连接池 `MaxOpenConns(3)`，DSN 加 `mode=ro&_pragma=query_only(1)`。
- 每连接 `busy_timeout` 是兜底，不是并发策略——真正避免 `SQLITE_BUSY` 的是单写连接。

### 5.2 表结构草案

```sql
CREATE TABLE paste (
  id            TEXT PRIMARY KEY,          -- ULID
  slug          TEXT NOT NULL UNIQUE,
  title         TEXT NOT NULL DEFAULT '',
  body          TEXT NOT NULL,
  language      TEXT NOT NULL DEFAULT 'text',
  visibility    TEXT NOT NULL CHECK (visibility IN ('public','unlisted','private')),
  password_hash TEXT,
  expires_at    INTEGER,
  burn_after_read INTEGER NOT NULL DEFAULT 0 CHECK (burn_after_read IN (0,1)),
  size_bytes    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  deleted_at    INTEGER
) STRICT;

CREATE TABLE feed (
  id INTEGER PRIMARY KEY,
  url TEXT NOT NULL UNIQUE,
  site_url TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  fetch_interval INTEGER NOT NULL,         -- 秒
  etag TEXT, last_modified TEXT,
  last_fetch_at INTEGER, next_fetch_at INTEGER,
  last_status INTEGER, last_error TEXT,
  error_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE entry (
  id INTEGER PRIMARY KEY,
  feed_id INTEGER NOT NULL REFERENCES feed(id) ON DELETE CASCADE,
  guid TEXT NOT NULL,
  url TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  author TEXT NOT NULL DEFAULT '',
  published_at INTEGER,
  fetched_at INTEGER NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  content_html TEXT NOT NULL DEFAULT '',   -- 已本地化的最终形态
  content_text TEXT NOT NULL DEFAULT '',   -- 纯文本，供检索
  read_at INTEGER, starred_at INTEGER,
  UNIQUE (feed_id, guid)
) STRICT;

CREATE TABLE blob (
  sha256 TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  mime TEXT NOT NULL,
  ext TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL CHECK (kind IN ('image','audio','video','font','attachment','other')),
  ref_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
) STRICT;

CREATE TABLE entry_blob (
  entry_id INTEGER NOT NULL REFERENCES entry(id) ON DELETE CASCADE,
  sha256 TEXT NOT NULL REFERENCES blob(sha256),
  PRIMARY KEY (entry_id, sha256)
) STRICT, WITHOUT ROWID;

CREATE TABLE media_fetch (
  id INTEGER PRIMARY KEY,
  entry_id INTEGER NOT NULL REFERENCES entry(id) ON DELETE CASCADE,
  source_url TEXT NOT NULL,
  sha256 TEXT,
  status TEXT NOT NULL CHECK (status IN
    ('pending','ok','failed','too_large','blocked','unsupported','quota_exceeded')),
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_media_fetch_pending ON media_fetch(status, updated_at)
  WHERE status IN ('pending','failed');

CREATE TABLE auth_client (
  id TEXT PRIMARY KEY,           -- ULID
  name TEXT NOT NULL,
  public_key BLOB NOT NULL,      -- Ed25519 32 字节
  algorithm TEXT NOT NULL DEFAULT 'ed25519',
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER,
  revoked_at INTEGER
) STRICT;

CREATE TABLE auth_challenge (
  nonce TEXT PRIMARY KEY,
  client_id TEXT NOT NULL REFERENCES auth_client(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  consumed_at INTEGER
) STRICT;

CREATE TABLE session (
  token_hash BLOB PRIMARY KEY,   -- sha256(token)
  client_id TEXT NOT NULL REFERENCES auth_client(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER
) STRICT;

CREATE TABLE enrollment (
  token_hash BLOB PRIMARY KEY,
  name_hint TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER
) STRICT;

CREATE TABLE job_run (
  id INTEGER PRIMARY KEY,
  job TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  status TEXT NOT NULL,          -- running|ok|failed|timeout|skipped
  detail TEXT NOT NULL DEFAULT ''
) STRICT;
```

引用计数由触发器维护，但**只有 INSERT 触发器负责加，删除路径全部走显式 SQL**：

```sql
CREATE TRIGGER entry_blob_ins AFTER INSERT ON entry_blob BEGIN
  UPDATE blob SET ref_count = ref_count + 1 WHERE sha256 = NEW.sha256;
END;

CREATE TRIGGER entry_blob_del AFTER DELETE ON entry_blob BEGIN
  UPDATE blob SET ref_count = MAX(0, ref_count - 1) WHERE sha256 = OLD.sha256;
END;
```

**为什么不能只靠 `ON DELETE CASCADE`**：SQLite 的外键动作不会激活触发器（除非打开 `recursive_triggers`）。也就是说，删掉 `entry` 时 `entry_blob` 行会被级联删除，但 `AFTER DELETE` 触发器**不执行**，`ref_count` 只增不减，blob 永远回收不掉。

因此删除路径一律显式两步：

```sql
DELETE FROM entry_blob WHERE entry_id IN (...);
DELETE FROM entry WHERE id IN (...);
```

`Feed.Delete` 与 `Entry.Purge` 都按这个顺序写，并且 `media_fetch` 必须在 `entry` 之前删——删掉 entry 后再按 `feed_id` 子查询就取不到 id 了。

`MAX(0, …)` 是兜底：即使某条路径漏了，计数也不会漂到负数，而负数的 `ref_count` 永远不会被 GC 回收。`blob-gc` 每天还会用 `entry_blob` 的真实行数全量校准一次，让任何遗漏在一天内自愈。

`blob.ref_count` 只是 GC 的快速判据。真正的回收还要交叉比对文件系统：数据库里 `ref_count > 0` 但文件缺失 → 内容损坏，记录告警并把引用降级为占位；文件存在但 `ref_count = 0` 且超过 grace → 删除文件与行。

### 5.3 迁移

- `internal/store/migrations/NNNN_name.sql`，`go:embed` 打进二进制。
- 版本号存 `PRAGMA user_version`，只前进不回退。
- 每个文件在**独立事务**内执行，失败则整库回滚并拒绝启动。
- 迁移脚本禁止 `PRAGMA` 之外的多语句事务控制（驱动会自行包裹）。
- 启动日志打印 `schema_version`，`/readyz` 暴露该值。

### 5.4 时间表示

数据库、API、前端三层用同一个单位：**Unix 秒**。

```text
SQLite INTEGER  →  Go int64  →  JSON number  →  JS number
                                                    ↓
                                          parseTime(): sec * 1000
```

只在最末一步转换。这样做的代价是前端必须记得乘 1000；收益是中间三层零转换、零格式歧义。把 `* 1000` 收进单个函数（`web/src/lib/format.ts` 的 `parseTime`）后，代价被压到一处，且可被测试覆盖。

零值约定：Go 的 `int64` 零值是 `0`，而 `0` 在 Unix 时间里是 `1970-01-01`。因此 `httpapi.util.ts()` 对 `0` 返回 `nil`，JSON 里是 `null`。反过来说，任何出现 `0` 的时间字段都是数据异常，前端 `parseTime` 会把它当作空值。

时长（`"30m"`）与时刻（`1793286035`）不混用。前者是人要读的配置量，后者是绝对时间点。

## 6. 并发模型

| 场景 | 策略 |
| --- | --- |
| HTTP 请求 | gin 默认 goroutine/请求，全局并发由 `http.Server` 与 Caddy 限制 |
| 写数据库 | 单连接串行，事务短小；批量操作每 500 行提交一次 |
| 读数据库 | 3 个只读连接，WAL 下不阻塞写 |
| feed 抓取 | 独立轮询循环，30 秒一扫，信号量 `fetch_concurrency`（默认 2）；上一轮未完则跳过 |
| 媒体下载 | 全局信号量 `media_concurrency`（默认 3），单条目的下载顺序受限 |
| 同一 feed 并发抓取 | 进程内 `inflight` 集合去重；抓取幂等，重复只是多一次请求 |
| 同一 blob 并发写入 | `singleflight` + 临时文件，先完成者 rename，后到者丢弃 |
| 清理任务 | cron `SkipIfStillRunning`；任务内部再按批处理 |
| SSE 广播 | 非阻塞发送到带缓冲 channel，满则丢弃并记数，绝不阻塞生产者 |

**内存安全阀**：所有下载都走 `io.LimitReader` + 流式落盘，任何路径都不允许把完整媒体读进内存。单条目 HTML 有 `max_entry_bytes`，feed 有 `max_feed_bytes`。

## 7. 资源预算

常驻内存：

| 项 | 预估 |
| --- | --- |
| Go 运行时 + 二进制代码页 | 25–40 MB |
| gin + 路由 + 中间件 | 5 MB |
| SQLite 页缓存（16 MB 上限） | 10–20 MB |
| 运行期对象与缓冲 | 20 MB |
| **合计** | **60–85 MB**（`GOMEMLIMIT=256MiB` 作为上限保护） |

峰值（同时 3 个媒体下载 + 1 个 feed 解析）：< 150 MB，因为媒体全程流式。

CPU：

- 空载 < 1%。
- 抓取一个含 20 张图的条目：约 1–3 s CPU（主要是 HTML 解析与哈希计算），I/O 主导。
- 每日清理任务集中在凌晨，单核占用可控（分批 + `time.Sleep` 让出）。

磁盘：

| 项 | 预估 |
| --- | --- |
| 二进制 | 25–35 MB（modernc sqlite 约 10 MB） |
| 数据库（3 个月、50 个 feed） | 100–500 MB |
| blobs（配额 5 GB） | ≤ 5 GB |
| 备份（7 份，增量） | 1–2 GB |
| 系统 + 日志 | 2 GB |
| **合计** | **< 9 GB / 20 GB** |

配额到顶时：拒绝新媒体下载（`media_fetch.status = too_large` 语义改为 `quota_exceeded`），保留正文与已下载资源，页面显示配额告警。绝不因为配额而删已有内容。

## 8. 启动与关闭

启动顺序（任一步失败即退出，退出码非 0）：

1. 解析子命令与 `-config`
2. 加载配置、展开 `${VAR}`、聚合校验
3. 初始化 slog（此时才有结构化日志可用）
4. 打开 SQLite，检查 `quick_check`，执行迁移
5. 打开 blob 目录，校验可写，清空 `tmp/`
6. 加载或生成服务端 Ed25519 密钥（`server_key_path`，权限 0600）
7. 构造 store / 领域服务 / handler
8. 构建 gin engine 与中间件
9. 启动 scheduler（先不触发任务）
10. 启动 HTTP server，打印监听地址与版本
11. 触发一次「启动补跑」：错过的清理任务与到期 feed

关闭顺序（SIGINT/SIGTERM，总预算 20 s）：

1. 取消 `baseCtx` → SSE handler 主动返回，连接立刻释放
2. `Shutdown` 停止接受新连接并等待在途请求（15 s 上限）
3. 停止 scheduler，等待运行中的任务（超时 10 s 后放弃并记录）
4. 关闭 SQLite（确保 WAL checkpoint）
5. 退出

顺序不能反：如果先 `Shutdown` 再取消 SSE，`Shutdown` 会一直挂着等那些长连接，直到超时。

## 9. 故障与恢复

| 故障 | 表现 | 处置 |
| --- | --- | --- |
| SQLite 损坏 | 启动 `quick_check` 失败 | 拒绝启动，日志给出备份恢复指引，不自动重建 |
| WAL 异常膨胀 | `-wal` 超过 512 MB | 日志告警，`/readyz` 降级，下一次 checkpoint 强制 `TRUNCATE` |
| 磁盘不足 | 可用空间 < 1 GB | 拒绝新媒体下载，保留数据库写入，系统页显示告警 |
| feed 连续失败 | `error_count` 递增 | 指数退避至 `max_interval`；连续 10 次标记 `enabled=0` 并在 UI 提示 |
| 宿主不可达 | `/readyz` 返回 503 | 已缓存内容照常可读；写操作快速失败并返回 502 |
| 进程被 OOM kill | systemd 重启 | `MemoryMax=512M` 让内核优先杀本进程而不是宿主；重启后清 `tmp/` |
| 时间跳变 | cron 误触发 | 任务自身幂等；`job_run` 记录时间戳便于排查 |

## 10. 技术选型

| 需求 | 选择 | 理由 | 放弃的备选 |
| --- | --- | --- | --- |
| HTTP 框架 | gin | 参数绑定与校验、路由组、SSE 支持成熟 | `net/http.ServeMux`：够用但绑定代码要手写 |
| SQLite 驱动 | `modernc.org/sqlite` | 纯 Go，`GOOS/GOARCH` 交叉编译零配置 | `mattn/go-sqlite3`：需要 CGO 与交叉编译工具链 |
| Feed 解析 | `mmcdole/gofeed` | RSS 0.9x/1.0/2.0、Atom、JSON Feed 统一模型 | 手写 `encoding/xml`：边界情况太多 |
| HTML 解析 | `golang.org/x/net/html` | 标准、容错、可遍历 | `goquery`：只是它的封装，多一层依赖 |
| cron | `robfig/cron/v3` | 标准 5 段表达式、时区、`SkipIfStillRunning` | 自写 ticker：表达力不足 |
| 日志 | `log/slog` | 标准库、结构化、零依赖 | `zerolog`：更快但无必要 |
| 密码哈希 | `golang.org/x/crypto/argon2` | 现代 KDF，参数可调 | `bcrypt`：72 字节上限 |
| 主键 | ULID | 按时间可排序，索引友好 | UUIDv4：随机分布导致页分裂 |
| 时区数据 | `time/tzdata` 内嵌 | 容器/精简系统没有 tzdata | 依赖系统包：易漏 |

## 11. 命令行接口

```text
homehub-extension [serve] -config <path>          启动服务（默认子命令）
homehub-extension enroll -config <path> -name <n> 生成一次性配对码
homehub-extension migrate -config <path>          执行迁移后退出，打印 schema_version
homehub-extension backup -config <path> -out <dir> 导出一份数据库副本
homehub-extension version                          版本与构建信息，不读配置
homehub-extension help | -h | --help              打印顶层用法，退出 0
```

- 无子命令等价于 `serve`，所以 `homehub-extension -config x.json` 与 `homehub-extension serve -config x.json` 行为一致。
- 子命令必须写在 flag 之前。`serve`/`enroll`/`migrate`/`backup` 四个需要配置的子命令共用 `runWithConfig` 里同一个 flag set（`-config` / `-name` / `-out` / `-dev`），不按子命令裁剪：给 `serve` 传 `-name` 不会报错，只是被忽略。`version`/`help` 在读配置之前就返回，不注册这些 flag。
- `enroll` 把配对码写到 stdout（方便 `ssh` 复制），同时写一条审计日志；不写入文件。
- `-h` / `--help` 写在子命令位置时在进入 `serve` 之前被拦下，打印顶层用法并退出 0。注意 `serve -h` 不是同一个东西：它直接把 flag 包的帮助打出来（serve 自己的 flag 列表），退出 1。
- `migrate` 默认在 `serve` 启动时自动执行；单独的 `migrate` 子命令用于升级前把「改库结构」和「重启」拆开。systemd 单元里的 `ExecStartPre` 就是它。
- 退出码只有 `0`（成功）和 `1`（配置、迁移或启动失败）。用法错误也返回 `1`，不区分 `2`——脚本里用 `if ! cmd` 而不是比对具体码值。

## 12. 待定项

1. Homehub 宿主的 API 模型（实体、服务、认证、是否支持推送）。当前只实现了宿主→Extension 这一个方向：入站签名校验、订阅推送/对账/删除/刷新，以及访问密钥的签发/列表/撤销，代码在 `internal/httpapi/handlers_hub.go`，没有单独的 `hub` 包——协议定型后再拆。
2. ~~是否多设备共享同一份阅读状态~~ **已定案**：`entry.read_at` 是 `entry` 表上的一列，本来就是全局共享——任何设备标为已读，其它设备看到的也是已读。单主人场景下这正是想要的。
3. 是否提供 OPML 导入导出（影响 feed 批量创建接口）。
4. 备份是否需要异地（当前只做本机 7 份滚动）。
5. ~~**访问密钥的身份模型**~~ **已定案（0004 + 0005）**：`auth_client` 增加 `owner_id` 列；访问密钥与配对设备产生的 client 都填 `store.OwnerPrimary`，`owner_hash` 按它推导。结果是同一主人的所有入口共享数据——密钥写的片段设备看得到，轮换密钥也不丢内容。0005 的 Go 侧搬迁把存量的 `c:<clientID>` 哈希重算成 `o:primary`，与 SQL 同一事务。

   仍未定的是**多主人**：`owner_id` 目前只有一个取值，没有配置入口。真要支持时，签发端点加一个签名覆盖的 `owner` 字段即可，数据层不用再迁移。届时还要一起决定：撤销一把密钥时是否连带撤销同身份的其它密钥会话（当前各密钥独立）。
