-- 011_artifact_lifecycle.sql
--
-- 存储阶段一：Artifact 生命周期闭环。
-- 幂等迁移（ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS），
-- 在 PostgreSQL 14 上可重复执行。
--
-- 设计要点：
--   * 复用 artifacts.retention 列作为 canonical retention_class，不建重复字段。
--   * size 含义固定为对象存储实际字节数；logical_size 记录未压缩逻辑字节数。
--   * 删除成功后不硬删行：deleted_at/delete_reason 永久保留墓碑，
--     不允许普通 upsert 直接复活（应用层同时用 deleted_at IS NULL 过滤）。
--   * 新增两个清理索引：
--       - 到期扫描：status='ready' 且 expires_at 升序
--       - 删除重试：status='deleting' 且 next_delete_attempt_at 升序

-- SQL migration 在 GORM AutoMigrate 之前执行。全新数据库还没有业务表，
-- 因此必须像既有迁移一样在表存在时才 ALTER；随后 AutoMigrate 会按模型建全表。
DO $$
BEGIN
  IF to_regclass('public.artifacts') IS NOT NULL THEN
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS expires_at timestamptz;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS retention_policy_version varchar(64);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS retention_task_state varchar(32);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS retention_not_before timestamptz;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS logical_size bigint DEFAULT 0;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS compression varchar(32);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS delete_reason varchar(128);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS delete_attempts integer DEFAULT 0;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS next_delete_attempt_at timestamptz;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS last_delete_error varchar(1024);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS updated_at timestamptz;

    CREATE INDEX IF NOT EXISTS idx_artifacts_ready_expiry
      ON artifacts(status, expires_at) WHERE status = 'ready';
    CREATE INDEX IF NOT EXISTS idx_artifacts_deleting_retry
      ON artifacts(status, next_delete_attempt_at) WHERE status IN ('deleting', 'failed');
  END IF;

  IF to_regclass('public.hotmethod_tasks') IS NOT NULL THEN
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS artifacts_pinned boolean DEFAULT false;
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS artifacts_pinned_at timestamptz;
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS artifacts_pinned_by varchar(128);
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS artifacts_pin_reason varchar(256);
  END IF;
END $$;
