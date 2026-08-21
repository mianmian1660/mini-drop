# 数据库性能采集设计

> 解决"持续采集只能看 CPU/调度/IO，看不到数据库内部锁/执行计划/慢查询"的问题。
> 不部署 `mysqld_exporter`/`postgres_exporter` 等现成工具，学习其采集口径后自研实现，集成进现有持续采集管道（`drop/common/ContinuousSampler.cpp`）。
> eBPF 协议解析（零账号抓 SQL）留作后续方向，不在本次范围。

## Summary

新增 `DBSnapshotSampler`，与 `PerfEventSampler`/`DualTrackContinuousSampler` 同级实现 `drop::Sampler` 接口，复用今天（2026-08-20，`bd03f86`）加固过的 `run_continuous_spool_loop()` 管道（spool/重试/ACK/背压/停止排空），不重新解决可靠性问题。分两阶段交付：

- **阶段一**（本次已实施）：借鉴 `mysqld_exporter`/`postgres_exporter` 的指标口径，采标量健康指标（连接数/QPS/buffer pool 命中率），写入现成的 `MetricPayload`。
- **阶段二**（未开始）：借鉴 `pg_stat_monitor`/Percona PMM Query Analytics 的 digest 聚合思路，采 SQL digest、锁等待链，需要新增 `WindowPayload.dbSnapshots` 字段。

---

## 1. 数据结构

`drop/common/ContinuousSampler.h`：

- `DBTargetConfig`（engine/instanceLabel/host/port/user/passwordRef/pollIntervalSec/queryTimeoutMs）——`passwordRef` 只是本机文件路径，密码不经服务端中转、不落 Postgres，与 `AgentDiscoveryConfig` 现有"不存密码"约定一致。
- `ContinuousSamplerConfig.dbTargets`：`std::vector<DBTargetConfig>`，和已有的 `targetProcesses` 字段同款模式。
- `DBSnapshotSampler : public Sampler`：新 Sampler 子类，不并入 `DualTrackContinuousSampler`（后者是 perf_event/eBPF 专用，`signals` 字段硬编码，不适合塞数据库轮询逻辑）。

## 2. 采集逻辑（阶段一）

`drop/common/ContinuousSampler.cpp`：

- `collect_db_window(cfg)`：遍历 `cfg.dbTargets`，MySQL 目标调用 `collect_mysql_target_metrics()`。
- MySQL 侧：短连接查询 `SHOW GLOBAL STATUS`（一次性拿全量计数器，本地过滤），换算出：
  - `db_active_connections`（`Threads_connected`）
  - `db_questions_total`（`Questions`）
  - `db_innodb_buffer_pool_hit_ratio_bps`（由 `Innodb_buffer_pool_read_requests`/`Innodb_buffer_pool_reads` 换算，放大 10000 倍存成整数，因为 `MetricPayload.value` 是 `uint64_t` 不支持小数）
- 凭据：从 `passwordRef` 指向的本机文件读密码，写一个 0600 的 `--defaults-extra-file` 临时文件给 `mysql` 客户端用（不走 argv，避免 `ps` 泄漏密码），查询完立即 `unlink`。
- 用 `mysql`/`psql` CLI 而不是链接 `libmysqlclient`/`libpq`——和项目里其他 Runner（`perf`/`bpftrace`/`async-profiler`）一样是"调用外部工具+解析输出"的路数，不引入新的 C++ 编译依赖。
- 查询失败只打日志、不推送指标，不拖垮采集循环（`exec_capture` 非零返回码直接 return）。

**未做**：PostgreSQL 分支（架构留了口子，`collect_db_window` 里按 `engine` 分支即可接入，还没写）；阶段二的 digest/锁查询。

## 3. 配置下发链路

三处代码，服务端零改动（`Labels` 本来就是无迁移的透传字段），Agent 侧两处：

