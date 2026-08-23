-- 012_storage_blobs.sql
--
-- 存储阶段二：逻辑 Artifact 引用 → 物理 Blob → MinIO 对象。
-- 幂等迁移（IF NOT EXISTS / to_regclass 守卫），PostgreSQL 14 上可重复执行。
--
-- 设计要点：
--   * storage_blobs：object_key 唯一；非空 (logical_sha256, format, compression)
--     部分唯一索引实现内容寻址去重（历史对象 logical_sha256 为空不参与）。
--   * storage_object_gc：保存迁移后旧物理 key，not_before = 入队 + 24h 宽限期。
--   * artifacts / symbol_files / kernel_symbol_files 增加 nullable blob_id，
--     blob_id 为空时兼容读取原 object_key。
--   * 全部为 additive，不回退字段和表。

DO $$
BEGIN
  IF to_regclass('public.storage_blobs') IS NULL THEN
    CREATE TABLE storage_blobs (
      id                  bigserial PRIMARY KEY,
      object_key          varchar(512) NOT NULL,
      logical_sha256      varchar(64),
      stored_sha256       varchar(64),
      stored_size         bigint NOT NULL DEFAULT 0,
      logical_size        bigint NOT NULL DEFAULT 0,
      format              varchar(32),
      schema_version      varchar(32),
      compression         varchar(32),
      content_encoding    varchar(32),
      content_type        varchar(128),
      status              varchar(32) NOT NULL DEFAULT 'ready',
      delete_reason       varchar(128),
      delete_attempts     integer NOT NULL DEFAULT 0,
      next_delete_attempt_at timestamptz,
      last_delete_error   varchar(1024),
      verified_at         timestamptz,
      deleted_at          timestamptz,
      created_at          timestamptz NOT NULL DEFAULT NOW(),
      updated_at          timestamptz NOT NULL DEFAULT NOW()
    );
    CREATE UNIQUE INDEX uidx_storage_blobs_object_key ON storage_blobs(object_key);
    CREATE UNIQUE INDEX uidx_storage_blobs_content
      ON storage_blobs(logical_sha256, format, compression)
      WHERE logical_sha256 IS NOT NULL;
    CREATE INDEX idx_storage_blobs_status_retry
      ON storage_blobs(status, next_delete_attempt_at)
      WHERE status IN ('deleting', 'failed');
  END IF;

  IF to_regclass('public.storage_object_gc') IS NULL THEN
    CREATE TABLE storage_object_gc (
      id                  bigserial PRIMARY KEY,
      object_key          varchar(512) NOT NULL,
      reason              varchar(64),
      not_before          timestamptz,
      delete_attempts     integer NOT NULL DEFAULT 0,
      next_delete_attempt_at timestamptz,
      last_delete_error   varchar(1024),
      deleted_at          timestamptz,
      created_at          timestamptz NOT NULL DEFAULT NOW(),
      updated_at          timestamptz NOT NULL DEFAULT NOW()
    );
    CREATE UNIQUE INDEX uidx_storage_object_gc_key ON storage_object_gc(object_key);
    CREATE INDEX idx_storage_object_gc_due
      ON storage_object_gc(not_before, next_delete_attempt_at)
      WHERE deleted_at IS NULL;
  END IF;

  IF to_regclass('public.artifacts') IS NOT NULL THEN
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS blob_id bigint;
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS format varchar(32);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS schema_version varchar(32);
    CREATE INDEX IF NOT EXISTS idx_artifacts_blob_id ON artifacts(blob_id) WHERE blob_id IS NOT NULL;
  END IF;

  IF to_regclass('public.symbol_files') IS NOT NULL THEN
    ALTER TABLE symbol_files ADD COLUMN IF NOT EXISTS blob_id bigint;
    CREATE INDEX IF NOT EXISTS idx_symbol_files_blob_id ON symbol_files(blob_id) WHERE blob_id IS NOT NULL;
  END IF;

  IF to_regclass('public.kernel_symbol_files') IS NOT NULL THEN
    ALTER TABLE kernel_symbol_files ADD COLUMN IF NOT EXISTS blob_id bigint;
    CREATE INDEX IF NOT EXISTS idx_kernel_symbol_files_blob_id ON kernel_symbol_files(blob_id) WHERE blob_id IS NOT NULL;
  END IF;
END $$;
