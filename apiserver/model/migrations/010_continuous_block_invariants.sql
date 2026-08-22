-- 010_continuous_block_invariants.sql
--
-- 数据库层强制每个 session + 小时桶最多只有一个 active 块。
-- compactor 在同一事务内先将旧块标记为 superseded，再插入新块，
-- 因此迟到 batch 的版本替换与该约束兼容。

CREATE UNIQUE INDEX IF NOT EXISTS uq_continuous_profile_blocks_active_bucket
    ON continuous_profile_blocks(session_sid, bucket_start)
    WHERE status = 'active';
