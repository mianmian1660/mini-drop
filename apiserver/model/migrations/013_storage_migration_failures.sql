-- 013_storage_migration_failures.sql
-- 单对象迁移失败退避，避免一个损坏/缺失的旧对象永久阻塞整个迁移队列。

CREATE TABLE IF NOT EXISTS storage_migration_failures (
  object_key      varchar(512) PRIMARY KEY,
  attempts        integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz,
  last_error      varchar(1024),
  created_at      timestamptz NOT NULL DEFAULT NOW(),
  updated_at      timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_storage_migration_failures_next_attempt
  ON storage_migration_failures(next_attempt_at);
