-- ============================================================
-- 026_host_metadata.sql — 主机身份与系统信息持久化
-- ============================================================
-- Agent 心跳新增 HostMetadata（os_name/os_version/kernel_version/
-- architecture/cpu_model/cpu_cores/uptime_seconds/collected_at_unix_ms），
-- apiserver 收到后写入 agent_infos.host_metadata（JSONB）。
-- 全部为 additive 迁移（ADD COLUMN IF NOT EXISTS），不删除历史数据，
-- 重放幂等；旧 Agent 不上报时该列为 NULL，前端显示"暂未上报"。
-- ============================================================

ALTER TABLE agent_infos ADD COLUMN IF NOT EXISTS host_metadata jsonb;