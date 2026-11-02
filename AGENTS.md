# AGENTS.md — HomehubExtension 工作约定

给在此仓库工作的 AI agent（以及新加入的人）的实用约定。**不是**面向用户的
文档；用户文档见 `README.md` 与 `docs/`。

## 仓库性质

Homehub 的扩展服务，**公开仓库**（`github.com/ignvex/HomehubExtension`）。

- 提交前默认假设「这段内容会被所有人看到」
- 任何凭据、内网地址、私人密钥都不得进入版本控制
- 与宿主协议相关的内容以 **Homehub 仓库的 `docs/hub-integration.md` 为唯一
  事实源**。本仓库实现它、读它，不维护副本——两侧各存一份全文已经漂过一次

## 技术栈

| 层 | 选型 |
| --- | --- |
| 后端 | Go，gin，SQLite（WAL），内容寻址 blob |
| 前端 | TypeScript，Vite；构建产物 `go:embed` 进可执行文件 |
| 包管理 | Go modules；前端 pnpm |

## 目录职责

```
internal/app/        启动编排
internal/auth/       浏览器鉴权 + 入站签名（HubVerifier）
internal/blob/       内容寻址文件存储
internal/config/     配置加载、校验、脱敏
internal/httpapi/    路由与处理器
internal/paste/      pastebin 模块
internal/rss/        抓取 → 清洗 → 本地化 → 入库
internal/scheduler/  定时任务
internal/store/      SQLite 访问
web/src/             前端 SPA
```

## 常用命令

```bash
make build     # 交叉编译 linux/amd64 单二进制，前端 embed 进去
make test      # go test ./...
make race      # 并发相关用例
make vet       # go vet + 前端 tsc
make lint      # gofmt + go vet + 前端检查，目标全绿
make units     # 单元测试
make sec       # gosec，medium 及以上必须清零
```

提交前至少要有 `make build`、`make test`、`make lint` 全绿。

## 提交

- Conventional Commits：`feat(scope): …` / `fix(scope): …` / `docs: …` / `chore: …`
- **标题与正文一律用英文。** 仓库是公开的，`git log` 常被非中文读者查阅；
  历史提交几乎全是中文，需要逐步收敛。代码、注释、文档正文仍用中文，
  这一条只管提交信息
- 提交信息正文说明**为什么**，不复述 diff
- 一个提交一件事

## 时间与数据表示

- **全局统一 Unix 秒**：SQLite `INTEGER` → Go `int64` → JSON number → JS number
- 只在 `web/src/lib/format.ts` 一处乘 1000
- 未设置是 `null` 而不是 `0`——`0` 是 1970-01-01
- 时长（`"30m"`）与时刻不混用

## 签名通道

`internal/auth/signature.go` 的 `HubVerifier` 校验 Homehub 宿主的入站请求。

- 待签消息的**方法一律大写**参与签名，与宿主侧 `strings.ToUpper` 对齐
- 验签通过之后才登记 nonce：未通过的请求不该污染去重表
- 公钥只接受 Ed25519；`auth.hub.public_key` 存的是**内容**（PEM 或裸
  base64），不是文件路径——不要把路径当成隐式支持的形态

## 部署约束

整套设计由部署环境倒推，改动时别忽略：

| 约束 | 取值 |
| --- | --- |
| CPU | 1 vCPU amd64，并发靠限流而非加核 |
| 内存 | 2 GB，进程硬上限 512 MB |
| 磁盘 | 20 GB，blob 配额 5 GB |
| 入口 | Caddy 反代，Go 只监听 `127.0.0.1:8080`，不终止 TLS |

## 安全底线

- 匿名可写的接口（pastebin）必须有大小上限与限流
- 抓取外部 URL 时禁止内网地址（SSRF）
- 密钥类字段永不出现在 `/api/v1/system/config` 的响应里
- `#nosec` 必须带 `--` 开头的理由，不靠维护白名单文件

## 已知的取舍

- **不做服务间双向鉴权**：宿主持有私钥，Extension 只验签，不反向调用宿主
- **条目本地化是硬性验收标准**：读完一篇文章，浏览器不应向任何第三方发出请求
