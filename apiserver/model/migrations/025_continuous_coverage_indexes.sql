-- ============================================================
-- 025_continuous_coverage_indexes.sql — 阶段九：按信号覆盖率查询索引
-- ============================================================
-- Timeline 按 (session_sid, signal_type, 时间范围) 分别计算每个信号的
-- 覆盖率与 coverage_bands，需要两个查询索引：
--   1) continuous_coverage_segments(session_sid, signal_type,
--      segment_start, segment_end)：历史压缩覆盖区间按信号过滤；
--   2) profile_windows(session_sid, signal_type, window_start,
--      window_end)：热窗口按信号过滤。
-- 全部为 additive 迁移（CREATE INDEX IF NOT EXISTS），不删除历史数据，
-- 保留现有唯一约束与旧索引，重放幂等。
-- ============================================================

CREATE INDEX IF NOT EXISTS idx_ccs_session_signal_time
    ON continuous_coverage_segments (session_sid, signal_type, segment_start, segment_end);

CREATE INDEX IF NOT EXISTS idx_pw_session_signal_time
    ON profile_windows (session_sid, signal_type, window_start, window_end);