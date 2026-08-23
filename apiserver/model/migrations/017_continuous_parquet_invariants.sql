-- ============================================================
-- 017_continuous_parquet_invariants.sql — 阶段五 v2 不变量（additive）
-- ============================================================
-- 1) (tenant, bucket_start, signal, resolution) 仅允许一个 active 版本。
--    用部分唯一索引实现：同分区只允许一个 status='active' 行。
--    PostgreSQL 部分唯一索引：WHERE status = 'active'。
-- 2) object_key 全局唯一（物理文件防重）。
-- 3) active 必须是 validation='passed'：
--    用 EXCLUDE 或 CHECK 无法直接跨列约束，这里用部分唯一索引
--    UNIQUE (tenant, bucket_start, signal, resolution) WHERE status='active'
--    由应用层保证 active 只会在校验通过后写入；同时建立
--    building/failed/superseded/deleting 的清理索引。
-- ============================================================

-- 1) 每分区最多一个 active 版本（部分唯一索引）
CREATE UNIQUE INDEX IF NOT EXISTS uq_cpq_active_partition
    ON continuous_parquet_blocks (tenant, bucket_start, signal_type, resolution)
    WHERE status = 'active';

-- 2) object key 全局唯一（block_files 已有，这里冗余双保险同列已建；
--    实际唯一约束建在 016 的 uq_continuous_parquet_block_files_object_key）

-- 3) 清理索引：非 active 状态的回收扫描（superseded/deleting/failed/building）
CREATE INDEX IF NOT EXISTS idx_cpq_cleanup_building
    ON continuous_parquet_blocks (created_at) WHERE status = 'building';

CREATE INDEX IF NOT EXISTS idx_cpq_cleanup_validating
    ON continuous_parquet_blocks (created_at) WHERE status = 'validating';

CREATE INDEX IF NOT EXISTS idx_cpq_cleanup_failed
    ON continuous_parquet_blocks (updated_at) WHERE status = 'failed';

CREATE INDEX IF NOT EXISTS idx_cpq_cleanup_superseded
    ON continuous_parquet_blocks (superseded_at) WHERE status = 'superseded';

CREATE INDEX IF NOT EXISTS idx_cpq_cleanup_deleting
    ON continuous_parquet_blocks (updated_at) WHERE status = 'deleting';
