# 前端工程

HomehubExtension 的浏览器端。TypeScript + Vite，原生 DOM，手写 CSS3，零运行时依赖。

## 环境

- Node ≥ 22（开发机实测 v26）
- pnpm ≥ 10（`packageManager` 字段锁定 12.6.0，`corepack` 会自动切换）

## 命令

```bash
pnpm install       # 安装依赖
pnpm dev           # 开发服务器 :5173，/api /media /p 代理到 :8080
pnpm build         # 清 dist → tsc --noEmit → vite build，产物在 dist/
pnpm check         # 只做类型检查
pnpm check:dist    # 检查产物是否满足 CSP 与体积预算（需先 build）
pnpm test:qr       # 二维码容量表对照标准向量
pnpm test:crypto   # 验证签名逻辑与服务端逐字节一致
pnpm verify        # 依次跑 check / test:qr / build / check:dist / test:crypto
```

开发时后端要先起来：`go run . serve -config ../config.json`。

## 目录

```text
src/
├── main.ts              入口
├── app.ts               AppContext 构造与全局布局
├── router.ts            History API 路由 + 鉴权守卫
├── api/                 请求封装与 DTO 类型
├── auth/                WebCrypto 密钥、IndexedDB 存储、会话
├── state/               极简订阅式 store
├── lib/                 DOM 构造、格式化、SSE、文案
├── components/          导航、Toast、骨架屏、空状态、图标
├── views/               每个路由一个视图
├── tools/               15 个开发工具：registry + 每个工具一个模块
└── styles/              @layer 分层样式
```

## 几个硬约束

**构建产物不能有内联脚本或内联样式。** 生产 CSP 是 `script-src 'self'; style-src 'self'`，任何内联片段都会被浏览器拒绝。`vite.config.ts` 关掉了 `modulePreload.polyfill`（它默认以内联 `<script>` 注入），`pnpm check:dist` 会断言产物里不含内联片段。

**唯一使用 `innerHTML` 的地方是渲染 `content_html`。** 安全性由服务端清洗 + CSP 兜底，前端不再清洗一次——前端没有 HTML 解析器，做不到正确清洗，二次处理只会引入错误。

**私钥永远不可导出。** 用 `crypto.subtle.generateKey(..., false, ...)` 生成，以 `CryptoKey` 对象存 IndexedDB。JavaScript 读不到字节，XSS 也无法把它带走。

**签名消息逐字节固定。** 与服务端 `auth.AuthMessage` 完全一致：

```
homehubext/auth/v1
<client_id>
<nonce>
```

换行必须是 `\n`。`pnpm test:crypto` 会校验这一点。

**主题不闪白也不依赖内联脚本。** `index.html` 里写 `data-theme="__THEME__"`，后端返回时读 `hh_theme` cookie 替换成实际值。首帧就是正确配色，CSP 不需要开 `'unsafe-inline'`。

## 体积预算

| 项 | 预算 | 当前 |
| --- | --- | --- |
| 首屏 JS（gzip） | ≤ 40 KB | 27.5 KB（2 个 chunk） |
| 按需 JS（gzip） | ≤ 80 KB | 27.3 KB（16 个 chunk） |
| CSS（gzip） | ≤ 20 KB | 8.4 KB |
| dist 未压缩总量 | ≤ 400 KB | 180.7 KB |

`pnpm check:dist` 在超出预算时失败。首屏按「`index.html` 直接引用的入口 chunk + shared」统计，与按需 chunk 分两档——只卡总量会逼着人删功能，只卡首屏则总量可能失控。体积会悄悄膨胀，必须有闸门。
