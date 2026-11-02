# 配置与定时清理

## 1. 加载顺序

路径按以下优先级确定，命中即停：

1. 命令行 `-config /path/to/config.json`（systemd 使用绝对路径）
2. 环境变量 `HOMEHUB_CONFIG`
3. `./config.json`
4. `/etc/hhe/config.json`

规则：

- 显式指定的路径（第 1、2 条）**不存在就直接失败**，不继续向下探测。否则"以为改的是生产配置、实际读的是当前目录旧文件"是线上最难排查的一类事故。
- 全部未命中时退出码 1，输出：

  ```text
  config not found: pass -config, set HOMEHUB_CONFIG, or place config.json in the working directory
  see config.example.json
  ```

- 不提供内置默认值的静默回落。默认值只用于**补齐省略字段**，文件本身必须存在。
- 启动日志打印最终使用的绝对路径与 `sha256` 前 8 位，便于确认部署的是哪份配置。

## 2. 通用规则

- 所有时长写成字符串，单位 `ms`/`s`/`m`/`h`（如 `"5s"`、`"168h"`），不用纳秒整数。
- 所有字节数写成整数，单位 byte。
- 路径接受相对路径，但启动时立即解析为绝对路径并打印。
- 支持 `${VAR}` 与 `${VAR:-default}` 展开，展开发生在 JSON 解析**之前**，作用于整个文件文本。未定义且无默认值的变量视为校验错误。
- JSON 解析使用 `DisallowUnknownFields`：字段拼错（`max_concurrent` vs `max_concurrency`）必须报错，而不是被静默忽略。
- 校验一次性收集所有问题，格式为 `config: <path>: <reason>`，全部列出后退出。

```text
config: rss.fetch_concurrency: must be between 1 and 8 (got 0)
config: cleanup.jobs[2].schedule: invalid cron expression "0 25 * * *"
config: database.path: must not be empty
```

## 3. 全字段说明

### 3.1 `version`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `version` | int | `1` | 配置结构版本。当前只接受 `1`；未来做结构变更时据此兼容或提示迁移。 |

### 3.2 `server`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `addr` | string | `127.0.0.1:8080` | 监听地址。生产环境必须是回环地址，Caddy 反代。 |
| `public_url` | string | 空 | 对外基址，如 `https://homehub.example.com`。用于生成 `Location`、`/p/{slug}` 绝对链接、enrollment 提示。末尾不带 `/`。 |
| `allow_remote_listen` | bool | `false` | 允许绑定非回环地址。仅在容器内需要监听 `0.0.0.0` 时打开；打开后启动日志会写一条 warn。 |
| `read_header_timeout` | duration | `5s` | 防慢速请求头攻击。 |
| `idle_timeout` | duration | `90s` | keep-alive 空闲上限。 |
| `trusted_proxies` | string[] | `["127.0.0.1/32","::1/128"]` | 可信反代 CIDR。只应包含 Caddy 所在地址。 |
| `max_body_bytes` | int | `1048576` | 请求体上限（1 MB）。Paste 的实际上限由 `pastebin.max_body_bytes` 再收窄。 |

`WriteTimeout` 固定为 `0`，不可配置——SSE 需要长连接，任何非零写超时都会周期性切断事件流。

### 3.3 `auth`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 关闭时所有 `/api/v1/*` 免鉴权。仅限本机调试。 |
| `server_key_path` | string | `<data>/server.key` | 服务端 Ed25519 私钥（PKCS#8 PEM，0600）。不存在则生成。 |
| `session_ttl` | duration | `168h` | 会话有效期。每次使用滑动续期至多一次/小时。 |
| `challenge_ttl` | duration | `2m` | 挑战 nonce 有效期。 |
| `enrollment_ttl` | duration | `15m` | 配对码有效期。 |
| `failed_attempt_limit` | int | `10` | 同一来源在窗口内允许的**失败**次数，超限返回 429 并带 `Retry-After`。 |
| `failed_attempt_window` | duration | `5m` | 失败计数的滑动窗口。 |

