-- 阶段五账本数据库不变量：应用层状态机之外再由 PostgreSQL 拒绝非法状态。

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_cpq_active_validation') THEN
        ALTER TABLE continuous_parquet_blocks
            ADD CONSTRAINT ck_cpq_active_validation
            CHECK (status <> 'active' OR validation = 'passed');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_cpq_status') THEN
        ALTER TABLE continuous_parquet_blocks
            ADD CONSTRAINT ck_cpq_status
            CHECK (status IN ('building','validating','active','failed','superseded','deleting','deleted'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_cpq_validation') THEN
        ALTER TABLE continuous_parquet_blocks
            ADD CONSTRAINT ck_cpq_validation
            CHECK (validation IN ('pending','passed','failed','skipped'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_cpq_signal') THEN
        ALTER TABLE continuous_parquet_blocks
            ADD CONSTRAINT ck_cpq_signal
            CHECK (signal_type IN ('cpu','metrics','histogram','db'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_cpq_resolution') THEN
        ALTER TABLE continuous_parquet_blocks
            ADD CONSTRAINT ck_cpq_resolution
            CHECK (resolution IN ('raw','5m','1h'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_cpq_files_block') THEN
        ALTER TABLE continuous_parquet_block_files
            ADD CONSTRAINT fk_cpq_files_block FOREIGN KEY (block_id)
            REFERENCES continuous_parquet_blocks(block_id) ON DELETE CASCADE;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_cpq_members_block') THEN
        ALTER TABLE continuous_parquet_block_members
            ADD CONSTRAINT fk_cpq_members_block FOREIGN KEY (block_id)
            REFERENCES continuous_parquet_blocks(block_id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_cpq_files_block_part
    ON continuous_parquet_block_files (block_id, part_index);

CREATE UNIQUE INDEX IF NOT EXISTS uq_cpq_members_source
    ON continuous_parquet_block_members (block_id, source_kind, source_ref);
