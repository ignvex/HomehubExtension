package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type blobStore struct{ d *DB }

func (d *DB) Blob() store.BlobStore { return blobStore{d} }

// execer 让同一段 SQL 既能跑在连接上，也能跑在事务里。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// 同一内容被重复下载时只更新 last_seen_at，不动 ref_count ——
// 引用计数由 entry_blob 的触发器维护，这里干预会重复计数。
const upsertBlobSQL = `
		INSERT INTO blob (sha256, size, mime, ext, kind, ref_count, created_at, last_seen_at)
		VALUES (?,?,?,?,?,0,?,?)
		ON CONFLICT(sha256) DO UPDATE SET
		    last_seen_at = excluded.last_seen_at,
		    mime = excluded.mime`

const linkBlobSQL = `INSERT INTO entry_blob (entry_id, sha256) VALUES (?,?) ON CONFLICT DO NOTHING`

// recordMediaSQL 与 mediaStore.Record 保持同一语义。
const recordMediaSQL = `
		INSERT INTO media_fetch (entry_id, source_url, sha256, status, error, attempts, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(entry_id, source_url) DO UPDATE SET
		    status = excluded.status,
		    sha256 = excluded.sha256,
		    error = excluded.error,
		    updated_at = excluded.updated_at`

func upsertBlob(ctx context.Context, x execer, b *store.Blob) error {
	_, err := x.ExecContext(ctx, upsertBlobSQL,
		b.SHA256, b.Size, b.MIME, b.Ext, b.Kind, b.CreatedAt, b.LastSeenAt)
	return err
}

func (s blobStore) Upsert(ctx context.Context, b *store.Blob) error {
	err := upsertBlob(ctx, s.d.rw, b)
	if err != nil {
		return fmt.Errorf("upsert blob: %w", err)
	}
	return nil
}

func (s blobStore) Get(ctx context.Context, sha string) (*store.Blob, error) {
	var b store.Blob
	err := s.d.ro.QueryRowContext(ctx, `
		SELECT sha256, size, mime, ext, kind, ref_count, created_at, last_seen_at
		  FROM blob WHERE sha256 = ?`, sha).
		Scan(&b.SHA256, &b.Size, &b.MIME, &b.Ext, &b.Kind, &b.RefCount, &b.CreatedAt, &b.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s blobStore) Link(ctx context.Context, entryID int64, sha string) error {
	_, err := s.d.rw.ExecContext(ctx, linkBlobSQL, entryID, sha)
	return err
}

func (s blobStore) SaveEntryMedia(
	ctx context.Context, entryID int64, blobs []store.Blob, records []store.MediaRecord,
) error {
	if len(blobs) == 0 && len(records) == 0 {
		return nil
	}
	return s.d.withTx(ctx, func(tx *sql.Tx) error {
		// blob 行必须先于 entry_blob：后者有外键与 ref_count 触发器。
		for i := range blobs {
			if err := upsertBlob(ctx, tx, &blobs[i]); err != nil {
				return fmt.Errorf("upsert blob %s: %w", blobs[i].SHA256, err)
			}
			if _, err := tx.ExecContext(ctx, linkBlobSQL, entryID, blobs[i].SHA256); err != nil {
				return fmt.Errorf("link blob %s: %w", blobs[i].SHA256, err)
			}
		}
		for _, r := range records {
			if _, err := tx.ExecContext(ctx, recordMediaSQL,
				entryID, r.SourceURL, r.SHA256, r.Status, r.Error, r.Attempts, r.UpdatedAt); err != nil {
				return fmt.Errorf("record media: %w", err)
			}
		}
		return nil
	})
}

func (s blobStore) Known(ctx context.Context, shas []string) (map[string]bool, error) {
	out := make(map[string]bool, len(shas))
	if len(shas) == 0 {
		return out, nil
	}
	if len(shas) > 500 {
		return nil, fmt.Errorf("known: limited to 500 shas, got %d", len(shas))
	}
	args := make([]any, len(shas))
	for i, sha := range shas {
		args[i] = sha
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(shas)), ",")
	// 占位符由 len(shas) 生成，不含用户输入；每个 sha 走参数绑定。
	query := `SELECT sha256 FROM blob WHERE sha256 IN (` + placeholders + `)` // #nosec G202 -- 占位符拼接，非用户输入
	rows, err := s.d.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

func (s blobStore) Unreferenced(ctx context.Context, before int64, limit int) ([]store.Blob, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT sha256, size, mime, ext, kind, ref_count, created_at, last_seen_at
		  FROM blob WHERE ref_count = 0 AND last_seen_at <= ?
		  ORDER BY last_seen_at LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Blob, 0, limit)
	for rows.Next() {
		var b store.Blob
		if err := rows.Scan(&b.SHA256, &b.Size, &b.MIME, &b.Ext, &b.Kind,
			&b.RefCount, &b.CreatedAt, &b.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s blobStore) DeleteUnreferenced(ctx context.Context, sha string, before int64) (bool, error) {
	res, err := s.d.rw.ExecContext(ctx,
		`DELETE FROM blob WHERE sha256 = ? AND ref_count = 0 AND last_seen_at <= ?`, sha, before)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s blobStore) Delete(ctx context.Context, sha string) error {
	return s.d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM entry_blob WHERE sha256 = ?`, sha); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM blob WHERE sha256 = ?`, sha)
		return err
	})
}

func (s blobStore) TotalSize(ctx context.Context) (int64, error) {
	var n int64
	err := s.d.ro.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM blob`).Scan(&n)
	return n, err
}

func (s blobStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.d.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob`).Scan(&n)
	return n, err
}

// Recalibrate 用 entry_blob 的真实行数重算 ref_count。
//
// blob-gc 每天做一次兜底：即便某条删除路径漏了显式清理，
// 计数也会在一天内自愈，不会永久泄漏。
func (s blobStore) Recalibrate(ctx context.Context) (int64, error) {
	res, err := s.d.rw.ExecContext(ctx, `
		UPDATE blob SET ref_count = (
		    SELECT COUNT(*) FROM entry_blob WHERE entry_blob.sha256 = blob.sha256
		)
		WHERE ref_count <> (
		    SELECT COUNT(*) FROM entry_blob WHERE entry_blob.sha256 = blob.sha256
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