1. **服务端约定**：`CreateContinuousSessionReq.Labels` 里放 `db_targets: [{engine, instance_label, host, port, user, password_ref, poll_interval_sec, query_timeout_ms}]`。服务端原样存 jsonb、原样通过 `/api/v1/internal/continuous/reconcile` 下发，不需要改 Go 代码。
2. **Agent 解析**（`drop/agent/ContinuousSessionManager.cpp` `ParseAssignments`）：**发现并修复了一个真实的序列化陷阱**——`model.ContinuousSession.Labels` 是 Go 的 `[]byte`，没有自定义 `MarshalJSON`，`encoding/json` 会把它编码成 **base64 字符串**，而不是嵌套 JSON 对象。这意味着 reconcile 响应里的 `"labels"` 字段是"JSON 包了一层 base64"，直接当 JSON 对象解析会失败。修的办法：C++ 侧新增了一个最小 base64 解码函数（项目里之前没有），解码后再 `json::parse`，取 `db_targets` 数组填进 `ContinuousAssignment.dbTargets`。解码/解析失败只打日志，不影响 CPU/IO/sched 主链路（数据库巡检是可选能力）。
3. **Sampler 实例化**（`ContinuousSessionManager::ReconcileDBSampler`，在 `ApplyAssignments` 里对每个非停止态 Runtime 调用）：数据库巡检**不接入** `SharedDualTrackContinuousSampler` 的"整机唯一物理采集器+按 PID 分流"模型——那套模型是为了绕开 perf_event/eBPF 只能有一个物理挂载点的硬件限制，数据库短连接查询没有这个约束。改成**每个 Session 的 `Runtime` 独立持有自己的 `DBSnapshotSampler`**，生命周期跟 Runtime 走，`StopRuntime()` 里一并 `Stop()`。

## 4. 已知限制（记录，不是本次要解决的点）

- `ReconcileDBSampler` 当前是"已运行就跳过"，`db_targets` 变化不会热更新，需要重建 Session 才生效。
- `DBSnapshotSampler` 和 `PerfEventSampler`/`SharedDualTrackContinuousSampler` 会往**同一个** `session_spool_directory`（按 `sessionSID` 派生）并发写批次文件——两者各自的 `run_continuous_spool_loop` 在独立线程里跑。文件级操作（原子写+rename+unlink）在 Linux 上对不同文件名是安全的，但这是本次新引入的"同一 session 目录下两个线程并发读写"场景，之前从未出现过（只有一个物理 sampler），**需要在阶段一验收时专门测一下高频并发下 spool 目录读写有没有意外**，不能只假设没问题。
- PostgreSQL 支持未实现。

## 5. 验收步骤

1. 编译验证（本地 Windows 环境没有 Linux 工具链，须由组员/CI 跑）：
   ```
   docker compose build drop_agent
   ```
   确认新增的 `DBSnapshotSampler`/base64 解码/`collect_mysql_target_metrics` 能过编译。
2. 起一个测试 MySQL 容器，创建 ContinuousSession 时在 `labels.db_targets` 填入该容器的连接信息，`password_ref` 指向 agent 容器内一个提前写好的密码文件。
3. 观察 agent 日志：应该能看到 `db_snapshot` sampler 启动（无 `db snapshot sampler failed to start` 错误），以及 MySQL 查询没有 `SHOW GLOBAL STATUS failed` 报错。
4. 服务端确认对应 Session 的批次里出现 `runtime` 前缀为 `mysql:` 的 `metrics` 条目（`db_active_connections`/`db_questions_total`/`db_innodb_buffer_pool_hit_ratio_bps`）。此时前端还没有专门的 `db` tab（属于后续子任务），可以先直接查对象存储里的 window JSON 或加一条临时日志确认。
5. 断网/关掉 MySQL 测试目标，确认 `collect_mysql_target_metrics` 只是跳过、不崩溃、不影响同一 session 里 CPU/IO/sched 主链路继续工作。
6. 停止 Session，确认 `dbSampler` 被 `StopRuntime()` 正确 Stop，且未上传完的批次能被 `AdvanceStoppingSessions` 正常排空（复用现有 spool 逻辑，理论上不需要额外代码，需要实测确认）。

---

## 进度记录

**2026-08-20**：
- 阶段一数据结构 + `DBSnapshotSampler` 骨架 + MySQL 标量采集（`SHOW GLOBAL STATUS`）实现完成：`drop/common/ContinuousSampler.h`/`.cpp`。
- 配置下发三处打通：`drop/agent/ContinuousSessionManager.h`/`.cpp`（`ContinuousAssignment.dbTargets`、`ParseAssignments` 新增 base64 解码+labels 解析、`Runtime.dbSampler`、`ReconcileDBSampler`、`StopRuntime` 一并停止）。
- **未编译验证**（本地无 Linux 工具链），需要 `docker compose build drop_agent` 确认。
- 未做：PostgreSQL 分支、阶段二 digest/锁查询、服务端查询分支、前端 `db` tab、单测。
