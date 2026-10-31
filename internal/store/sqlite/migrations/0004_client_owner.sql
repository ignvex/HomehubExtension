-- 0004_client_owner.sql — 长期密钥共用同一个身份
--
-- 背景：访问密钥在 0003 里被建模成"每把密钥一台无主客户端"，
-- owner_hash 由 client id 推导。这在单主人场景下有个副作用：
-- 轮换密钥（撤销旧的、签发新的）之后，旧密钥名下的私有片段
-- 就再也无法访问了——它们没被删，只是新密钥推不出同一个哈希。
--
-- 修法：给 client 加一个稳定的 owner 标识。设备配对产生的 client
-- 保持为空（继续按设备隔离，行为不变）；访问密钥产生的 client 填
-- 同一个实例级标识，于是同一主人的多把密钥共享归属。
--
-- SQLite 的 ALTER TABLE ADD COLUMN 不重建表，STRICT 表也支持，
-- 所以这次迁移没有数据搬迁，也没有停机窗口。

ALTER TABLE auth_client ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';

-- user_version 由 Go 侧的迁移器设置，不写在这里。
