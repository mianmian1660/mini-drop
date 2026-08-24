-- 阶段六修正：为 migration receipt GC 建立按 source_kind + updated_at + id
-- 的扫描索引。receipt GC 每轮扫描 source_kind='batch' 且超过保留期
-- （CONTINUOUS_MIGRATION_RECEIPT_RETENTION_HOURS，默认 72h）且对应
-- profile_batches.bid 已不存在的 receipt 行，按 updated_at 升序分批删除，
-- 防止 receipt 随业务长期运行无界增长。
CREATE INDEX IF NOT EXISTS idx_cmr_gc_scan
    ON continuous_migration_receipts (source_kind, updated_at, id);
