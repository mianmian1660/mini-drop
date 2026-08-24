-- ============================================================
-- 022_continuous_correctness.sql — 阶段一：持续采集数据正确性
-- ============================================================
-- 全部为 additive 迁移，不删除历史数据：
--   1) profile_batches  新增 collector_generation / batch_sequence /
--      content_sha256 / signal_counts（batch 层 sample_count 不再使用，写 0）
--   2) profile_windows  新增 window_id / collector_generation /
--      target_fingerprint / content_sha256 / signal_counts
--   3) continuous_sessions 新增 requested_signals（显式字符串数组镜像，
--      signals 列继续保留历史语义）
--   4) 部分唯一索引 (session_sid, window_id, signal_type)：
--      只约束非空的新协议 window_id，历史 v2 行不受影响
--   5) continuous_repair_audits：历史重复自动修复审计表
-- 重放幂等（所有 DDL 带 IF NOT EXISTS / 存在性检查）。
-- ============================================================

DO $$
BEGIN
  -- 1) ProfileBatch 新协议字段
  IF to_regclass('public.profile_batches') IS NOT NULL THEN
    ALTER TABLE profile_batches
        ADD COLUMN IF NOT EXISTS collector_generation varchar(64) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS batch_sequence bigint NOT NULL DEFAULT 0,
        ADD COLUMN IF NOT EXISTS content_sha256 varchar(64) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS signal_counts jsonb;
    CREATE INDEX IF NOT EXISTS idx_profile_batches_collector_generation
        ON profile_batches(collector_generation)
        WHERE collector_generation <> '';
    CREATE INDEX IF NOT EXISTS idx_profile_batches_session_sequence
        ON profile_batches(session_sid, batch_sequence);
  END IF;

  -- 2) ProfileWindow 新协议字段
  IF to_regclass('public.profile_windows') IS NOT NULL THEN
    ALTER TABLE profile_windows
        ADD COLUMN IF NOT EXISTS window_id varchar(128) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS collector_generation varchar(64) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS target_fingerprint varchar(256) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS content_sha256 varchar(64) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS signal_counts jsonb;

    -- 部分唯一索引：同一逻辑窗口（session+window_id+signal_type）最多入库一次。
    -- 只约束非空新协议 window_id，历史 v2 行（window_id=''）不受影响。
    -- 语义：window_id 由规范化字段生成（session、collector generation、窗口
    -- 序号、起止时间、target fingerprint），内容摘要不参与 ID——因此同一
    -- 逻辑窗口无论重传多少次都命中同一 window_id，天然幂等；内容不同也只
    -- 是同一 ID 的不同摘要，绝不允许生成第二个合法 ID。
    CREATE UNIQUE INDEX IF NOT EXISTS uq_profile_windows_logical_window
        ON profile_windows(session_sid, window_id, signal_type)
        WHERE window_id IS NOT NULL AND window_id <> '';

    CREATE INDEX IF NOT EXISTS idx_profile_windows_window_id
        ON profile_windows(window_id)
        WHERE window_id <> '';
  END IF;

  -- 3) ContinuousSession 显式 requested_signals（字符串数组镜像）
  IF to_regclass('public.continuous_sessions') IS NOT NULL THEN
    ALTER TABLE continuous_sessions
        ADD COLUMN IF NOT EXISTS requested_signals jsonb;
  END IF;
END $$;

-- 5) 历史重复自动修复审计表：记录 repair run、逻辑窗口键、保留/排除的
--    batch、双方摘要、处理原因和时间。原始对象暂不立即删除（先保留，
--    凭据完整后可再决定归档策略）。
CREATE TABLE IF NOT EXISTS continuous_repair_audits (
    id                   BIGSERIAL PRIMARY KEY,
    run_id               VARCHAR(64) NOT NULL,
    tenant               VARCHAR(64) NOT NULL DEFAULT 'default',
    session_sid          VARCHAR(64) NOT NULL,
    logical_window_key   VARCHAR(512) NOT NULL,
    window_id            VARCHAR(128) NOT NULL DEFAULT '',
    signal_type          VARCHAR(32) NOT NULL,
    window_start         TIMESTAMPTZ NOT NULL,
    window_end           TIMESTAMPTZ NOT NULL,
    kept_batch_bid       VARCHAR(64) NOT NULL,
    excluded_batch_bid   VARCHAR(64) NOT NULL,
    kept_content_sha256  VARCHAR(64) NOT NULL DEFAULT '',
    excluded_content_sha256 VARCHAR(64) NOT NULL DEFAULT '',
    reason               VARCHAR(128) NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_repair_audits_run
    ON continuous_repair_audits(run_id);
CREATE INDEX IF NOT EXISTS idx_repair_audits_session
    ON continuous_repair_audits(session_sid, window_start);
