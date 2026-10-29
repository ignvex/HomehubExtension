package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type mediaStore struct{ d *DB }

func (d *DB) Media() store.MediaStore { return mediaStore{d} }

func (s mediaStore) Record(ctx context.Context, r *store.MediaRecord) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO media_fetch (entry_id, source_url, sha256, status, error, attempts, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(entry_id, source_url) DO UPDATE SET
		    status = excluded.status,
		    sha256 = excluded.sha256,
		    error = excluded.error,
		    updated_at = excluded.updated_at`,
		r.EntryID, r.SourceURL, r.SHA256, r.Status, r.Error, r.Attempts, r.UpdatedAt)
	return err
}

func (s mediaStore) Retryable(
	ctx context.Context,
	maxAttempts int,
	after int64,
	limit int,
) ([]store.MediaRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT id, entry_id, source_url, sha256, status, error, attempts, updated_at
		  FROM media_fetch
		 WHERE status IN ('pending','failed')
		   AND attempts < ?
		   AND updated_at >= ?
		 ORDER BY updated_at ASC
		 LIMIT ?`, maxAttempts, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.MediaRecord, 0, 32)
	for rows.Next() {
		var r store.MediaRecord
		if err := rows.Scan(&r.ID, &r.EntryID, &r.SourceURL, &r.SHA256,
			&r.Status, &r.Error, &r.Attempts, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s mediaStore) Update(ctx context.Context, r *store.MediaRecord) error {
	_, err := s.d.rw.ExecContext(ctx, `
		UPDATE media_fetch
		   SET status = ?, sha256 = ?, error = ?, attempts = ?, updated_at = ?
		 WHERE id = ?`,
		r.Status, r.SHA256, r.Error, r.Attempts, r.UpdatedAt, r.ID)
	return err
}

func (s mediaStore) ByEntry(ctx context.Context, entryID int64) ([]store.MediaRecord, error) {
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT id, entry_id, source_url, sha256, status, error, attempts, updated_at
		  FROM media_fetch WHERE entry_id = ?`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.MediaRecord, 0, 32)
	for rows.Next() {
		var r store.MediaRecord
		if err := rows.Scan(&r.ID, &r.EntryID, &r.SourceURL, &r.SHA256,
			&r.Status, &r.Error, &r.Attempts, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s mediaStore) SetEntryHTML(ctx context.Context, entryID int64, html, text string) error {
	res, err := s.d.rw.ExecContext(ctx,
		`UPDATE entry SET content_html = ?, content_text = ? WHERE id = ?`, html, text, entryID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

var _ = sql.ErrNoRows
var _ = errors.Is
