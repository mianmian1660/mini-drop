-- ============================================================
-- 023_continuous_v4_status.sql — 阶段三：schema v4 存储闭环
-- ============================================================
-- 全部为 additive 迁移，不删除历史数据：
--   1) profile_windows 新增每信号 collection status / reason /
--      physical/effective sample rate（旧行默认为 unknown/0）
--   2) profile_windows 新增 identity_unavailable_count（身份不完整被丢弃的
--      样本数，process Session 无法归属时记录）
--   3) 索引：按 (session_sid, signal_type, status) 查询 coverage 状态
-- 重放幂等（所有 DDL 带 IF NOT EXISTS / 存在性检查）。
-- ============================================================

DO $$
BEGIN
  IF to_regclass('public.profile_windows') IS NOT NULL THEN
    ALTER TABLE profile_windows
        ADD COLUMN IF NOT EXISTS signal_status varchar(16) NOT NULL DEFAULT 'unknown',
        ADD COLUMN IF NOT EXISTS signal_status_reason varchar(512) NOT NULL DEFAULT '',
        ADD COLUMN IF NOT EXISTS signal_lost_events bigint NOT NULL DEFAULT 0,
        ADD COLUMN IF NOT EXISTS physical_sample_rate_hz integer NOT NULL DEFAULT 0,
        ADD COLUMN IF NOT EXISTS effective_sample_rate_hz integer NOT NULL DEFAULT 0,
        ADD COLUMN IF NOT EXISTS identity_unavailable_count bigint NOT NULL DEFAULT 0;

    CREATE INDEX IF NOT EXISTS idx_profile_windows_signal_status
        ON profile_windows(session_sid, signal_type, signal_status)
        WHERE signal_status <> 'unknown';
  END IF;
END $$;