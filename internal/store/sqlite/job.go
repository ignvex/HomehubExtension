package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type jobStore struct{ d *DB }

func (d *DB) Job() store.JobStore { return jobStore{d} }

func (s jobStore) Start(ctx context.Context, job string, at int64) (int64, error) {
	res, err := s.d.rw.ExecContext(ctx,
		`INSERT INTO job_run (job, started_at, status) VALUES (?,?, 'running')`, job, at)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s jobStore) Finish(ctx context.Context, id int64, status, detail string, at int64) error {
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE job_run SET finished_at = ?, status = ?, detail = ? WHERE id = ?`,
		at, status, detail, id)
	return err
}

func (s jobStore) Recent(ctx context.Context, limit int) ([]store.JobRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT id, job, started_at, finished_at, status, detail
		  FROM job_run ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 必须是非 nil 的空切片：nil slice 序列化成 JSON 的 null，
	// 而客户端拿到 null 后调 .map()/.length 会直接抛错。
	// 「没有记录」和「没有这个字段」是两回事。
	out := make([]store.JobRun, 0, 16)
	for rows.Next() {
		var j store.JobRun
		var finished sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Job, &j.StartedAt, &finished, &j.Status, &j.Detail); err != nil {
			return nil, err
		}
		j.FinishedAt = ptrInt(finished)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s jobStore) Purge(ctx context.Context, before int64) (int64, error) {
	res, err := s.d.rw.ExecContext(ctx,
		`DELETE FROM job_run WHERE started_at < ? AND status <> 'running'`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Stats 汇总若干计数，供 /api/v1/system/stats 与系统页。
func (d *DB) Stats(ctx context.Context) (store.Stats, error) {
	var s store.Stats
	err := d.ro.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM paste WHERE deleted_at IS NULL),
		  (SELECT COUNT(*) FROM feed),
		  (SELECT COUNT(*) FROM entry),
		  (SELECT COUNT(*) FROM entry WHERE read_at IS NULL),
		  (SELECT COUNT(*) FROM blob),
		  (SELECT COALESCE(SUM(size),0) FROM blob),
		  (SELECT COUNT(*) FROM media_fetch WHERE status IN ('pending','failed'))`).
		Scan(&s.Pastes, &s.Feeds, &s.Entries, &s.Unread, &s.Blobs, &s.BlobBytes, &s.PendingMedia)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	return s, err
}
