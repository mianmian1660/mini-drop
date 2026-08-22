# 数据库性能采集设计

> 解决"持续采集只能看 CPU/调度/IO，看不到数据库内部锁/执行计划/慢查询"的问题。
> 不部署 `mysqld_exporter`/`postgres_exporter` 等现成工具，学习其采集口径后自研实现，集成进现有持续采集管道（`drop/common/ContinuousSampler.cpp`）。
> eBPF 协议解析（零账号抓 SQL）留作后续方向，不在本次范围。

## Summary

新增 `DBSnapshotSampler`，与 `PerfEventSampler`/`DualTrackContinuousSampler` 同级实现 `drop::Sampler` 接口，复用今天（2026-08-20，`bd03f86`）加固过的 `run_continuous_spool_loop()` 管道（spool/重试/ACK/背压/停止排空），不重新解决可靠性问题。分两阶段交付：

- **阶段一**（已实施）：借鉴 `mysqld_exporter`/`postgres_exporter` 的指标口径，采标量健康指标（连接数/QPS/buffer pool 命中率），写入现成的 `MetricPayload`。
- **阶段二**（已实施）：借鉴 `pg_stat_monitor`/Percona PMM Query Analytics 的 digest 聚合思路，采 SQL digest（跨轮次增量 diff）与锁等待链，新增 `WindowPayload.dbSnapshots` 字段，打通服务端 `db_snapshot` 查询分支与前端"数据库"信号页。

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

**未做**：PostgreSQL 分支（架构留了口子，`collect_db_window` 里按 `engine` 分支即可接入，还没写）。

## 2b. 采集逻辑（阶段二）

### SQL digest（`collect_mysql_target_digests`）

查 `performance_schema.events_statements_summary_by_digest`，按 `SUM_TIMER_WAIT` 取 TopN 50。

**关键点：这张表里的计数器是自服务器启动（或上次 TRUNCATE）以来的累计值，不是窗口值。** 直接上报会把"服务器启动至今的全部历史调用"当成本窗口发生的事，数字完全失真。因此采集器保存上一轮的 `COUNT_STAR`/`SUM_TIMER_WAIT`/`SUM_ROWS_EXAMINED`，只上报差值：

- 第一次见到某个 digest：只记基线，本轮不上报（否则首个窗口会是一个巨大的假尖峰）。
- 检测到 `COUNT_STAR` 变小：说明表被 `TRUNCATE` 或服务器重启，视为新基线，本轮不上报。
- 差值为 0（本窗口没有新调用）：跳过，不产生空条目。

这份跨轮次状态放在进程内的 `g_digestState`（`instanceLabel|digest` 为 key，`g_digestStateMutex` 保护）——不同 Session 可能采同一个数据库实例，各自的 `DBSnapshotSampler` 跑在独立线程里。

耗时单位换算：`SUM_TIMER_WAIT` 是皮秒，除以 `1e6` 转成微秒后存入 `totalLatencyUs`。

SQL 只存数据库自己归一化过的 `DIGEST_TEXT`（`SELECT * FROM t WHERE id = ?` 这种占位符形式），**不存原始 SQL 和参数值**。

### 锁等待链（`collect_mysql_target_lock_waits`）

查 MySQL 内置的 `sys.innodb_lock_waits` 视图，直接拿到 `waiting_pid`/`waiting_query`/`blocking_pid`/`blocking_query`/`wait_age_secs`/`locked_table`。

选这个视图而不是自己 join `performance_schema.data_locks` + `data_lock_waits` + `information_schema.innodb_trx`：`sys` 是 MySQL 5.7.7+ 自带的系统 schema（不是第三方工具），它已经把三表关联和 pid 映射拼好了，自己手写这个 join 更容易出错。**这仍然属于"自研"**——我们自己写 SQL、自己解析输出、自己决定采样窗口和上报格式，只是没有重复造一个已经在数据库里的视图。

`sys` schema 可能被 DBA 删除或禁用，查询失败只记日志，不影响标量指标和 digest 采集继续跑。

两个查询都用 `REPLACE(REPLACE(x, '\n', ' '), '\t', ' ')` 把 SQL 文本里的换行/制表符换成空格——这两个字符会破坏 `--batch` 输出的行/列分隔，在 SQL 侧处理比在 C++ 侧写转义解析更简单可靠。

### 数据结构与链路

