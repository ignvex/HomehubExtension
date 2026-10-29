-- 0001_init.sql — 初始 schema
--
-- 约定：本文件在首次发布前可以自由修改（尚未有任何部署应用过它）。
-- 一旦打上版本号发布，任何 schema 变更都必须新增 0002_xxx.sql，
-- 直接改这里会让已经部署的库与代码不一致，而且迁移器不会重跑。
--
-- 约定：
--   * 时间一律 Unix 秒（INTEGER），不用 TEXT。
--   * 布尔用 INTEGER 0/1 并加 CHECK。
--   * 所有表 STRICT，字段类型错误在写入时就报，而不是静默转换。

CREATE TABLE paste (
  id              TEXT PRIMARY KEY,
  slug            TEXT NOT NULL UNIQUE,
  title           TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL,
  language        TEXT NOT NULL DEFAULT 'text',
  visibility      TEXT NOT NULL CHECK (visibility IN ('public','unlisted','private')),
  password_hash   TEXT NOT NULL DEFAULT '',
  expires_at      INTEGER,
  burn_after_read INTEGER NOT NULL DEFAULT 0 CHECK (burn_after_read IN (0,1)),
  size_bytes      INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  deleted_at      INTEGER
) STRICT;

CREATE INDEX idx_paste_expires ON paste(expires_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_paste_created ON paste(created_at DESC);

CREATE TABLE feed (
  id             INTEGER PRIMARY KEY,
  url            TEXT NOT NULL UNIQUE,
  site_url       TEXT NOT NULL DEFAULT '',
  title          TEXT NOT NULL DEFAULT '',
  source         TEXT NOT NULL DEFAULT 'hub' CHECK (source IN ('hub','manual')),
  enabled        INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  fetch_interval INTEGER NOT NULL,
  etag           TEXT NOT NULL DEFAULT '',
  last_modified  TEXT NOT NULL DEFAULT '',
  last_fetch_at  INTEGER,
  next_fetch_at  INTEGER,
  last_status    INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  error_count    INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_feed_due ON feed(next_fetch_at) WHERE enabled = 1;

CREATE TABLE entry (
  id           INTEGER PRIMARY KEY,
  feed_id      INTEGER NOT NULL REFERENCES feed(id),
  guid         TEXT NOT NULL,
  url          TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  author       TEXT NOT NULL DEFAULT '',
  published_at INTEGER,
  fetched_at   INTEGER NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  content_html TEXT NOT NULL DEFAULT '',
  content_text TEXT NOT NULL DEFAULT '',
  read_at      INTEGER,
  starred_at   INTEGER,
  UNIQUE (feed_id, guid)
) STRICT;

CREATE INDEX idx_entry_feed_pub ON entry(feed_id, published_at DESC);
CREATE INDEX idx_entry_pub ON entry(published_at DESC);
CREATE INDEX idx_entry_unread ON entry(feed_id) WHERE read_at IS NULL;

CREATE TABLE blob (
  sha256       TEXT PRIMARY KEY,
  size         INTEGER NOT NULL,
  mime         TEXT NOT NULL,
  ext          TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL CHECK (kind IN ('image','audio','video','font','attachment','other')),
  ref_count    INTEGER NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_blob_unref ON blob(ref_count, last_seen_at) WHERE ref_count = 0;

CREATE TABLE entry_blob (
  entry_id INTEGER NOT NULL REFERENCES entry(id),
  sha256   TEXT NOT NULL REFERENCES blob(sha256),
  PRIMARY KEY (entry_id, sha256)
) STRICT, WITHOUT ROWID;

CREATE TABLE media_fetch (
  id         INTEGER PRIMARY KEY,
  entry_id   INTEGER NOT NULL REFERENCES entry(id),
  source_url TEXT NOT NULL,
  sha256     TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL CHECK (status IN
    ('pending','ok','failed','too_large','blocked','unsupported','quota_exceeded')),
  error      TEXT NOT NULL DEFAULT '',
  attempts   INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  UNIQUE (entry_id, source_url)
) STRICT;

CREATE INDEX idx_media_fetch_retry ON media_fetch(status, updated_at)
  WHERE status IN ('pending','failed');
CREATE INDEX idx_media_fetch_entry ON media_fetch(entry_id);

CREATE TABLE auth_client (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  public_key  BLOB NOT NULL,
  algorithm   TEXT NOT NULL DEFAULT 'ed25519' CHECK (algorithm IN ('ed25519','p256')),
  created_at  INTEGER NOT NULL,
  last_seen_at INTEGER,
  revoked_at  INTEGER
) STRICT;

CREATE TABLE auth_challenge (
  nonce       TEXT PRIMARY KEY,
  client_id   TEXT NOT NULL REFERENCES auth_client(id),
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  consumed_at INTEGER
) STRICT;

CREATE INDEX idx_challenge_expiry ON auth_challenge(expires_at);

CREATE TABLE session (
  token_hash   BLOB PRIMARY KEY,
  client_id    TEXT NOT NULL REFERENCES auth_client(id),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT '',
  revoked_at   INTEGER
) STRICT;

CREATE INDEX idx_session_client ON session(client_id);
CREATE INDEX idx_session_expiry ON session(expires_at);

CREATE TABLE enrollment (
  token_hash BLOB PRIMARY KEY,
  name_hint  TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
) STRICT;

CREATE TABLE job_run (
  id          INTEGER PRIMARY KEY,
  job         TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  status      TEXT NOT NULL CHECK (status IN ('running','ok','failed','timeout','skipped')),
  detail      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_job_run_started ON job_run(started_at DESC);

-- app_pref 保存工具台里每个应用的轻量偏好（排序、是否隐藏）。
-- 应用本身由代码注册，不落库；这张表只存用户的选择。
CREATE TABLE app_pref (
  app_id     TEXT PRIMARY KEY,
  pinned     INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
  hidden     INTEGER NOT NULL DEFAULT 0 CHECK (hidden IN (0,1)),
  sort_order INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
) STRICT;

-- ---- 引用计数 ----
--
-- 只有 INSERT 触发器。删除路径一律在应用层显式执行：
--     DELETE FROM entry_blob WHERE entry_id = ?;
--     DELETE FROM entry WHERE id = ?;
-- 原因是 SQLite 的外键 CASCADE 动作不会激活触发器（除非开 recursive_triggers），
-- 依赖 CASCADE 会让 ref_count 只增不减，blob 永远回收不掉。
-- blob-gc 每日还会全量校准一次，作为兜底。

CREATE TRIGGER entry_blob_ins AFTER INSERT ON entry_blob BEGIN
  UPDATE blob SET ref_count = ref_count + 1, last_seen_at = CAST(strftime('%s','now') AS INTEGER)
   WHERE sha256 = NEW.sha256;
END;

CREATE TRIGGER entry_blob_del AFTER DELETE ON entry_blob BEGIN
  UPDATE blob SET ref_count = MAX(0, ref_count - 1) WHERE sha256 = OLD.sha256;
END;

-- user_version 由 Go 侧的迁移执行器设置，不写在这里：
-- 事务中的 PRAGMA 与驱动包裹的行为不同，集中在一处更容易推理。