限流只作用于接受未认证输入的三个端点：`/auth/enroll`、`/auth/challenge`、`/auth/token`。

计数规则：

- 只有 401/403 计入失败。成功会清空该来源的计数——否则用户打错几次密码、之后成功，几分钟内的正常使用会被历史失败误伤。
- 5xx 不计入：那是服务端的问题，不该惩罚客户端。
- 达到上限时写一条 warn 日志（只在刚被拦住时记，避免持续暴力尝试刷满日志）。
- 表按窗口自动回收，不依赖外部调度。

### 3.3.1 `auth.hub`

Homehub 宿主推送订阅时使用的入站签名通道。

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `hub.enabled` | bool | `false` | 是否启用。关掉后 `/api/v1/hub/*`、`/api/v1/fetch`、`/api/v1/avatar/{hash}` 全部返回 503。 |
| `hub.public_key` | string | 空 | 宿主公钥**内容**，接受 PKIX PEM 或裸 base64，不是文件路径。裸 base64 指的是 **32 字节 raw 公钥**的 base64，不是 PEM body 那段 DER（44 字节）；`hhe key import` 会把后者自动转成前者的形式，手写时填错会报解码长度不符。多行 PEM 粘进 JSON 字符串时必须把换行写成 `\n`，否则文件本身就不合法；用单行 base64 最省事。为空时签名通道不启用（`/hub/*`、`/fetch`、`/avatar` 全部 503），但**不会拒绝启动**——很多部署一开始只用浏览器。从旧版升级时若配置里还留着 `public_key_path`，执行 `sudo hhe repair` 自动迁移（会自动备份原配置）。 |
| `hub.clock_skew` | duration | `60s` | 允许的时间戳偏差。放宽意味着重放窗口变长，不要超过 5 分钟。 |
| `hub.nonce_ttl` | duration | `2m` | nonce 去重表的 TTL，需大于 `clock_skew`。 |

私钥留在 Homehub 那一侧，Extension 只存公钥。已通过签名的请求不会因为公钥泄露而被伪造——泄露公钥只能用来**验证**，不能用来签名。

同一把公钥同时护着 `/api/v1/fetch` 与 `/api/v1/avatar/{hash}`：这两条资源代理与 `/hub/*` 共用签名校验，不引入第三种凭据。`/hub/subscriptions*` 还额外要求 `rss.enabled`，资源代理不需要。

### 3.4 `database`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 必填 | SQLite 文件路径。父目录必须已存在且可写。 |
| `busy_timeout` | duration | `5s` | 映射为 `PRAGMA busy_timeout`。 |
| `read_conns` | int | `3` | 只读连接池大小，取值 1–8。 |

### 3.5 `storage`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `blob_dir` | string | 必填 | 内容寻址文件根目录。 |
| `tmp_dir` | string | `<blob_dir>/tmp` | 下载临时目录。必须与 `blob_dir` 在同一文件系统。 |
| `max_blob_bytes` | int | `52428800` | 单文件上限（50 MB）。超出记为 `too_large`，不下载。 |
| `quota_bytes` | int | `5368709120` | blob 总配额（5 GB）。达到后拒绝新下载，已有内容不受影响。 |
| `min_free_bytes` | int | `1073741824` | 磁盘可用空间下限（1 GB）。低于此值拒绝下载。 |
| `gc_grace` | duration | `1h` | 引用归零后等待多久才删文件，防止事务回滚与并发写造成误删。 |

### 3.6 `pastebin`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 关闭时路由不注册，返回 404。 |
| `max_body_bytes` | int | `262144` | 单个片段上限（256 KB）。 |
| `default_expiry` | duration | `168h` | 未指定时的默认过期时间。`0` 表示永久。 |
| `max_expiry` | duration | `8760h` | 客户端可指定的上限（1 年）。 |
| `slug_length` | int | `6` | 短链长度，取值 4–12。base62。 |
| `allow_public` | bool | `true` | 是否允许 `visibility=public`（出现在公开列表）。关闭后只能创建 unlisted/private。 |
| `language_list` | string[] | 常用 21 种 | 允许写入的语言标识。不在列表内的值回落为 `text`。前端据此生成选择器，不做语法高亮。 |

