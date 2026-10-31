package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ignvex/HomehubExtension/internal/store"
)

type authStore struct{ d *DB }

func (d *DB) Auth() store.AuthStore { return authStore{d} }

// ---- 设备 ----

func (s authStore) CreateClient(ctx context.Context, c *store.Client) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO auth_client (id, name, public_key, algorithm, owner_id, created_at)
		VALUES (?,?,?,?,?,?)`,
		c.ID, c.Name, c.PublicKey, c.Algorithm, c.OwnerID, c.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrConflict
		}
		return fmt.Errorf("insert client: %w", err)
	}
	return nil
}

func (s authStore) GetClient(ctx context.Context, id string) (*store.Client, error) {
	var c store.Client
	var lastSeen, revoked sql.NullInt64
	err := s.d.ro.QueryRowContext(ctx, `
		SELECT id, name, public_key, algorithm, owner_id, created_at, last_seen_at, revoked_at
		  FROM auth_client WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.PublicKey, &c.Algorithm, &c.OwnerID, &c.CreatedAt, &lastSeen, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.LastSeenAt = ptrInt(lastSeen)
	c.RevokedAt = ptrInt(revoked)
	return &c, nil
}

func (s authStore) ListClients(ctx context.Context) ([]store.Client, error) {
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT id, name, public_key, algorithm, owner_id, created_at, last_seen_at, revoked_at
		  FROM auth_client ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Client, 0, 8)
	for rows.Next() {
		var c store.Client
		var lastSeen, revoked sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.PublicKey, &c.Algorithm, &c.OwnerID,
			&c.CreatedAt, &lastSeen, &revoked); err != nil {
			return nil, err
		}
		c.LastSeenAt = ptrInt(lastSeen)
		c.RevokedAt = ptrInt(revoked)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s authStore) RevokeClient(ctx context.Context, id string, nowSec int64) error {
	return s.d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE auth_client SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, nowSec, id)
		if err != nil {
			return err
		}
		if err := requireAffected(res); err != nil {
			return err
		}
		// 撤销设备必须同时清掉它的会话与挑战，否则旧 refresh token 还能续命。
		if _, err := tx.ExecContext(ctx,
			`UPDATE session SET revoked_at = ? WHERE client_id = ? AND revoked_at IS NULL`,
			nowSec, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM auth_challenge WHERE client_id = ?`, id)
		return err
	})
}

func (s authStore) TouchClient(ctx context.Context, id string, nowSec int64) error {
	_, err := s.d.rw.ExecContext(ctx, `UPDATE auth_client SET last_seen_at = ? WHERE id = ?`, nowSec, id)
	return err
}

// ---- 挑战 ----

func (s authStore) CreateChallenge(ctx context.Context, ch *store.Challenge) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO auth_challenge (nonce, client_id, created_at, expires_at)
		VALUES (?,?,?,?)`, ch.Nonce, ch.ClientID, ch.CreatedAt, ch.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrConflict
		}
		return err
	}
	return nil
}

// ConsumeChallenge 在一个语句里完成"校验并标记已用"，避免两个并发请求都通过。
func (s authStore) ConsumeChallenge(ctx context.Context, nonce string, nowSec int64) (*store.Challenge, error) {
	var ch store.Challenge
	var consumed sql.NullInt64
	err := s.d.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT nonce, client_id, created_at, expires_at, consumed_at
			  FROM auth_challenge WHERE nonce = ?`, nonce).
			Scan(&ch.Nonce, &ch.ClientID, &ch.CreatedAt, &ch.ExpiresAt, &consumed)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if consumed.Valid || ch.ExpiresAt <= nowSec {
			return store.ErrNotFound
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE auth_challenge SET consumed_at = ? WHERE nonce = ? AND consumed_at IS NULL`,
			nowSec, nonce)
		if err != nil {
			return err
		}
		return requireAffected(res)
	})
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// ---- 会话 ----

func (s authStore) CreateSession(ctx context.Context, sess *store.Session) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO session (token_hash, client_id, created_at, expires_at, last_used_at, user_agent)
		VALUES (?,?,?,?,?,?)`,
		sess.TokenHash, sess.ClientID, sess.CreatedAt, sess.ExpiresAt, sess.LastUsedAt, sess.UserAgent)
	return err
}

func (s authStore) GetSession(ctx context.Context, tokenHash []byte) (*store.Session, error) {
	var sess store.Session
	var revoked sql.NullInt64
	err := s.d.ro.QueryRowContext(ctx, `
		SELECT token_hash, client_id, created_at, expires_at, last_used_at, user_agent, revoked_at
		  FROM session WHERE token_hash = ?`, tokenHash).
		Scan(&sess.TokenHash, &sess.ClientID, &sess.CreatedAt, &sess.ExpiresAt,
			&sess.LastUsedAt, &sess.UserAgent, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.RevokedAt = ptrInt(revoked)
	return &sess, nil
}

func (s authStore) TouchSession(ctx context.Context, tokenHash []byte, nowSec int64) error {
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE session SET last_used_at = ? WHERE token_hash = ?`, nowSec, tokenHash)
	return err
}

func (s authStore) RevokeSession(ctx context.Context, tokenHash []byte, nowSec int64) error {
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE session SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`,
		nowSec, tokenHash)
	return err
}

func (s authStore) RevokeClientSessions(ctx context.Context, clientID string, nowSec int64) error {
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE session SET revoked_at = ? WHERE client_id = ? AND revoked_at IS NULL`,
		nowSec, clientID)
	return err
}

