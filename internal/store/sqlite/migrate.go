package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/ignvex/HomehubExtension/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration 是编号的 SQL 文件。版本号取自文件名前缀，只前进不回退。
type migration struct {
	version int
	name    string
	sql     string
	// fix 是在同一事务内、紧接 sql 之后执行的 Go 侧数据搬迁。
	//
	// 有些搬迁 SQLite 表达不了：需要 sha256 的哈希重算没有内置函数。
	// 把这类步骤和 SQL 放进同一个事务，失败就一起回滚，不会留下
	// "表结构升级了、数据还是旧的"这种半截状态。
	fix func(ctx context.Context, tx *sql.Tx) error
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		idx := strings.IndexByte(e.Name(), '_')
		if idx <= 0 {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(e.Name()[:idx])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := range out {
		out[i].fix = dataFixes[out[i].version]
	}
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d (%s, %s)", out[i].version, out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// migrate 顺序执行未应用的迁移。每个文件一个事务，失败则整库停在旧版本。
func migrate(ctx context.Context, db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		current = m.version
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if m.fix != nil {
		if err := m.fix(ctx, tx); err != nil {
			return err
		}
	}
	// PRAGMA 不支持参数绑定；版本号来自文件名解析出的整数，不存在注入面。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// dataFixes 是按迁移版本挂载的 Go 侧数据搬迁。
//
// 只在对应版本首次应用时执行——migrate 按 user_version 跳过已应用的
// 版本，所以这里天然幂等，不需要额外的标记表。
var dataFixes = map[int]func(context.Context, *sql.Tx) error{
	5: shareOwnerScope,
}

// shareOwnerScope 把按设备算出的旧归属统一到实例级归属。
//
// 0005 的 SQL 已经把 auth_client.owner_id 补齐，但历史上那些
// paste.owner_hash 是按 "c:<clientID>" 算的，不重算的话，存量片段
// 会在切换后立刻"消失"——数据还在，只是没有任何凭证能匹配上。
//
// 先收集再更新：在同一个事务里一边遍历 rows 一边写别的表，不同驱动
// 的行为不一致，modernc 的 sqlite 会直接拒绝。
func shareOwnerScope(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM auth_client WHERE owner_id = ?`, store.OwnerPrimary)
	if err != nil {
		return fmt.Errorf("scan clients for owner scope: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	target := store.OwnerHashForOwner(store.OwnerPrimary)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE paste SET owner_hash = ? WHERE owner_hash = ?`,
			target, store.OwnerHashForClient(id)); err != nil {
			return fmt.Errorf("rewrite paste owner for client %s: %w", id, err)
		}
	}
	return nil
}
