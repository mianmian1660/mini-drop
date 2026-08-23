-- 014_single_shot_layout.sql
--
-- 阶段 4 单次采样最终存储模型：第一步"扩展迁移"（Release A 桥接版本）。
-- 幂等迁移（ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS / 条件回填），
-- 在 PostgreSQL 14 上可重复执行。
--
-- 新增列：
--   analysis_jobs.attempt_id     采集尝试 ID（RAW 所属 attempt；分析产物代次输入）
--   analysis_jobs.generation     同一任务内从 1 单调递增的代次号
--   analysis_jobs.trigger        initial（采集通知自动创建）/ manual（人工重分析）
--   analysis_jobs.requested_by   人工重分析的请求者（uid/name）
--   analysis_jobs.superseded_at  被新代次替换的时间（active 切换时写入）
--   artifacts.analysis_job_id    分析产物所属 generation（RAW 为 NULL）
--   artifacts.logical_name       稳定文件角色名（perf.data / top.json ...）
--   hotmethod_tasks.active_analysis_job_id  当前对用户展示的成功结果
--   analysis_suggestions.analysis_job_id    建议所属 generation
--
-- 回填：
--   现有 AnalysisJob generation=1、trigger='initial'；
--   attempt_id 从 input_artifact_ids 关联 artifacts 回填，缺失时取最新 attempt；
--   已成功的历史任务把成功 AnalysisJob 设为 active（无法确认的保持 NULL）。
--
-- 索引：
--   (task_tid, generation) 唯一索引 —— 第二步"多代迁移"删除单列唯一约束后生效；
--   task/attempt/status 查询索引；初始作业幂等索引（partial unique）。

DO $$
BEGIN
  IF to_regclass('public.analysis_jobs') IS NOT NULL THEN
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS attempt_id bigint;
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS generation integer DEFAULT 0;
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS trigger varchar(32) DEFAULT 'initial';
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS requested_by varchar(128);
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS superseded_at timestamptz;
  END IF;

  IF to_regclass('public.artifacts') IS NOT NULL THEN
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS analysis_job_id bigint;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS logical_name varchar(256);
    CREATE INDEX IF NOT EXISTS idx_artifacts_analysis_job
      ON artifacts(analysis_job_id) WHERE analysis_job_id IS NOT NULL;
  END IF;

  IF to_regclass('public.hotmethod_tasks') IS NOT NULL THEN
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS active_analysis_job_id bigint;
    CREATE INDEX IF NOT EXISTS idx_tasks_active_analysis_job
      ON hotmethod_tasks(active_analysis_job_id) WHERE active_analysis_job_id IS NOT NULL;
  END IF;

  IF to_regclass('public.analysis_suggestions') IS NOT NULL THEN
    ALTER TABLE analysis_suggestions ADD COLUMN IF NOT EXISTS analysis_job_id bigint;
    CREATE INDEX IF NOT EXISTS idx_analysis_suggestions_job
      ON analysis_suggestions(analysis_job_id) WHERE analysis_job_id IS NOT NULL;
  END IF;
END $$;

-- 回填：generation / trigger（先于唯一索引创建，保证已有行取值一致）
UPDATE analysis_jobs SET generation = 1
  WHERE generation IS NULL OR generation <= 0;
UPDATE analysis_jobs SET trigger = 'initial'
  WHERE trigger IS NULL OR trigger = '';

-- 回填：attempt_id —— 优先从 input_artifact_ids 关联的 RAW 产物取 attempt_id。
UPDATE analysis_jobs aj
SET attempt_id = sub.attempt_id
FROM (
  SELECT aj2.id AS job_id, MAX(a.attempt_id) AS attempt_id
  FROM analysis_jobs aj2
  LEFT JOIN LATERAL (
    SELECT value::bigint AS artifact_id
    FROM jsonb_array_elements_text(aj2.input_artifact_ids::jsonb)
  ) ids ON ids.artifact_id IS NOT NULL
  LEFT JOIN artifacts a ON a.id = ids.artifact_id AND a.deleted_at IS NULL
  WHERE aj2.attempt_id IS NULL OR aj2.attempt_id = 0
  GROUP BY aj2.id
) sub
WHERE aj.id = sub.job_id AND sub.attempt_id IS NOT NULL;

-- 回填：attempt_id —— 仍缺失时取该任务最新的 TaskAttempt。
UPDATE analysis_jobs aj
SET attempt_id = (
  SELECT id FROM task_attempts
  WHERE task_tid = aj.task_tid
  ORDER BY attempt_seq DESC, id DESC
  LIMIT 1
)
WHERE (aj.attempt_id IS NULL OR aj.attempt_id = 0)
  AND EXISTS (SELECT 1 FROM task_attempts ta WHERE ta.task_tid = aj.task_tid);

-- 回填：已成功历史任务设置 active_analysis_job_id（每个任务取成功 job 中 id 最大者）。
UPDATE hotmethod_tasks t
SET active_analysis_job_id = aj.id
FROM analysis_jobs aj
WHERE t.active_analysis_job_id IS NULL
  AND aj.task_tid = t.tid
  AND aj.status = 'success'
  AND aj.id = (
    SELECT id FROM analysis_jobs
    WHERE task_tid = t.tid AND status = 'success'
    ORDER BY id DESC LIMIT 1
  );

DO $$
BEGIN
  IF to_regclass('public.analysis_jobs') IS NOT NULL THEN
    -- 代次唯一：(task_tid, generation)。Release A 阶段 task_tid 单列唯一仍存在，
    -- 该复合索引冗余但无害；015 删除单列唯一后由它承担唯一性。
    CREATE UNIQUE INDEX IF NOT EXISTS uidx_analysis_jobs_task_generation
      ON analysis_jobs(task_tid, generation);

    -- 查询索引：按任务 + attempt + 状态
    CREATE INDEX IF NOT EXISTS idx_analysis_jobs_task_attempt_status
      ON analysis_jobs(task_tid, attempt_id, status);

    -- 初始作业幂等：同一 (task, attempt, pipeline) 只允许一条 trigger='initial' 作业。
    -- 重复采集通知 ON CONFLICT DO NOTHING 命中此索引。
    CREATE UNIQUE INDEX IF NOT EXISTS uidx_analysis_jobs_initial_once
      ON analysis_jobs(task_tid, attempt_id, pipeline)
      WHERE trigger = 'initial';
  END IF;
END $$;
