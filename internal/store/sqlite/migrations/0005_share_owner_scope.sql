-- 0005_share_owner_scope.sql — 设备与访问密钥共享同一个归属
--
-- 0004 只让访问密钥之间共享归属，配对设备仍然按 client id 隔离。
-- 实际使用中这很别扭：用密钥建的私有片段，配对的浏览器看不到；
-- 反过来也一样。同一个人不该因为换了入口就看不到自己的东西。
--
-- 这一步把所有既有 client 的 owner_id 补成实例主人标识。SQL 部分
-- 只改 client 表；粘贴的 owner_hash 重算需要 sha256，SQLite 没有
-- 内置函数，由 Go 侧的数据搬迁（migrate.go 的 dataFixes[5]）在同一
-- 事务里完成——见那条注释。

UPDATE auth_client SET owner_id = 'primary' WHERE owner_id = '';

-- user_version 由 Go 侧的迁移器设置，不写在这里。
