-- 0003_access_key.sql — 宿主签发的长期访问密钥
--
-- 背景：原来只有一条拿凭证的路——SSH 到服务器跑 `enroll` 生成配对码，
-- 用户再在浏览器里输码、生成设备密钥、做一次挑战-响应。宿主要给
-- 用户发一把自己就能登录的凭证，隔着这层做不到。
--
-- 设计：每把密钥绑定一台「无主客户端」。签发时 Extension 生成一对
-- Ed25519 密钥、把公钥写进 auth_client、私钥当场丢弃。于是：
--
--   1. auth_client 的约束一个都不用改（algorithm 仍是 ed25519，
--      public_key 仍是合法公钥），不触发 SQLite 重建表；
--   2. 这台 client 永远无法用挑战-响应登录——私钥在签发那一刻
--      就没了，谁也签不出消息；
--   3. 密钥兑换时直接拿它绑定的 client 走现有的 issue()，
--      会话、access token、CSRF、owner_hash 全部无需改动；
--   4. 撤销密钥时连带撤销 client，已签发的会话立即失效。
--
-- 只存 key 的 sha256，明文只在签发响应里出现一次。与配对码一致。

CREATE TABLE access_key (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  -- sha256(规范化后的明文)。UNIQUE 自带索引，兑换时按它查。
  key_hash     BLOB NOT NULL UNIQUE,
  -- 绑定的无主客户端。撤销密钥时连带撤销它。
  client_id    TEXT NOT NULL REFERENCES auth_client(id),
  created_at   INTEGER NOT NULL,
  -- NULL 表示永不过期。宿主签发时可以指定 TTL。
  expires_at   INTEGER,
  last_used_at INTEGER,
  revoked_at   INTEGER
) STRICT;

CREATE INDEX idx_access_key_client ON access_key(client_id);

-- user_version 由 Go 侧的迁移器设置，不写在这里。
