-- 003_kernel_symbol_store.sql — /proc/kallsyms 快照去重账本

CREATE TABLE IF NOT EXISTS kernel_symbol_files (
    sha256         varchar(64) PRIMARY KEY,
    object_key     varchar(512) NOT NULL,
    kernel_release varchar(128),
    hostname       varchar(256),
    target_ip      varchar(45),
    size_bytes     bigint NOT NULL,
    status         smallint NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz
);
CREATE INDEX IF NOT EXISTS idx_kernel_symbol_files_status ON kernel_symbol_files(status);
CREATE INDEX IF NOT EXISTS idx_kernel_symbol_files_target_ip ON kernel_symbol_files(target_ip);
