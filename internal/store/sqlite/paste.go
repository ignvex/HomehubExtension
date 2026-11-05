package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type pasteStore struct{ d *DB }

func (d *DB) Paste() store.PasteStore { return pasteStore{d} }

const pasteCols = `id, slug, title, body, language, visibility, password_hash,
                   expires_at, burn_after_read, size_bytes, created_at, updated_at,
                   deleted_at, owner_hash`

func scanPaste(row interface{ Scan(...any) error }) (*store.Paste, error) {
	var p store.Paste
	var expires, deleted sql.NullInt64
	var burn int
	err := row.Scan(&p.ID, &p.Slug, &p.Title, &p.Body, &p.Language, &p.Visibility,
		&p.PasswordHash, &expires, &burn, &p.SizeBytes, &p.CreatedAt, &p.UpdatedAt,
		&deleted, &p.OwnerHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expires.Valid {
		p.ExpiresAt = &expires.Int64
	}
	if deleted.Valid {
		p.DeletedAt = &deleted.Int64
	}
	p.BurnAfterRead = burn == 1
	return &p, nil
}

func (s pasteStore) Create(ctx context.Context, p *store.Paste) error {
	burn := 0
	if p.BurnAfterRead {
		burn = 1
	}
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO paste (id, slug, title, body, language, visibility, password_hash,
		                   expires_at, burn_after_read, size_bytes, created_at, updated_at,
		                   owner_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Slug, p.Title, p.Body, p.Language, p.Visibility, p.PasswordHash,
		nullInt(p.ExpiresAt), burn, p.SizeBytes, p.CreatedAt, p.UpdatedAt, p.OwnerHash)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrConflict
		}
		return fmt.Errorf("insert paste: %w", err)
	}
	return nil
}

func (s pasteStore) GetBySlug(ctx context.Context, slug string) (*store.Paste, error) {
	row := s.d.ro.QueryRowContext(ctx, `SELECT `+pasteCols+` FROM paste WHERE slug = ?`, slug)
	return scanPaste(row)
}

func (s pasteStore) Update(ctx context.Context, p *store.Paste) error {
	res, err := s.d.rw.ExecContext(ctx, `
		UPDATE paste SET title=?, body=?, language=?, visibility=?, password_hash=?,
		                 expires_at=?, burn_after_read=?, size_bytes=?, updated_at=?
		 WHERE slug=? AND deleted_at IS NULL`,
		p.Title, p.Body, p.Language, p.Visibility, p.PasswordHash,
		nullInt(p.ExpiresAt), boolInt(p.BurnAfterRead), p.SizeBytes, p.UpdatedAt, p.Slug)
	if err != nil {
		return fmt.Errorf("update paste: %w", err)
	}
	return requireAffected(res)
}

func (s pasteStore) SoftDelete(ctx context.Context, slug string) error {
	res, err := s.d.rw.ExecContext(ctx,
		`UPDATE paste SET deleted_at=? WHERE slug=? AND deleted_at IS NULL`, now(), slug)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s pasteStore) List(
	ctx context.Context,
	ownerHash string,
	limit int,
	cursor string,
) ([]store.Paste, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// ULID 的时间前缀让字典序等于时间序，可以直接按主键回溯。
	// 空游标表示从头开始：不加 id 条件，而不是与空字符串比较（那会返回空集）。
	query := `SELECT ` + pasteCols + ` FROM paste WHERE deleted_at IS NULL`
	args := []any{}
	// 始终按归属精确过滤，空字符串也一样。
	//
	// 早先的写法是「ownerHash 为空则不过滤」，结果匿名列表会返回所有人的片段。
	// 现在空字符串按字面值匹配 owner_hash=''，即迁移前没有归属的历史片段——
	// 这些片段不属于任何当前调用方，所以这里**不是** fail-closed 的终点：
	// 面向请求的调用方（paste.Service.List）必须在 ownerHash 为空时
	// 自行返回空集，不能把空值传进来。存储层保留字面匹配只是为了
	// 运维/测试能按值查询。
	query += ` AND owner_hash = ?`
	args = append(args, ownerHash)
	if cursor != "" {
		query += ` AND id < ?`
		args = append(args, cursor)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.d.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out := make([]store.Paste, 0, limit)
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func (s pasteStore) PurgeExpired(ctx context.Context, nowSec, softDeletedBefore int64) (int64, error) {
	// 只删两类：真正过期的，以及软删后已超过保留期的。
	// 不能出现 "deleted_at IS NOT NULL" 这样的无条件分支，否则软删等于立即物理删除。
	res, err := s.d.rw.ExecContext(ctx, `
		DELETE FROM paste
		 WHERE (expires_at IS NOT NULL AND expires_at <= ?)
		    OR (deleted_at IS NOT NULL AND deleted_at <= ?)`,
		nowSec, softDeletedBefore)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClaimBurn 用一条带条件的 UPDATE 抢占一次性读取权。
// 两个并发请求只有一个能把 deleted_at 从 NULL 改成非 NULL，
// 另一个的 RowsAffected 为 0，从而拿到"已被读走"的结论。
func (s pasteStore) ClaimBurn(ctx context.Context, slug string, nowSec int64) (bool, error) {
	res, err := s.d.rw.ExecContext(ctx, `
		UPDATE paste SET deleted_at = ?
		 WHERE slug = ? AND burn_after_read = 1 AND deleted_at IS NULL`, nowSec, slug)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CountByOwner 统计某归属下未删除的片段数。
//
// 与 paste.Service.List 口径一致：工具台的角标必须和列表里的条数一致，
// 否则会出现「首页说 2 条、列表是空的」这种自相矛盾的界面。
// ownerHash 为空（完全匿名）时直接返回 0，不查库：Service.List 对空归属
// 返回空列表，而 handler 是直接调用这里的，没有 service 层兜底。
func (s pasteStore) CountByOwner(ctx context.Context, ownerHash string) (int64, error) {
	if ownerHash == "" {
		return 0, nil
	}
	var n int64
	err := s.d.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM paste WHERE deleted_at IS NULL AND owner_hash = ?`,
		ownerHash).Scan(&n)
	return n, err
}