### 3.7 `rss`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | 关闭时 scheduler 不注册抓取任务。 |
| `default_interval` | duration | `30m` | 新建订阅源的默认抓取间隔。 |
| `min_interval` | duration | `10m` | 间隔下限，防止用户把源打爆。 |
| `max_interval` | duration | `6h` | 失败退避的上限。 |
| `fetch_concurrency` | int | `2` | 全局同时抓取的 feed 数，1–8。约束所有触发路径：定时轮询、宿主推送后的首抓、手动刷新。 |
| `media_concurrency` | int | `3` | 同时下载的媒体数，1–8。 |
| `fetch_timeout` | duration | `90s` | 单个 feed 一轮抓取（含媒体下载）的总预算。 |
| `request_timeout` | duration | `20s` | 单次 HTTP 请求超时。 |
| `user_agent` | string | `HomehubExtension/<version>` | 出站 UA。建议带联系地址，便于站点管理员识别。 |
| `max_feed_bytes` | int | `5242880` | feed 文档上限（5 MB）。超限视为失败。 |
| `max_entry_bytes` | int | `2097152` | 单条目 HTML 上限（2 MB）。超限截断并标记。 |
| `allow_private_hosts` | bool | `false` | 允许抓取内网地址。默认关闭以阻断 SSRF。仅在你确实要订阅局域网服务时打开。 |
| `referer` | string | `auto` | 媒体下载的 Referer 策略：`auto` 用条目链接、`site` 用站点首页、`none` 不带。部分图床有防盗链，`auto` 失败时可试 `site`。 |

### 3.8 `cleanup`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `timezone` | string | `Local` | cron 计算所用时区，如 `Asia/Shanghai`。二进制内嵌 tzdata，不依赖系统。 |
| `jobs` | object[] | 见下 | 清理任务列表。空数组表示不调度任何清理。 |
| `catch_up` | bool | `true` | 启动 30 秒后补跑已错过的任务，见 4.4。 |

**这是全系统唯一定义保留期与回收策略的地方。** `pastebin`、`rss` 段里不重复出现 `retention` 字段，避免两处配置互相矛盾。

### 3.9 `log`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `level` | string | `info` | `debug`/`info`/`warn`/`error`。 |
| `format` | string | `json` | `json` 或 `text`。systemd + journald 场景用 `json`；本地开发用 `text`。 |
| `access_log` | bool | `true` | 是否记录每个 HTTP 请求。`/healthz`、`/readyz`、`/media/*` 永不记录。 |

## 4. 清理任务（crontab）

### 4.1 任务定义

```json
{
  "name": "rss-prune",
  "schedule": "30 4 * * *",
  "timeout": "20m",
  "enabled": true,
  "params": { "older_than": "2160h", "keep_starred": true }
}
```

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 必填 | 任务类型，必须是下表之一。同名任务只能出现一次。 |
| `schedule` | string | 必填 | 标准 5 段 cron（分 时 日 月 周），或 `@hourly`/`@daily`/`@weekly`/`@monthly`。**不支持秒级**，避免误配导致高频扫描。 |
| `timeout` | duration | `10m` | 单次执行上限。超时后 ctx 取消，任务在下一个检查点退出并记为 `timeout`。 |
| `enabled` | bool | `true` | 关闭后仍保留定义，便于临时停用。 |
| `params` | object | `{}` | 按任务类型校验，未知键报错。 |

### 4.2 内置任务

