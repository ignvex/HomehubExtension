package sqlite

import (
	"database/sql"
	"strings"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
)

func now() int64 { return time.Now().Unix() }

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requireAffected 把"没有行被改动"翻译成 ErrNotFound。
// 少了这一步，更新一个不存在的资源会静默成功。
func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// modernc 驱动把 SQLite 的错误码放在消息里，没有导出类型化的错误。
	// 匹配 "UNIQUE constraint failed" 是这套驱动下最稳的判断方式。
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
