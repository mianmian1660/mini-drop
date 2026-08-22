-- 009_continuous_blocks.sql - 阶段三：内建持续剖析块存储
--
-- 仅借鉴 Parca/Pyroscope 的块、压缩、索引、合并和替换机制；不部署、不依赖、
-- 不调用任何开源 profiling 后端。将"每分钟一个 JSON 对象"的持续采集存储改为
-- "每个 session 每小时一个 gzip 压缩块"。
--
-- 1) profile_batches 新增：
--    - block_id          所属小时块（为空 = 热数据，仍读原分钟对象）
--    - source_object_key compaction 前的原始分钟对象 key（删除失败时保留，供 sweep 重试）
--    - compacted_at      首次压缩进块的时间
--    - payload_bytes     原始 payload JSON 字节数（供 compactor 磁盘余量估算输入大小）
-- 2) 新建 continuous_profile_blocks 表，记录块元数据（版本/状态/压缩率/替换关系）。
--
-- 与既有迁移一致的容错约定：profile_batches 表可能尚未由 GORM AutoMigrate
-- 创建（全新初始化数据库），用 to_regclass 包一层安全跳过；列全部使用
-- ADD COLUMN IF NOT EXISTS 保证重复执行安全。

DO $$
BEGIN
  IF to_regclass('public.profile_batches') IS NOT NULL THEN
    ALTER TABLE profile_batches
        ADD COLUMN IF NOT EXISTS block_id varchar(64),
        ADD COLUMN IF NOT EXISTS source_object_key varchar(512),
        ADD COLUMN IF NOT EXISTS compacted_at timestamptz,
        ADD COLUMN IF NOT EXISTS payload_bytes bigint NOT NULL DEFAULT 0;

    -- 查询未压缩（热数据）batch：compactor 扫描候选桶走该索引。
    -- block_id 可能是 NULL（旧行/迁移新列）或 ''（GORM 空字符串），两种都算未压缩。
    CREATE INDEX IF NOT EXISTS idx_profile_batches_uncompacted
        ON profile_batches(session_sid, start_time) WHERE block_id IS NULL OR block_id = '';
    -- sweep 重试源对象删除
    CREATE INDEX IF NOT EXISTS idx_profile_batches_source_object
        ON profile_batches(source_object_key) WHERE source_object_key IS NOT NULL AND source_object_key <> '';
    -- 保留期重写：查块内过期成员
    CREATE INDEX IF NOT EXISTS idx_profile_batches_block_end
        ON profile_batches(block_id, end_time) WHERE block_id IS NOT NULL;
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS continuous_profile_blocks (
    id             bigserial PRIMARY KEY,
    block_id       varchar(64) NOT NULL UNIQUE,
    session_sid    varchar(64) NOT NULL,
    bucket_start   timestamptz NOT NULL,
    bucket_end     timestamptz NOT NULL,
    object_key     varchar(512) NOT NULL,
    compression    varchar(16) NOT NULL DEFAULT 'gzip',
    schema_version integer NOT NULL DEFAULT 1,
    version        integer NOT NULL DEFAULT 1,
    status         varchar(16) NOT NULL DEFAULT 'active',
    batch_count    integer NOT NULL DEFAULT 0,
    sample_count   bigint NOT NULL DEFAULT 0,
    bytes_before   bigint NOT NULL DEFAULT 0,
    bytes_after    bigint NOT NULL DEFAULT 0,
    superseded_at  timestamptz,
    replaced_by    varchar(64),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- compactor 每轮查询各 session 的当前 active 块
CREATE INDEX IF NOT EXISTS idx_continuous_profile_blocks_active
    ON continuous_profile_blocks(session_sid, bucket_start) WHERE status = 'active';

-- sweep 清理已过 15 分钟宽限期的 superseded 块对象
CREATE INDEX IF NOT EXISTS idx_continuous_profile_blocks_superseded
    ON continuous_profile_blocks(status, superseded_at) WHERE status = 'superseded';