| `name` | 作用 | `params` |
| --- | --- | --- |
| `paste-expired` | 物理删除 `expires_at < now` 或 `deleted_at` 超过保留期的片段 | `deleted_retention`（duration，默认 `24h`） |
| `session-expired` | 删除过期或已撤销的 session、已消费的 challenge、过期 enrollment | 无 |
| `rss-prune` | 删除超期的未标星条目（级联减少 blob 引用） | `older_than`（duration，默认 `2160h` = 90 天）、`keep_starred`（bool，默认 `true`）、`keep_unread`（bool，默认 `false`） |
| `media-retry` | 重试 `status=pending` 或 `status=failed` 的媒体下载 | `max_attempts`（int，默认 3）、`within`（duration，默认 `72h`，只重试这个时间窗内的条目） |
| `blob-gc` | 删除 `ref_count = 0` 且超过 grace 的 blob 行与文件；清理孤儿文件 | `grace`（duration，默认取 `storage.gc_grace`）、`dry_run`（bool，默认 `false`） |
| `db-vacuum` | 回收空闲页 | `mode`：`incremental`（默认）或 `full` |
| `job-log-prune` | 删除 `job_run` 历史 | `keep`（duration，默认 `720h`） |

### 4.3 建议配置

```json
"cleanup": {
  "timezone": "Asia/Shanghai",
  "catch_up": true,
  "jobs": [
    { "name": "paste-expired",   "schedule": "0 * * * *" },
    { "name": "session-expired", "schedule": "15 * * * *" },
    { "name": "media-retry",     "schedule": "*/30 * * * *" },
    { "name": "rss-prune",       "schedule": "30 4 * * *" },
    { "name": "blob-gc",         "schedule": "0 5 * * *" },
    { "name": "db-vacuum",       "schedule": "30 5 * * 0" },
    { "name": "job-log-prune",   "schedule": "0 6 * * 1" }
  ]
}
```

设计意图：

- 小时级任务只做便宜的标记与删除，分钟级只有媒体重试。
- 重任务全在凌晨 4–6 点，彼此错开 30 分钟，避免单核争抢。
- `blob-gc` 放在 `rss-prune` 之后：先减少引用，再回收文件，同一天内完成闭环。
- `db-vacuum` 每周一次。完整 `VACUUM` 需要约 2 倍数据库大小的临时空间且全程阻塞写入，所以默认 `incremental`；数据库超过 1 GB 再考虑 `full`。

### 4.4 执行语义

- **单实例**：同一任务上一轮未结束时，本轮直接跳过并记一条日志，不排队、不并发。cron 本身不加 chain，并发保护由运行表统一处理——这样 `POST /system/jobs/{name}/run` 的手动触发与定时触发共享同一把锁，不会出现"手动跑着、定时又跑一遍"。
- **幂等**：每个任务都必须能在任意中断点重跑而不产生副作用。删除类任务按主键分批。
- **分批**：默认每批 500 行，事务提交后 `time.Sleep(50ms)` 让出 CPU 给 HTTP 请求。
- **超时**：`ctx` 传到每一条 SQL 与每一次文件操作。
- **记录**：每次执行写 `job_run(job, started_at, finished_at, status, detail)`；`status` ∈ `running|ok|failed|timeout|skipped`；`detail` 记录处理行数与错误摘要。
- **补跑**：`catch_up=true` 时，启动 30 秒后检查每个任务的最近一次成功执行时间。若距现在已经超过一个完整调度周期，就补跑一次。

  为什么需要：cron 调度器随进程启停，downtime 期间的触发点全部丢失。一台每天重启一次的机器上，定在凌晨 4 点的 `rss-prune` 永远不会执行，过期条目会无限堆积直到撑满磁盘。

  周期怎么算：用 `Schedule.Next` 求两个连续触发点的时间差。对 `30 4 * * *` 是 24 小时，对 `0 5 * * 0` 是 7 天，语义正确。

  两个例外：

  - `db-vacuum`：全量 VACUUM 需要约两倍数据库大小的临时空间且全程阻塞写入，启动时跑会在最不该卡的时候卡住。
  - `media-retry`：会立刻发起大量出站请求，启动阶段应该先让服务可用。

  从未成功执行过的任务不补跑——刚部署时不该把几个重任务一起拉起来。
- **可观测**：`GET /api/v1/system/jobs` 返回最近 50 条执行记录；前端「系统 → 任务」页展示。清理摘要同时写一条 info 日志。
- **不自动删除**：`blob-gc` 只删文件，从不删除有引用的行；`rss-prune` 不动 `starred` 条目（除非 `keep_starred=false`）。