- `DBSnapshotSample`（`ContinuousSampler.cpp`）：`kind` 字段区分 `"digest"` / `"lock_wait"`，两种语义共用一个结构体，未用到的字段留零值——沿用 `HistogramPayload` 用 `unavailable`/`reason` 标记状态的做法，避免为每种 kind 单开结构体和序列化分支。
- `WindowPayload.dbSnapshots` 新数组字段，在 `build_batch_json` 里序列化成 window 级的 `db_snapshots` 数组。
- 服务端 `ContinuousDBSnapshotIngest`（`apiserver/server/continuous.go`）反序列化；`continuousWindowSignalRows` 在窗口含 `db_snapshots` 时登记 `db_snapshot` 信号行，`backend` 记为 `db_system_views`。
- 查询端点 `GET /api/v1/continuous/db-snapshot`（`QueryContinuousDBSnapshot`），仿照 `queryNativeContinuousHistogram` 的模式从对象存储加载批次。**digest 跨窗口累加后按总耗时排序取 TopN 50；锁等待逐条保留只按等待时长排序**——因为"谁在等谁"是时点事实，聚合会丢掉定位根因需要的关键信息。
- 前端 `ContinuousProfilingPanel.js` 新增 `signalTab === 'db'`（信号切换条上的"数据库"按钮），渲染 `DBSnapshotPanel`：三个 `Metric` 概览卡 + 锁等待链表格 + 慢查询 digest 表格，复用已有的 `S.table`/`S.th`/`S.td`/`S.summaryGrid` 样式。

## 3. 配置下发链路

三处代码，服务端零改动（`Labels` 本来就是无迁移的透传字段），Agent 侧两处：

1. **服务端约定**：`CreateContinuousSessionReq.Labels` 里放 `db_targets: [{engine, instance_label, host, port, user, password_ref, poll_interval_sec, query_timeout_ms}]`。服务端原样存 jsonb、原样通过 `/api/v1/internal/continuous/reconcile` 下发，不需要改 Go 代码。
2. **Agent 解析**（`drop/agent/ContinuousSessionManager.cpp` `ParseAssignments`）：**发现并修复了一个真实的序列化陷阱**——`model.ContinuousSession.Labels` 是 Go 的 `[]byte`，没有自定义 `MarshalJSON`，`encoding/json` 会把它编码成 **base64 字符串**，而不是嵌套 JSON 对象。这意味着 reconcile 响应里的 `"labels"` 字段是"JSON 包了一层 base64"，直接当 JSON 对象解析会失败。修的办法：C++ 侧新增了一个最小 base64 解码函数（项目里之前没有），解码后再 `json::parse`，取 `db_targets` 数组填进 `ContinuousAssignment.dbTargets`。解码/解析失败只打日志，不影响 CPU/IO/sched 主链路（数据库巡检是可选能力）。
3. **Sampler 实例化**（`ContinuousSessionManager::ReconcileDBSampler`，在 `ApplyAssignments` 里对每个非停止态 Runtime 调用）：数据库巡检**不接入** `SharedDualTrackContinuousSampler` 的"整机唯一物理采集器+按 PID 分流"模型——那套模型是为了绕开 perf_event/eBPF 只能有一个物理挂载点的硬件限制，数据库短连接查询没有这个约束。改成**每个 Session 的 `Runtime` 独立持有自己的 `DBSnapshotSampler`**，生命周期跟 Runtime 走，`StopRuntime()` 里一并 `Stop()`。

## 4. 已知限制（记录，不是本次要解决的点）

- `ReconcileDBSampler` 当前是"已运行就跳过"，`db_targets` 变化不会热更新，需要重建 Session 才生效。
- `DBSnapshotSampler` 和 `PerfEventSampler`/`SharedDualTrackContinuousSampler` 会往**同一个** `session_spool_directory`（按 `sessionSID` 派生）并发写批次文件——两者各自的 `run_continuous_spool_loop` 在独立线程里跑。文件级操作（原子写+rename+unlink）在 Linux 上对不同文件名是安全的，但这是本次新引入的"同一 session 目录下两个线程并发读写"场景，之前从未出现过（只有一个物理 sampler），**需要在阶段一验收时专门测一下高频并发下 spool 目录读写有没有意外**，不能只假设没问题。
- PostgreSQL 支持未实现（标量、digest、锁三类都没有）。
- 阶段二每轮采集会对同一个 MySQL 目标发起**三次独立短连接**（标量 / digest / 锁），各自建连、各自写一次临时 defaults 文件。目标库连接数紧张时这会放大压力，可以合并成一次连接多条查询，但会让三类采集的失败互相牵连（现在是一类失败不影响另外两类）。默认 10s 轮询间隔下三连接的开销可接受，先保持隔离性。
- `g_digestState` 只增不删：长期运行且 digest 基数很大的实例上，这个 map 会持续增长。需要加按最后访问时间的淘汰，本次未做。
- digest 的第一个采集窗口必然为空（要先建基线才能算差值），Session 刚启动后约一个轮询周期内前端 digest 表为空属于预期行为，不是故障。
- `sys.innodb_lock_waits` 需要账号有 `PROCESS` 权限及对 `sys` 的 `SELECT` 权限；`events_statements_summary_by_digest` 需要 `performance_schema` 已启用（MySQL 5.6+ 默认开启）。权限不足时对应查询失败，只记日志。
- `db_snapshot` 信号目前**没有冷层摘要**：`downsampleContinuousWindows` 只对 `cpu_profile` 做降采样保留 7 天，`db_snapshot` 和 `io_latency`/`sched_latency` 一样，过了 `retention_hours`（默认 24h）就硬删。凌晨 3 点的锁等待现场如果隔天才排查，仍然查不到——这是本次没有解决的遗留问题。

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

