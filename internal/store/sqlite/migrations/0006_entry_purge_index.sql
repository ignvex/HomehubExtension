-- 0006_entry_purge_index.sql — 给 rss-prune 的保留期判定加表达式索引
--
-- Purge 按 COALESCE(published_at, fetched_at) 判断条目是否超期。
-- 没有这条索引时每一批（LIMIT 500）都要全表扫描 entry，
-- 条目上万后在 1 vCPU 上一次清理就是数秒的写锁。
--
-- SQLite 只在查询里的表达式与索引定义「逐字一致」时才使用表达式索引：
-- 列顺序、函数名都要相同。entry.go 的 purgeBatchSQL 写的是同一个表达式，
-- 改动任一处都要同步另一处，并用 EXPLAIN QUERY PLAN 确认仍然命中
-- （db_test.go 的 TestPurgeUsesExpressionIndex 守着这一点）。

CREATE INDEX IF NOT EXISTS idx_entry_age ON entry(COALESCE(published_at, fetched_at));

-- user_version 由 Go 侧的迁移器设置，不写在这里。