### 4.5 为什么不用系统 crontab

系统 cron 也能跑，但需要额外脚本、额外日志、额外权限，而且清理逻辑无法与应用共享事务与引用计数。进程内调度换来三件事：任务与业务在同一份代码里、结果可从 API 观察、失败的上下文能带完整错误。代价是调度器随进程启停——`catch_up` 弥补这一点。

## 5. systemd 集成

### 5.1 unit

完整单元见 [`deploy/hhe.service`](../deploy/hhe.service)（守护进程名 `hhe`）。
这里不再贴副本：文档里曾经贴过一份，结果和实际文件静默漂移了（文档写着
`Type=simple` 和一条 `ExecReload`，而进程根本不处理 SIGHUP）。唯一来源是 deploy 下的文件。

几个不显然的取值，以及它们各自防的是什么：

- **`Type=notify` + `WatchdogSec=60s`**：进程在端口真正 bind 成功之后才发 `READY=1`，
  `systemctl start` 返回时服务已经可用，部署脚本和 `After=` 依赖它的 unit 都不必再轮询 `/healthz`。
  心跳每 30 秒一次，超过 60 秒没有心跳 systemd 判定假死并重启——这是 `Restart=on-failure`
  完全看不到的一类故障：进程还在、端口还占着，但已经不干活。
- **`StateDirectory=hhe`**：展开为 `/var/lib/hhe`，目录由 systemd 创建并 chown，同时自动放行给
  `ProtectSystem=strict`。安装时不需要手工 `mkdir` + `chown`。
- **`GOMEMLIMIT=256MiB` 与 `MemoryMax=512M` 必须成对**：前者让 Go 的 GC 提前压低堆，
  后者是 cgroup 硬墙。只配 `MemoryMax` 的话，触墙就是 OOM kill，没有减震。
- **`ExecStartPre=... migrate`**：迁移失败停在启动前，日志直接指向配置或权限问题，
  而不是让 serve 反复崩在 `Type=notify` 的启动超时上。
- **没有 `ExecReload`**：进程只处理 `SIGINT`/`SIGTERM`，SIGHUP 走默认处置直接终止进程。
  写一条 `ExecReload=/bin/kill -HUP $MAINPID` 得到的不是热重载，而是 `systemctl reload`
  把服务杀掉——随后被 `Restart=on-failure` 拉起，看起来像是「reload 生效了」。
- **`TimeoutStopSec=30s`** 要大于应用内部的 20 秒关闭预算，否则 systemd 会强杀，
  WAL 来不及 checkpoint。
- **`Restart=on-failure` 而不是 `always`**，配 `StartLimitBurst=5` / `StartLimitIntervalSec=60`：
  配置错误这类启动失败重试几次就停在 failed，不会无限刷日志。
- **`SystemCallFilter=@system-service` + `SystemCallErrorNumber=EPERM`**：被过滤的调用返回
  错误而不是发 `SIGSYS` 杀进程，真出问题时是一个可读的错误而不是无声崩溃。

加固程度可以量化：`systemd-analyze security --offline=yes deploy/hhe.service`
当前是 **1.5 OK**（首版是 6.3 MEDIUM）。这个分数越低越好；改完文件后跑一次，
超过 2.0 就值得看看是哪一项退化了。

### 5.2 环境文件

```bash
# /etc/hhe/env
HOMEHUB_TOKEN=redacted
```

### 5.3 首次部署

```bash
sudo install -m 0755 dist/homehub-extension /usr/bin/
sudo install -m 0755 deploy/backup.sh /usr/bin/homehub-backup.sh
# 数据目录由 unit 里的 StateDirectory 创建并 chown，不需要手工 mkdir
sudo install -d -o root -g homehub -m 0750 /etc/hhe   # 组必须是服务用户的主组
sudo install -m 0640 -o root -g homehub config.json /etc/hhe/config.json
sudo install -m 0600 -o root -g homehub deploy/env.example /etc/hhe/env
sudo install -m 0644 deploy/hhe.service \
                    deploy/hhe-backup.service deploy/hhe-backup.timer /etc/systemd/system/
sudo install -d -o homehub -g homehub -m 0750 /var/backups/hhe
sudo systemctl daemon-reload
sudo systemctl enable --now hhe hhe-backup.timer
journalctl -u hhe -f
```

