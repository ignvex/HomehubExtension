package httpapi

import (
	"encoding/base64"

	"github.com/ignvex/HomehubExtension/internal/store"
)

// API 里所有「绝对时刻」字段统一为 Unix 秒（int64）。
//
// 为什么是整数而不是 RFC3339 字符串：
//
//  1. 与存储一致。SQLite 里存的就是 INTEGER 秒，输出字符串等于每次请求
//     都要格式化一次、客户端每次再解析回来。
//  2. 只有一种形态。字符串会有 "Z" 与 "+08:00"、有无小数秒的差异，
//     一旦某处序列化方式漂移，客户端就得写兼容分支——这个项目已经踩过。
//  3. 比较与排序直接比大小，不需要解析。
//  4. 前端 `new Date(sec * 1000)` 零依赖，`Date.parse` 的跨浏览器差异消失。
//
// 为什么是秒不是毫秒：数据库存秒，用秒是零转换。毫秒要在每个 handler
// 各乘一次 1000，漏一处就是差 1000 倍的经典 bug；而前端的转换点只有
// lib/format.ts 的 parseTime 一个，集中且可测。
//
// 时长为零值（未设置）的字段返回 null，不返回 0——0 在 Unix 时间里
// 是 1970-01-01，一个有意义的时刻，不能拿它当「没有」。
func ts(sec int64) *int64 {
	if sec == 0 {
		return nil
	}
	return &sec
}

// optTS 处理可空时刻。nil 进 nil 出，不做 0 值兜底。
func optTS(sec *int64) *int64 {
	if sec == nil || *sec == 0 {
		return nil
	}
	return sec
}

func b64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// pasteModel 是 store.Paste 的本地别名，让 handler 里的类型名更短。
type pasteModel = store.Paste

// cursorOrNil 把空游标转成 null。
//
// 空字符串与 null 在 JS 里都是 falsy，行为一致，但契约上应该只有一种表达。
// 既然「未设置是 null」已经是这个 API 的规矩，游标也遵守。
func cursorOrNil(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}
