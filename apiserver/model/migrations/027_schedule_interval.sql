-- ============================================================
-- 027_schedule_interval.sql — 周期计划"间隔 + 开始时间"字段
-- ============================================================
-- 新建周期任务从"用户填写 Cron 表达式"改为"采样间隔 interval_seconds +
-- 开始时间 start_at"。保留 cron_expr 列用于旧任务兼容读取；新任务只写
-- interval_seconds/start_at/next_run_at。
-- 全部为 additive 迁移（ADD COLUMN IF NOT EXISTS），重放幂等；旧行
-- interval_seconds 默认 0，仍按 cron 兼容路径运行。
-- ============================================================

ALTER TABLE schedule_tasks ADD COLUMN IF NOT EXISTS interval_seconds bigint DEFAULT 0;
ALTER TABLE schedule_tasks ADD COLUMN IF NOT EXISTS start_at timestamptz;
