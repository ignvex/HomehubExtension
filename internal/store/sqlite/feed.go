package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type feedStore struct{ d *DB }

func (d *DB) Feed() store.FeedStore { return feedStore{d} }

const feedCols = `id, url, site_url, title, enabled, fetch_interval, etag, last_modified,
                  last_fetch_at, next_fetch_at, last_status, last_error, error_count, created_at`

// feedColsWithUnread 在 feedCols 之后追加未读条目数。
//
// 子查询必须用 feed.id 限定：entry 自己也有 id 列，不限定的话
// `e.feed_id = id` 会在子查询作用域里解析成 entry.id，计数永远是错的。
const feedColsWithUnread = feedCols + `,
                  (SELECT COUNT(*) FROM entry e WHERE e.feed_id = feed.id AND e.read_at IS NULL)`

func scanFeed(row interface{ Scan(...any) error }) (*store.Feed, error) {
	var f store.Feed
	var enabled int
	var lastFetch, nextFetch sql.NullInt64
	err := row.Scan(&f.ID, &f.URL, &f.SiteURL, &f.Title, &enabled, &f.FetchInterval,
		&f.Etag, &f.LastModified, &lastFetch, &nextFetch,
		&f.LastStatus, &f.LastError, &f.ErrorCount, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Enabled = enabled == 1
	f.LastFetchAt = ptrInt(lastFetch)
	f.NextFetchAt = ptrInt(nextFetch)
	return &f, nil
}

// scanFeedWithUnread 对应 feedColsWithUnread。
//
// 两个扫描函数而不是一个可变参数版本：列数固定，多一列少一列在
// 编译期看不出，Scan 只会在运行时报 "expected 15 destination arguments"。
func scanFeedWithUnread(row interface{ Scan(...any) error }) (*store.Feed, error) {
	var f store.Feed
	var enabled int
	var lastFetch, nextFetch sql.NullInt64
	err := row.Scan(&f.ID, &f.URL, &f.SiteURL, &f.Title, &enabled, &f.FetchInterval,
		&f.Etag, &f.LastModified, &lastFetch, &nextFetch,
		&f.LastStatus, &f.LastError, &f.ErrorCount, &f.CreatedAt, &f.UnreadCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Enabled = enabled == 1
	f.LastFetchAt = ptrInt(lastFetch)
	f.NextFetchAt = ptrInt(nextFetch)
	return &f, nil
}

// Upsert 按 url 唯一键插入或更新。
//
// 语义刻意区分字段：Homehub 每次推送都应覆盖调度参数（interval、enabled），
// 但不应清空已经学到的 site_url / title / etag —— 空值视为"没说"，不是"清空"。
//
// next_fetch_at 由调用方算好传入，而不是在这里用 now+interval 重算：
// 只有间隔真的变了才该重排，而「变没变」需要对比旧值，那只有调用方
// 看得到（本函数拿到的是合并后的新值）。
func (s feedStore) Upsert(ctx context.Context, f *store.Feed) (store.Feed, bool, error) {
	var created bool
	err := s.d.withTx(ctx, func(tx *sql.Tx) error {
		var existingID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM feed WHERE url = ?`, f.URL).Scan(&existingID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
			res, err := tx.ExecContext(ctx, `
				INSERT INTO feed (url, site_url, title, source, enabled, fetch_interval,
				                  etag, last_modified, last_fetch_at, next_fetch_at,
				                  last_status, last_error, error_count, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				f.URL, f.SiteURL, f.Title, f.Source, boolInt(f.Enabled), f.FetchInterval,
				f.Etag, f.LastModified, nullInt(f.LastFetchAt), nullInt(f.NextFetchAt),
				f.LastStatus, f.LastError, f.ErrorCount, f.CreatedAt)
			if err != nil {
				return fmt.Errorf("insert feed: %w", err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			f.ID = id
		case err != nil:
			return err
		default:
			created = false
			f.ID = existingID
			_, err := tx.ExecContext(ctx, `
				UPDATE feed SET
				    fetch_interval = ?,
				    enabled        = ?,
				    next_fetch_at  = ?,
				    site_url       = CASE WHEN ? <> '' THEN ? ELSE site_url END,
				    title          = CASE WHEN ? <> '' THEN ? ELSE title END
				WHERE id = ?`,
				f.FetchInterval, boolInt(f.Enabled), nullInt(f.NextFetchAt),
				f.SiteURL, f.SiteURL,
				f.Title, f.Title,
				existingID)
			if err != nil {
				return fmt.Errorf("update feed: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return store.Feed{}, false, err
	}
	got, err := s.Get(ctx, f.ID)
	if err != nil {
		return store.Feed{}, false, err
	}
	return *got, created, nil
}

// Get 带未读计数。
//
// 宿主推送订阅后，响应里的 unread_count 就来自这里——如果只查
// feed 表本身，那个字段永远是 0，而随后的 GET 列表却是有值的。
// 同一个 toHubSubscription 会给出两种结果，对账时最难查。
func (s feedStore) Get(ctx context.Context, id int64) (*store.Feed, error) {
	return scanFeedWithUnread(s.d.ro.QueryRowContext(ctx,
		`SELECT `+feedColsWithUnread+` FROM feed WHERE id = ?`, id))
}

func (s feedStore) GetByURL(ctx context.Context, url string) (*store.Feed, error) {
	return scanFeedWithUnread(s.d.ro.QueryRowContext(ctx,
		`SELECT `+feedColsWithUnread+` FROM feed WHERE url = ?`, url))
}

func (s feedStore) List(ctx context.Context) ([]store.Feed, error) {
	rows, err := s.d.ro.QueryContext(ctx,
		`SELECT `+feedColsWithUnread+` FROM feed ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Feed, 0, 8)
	for rows.Next() {
		f, err := scanFeedWithUnread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s feedStore) Update(ctx context.Context, f *store.Feed) error {
	res, err := s.d.rw.ExecContext(ctx, `
		UPDATE feed SET site_url=?, title=?, enabled=?, fetch_interval=?, etag=?,
		                last_modified=?, last_fetch_at=?, next_fetch_at=?,
		                last_status=?, last_error=?, error_count=?
		 WHERE id=?`,
		f.SiteURL, f.Title, boolInt(f.Enabled), f.FetchInterval, f.Etag,
		f.LastModified, nullInt(f.LastFetchAt), nullInt(f.NextFetchAt),
		f.LastStatus, f.LastError, f.ErrorCount, f.ID)
	if err != nil {
		return fmt.Errorf("update feed: %w", err)
	}
	return requireAffected(res)
}

// Delete 先删 entry_blob 再删 entry，最后删 feed。
//
// 不能只靠 ON DELETE CASCADE：SQLite 的外键动作不会激活触发器，
// blob.ref_count 只增不减，文件永远回收不掉。
func (s feedStore) Delete(ctx context.Context, id int64) error {
	return s.d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM entry_blob
			 WHERE entry_id IN (SELECT id FROM entry WHERE feed_id = ?)`, id); err != nil {
			return err
		}
		// media_fetch 必须在 entry 之前删：删掉 entry 后再按 feed_id 子查询就取不到 id 了。
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM media_fetch
			 WHERE entry_id IN (SELECT id FROM entry WHERE feed_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM entry WHERE feed_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM feed WHERE id = ?`, id)
		if err != nil {
			return err
		}
		return requireAffected(res)
	})
}

func (s feedStore) Due(ctx context.Context, nowSec int64, limit int) ([]store.Feed, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT `+feedCols+` FROM feed
		 WHERE enabled = 1 AND (next_fetch_at IS NULL OR next_fetch_at <= ?)
		 ORDER BY COALESCE(next_fetch_at, 0) ASC
		 LIMIT ?`, nowSec, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Feed, 0, 8)
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}
