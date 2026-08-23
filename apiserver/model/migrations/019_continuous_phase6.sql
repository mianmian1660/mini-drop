-- ============================================================
-- 019_continuous_phase6.sql — 阶段六：细粒度目录瘦身与 Parquet 全量接管
-- ============================================================
-- 全部为 additive 迁移，不直接删除历史数据：
--   1) continuous_parquet_blocks 增加对账状态（reconcile_status 等）
--   2) continuous_parquet_block_members 补充 session_sid / row_count 审计字段
--   3) continuous_coverage_segments：精确覆盖区间（独立于 raw Block 生命周期）
--   4) continuous_migration_failures：迁移/读取失败异常记录
--   5) profile_windows.batch_bid → profile_batches.bid ON DELETE CASCADE 外键：
--      NOT VALID 安装 → 现有 orphan 转异常表并清理 → VALIDATE CONSTRAINT
-- 重放幂等（所有 DDL 带 IF NOT EXISTS / 约束存在性检查）。
-- ============================================================

-- 1) Parquet Block 对账状态
ALTER TABLE continuous_parquet_blocks ADD COLUMN IF NOT EXISTS reconcile_status VARCHAR(16) NOT NULL DEFAULT 'pending';
ALTER TABLE continuous_parquet_blocks ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ;
ALTER TABLE continuous_parquet_blocks ADD COLUMN IF NOT EXISTS reconcile_error TEXT;

CREATE INDEX IF NOT EXISTS idx_cpq_reconcile_status
    ON continuous_parquet_blocks (reconcile_status)
    WHERE status = 'active';

-- 2) Block Member 审计字段
ALTER TABLE continuous_parquet_block_members ADD COLUMN IF NOT EXISTS session_sid VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE continuous_parquet_block_members ADD COLUMN IF NOT EXISTS row_count BIGINT NOT NULL DEFAULT 0;

-- 3) continuous_coverage_segments：精确覆盖区间
CREATE TABLE IF NOT EXISTS continuous_coverage_segments (
    id             BIGSERIAL PRIMARY KEY,
    tenant         VARCHAR(64) NOT NULL DEFAULT 'default',
    session_sid    VARCHAR(64) NOT NULL,
    signal_type    VARCHAR(32) NOT NULL,
    segment_start  TIMESTAMPTZ NOT NULL,
    segment_end    TIMESTAMPTZ NOT NULL,
    sample_count   BIGINT NOT NULL DEFAULT 0,
    source_block   VARCHAR(64) NOT NULL DEFAULT '',
    source_version INTEGER NOT NULL DEFAULT 1,
    resolution     VARCHAR(16) NOT NULL DEFAULT 'raw',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 唯一约束：同一 (session, signal, start, end) 不允许重复 segment
CREATE UNIQUE INDEX IF NOT EXISTS uq_ccs_segment
    ON continuous_coverage_segments (session_sid, signal_type, segment_start, segment_end);

-- 范围查询索引（timeline / 对账按 session+signal+时间范围扫描）
CREATE INDEX IF NOT EXISTS idx_ccs_range
    ON continuous_coverage_segments (session_sid, signal_type, segment_start);

-- 过期扫描（segment 保留 30 天，按 segment_end 排序回收）
CREATE INDEX IF NOT EXISTS idx_ccs_expiry
    ON continuous_coverage_segments (segment_end);

-- 4) continuous_migration_failures：迁移/读取失败异常记录
CREATE TABLE IF NOT EXISTS continuous_migration_failures (
    id            BIGSERIAL PRIMARY KEY,
    source_kind   VARCHAR(16) NOT NULL,
    source_ref    VARCHAR(128) NOT NULL,
    session_sid   VARCHAR(64) NOT NULL DEFAULT '',
    object_key    VARCHAR(768) NOT NULL DEFAULT '',
    error_type    VARCHAR(64) NOT NULL DEFAULT 'unknown',
    error_message TEXT,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    retry_count   INTEGER NOT NULL DEFAULT 0,
    status        VARCHAR(16) NOT NULL DEFAULT 'retrying',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 同一来源只允许一条异常记录（upsert 语义）
CREATE UNIQUE INDEX IF NOT EXISTS uq_cmf_source
    ON continuous_migration_failures (source_kind, source_ref);

-- 重试/隔离扫描索引
CREATE INDEX IF NOT EXISTS idx_cmf_status
    ON continuous_migration_failures (status, last_seen_at);

-- 5) profile_windows.batch_bid → profile_batches.bid 外键（ON DELETE CASCADE）
--    先 NOT VALID 安装，清理现有 orphan 后再 VALIDATE，避免大表加锁。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_profile_windows_batch') THEN
        ALTER TABLE profile_windows
            ADD CONSTRAINT fk_profile_windows_batch
            FOREIGN KEY (batch_bid) REFERENCES profile_batches(bid)
            ON DELETE CASCADE NOT VALID;
    END IF;
END $$;

-- 现有 orphan window（找不到 batch）转入异常记录（quarantined，审计后清理）
INSERT INTO continuous_migration_failures
    (source_kind, source_ref, session_sid, object_key, error_type, error_message, status)
SELECT 'window', CAST(w.id AS TEXT), w.session_sid, w.object_key,
       'orphan_window',
       'window references missing batch_bid=' || COALESCE(w.batch_bid, ''),
       'quarantined'
FROM profile_windows w
LEFT JOIN profile_batches b ON b.bid = w.batch_bid
WHERE b.bid IS NULL
ON CONFLICT (source_kind, source_ref) DO NOTHING;

-- 删除 orphan window（外键 NOT VALID 期间不受 CASCADE 影响，显式清理）
DELETE FROM profile_windows w
USING profile_windows x
LEFT JOIN profile_batches b ON b.bid = x.batch_bid
WHERE w.id = x.id AND b.bid IS NULL;

-- 校验外键（孤儿已清，约束立即生效；重放时已是 VALID 状态则无操作）
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_profile_windows_batch' AND NOT convalidated) THEN
        ALTER TABLE profile_windows VALIDATE CONSTRAINT fk_profile_windows_batch;
    END IF;
END $$;
