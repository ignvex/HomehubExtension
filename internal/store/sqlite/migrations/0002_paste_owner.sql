-- 0002_paste_owner.sql — 片段归属
--
-- 背景：创建片段不再要求登录，因此需要一个不依赖账号的归属标识。
--
-- 方案：owner_hash 存"持有即拥有"凭证的 sha256。
--   * 登录用户创建：凭证是 client id，hash 前缀 "c:" 区分来源
--   * 匿名用户创建：服务端生成随机 token，只在创建响应里返回一次
--
-- 为什么不加一张 owner 表：归属只是一句 "谁能在列表里看到它"，
-- 不需要独立生命周期。存哈希即可，凭证本身只在客户端手里。
--
-- 空字符串表示历史数据（迁移前创建的片段），这类记录只有登录用户
-- 在列表里能看到全部，符合迁移前的行为。

ALTER TABLE paste ADD COLUMN owner_hash TEXT NOT NULL DEFAULT '';

-- 列表按归属过滤，这是最主要的查询路径。
CREATE INDEX idx_paste_owner ON paste(owner_hash, created_at DESC)
  WHERE deleted_at IS NULL;

-- user_version 由 Go 侧迁移器设置，不写在这里。
