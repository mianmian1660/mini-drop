-- 阶段六：完善 Continuous Selector 模型
-- 1. continuous_sessions 增加 selector_params（结构化 selector 参数 jsonb）：
--    pid_instance: {pid, process_start_ms, exe}
--    exe_all_instances: {exe}
--    cgroup: {cgroup}
--    container_id: {container_id}
-- 2. continuous_process_snapshots 增加 cgroup_path / container_id（Agent 从
--    /proc/<pid>/cgroup 读取并识别，供 cgroup/container_id selector 选择）。
-- 3. 旧数据归一化：历史 scope=process + selector_mode=all_instances 归一化为
--    exe_all_instances（语义等价：按 exe 跟随全部实例）。

ALTER TABLE continuous_sessions
    ADD COLUMN IF NOT EXISTS selector_params jsonb;

ALTER TABLE continuous_process_snapshots
    ADD COLUMN IF NOT EXISTS cgroup_path varchar(1024),
    ADD COLUMN IF NOT EXISTS container_id varchar(128);

CREATE INDEX IF NOT EXISTS idx_continuous_process_cgroup
    ON continuous_process_snapshots (target_ip, cgroup_path);
CREATE INDEX IF NOT EXISTS idx_continuous_process_container
    ON continuous_process_snapshots (target_ip, container_id);

-- 旧数据归一化：all_instances 是 exe_all_instances 的历史别名。
UPDATE continuous_sessions
   SET selector_mode = 'exe_all_instances'
 WHERE scope = 'process' AND selector_mode = 'all_instances';