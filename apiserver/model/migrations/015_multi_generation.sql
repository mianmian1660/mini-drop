-- 015_multi_generation.sql
--
-- 阶段 4 单次采样最终存储模型：第二步"多代迁移"（Release B）。
-- 删除 analysis_jobs.task_tid 单列唯一约束，放开多代写入。
--
-- 前置条件：014 已上线，第一版桥接程序（不再依赖 ON CONFLICT(task_tid)）
-- 已运行，回滚目标固定为桥接版本。本迁移不回退；多代迁移后只能回滚到
-- 桥接版本，不能直接回滚到阶段 2 旧镜像。
--
-- GORM 对 `uniqueIndex`（未命名）生成的索引名可能是 idx_analysis_jobs_task_tid
-- 或 idx_analysis_jobs_task_t_id（实测 PostgreSQL 上被 GORM 截断/改写成
-- idx_analysis_jobs_task_t_id）；为兼容手工建表/其它命名，一并尝试删除。

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_indexes
    WHERE tablename = 'analysis_jobs' AND indexname = 'idx_analysis_jobs_task_tid'
  ) THEN
    DROP INDEX idx_analysis_jobs_task_tid;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_indexes
    WHERE tablename = 'analysis_jobs' AND indexname = 'idx_analysis_jobs_task_t_id'
  ) THEN
    DROP INDEX idx_analysis_jobs_task_t_id;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_indexes
    WHERE tablename = 'analysis_jobs' AND indexname = 'uni_analysis_jobs_task_tid'
  ) THEN
    DROP INDEX uni_analysis_jobs_task_tid;
  END IF;
  -- 兜底：约束形式（约束可能用唯一索引实现，名字形如 analysis_jobs_task_tid_key）
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    WHERE t.relname = 'analysis_jobs'
      AND c.contype = 'u'
      AND pg_get_constraintdef(c.oid) LIKE '%task_tid%'
      AND pg_get_constraintdef(c.oid) NOT LIKE '%generation%'
  ) THEN
    EXECUTE (
      SELECT 'ALTER TABLE analysis_jobs DROP CONSTRAINT ' || quote_ident(c.conname)
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
      WHERE t.relname = 'analysis_jobs'
        AND c.contype = 'u'
        AND pg_get_constraintdef(c.oid) LIKE '%task_tid%'
        AND pg_get_constraintdef(c.oid) NOT LIKE '%generation%'
      LIMIT 1
    );
  END IF;
END $$;
