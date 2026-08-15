-- 002_symbol_store.sql — 阶段三：Build ID 符号库（跨任务/跨 Agent 去重）
--
-- symbol_files：以 build_id 为主键的符号文件账本，天然去重——同一个二进制
-- 不管被多少个任务、多少台 Agent 采集到，只存一份。
--
-- task_build_ids：记录某个任务的 perf.data 引用了哪些 build_id，供 Analysis
-- 直接查询"这个任务需要哪些符号"，不用重新跑一遍 perf buildid-list。
--
-- 新建表，不需要 DO $$ 包裹（对比 001_stage1_contract.sql 里的 ALTER 场景，
-- 那些是修改已有表才需要用 DO $$ 做存在性判断）。

CREATE TABLE IF NOT EXISTS symbol_files (
    build_id     varchar(128) PRIMARY KEY,
    file_name    varchar(512) NOT NULL,
    object_key   varchar(512) NOT NULL,
    size_bytes   bigint NOT NULL,
    sha256       varchar(64) NOT NULL,
    status       smallint NOT NULL DEFAULT 0,  -- 0 上传中 / 1 就绪 / 2 失败
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX IF NOT EXISTS idx_symbol_files_status ON symbol_files(status);

CREATE TABLE IF NOT EXISTS task_build_ids (
    tid       varchar(64) NOT NULL,
    build_id  varchar(128) NOT NULL,
    dso_path  varchar(1024) NOT NULL,
    PRIMARY KEY (tid, build_id)
);
CREATE INDEX IF NOT EXISTS idx_task_build_ids_build_id ON task_build_ids(build_id);
