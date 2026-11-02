# Homehub 宿主对接指南

> **本文件不是协议定义，只是指针。**
>
> /api/v1/hub/* 的协议唯一事实源在 Homehub 仓库：
> https://github.com/ignvex/Homehub/blob/main/docs/hub-integration.md
>
> 契约由 Homehub 提出，本仓库实现它。不要在这里维护副本——两侧各存
> 全文漂移过一次，结果是协议里有一半在对方仓库查不到。
>
> 端点实现在 internal/httpapi/，签名校验在 internal/auth/signature.go。
> 改行为之前先读 Homehub 的文档。
