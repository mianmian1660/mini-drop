-- 阶段六修正：把 GC 所需 lineage 从会随 raw block 删除的 member 中独立出来。
CREATE TABLE IF NOT EXISTS continuous_migration_receipts (
    id            BIGSERIAL PRIMARY KEY,
    tenant        VARCHAR(64) NOT NULL DEFAULT 'default',
    source_kind   VARCHAR(16) NOT NULL,
    source_ref    VARCHAR(128) NOT NULL,
    session_sid   VARCHAR(64) NOT NULL DEFAULT '',
    signal_type   VARCHAR(32) NOT NULL,
    block_id      VARCHAR(64) NOT NULL,
    bucket_start  TIMESTAMPTZ NOT NULL,
    bucket_end    TIMESTAMPTZ NOT NULL,
    start_time    TIMESTAMPTZ NOT NULL,
    end_time      TIMESTAMPTZ NOT NULL,
    sample_count  BIGINT NOT NULL DEFAULT 0,
    value_total   BIGINT NOT NULL DEFAULT 0,
    row_count     BIGINT NOT NULL DEFAULT 0,
    status        VARCHAR(16) NOT NULL DEFAULT 'passed',
    revoke_reason TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_cmr_source_block
    ON continuous_migration_receipts (source_kind, source_ref, block_id);
CREATE INDEX IF NOT EXISTS idx_cmr_source_signal
    ON continuous_migration_receipts (source_ref, signal_type, status);
CREATE INDEX IF NOT EXISTS idx_cmr_block
    ON continuous_migration_receipts (block_id);

-- 修正阶段六 coverage 唯一键遗漏 tenant。
DROP INDEX IF EXISTS uq_ccs_segment;
CREATE UNIQUE INDEX uq_ccs_segment
    ON continuous_coverage_segments (tenant, session_sid, signal_type, segment_start, segment_end);

-- 019 的 catalog 混合了相邻小时和 histogram subtype，属于可重建派生数据。
-- 新 worker 启动时会从 active raw blocks 立即按精确小时重建。
DELETE FROM continuous_coverage_segments;

-- 部署时为当前仍保有 member 的已验证 raw block 补永久凭证。
INSERT INTO continuous_migration_receipts
    (tenant, source_kind, source_ref, session_sid, signal_type, block_id,
     bucket_start, bucket_end, start_time, end_time, sample_count,
     value_total, row_count, status, created_at, updated_at)
SELECT b.tenant, m.source_kind, m.source_ref, m.session_sid, b.signal_type,
       b.block_id, b.bucket_start, b.bucket_end, m.start_time, m.end_time,
       m.sample_count, m.value_total, m.row_count, 'passed', now(), now()
FROM continuous_parquet_block_members m
JOIN continuous_parquet_blocks b ON b.block_id = m.block_id
WHERE m.source_kind = 'batch'
  AND b.resolution = 'raw'
  AND b.validation = 'passed'
  AND b.reconcile_status = 'passed'
ON CONFLICT (source_kind, source_ref, block_id) DO UPDATE SET
    session_sid = EXCLUDED.session_sid,
    signal_type = EXCLUDED.signal_type,
    start_time = EXCLUDED.start_time,
    end_time = EXCLUDED.end_time,
    sample_count = EXCLUDED.sample_count,
    value_total = EXCLUDED.value_total,
    row_count = EXCLUDED.row_count,
    status = 'passed',
    revoke_reason = '',
    updated_at = now();
