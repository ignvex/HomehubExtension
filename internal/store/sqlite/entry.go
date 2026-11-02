package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type entryStore struct{ d *DB }

func (d *DB) Entry() store.EntryStore { return entryStore{d} }

const entryCols = `id, feed_id, guid, url, title, author, published_at, fetched_at,
                   summary, content_html, content_text, read_at, starred_at`

func scanEntry(row interface{ Scan(...any) error }) (*store.Entry, error) {
	var e store.Entry
	var published, read, starred sql.NullInt64
	err := row.Scan(&e.ID, &e.FeedID, &e.GUID, &e.URL, &e.Title, &e.Author,
		&published, &e.FetchedAt, &e.Summary, &e.ContentHTML, &e.ContentText,
		&read, &starred)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.PublishedAt = ptrInt(published)
	e.ReadAt = ptrInt(read)
	e.StarredAt = ptrInt(starred)
	return &e, nil
}

func (s entryStore) InsertIfNew(ctx context.Context, e *store.Entry) (bool, error) {
	res, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO entry (feed_id, guid, url, title, author, published_at, fetched_at,
		                   summary, content_html, content_text)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(feed_id, guid) DO NOTHING`,
		e.FeedID, e.GUID, e.URL, e.Title, e.Author, nullInt(e.PublishedAt),
		e.FetchedAt, e.Summary, e.ContentHTML, e.ContentText)
	if err != nil {
		return false, fmt.Errorf("insert entry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return true, nil // 已存在，未修改
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	e.ID = id
	return false, nil
}

func (s entryStore) Get(ctx context.Context, id int64) (*store.Entry, error) {
	return scanEntry(s.d.ro.QueryRowContext(ctx, `SELECT `+entryCols+` FROM entry WHERE id = ?`, id))
}

func (s entryStore) List(ctx context.Context, q store.EntryQuery) ([]store.Entry, string, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	// 列表不带正文：一页 50 条的 content_html 可能有几十 MB。
	var b strings.Builder
	b.WriteString(`SELECT ` + entryCols + ` FROM entry WHERE 1=1`)
	args := []any{}

	if q.FeedID > 0 {
		b.WriteString(` AND feed_id = ?`)
		args = append(args, q.FeedID)
	}
	if q.Unread {
		b.WriteString(` AND read_at IS NULL`)
	}
	if q.Starred {
		b.WriteString(` AND starred_at IS NOT NULL`)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		// ESCAPE '\\' 不能省。escapeLike 会把 % 和 _ 转成 \% 与 \_，
		// 但 SQLite 的 LIKE 默认不把反斜杠当转义符：没有这个子句时
		// 模式里的 \ 只是一个普通字符，搜 "50%" 会变成「含反斜杠」
		// 这种永远匹配不到的形状。
		b.WriteString(` AND (title LIKE ? ESCAPE '\' OR content_text LIKE ? ESCAPE '\')`)
		pat := "%" + escapeLike(s) + "%"
		args = append(args, pat, pat)
	}
	if q.Cursor != "" {
		b.WriteString(` AND id < ?`)
		args = append(args, q.Cursor)
	}
	b.WriteString(` ORDER BY id DESC LIMIT ?`)
	args = append(args, q.Limit+1)

	rows, err := s.d.ro.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	out := make([]store.Entry, 0, q.Limit)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, "", err
		}
		// 查询本身始终带正文列（entryCols 固定），这里按需裁掉：
		// 一页 50 条的 content_html 可能有几十 MB，网页列表用不到。
		// 宿主读内容时传 WithContent=true 保留正文。
		if !q.WithContent {
			e.ContentHTML = ""
			e.ContentText = ""
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(out) > q.Limit {
		out = out[:q.Limit]
		next = fmt.Sprint(out[len(out)-1].ID)
	}
	return out, next, nil
}

// escapeLike 转义 LIKE 的通配符，避免用户输入的 % 变成全表扫描。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func (s entryStore) SetRead(ctx context.Context, id int64, read bool) error {
	var val any
	if read {
		val = now()
	}
	res, err := s.d.rw.ExecContext(ctx, `UPDATE entry SET read_at = ? WHERE id = ?`, val, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s entryStore) SetStarred(ctx context.Context, id int64, starred bool) error {
	var val any
	if starred {
		val = now()
	}
	res, err := s.d.rw.ExecContext(ctx, `UPDATE entry SET starred_at = ? WHERE id = ?`, val, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (s entryStore) BulkSetRead(ctx context.Context, ids []int64, read bool) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 200 {
		return fmt.Errorf("bulk update limited to 200 ids, got %d", len(ids))
	}
	var val any
	if read {
		val = now()
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, val)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE entry SET read_at = ? WHERE id IN (`+placeholders+`)`, args...)
	return err
}

func (s entryStore) UnreadCount(ctx context.Context, feedID int64) (int64, error) {
	var n int64
	var err error
	if feedID > 0 {
		err = s.d.ro.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entry WHERE feed_id = ? AND read_at IS NULL`, feedID).Scan(&n)
	} else {
		err = s.d.ro.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entry WHERE read_at IS NULL`).Scan(&n)
	}
	return n, err
}

func (s entryStore) Purge(ctx context.Context, before int64, keepStarred bool) (int64, error) {
	var total int64
	// 分批删除：一次删几万行会长时间持有写锁，1C2G 上会把接口卡住。
	for {
		var deleted int64
		err := s.d.withTx(ctx, func(tx *sql.Tx) error {
			cond := `AND starred_at IS NULL`
			if !keepStarred {
				cond = ``
			}
			// 先删关联，让 AFTER DELETE 触发器把 ref_count 减回去。
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM entry_blob WHERE entry_id IN (
					SELECT id FROM entry
					 WHERE COALESCE(published_at, fetched_at) < ? `+cond+`
					 LIMIT 500)`, before); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM media_fetch WHERE entry_id IN (
					SELECT id FROM entry
					 WHERE COALESCE(published_at, fetched_at) < ? `+cond+`
					 LIMIT 500)`, before); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
				DELETE FROM entry WHERE id IN (
					SELECT id FROM entry
					 WHERE COALESCE(published_at, fetched_at) < ? `+cond+`
					 LIMIT 500)`, before)
			if err != nil {
				return err
			}
			deleted, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted == 0 {
			return total, nil
		}
	}
}