`homehub` 系统用户需要先存在：

```bash
sudo useradd --system --home /var/lib/hhe \
  --shell /usr/sbin/nologin homehub
```

首次配对：

```bash
sudo -u homehub /usr/bin/homehub-extension enroll \
  -config /etc/hhe/config.json \
  -name "macbook-chrome"
```

命令输出一次性配对码，15 分钟内有效。浏览器打开 `https://<public_url>/pair` 输入即可。

### 5.4 改配置

**一律 `systemctl restart homehub-extension`。** 目前没有热重载，进程不处理 SIGHUP。

不做热重载是有意的取舍。真正可热改的只有 `log.level`、`cleanup.jobs` 的调度、
`auth.failed_attempt_*` 三项，而这三项的实现代价并不低——尤其是 `cleanup.jobs`：
`scheduler.Apply` 目前不支持重复调用（每条计划都会再注册一个 cron 条目，
`planned` 也只追加不替换），要支持热改就得先把计划表做成可替换的，
再处理「新配置只有一半合法」时的一致性。

收益那一侧很薄：重启是原子的，`RestartSec=5s` 加上 `Type=notify` 保证 `start` 返回时
已经可用；而部分热改一旦留下半应用状态，排查成本远高于一次重启。

`addr`、`database.path`、`storage.blob_dir` 本来就只能重启生效——热改监听地址会带来
连接中断与 Caddy 502，收益小于风险。

## 6. 备份与恢复

```bash
# deploy/backup.sh 每日 03:00 跑一次，保留 7 份；调度交给 systemd timer
sudo install -m 0755 deploy/backup.sh /usr/bin/homehub-backup.sh
sudo install -m 0644 deploy/hhe-backup.service deploy/hhe-backup.timer /etc/systemd/system/
sudo install -d -o homehub -g homehub -m 0750 /var/backups/hhe
sudo systemctl daemon-reload
sudo systemctl enable --now hhe-backup.timer
systemctl list-timers hhe-backup.timer
```

用 timer 而不是 cron：日志直接进 journal（`journalctl -u hhe-backup`），
`Persistent=true` 会补跑关机期间错过的触发点，`RandomizedDelaySec` 负责错峰——
这三件事 cron 都要另写脚本解决。

数据库备份走二进制自带的子命令，**不依赖系统装 sqlite3**：

```bash
homehub-extension backup -config /etc/hhe/config.json -out /var/backups/hhe/2026-10-29
```

内部用 `VACUUM INTO`：

- 产出一份整理过、事务一致的完整副本，且不阻塞读。
- 先写 `.partial` 再 rename：备份中途被中断，不会留下一个"看起来正常"的半个文件。
- SQLite **不要**直接 `cp` 数据库文件——WAL 里可能有未合并的事务，恢复出来是旧数据且不会报错。

媒体文件由脚本用 `rsync --link-dest` 增量同步。内容寻址意味着地址变了内容必然变，未改动的文件只建硬链接，7 份备份的实际占用约等于 1 份。

恢复步骤：

```bash
sudo systemctl stop homehub-extension
sudo -u homehub cp /var/backups/hhe/2026-10-29/homehub.db /var/lib/hhe/homehub.db
sudo rm -f /var/lib/hhe/homehub.db-wal /var/lib/hhe/homehub.db-shm
sudo systemctl start homehub-extension
```

删 `-wal`/`-shm` 是必需的：残留的 WAL 会被当成新库的一部分重放，把恢复的库改回旧状态。

数据库与 blob 目录的备份必须来自同一时间点，否则会出现引用指向缺失文件。`blob-gc` 每天会交叉比对，把这种情况报告为内容损坏，而不是静默通过。
