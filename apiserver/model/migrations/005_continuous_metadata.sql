-- 005_continuous_metadata.sql — continuous profiling backend metadata + symbol refs
--
-- 借鉴 Pyroscope/Parca 的写路径分离设计：每个 batch/window 记录采集后端状态、
-- 不可用原因、尝试过的后端列表、最终选中的后端，以及符号引用（build-id / kallsyms）。
-- 查询时根据 symbol_refs 推断 symbol_status（complete/partial/missing/not_applicable），
-- 前端诊断区展示缺失符号、浅栈、unknown frame 的原因。
--
-- 所有列均使用 ADD COLUMN IF NOT EXISTS + DEFAULT，保证对旧 agent 上传的 batch 向后兼容。

ALTER TABLE profile_batches
    ADD COLUMN IF NOT EXISTS profile_format varchar(32) NOT NULL DEFAULT 'json',
    ADD COLUMN IF NOT EXISTS backend_status varchar(32) NOT NULL DEFAULT 'ok',
    ADD COLUMN IF NOT EXISTS backend_reason text,
    ADD COLUMN IF NOT EXISTS attempted_backends jsonb,
    ADD COLUMN IF NOT EXISTS selected_backend varchar(64),
    ADD COLUMN IF NOT EXISTS symbol_refs jsonb;

ALTER TABLE profile_windows
    ADD COLUMN IF NOT EXISTS profile_format varchar(32) NOT NULL DEFAULT 'json',
    ADD COLUMN IF NOT EXISTS backend_status varchar(32) NOT NULL DEFAULT 'ok',
    ADD COLUMN IF NOT EXISTS backend_reason text,
    ADD COLUMN IF NOT EXISTS attempted_backends jsonb,
    ADD COLUMN IF NOT EXISTS selected_backend varchar(64),
    ADD COLUMN IF NOT EXISTS symbol_refs jsonb;

CREATE INDEX IF NOT EXISTS idx_profile_windows_backend_status
    ON profile_windows(backend_status);
CREATE INDEX IF NOT EXISTS idx_profile_batches_backend_status
    ON profile_batches(backend_status);
