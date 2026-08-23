-- ============================================================
-- 016_continuous_parquet_blocks.sql — 阶段五 v2 目录账本（additive）
-- ============================================================
-- 新增三张表：逻辑块 / 物理 shard / lineage 成员。
-- 全部为新增表，不修改既有表；迁移重放与旧查询完全兼容。
-- 状态机：building → validating → active（↘ failed）；
-- active → superseded → deleting → deleted（墓碑保留）。
-- ============================================================

CREATE TABLE IF NOT EXISTS continuous_parquet_blocks (
    id                  BIGSERIAL PRIMARY KEY,
    block_id            VARCHAR(64) NOT NULL,
    tenant              VARCHAR(64) NOT NULL DEFAULT 'default',
    bucket_start        TIMESTAMPTZ NOT NULL,
    bucket_end          TIMESTAMPTZ NOT NULL,
    signal_type         VARCHAR(32) NOT NULL,
    resolution          VARCHAR(16) NOT NULL,
    version             INTEGER NOT NULL DEFAULT 1,
    status              VARCHAR(16) NOT NULL DEFAULT 'building',
    validation          VARCHAR(16) NOT NULL DEFAULT 'pending',
    source_block_id     VARCHAR(64) NOT NULL DEFAULT '',
    member_count        INTEGER NOT NULL DEFAULT 0,
    row_count           BIGINT NOT NULL DEFAULT 0,
    value_total         BIGINT NOT NULL DEFAULT 0,
    sample_total        BIGINT NOT NULL DEFAULT 0,
    session_count       INTEGER NOT NULL DEFAULT 0,
    process_count       INTEGER NOT NULL DEFAULT 0,
    bytes_total         BIGINT NOT NULL DEFAULT 0,
    first_row_time      TIMESTAMPTZ,
    last_row_time       TIMESTAMPTZ,
    row_group_boundaries JSONB,
    superseded_at       TIMESTAMPTZ,
    replaced_by         VARCHAR(64) NOT NULL DEFAULT '',
    delete_reason       VARCHAR(64) NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_continuous_parquet_blocks_block_id
    ON continuous_parquet_blocks (block_id);

CREATE INDEX IF NOT EXISTS idx_continuous_parquet_blocks_partition
    ON continuous_parquet_blocks (tenant, signal_type, resolution, bucket_start);

CREATE INDEX IF NOT EXISTS idx_continuous_parquet_blocks_status
    ON continuous_parquet_blocks (status, bucket_start);

CREATE TABLE IF NOT EXISTS continuous_parquet_block_files (
    id              BIGSERIAL PRIMARY KEY,
    block_id        VARCHAR(64) NOT NULL,
    part_index      INTEGER NOT NULL DEFAULT 0,
    object_key      VARCHAR(768) NOT NULL,
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    sha256          VARCHAR(64) NOT NULL DEFAULT '',
    row_group_count INTEGER NOT NULL DEFAULT 0,
    row_count       BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_continuous_parquet_block_files_object_key
    ON continuous_parquet_block_files (object_key);

CREATE INDEX IF NOT EXISTS idx_continuous_parquet_block_files_block
    ON continuous_parquet_block_files (block_id, part_index);

CREATE TABLE IF NOT EXISTS continuous_parquet_block_members (
    id           BIGSERIAL PRIMARY KEY,
    block_id     VARCHAR(64) NOT NULL,
    source_kind  VARCHAR(16) NOT NULL,
    source_ref   VARCHAR(128) NOT NULL,
    start_time   TIMESTAMPTZ,
    end_time     TIMESTAMPTZ,
    sample_count BIGINT NOT NULL DEFAULT 0,
    value_total  BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_continuous_parquet_block_members_block
    ON continuous_parquet_block_members (block_id);

CREATE INDEX IF NOT EXISTS idx_continuous_parquet_block_members_source
    ON continuous_parquet_block_members (source_kind, source_ref);