func (s authStore) PurgeSessions(ctx context.Context, nowSec int64) (int64, error) {
	var total int64
	res, err := s.d.rw.ExecContext(ctx,
		`DELETE FROM session WHERE expires_at <= ? OR revoked_at IS NOT NULL`, nowSec)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	total += n

	res, err = s.d.rw.ExecContext(ctx,
		`DELETE FROM auth_challenge WHERE expires_at <= ? OR consumed_at IS NOT NULL`, nowSec)
	if err != nil {
		return total, err
	}
	n, _ = res.RowsAffected()
	return total + n, nil
}

// ---- 配对 ----

func (s authStore) CreateEnrollment(ctx context.Context, e *store.Enrollment) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO enrollment (token_hash, name_hint, created_at, expires_at)
		VALUES (?,?,?,?)`, e.TokenHash, e.NameHint, e.CreatedAt, e.ExpiresAt)
	return err
}

func (s authStore) ConsumeEnrollment(ctx context.Context, tokenHash []byte, nowSec int64) (*store.Enrollment, error) {
	var e store.Enrollment
	var used sql.NullInt64
	err := s.d.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT token_hash, name_hint, created_at, expires_at, used_at
			  FROM enrollment WHERE token_hash = ?`, tokenHash).
			Scan(&e.TokenHash, &e.NameHint, &e.CreatedAt, &e.ExpiresAt, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if used.Valid || e.ExpiresAt <= nowSec {
			return store.ErrNotFound
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE enrollment SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
			nowSec, tokenHash)
		if err != nil {
			return err
		}
		return requireAffected(res)
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s authStore) PurgeEnrollments(ctx context.Context, nowSec int64) (int64, error) {
	res, err := s.d.rw.ExecContext(ctx,
		`DELETE FROM enrollment WHERE expires_at <= ? OR used_at IS NOT NULL`, nowSec)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- 宿主签发的长期密钥 ----

func (s authStore) CreateAccessKey(ctx context.Context, k *store.AccessKey) error {
	_, err := s.d.rw.ExecContext(ctx, `
		INSERT INTO access_key (id, name, key_hash, client_id, created_at, expires_at)
		VALUES (?,?,?,?,?,?)`,
		k.ID, k.Name, k.KeyHash, k.ClientID, k.CreatedAt, nullInt(k.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrConflict
		}
		return fmt.Errorf("insert access key: %w", err)
	}
	return nil
}

// GetAccessKeyByHash 按明文哈希查可用密钥。
//
// 已撤销与已过期在这里就被过滤掉，返回 ErrNotFound——调用方不需要
// 再判断一次 Live()，也不会因为漏判把过期密钥放进来。
func (s authStore) GetAccessKeyByHash(ctx context.Context, keyHash []byte, nowSec int64) (*store.AccessKey, error) {
	var k store.AccessKey
	var expires, lastUsed, revoked sql.NullInt64
	err := s.d.ro.QueryRowContext(ctx, `
		SELECT id, name, key_hash, client_id, created_at, expires_at, last_used_at, revoked_at
		  FROM access_key WHERE key_hash = ?`, keyHash).
		Scan(&k.ID, &k.Name, &k.KeyHash, &k.ClientID, &k.CreatedAt, &expires, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.ExpiresAt = ptrInt(expires)
	k.LastUsedAt = ptrInt(lastUsed)
	k.RevokedAt = ptrInt(revoked)
	if !k.Live(nowSec) {
		return nil, store.ErrNotFound
	}
	return &k, nil
}

func (s authStore) ListAccessKeys(ctx context.Context) ([]store.AccessKey, error) {
	rows, err := s.d.ro.QueryContext(ctx, `
		SELECT id, name, key_hash, client_id, created_at, expires_at, last_used_at, revoked_at
		  FROM access_key ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.AccessKey, 0, 4)
	for rows.Next() {
		var k store.AccessKey
		var expires, lastUsed, revoked sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.ClientID, &k.CreatedAt,
			&expires, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		k.ExpiresAt = ptrInt(expires)
		k.LastUsedAt = ptrInt(lastUsed)
		k.RevokedAt = ptrInt(revoked)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s authStore) TouchAccessKey(ctx context.Context, id string, nowSec int64) error {
	_, err := s.d.rw.ExecContext(ctx,
		`UPDATE access_key SET last_used_at = ? WHERE id = ?`, nowSec, id)
	return err
}

// RevokeAccessKey 撤销密钥，并连带撤销它绑定的无主客户端。
//
// 两件事必须在同一个事务里：只撤销密钥的话，之前换到的 refresh token
// 还能继续续期——"撤销"就成了一个只对新登录生效的假动作。
func (s authStore) RevokeAccessKey(ctx context.Context, id string, nowSec int64) error {
	return s.d.withTx(ctx, func(tx *sql.Tx) error {
		var clientID string
		err := tx.QueryRowContext(ctx,
			`SELECT client_id FROM access_key WHERE id = ?`, id).Scan(&clientID)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE access_key SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, nowSec, id)
		if err != nil {
			return err
		}
		if err := requireAffected(res); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE auth_client SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			nowSec, clientID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE session SET revoked_at = ?
			  WHERE client_id = ? AND revoked_at IS NULL`, nowSec, clientID)
		return err
	})
}