### 阶段二验收步骤

7. **digest 增量正确性**：在测试库上跑 N 次同一条 SQL，等一个采集窗口，确认前端 digest 表里该条的"调用次数"等于 N，而不是数据库启动至今的累计值。再等一个窗口不发请求，确认该条不再出现（差值为 0 时不上报）。
8. **首窗口基线行为**：新建 Session 后第一个窗口 digest 表应为空，第二个窗口开始有数据。这是预期行为，不是 bug。
9. **锁等待链**：开两个 MySQL 会话，A 执行 `BEGIN; SELECT ... FOR UPDATE;` 不提交，B 对同一行执行 `UPDATE`，B 会挂起。等一个采集窗口后确认前端"锁等待链"表出现一条记录，`blocking_pid` 是 A 的连接 id、`waiting_pid` 是 B 的、`locked_table` 是对应表名。提交 A 后确认后续窗口不再有该记录。
10. **权限降级**：用一个没有 `PROCESS` 权限的账号跑一遍，确认锁等待查询失败只打日志，标量指标和 digest 仍然正常上报（三类采集互不牵连）。
11. **端到端**：`GET /api/v1/continuous/db-snapshot?...` 直接调用，确认返回 `digests`/`lock_waits` 两个数组；前端切到"数据库"信号页确认两张表正常渲染。

---

## 进度记录

**2026-08-20**：
- 阶段一数据结构 + `DBSnapshotSampler` 骨架 + MySQL 标量采集（`SHOW GLOBAL STATUS`）实现完成：`drop/common/ContinuousSampler.h`/`.cpp`。
- 配置下发三处打通：`drop/agent/ContinuousSessionManager.h`/`.cpp`（`ContinuousAssignment.dbTargets`、`ParseAssignments` 新增 base64 解码+labels 解析、`Runtime.dbSampler`、`ReconcileDBSampler`、`StopRuntime` 一并停止）。
- **未编译验证**（本地无 Linux 工具链），需要 `docker compose build drop_agent` 确认。
- 未做：PostgreSQL 分支、阶段二 digest/锁查询、服务端查询分支、前端 `db` tab、单测。

**2026-08-22**（阶段二）：
- Agent：`DBSnapshotSample` 结构 + `WindowPayload.dbSnapshots` + `build_batch_json` 序列化；`collect_mysql_target_digests`（含跨轮次增量 diff 状态 `g_digestState`）、`collect_mysql_target_lock_waits`，接入 `collect_db_window`。文件：`drop/common/ContinuousSampler.cpp`。
- 服务端：`ContinuousDBSnapshotIngest` 结构；`continuousWindowSignalRows`/`continuousWindowSampleCount`/`normalizeContinuousSignalTypes` 三处登记 `db_snapshot` 信号；新增 `QueryContinuousDBSnapshot` + `queryNativeContinuousDBSnapshot`；路由 `GET /api/v1/continuous/db-snapshot`。文件：`apiserver/server/continuous.go`、`apiserver/server/server.go`。
- 前端：`continuous.dbSnapshot` API；`signalTab === 'db'` 分支 + `DBSnapshotPanel` 组件（锁等待链表 + digest 表）。文件：`web_frontend/src/api/index.js`、`web_frontend/src/components/ContinuousProfilingPanel.js`。
- **编译状态**：`go build ./...` 通过，`npm run build` 通过。**C++ 仍未编译验证**（本地 Docker daemon 未启动），阶段一阶段二的 C++ 改动都需要一次 `docker compose build drop_agent`。
- 未做：PostgreSQL 分支、单测（`drop/tests/test_continuous_sampler.cpp` 的 digest diff 解析用例、`apiserver/server/continuous_test.go` 的 db_snapshot ingest/query 用例）、`db_snapshot` 冷层摘要。
