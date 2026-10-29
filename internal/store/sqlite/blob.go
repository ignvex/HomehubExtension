package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type blobStore struct{ d *DB }

func (d *DB) Blob() store.BlobStore { return blobStore{d} }

func (s blobStore) Upsert(ctx context.Context, b *store.Blob) error {
	// 同一内容被重复下载时只更新 last_seen_at，不动 ref_count ——
	// 引用计数由 entry_blob 的触发器维护，这里干预会重复计数。
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO blob (sha256, size, mime, ext, kind, ref_count, created_at, last_seen_at)
		VALUES (?,?,?,?,?,0,?,?)
		ON CONFLICT(sha256) DO UPDATE SET
		    last_seen_at = excluded.last_seen_at,
		    mime = excluded.mime`,
		b.SHA256, b.Size, b.MIME, b.Ext, b.Kind, b.CreatedAt, b.LastSeenAt)
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
	_, err := s.d.rw.ExecContext(ctx,
		`INSERT INTO entry_blob (entry_id, sha256) VALUES (?,?) ON CONFLICT DO NOTHING`, entryID, sha)
	return err
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
