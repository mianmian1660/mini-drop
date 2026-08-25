# 数据库性能采集功能 — 现状总结 / 验收清单 / 未解决问题

> 配合设计文档一起看:[docs/db-performance-collection-design.md](db-performance-collection-design.md)
> 本文是 2026-08-22 联调阶段的状态快照,重点是"卡在哪、要验什么、还差什么",不是设计说明。

---

## 1. 功能代码总结(现状架构)

一句话:双层采集(外层 perf/eBPF 已有,内层 `DBSnapshotSampler` 本次新增),`DBSnapshotSampler` 分两阶段(标量指标 / digest+锁等待链),数据经既有的 spool/重试/ACK 管道上传,前端在"数据库"tab 展示。**联调阶段已打通数据链路(2026-08-23 09:00),阶段一/阶段二的实际效果已通过持续流量验证,故障注入验收(见第 2 节)待开始。**

### 1.1 Agent 侧(C++)

| 模块 | 文件:行号 | 内容 |
|---|---|---|
| 数据结构 | [ContinuousSampler.h:37-47](../drop/common/ContinuousSampler.h:37) | `DBTargetConfig`(engine/host/port/user/passwordRef/pollIntervalSec/queryTimeoutMs) |
| 数据结构 | [ContinuousSampler.cpp:444-467](../drop/common/ContinuousSampler.cpp:444)(约) | `DBSnapshotSample`(kind=digest\|lock_wait)、`WindowPayload.dbSnapshots` |
| 阶段一采集 | [ContinuousSampler.cpp:3164](../drop/common/ContinuousSampler.cpp:3164) `collect_mysql_target_metrics` | `SHOW GLOBAL STATUS` → 连接数/QPS/buffer pool 命中率,写入 `MetricPayload` |
| 阶段二采集 | [ContinuousSampler.cpp:3349](../drop/common/ContinuousSampler.cpp:3349) `collect_mysql_target_digests` | `performance_schema.events_statements_summary_by_digest` 增量 diff,跨轮次状态用静态 map 保存 |
| 阶段二采集 | [ContinuousSampler.cpp:3428](../drop/common/ContinuousSampler.cpp:3428) `collect_mysql_target_lock_waits` | `sys.innodb_lock_waits` 一次性查询,拼装 waiting/blocking pid+SQL |
| 采集入口 | [ContinuousSampler.cpp:3493](../drop/common/ContinuousSampler.cpp:3493) `collect_db_window` | 遍历 `dbTargets`,只有 `engine=="mysql"` 会真正采;PostgreSQL 分支未实现 |
| 节流修复 | [ContinuousSampler.cpp:3506-3524](../drop/common/ContinuousSampler.cpp:3506) | 2026-08-22 新增:采集完补 sleep 到 `aggregationWindowSec`,并兜底 `endMs>startMs`。修复本身有效;当时"问题依旧"的真相是磁盘背压暂停采集 + 测试流量 SCHEMA_NAME 问题(见第 3 节) |
| Sampler 生命周期 | [ContinuousSampler.cpp:3531-3592](../drop/common/ContinuousSampler.cpp:3531) `DBSnapshotSampler::*` | 与 `PerfEventSampler` 同级实现,复用 `run_continuous_spool_loop` |
| 配置解析 | [ContinuousSessionManager.cpp:43](../drop/agent/ContinuousSessionManager.cpp:43) `base64_decode` | 修了 Go `[]byte` jsonb 字段被自动 base64 编码这个坑,没有这一步 `labels.db_targets` 会静默解析失败 |
| 配置解析 | [ContinuousSessionManager.cpp:428-437](../drop/agent/ContinuousSessionManager.cpp:428) | `ParseAssignments` 解码 labels → 填充 `assignment.dbTargets` |
| Sampler 挂接 | [ContinuousSessionManager.cpp:582](../drop/agent/ContinuousSessionManager.cpp:582) `ReconcileDBSampler` | 每个 Runtime 独立持有一个 `DBSnapshotSampler`,不走 `SharedDualTrackContinuousSampler` 的整机共享模型 |

### 1.2 服务端(Go)

| 模块 | 文件:行号 | 内容 |
|---|---|---|
| 批次校验 | [continuous.go:698](../apiserver/server/continuous.go:698) `!req.StartTime.Before(req.EndTime)` | 就是这行在拒收——本次卡点的直接触发点 |
| 数据模型 | [continuous.go:102-109](../apiserver/server/continuous.go:102) | `ContinuousDBSnapshotIngest` |
| 查询接口 | [continuous.go:2323](../apiserver/server/continuous.go:2323) `QueryContinuousDBSnapshot` | `GET /api/v1/continuous/db-snapshot`,已注册路由([server.go:737](../apiserver/server/server.go:737)) |
| 信号注册 | [continuous.go:3125/3173/3228](../apiserver/server/continuous.go:3125) | `db_snapshot` 已接入信号统计/去重三处函数 |

### 1.3 前端(React)

| 模块 | 文件:行号 | 内容 |
|---|---|---|
| Tab 入口 | [ContinuousProfilingPanel.js:100-106](../web_frontend/src/components/ContinuousProfilingPanel.js:100) `SIGNAL_TAB_OPTIONS` | 加了 `db` tab,靠 `labels.db_targets` 非空触发显示(不是走 `session.signals`) |
| 展示组件 | [ContinuousProfilingPanel.js:1745](../web_frontend/src/components/ContinuousProfilingPanel.js:1745) `DBSnapshotPanel` | 摘要卡片 + 锁等待链表 + digest 表 |
| **已知缺口** | [CreateContinuousSessionModal.js](../web_frontend/src/components/CreateContinuousSessionModal.js) | 建 Session 弹窗**没有** `db_targets` 输入框,目前只能直接调 API 创建 |
| **已知缺口** | [ContinuousSessionList.js:164-171](../web_frontend/src/components/ContinuousSessionList.js:164) | 会话列表页看不出哪个 Session 配了数据库巡检 |

---

## 2. 验收任务清单

链路已打通(2026-08-23 09:00,见 3.4),以下 7 个场景全部**待执行**。任务按优先级排序,不是表格原顺序——排序理由和每个任务的前置条件见下方。所有任务默认都要先满足 [4.3 环境层](#43-用户操作注意事项验收复跑必读) 的三条(密码文件重写、磁盘 >=1GiB、二进制是新版),不再逐条重复。

| # | 任务 | 优先级理由 | 前置条件(除环境层外) | 状态 |
|---|---|---|---|---|
| 1 | **锁等待**(双连接互相持锁阻塞) | 唯一一个"刚修完权限、还没在真实竞争下验证过"的场景——3.3 只验证过"无锁时不报错",没验证过"真有锁等待时能不能采到";优先级最高 | 4.3 §7:锁等待要维持 ≥30s(跨过至少一个 10s poll 周期),不能一闪而过 | ✅ 已通过(2026-08-23:waiting=216/blocking=215,minio batch 中 lock_wait wait_seconds 5→15→25s 持续增长,locked_table 正确;3.3 权限修复验证生效) |
| 2 | **死锁**(双连接交叉加锁) | 和任务 1 同一套连接脚本改一下加锁顺序即可复用,顺手做 | 依赖任务 1 跑通后的脚本 | ⚠️ 部分验证(2026-08-23:InnoDB 确认 LATEST DETECTED DEADLOCK,A 锁行1请求行2 vs B 锁行2请求行1,MySQL 自动回滚;但 lock_wait 链未被采到——死锁 1-2s 内结束,agent 10s poll 周期错过,属已知时序限制:lock_wait 只对持续阻塞>10s 的锁等待有效) |
| 3 | **QPS 骤降**(导师点名重点场景) | 导师会议记录里唯一点名的场景,四类里最高优先级 | 4.3 §5/§6:sysbench 起量后要跑够久再 `pkill`,`pkill` 后等 90s 再查`db_questions_total` 增量拐点 | ✅ 已通过(2026-08-23:流量跑时 `db_questions_total` +45~49/10s,停流量后骤降为 +9/10s(agent 自身查询残留),增量下降约 80%,拐点清晰) |
| 4 | **连接风暴**(50 个 `SLEEP(600)` 连接) | 实现最简单,验证 `db_active_connections` 阶段一指标即可 | 无特殊前置,注意连接数别把 MySQL `max_connections` 打爆导致误判成"数据库不可达" | ✅ 已通过(2026-08-23:注入 30 个 SLEEP 后 `Threads_connected=31`,minio batch 中 `db_active_connections=31` 跨窗口持续采集,时间对齐;但无查询接口,仅从 minio 原始数据验证——见 3.6) |
| 5 | **buffer pool 命中率骤降** | 同上,阶段一标量指标验证 | 需要一张明显大于 buffer pool 的表做全表扫,`orders` 库如果太小要先灌数据 | ✅ 已通过(2026-08-23:灌数据到 order_line 478MB(1258 万行)>> 128MB buffer pool,重启 MySQL 清空缓存后无索引全表扫,`db_innodb_buffer_pool_hit_ratio_bps` 从 9998 骤降到 8859,拐点与扫描对齐。要点:①`information_schema` 表大小统计滞后需 `ANALYZE`;②`FLUSH TABLES` 不清 InnoDB buffer pool 页,必须重启 MySQL 清空缓存才能触发磁盘读) |
| 6 | **慢查询堆积**(digest) | 3.4 排障过程里已经用等价流量side-effect验证过 digest 能落库,这里是补一次**正式**的验收留证据,预期风险最低 | 4.3 §4:流量必须带默认库,否则复现 3.4 的 SCHEMA_NAME=NULL 老坑 | ✅ 已通过(2026-08-23:1258 万行表全表扫流量,digest 样本 `SELECT COUNT ( * ) FROM order_line WHERE STATUS = ?` call_count 2→3 增量正常,`total_latency_us` 7-10s 清晰暴露慢查询) |
| 7 | **数据库不可达**(`docker stop fault-mysql`) | 已知盲区,验的是"不崩"不是"采到数据",优先级最低但成本也最低,可以最后顺手做 | 无 | ✅ 已通过(2026-08-23:`docker stop fault-mysql` 后 30s,agent 容器 Up 心跳正常、cpu batch 正常 ACK、三条 db 查询 `failed rc=1` 仅打日志——采集跳过不拖垮主链路) |

**验收标准统一按 4.3 §9/§10**:`profile_windows` 出现对应 `signal_type='db_snapshot'` 窗口且 `window_end>window_start`;排查顺序 Session `degradation_reason` → agent 日志 → minio batch `db_snapshots` 数量。

---

## 2.5 端到端开销对比测试（导师要求 1，2026-08-23 完成 ✅）

**目的**:回答"DBSnapshotSampler 是否挤占主链路(纯 CPU profiling)"。设计为同机双轮对照,唯一变量是 `db_targets`:

| 轮次 | Session 配置 | MySQL 流量 |
|---|---|---|
| baseline | 无 `db_targets`(纯 CPU profiling) | 无 |
| withdb | 带 `db_targets`(CPU + db 采集) | 持续轻量查询(走主键,产生 digest 增量) |

两轮都是 host scope、`signals=["cpu_profile","io_latency","sched_latency"]`、`sample_rate_hz=9`、`aggregation_window_sec=10`、`upload_batch_sec=30`,各跑约 3 分钟。测试脚本:`scripts/db_overhead_compare.sh`。

**结果**:

| 指标 | baseline | withdb | 增量 |
|---|---|---|---|
| agent CPU 均值(24×4s 采样) | 30.75% | 34.34% | **+3.59pp**(相对 +11.7%) |
| agent 内存均值 | 115.01 MiB | 122.67 MiB | **+7.66 MiB**(相对 +6.7%) |
| cpu_profile 窗口数 | 86 | 85 | 持平(均 ~10s/窗口) |
| cpu 采样 sample_count 均值 | 6 | 8 | 持平 |
| db_snapshot 窗口 | — | **20 个连续** | 链路全程工作 |

**验证 withdb 阶段 db 采集确实在采**(非空窗口):minio batch 中 digest 样本 `SELECT id,status FROM order_line WHERE id=?` call_count=9/total_latency_us=1666,标量 `db_active_connections=1`、`db_questions_total=269`、`db_innodb_buffer_pool_hit_ratio_bps=9333`;20 个 `db_snapshot` 窗口每 10s 一个、无断点。

**结论**:
1. **db 采集额外 CPU 开销约 +3.6 个百分点,内存约 +7.7 MiB**。agent 基线 CPU 本身约 30%(perf 采样 + eBPF 主链路),db 采集是其 ~1/8,绝对增量很小。
2. **不挤占主链路**:cpu_profile 窗口数与 sample_count 两轮持平,证明 db 采集(每 10s 一条连接、三条 SQL)没有影响 CPU 采样质量。
3. 设计目标达成——DBSnapshotSampler 作为独立采集器,复用 `run_continuous_spool_loop`,对主链路影响可忽略。

---

## 3. 目前的问题

### 3.1 阻塞项(P0,必须先解决,故障注入验收才能开始)

**`profile_windows` 里 0 条 `db_snapshot` 数据。** 最初表象是 batch 被服务端拒收 `"batch 时间范围不合法"`,2026-08-23 凌晨连查后澄清为**两件事叠加,根因已分别定位**:

1. **`startMs==endMs` 拒收(已被节流修复解决,commit `6221a34`)**。`collect_db_window` 内部三条 SQL 毫秒级返回导致窗口起止同毫秒,`!StartTime.Before(EndTime)`([continuous.go:698](../apiserver/server/continuous.go:698))拒收。修复已提交并部署:[ContinuousSampler.cpp:3506-3524](../drop/common/ContinuousSampler.cpp:3506) 补节流 sleep 到 `aggregationWindowSec` + `endMs>startMs` 兜底。
2. **真正长期阻塞:磁盘背压暂停采集(2026-08-23 凌晨最终定位)**。节流修复部署后 `profile_windows` 仍空,追查发现 agent 一直处于"采集暂停"状态:
   - agent 日志刷 `[native-cp] spool backpressure ... free_bytes=1.74GB min_free_bytes=2147483648`
   - Session `degradation_reason="shared spool backpressure: free disk is below the configured reserve; collection is paused and will resume automatically"`
   - 机制:`spool_free_bytes`([ContinuousSampler.cpp:296](../drop/common/ContinuousSampler.cpp:296))用 `statvfs(spoolDirectory)` 统计磁盘剩余,**低于 `spoolMinFreeBytes`(1GiB)时暂停全部持续采集**(防 spool 写满把系统写崩),磁盘恢复到 >=1GiB 后自动 resume,这是设计内的自我保护,不是 bug。
   - 触发原因:VM 磁盘 94% 满(25G/29G)。`docker builder prune -af` + `docker system prune -f` 清出 3.4G 可用后背压解除(`grep -c "spool backpressure"` = 0)。
   - 经验点:VM 的 docker 数据不在 `/var/lib/docker`(`du -sh` 仅 4.0K),疑似 rootless docker(数据在 `~/.local/share/docker`),排查磁盘占用时别只看 `/var/lib/docker`。

**当前状态(2026-08-23 01:30)**:背压已解除、db 采集已恢复运行(日志出现 `db target=orders-mysql lock wait query failed` 证明 dbTargets 解析、密码读取、采集循环全部正常,仅 lock_wait 单独失败)。**digest/标量落库待最终确认**(见 3.4)。

### 3.2 联调阶段顺带发现的新问题(已修复)

**坏批次会被重试到天荒地老 —— 已修复(合并自组员分支,commit 见 `cd58268` 附近的 merge)。** `drain_one_spooled_batch` 曾经只区分 `Acknowledged`/`BatchIDConflict` 两种结果,其余一律无限期指数退避重试。现在已经加了第三种结果 `SpoolPostResult::PermanentlyRejected`([ContinuousSampler.cpp:2412-2470](../drop/common/ContinuousSampler.cpp:2412)):`response_is_permanent_rejection()` 读 4xx + 响应体里的 `"retryable":false`,命中后 `quarantine_rejected_spooled_batch()`([2472](../drop/common/ContinuousSampler.cpp:2472))把批次隔离掉,不再重试。两处调用点(`drain_one_spooled_batch` 内、约 2634/3212 行)都已接上。这条不用再跟进。

### 3.3 阶段二 lock_wait 采集的 MySQL 权限问题(2026-08-23 定位,已修复)

**现象**:db 采集恢复后,agent 日志持续刷 `db target=orders-mysql lock wait query failed rc=1`。

**根因**:`sys.innodb_lock_waits` 是 `SQL SECURITY INVOKER` 视图(`SHOW CREATE VIEW` 确认),其定义底层引用 `performance_schema.data_lock_waits`/`performance_schema.data_locks`/`information_schema.INNODB_TRX`,并调用 `sys.quote_identifier()`/`sys.format_statement()` 存储函数。采集账号 `mini_drop` 缺权限 → `ERROR 1356 (HY000): View 'sys.innodb_lock_waits' references invalid table(s) or column(s) ... or definer/invoker of view lack rights to use them`。

**教训**:`GRANT SELECT ON sys.*` 和 `GRANT SELECT ON performance_schema.*` 通配在 MySQL 8 不够——视图内部函数需要 `EXECUTE`,`performance_schema` 的 `data_locks`/`data_lock_waits` 等表需要显式逐表授权。

**修复(已执行)**:
```sql
GRANT EXECUTE ON sys.* TO 'mini_drop'@'%';
GRANT SELECT ON performance_schema.data_locks TO 'mini_drop'@'%';
GRANT SELECT ON performance_schema.data_lock_waits TO 'mini_drop'@'%';
GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO 'mini_drop'@'%';
FLUSH PRIVILEGES;
```
手动查询已不再报错(无锁等待时返回空集,rc=0)。**待办**:注入真实锁等待场景,确认 `sys.innodb_lock_waits` 能采到 waiting/blocking 对。

### 3.4 digest 采不到数据的根因（2026-08-23 09:00 定位并解决）

**现象**:`profile_windows` 无 `db_snapshot`,minio batch 里 `db_snapshots` 恒为 0。

**根因**:测试流量循环 `mysql -uroot -proot -e "SELECT COUNT(*) FROM orders.order_line WHERE status='new'"` **不带默认库** → `events_statements_summary_by_digest.SCHEMA_NAME` = **NULL** → agent 的 digest 查询 `WHERE SCHEMA_NAME IS NOT NULL AND DIGEST IS NOT NULL`([ContinuousSampler.cpp:3363](../drop/common/ContinuousSampler.cpp:3363))把这些语句全过滤 → digest 表对 agent 恒空 → 无增量 → dbSnapshots=0。**不是代码 bug,是测试流量的 SCHEMA_NAME 问题。**

**证据**:
- digest 表里 `NULL | SELECT COUNT ( * ) FROM orders . order_line WHERE STATUS = ? | 303`——SELECT 在表里但 SCHEMA_NAME 是 NULL。
- agent 自己的 digest 查询语句也在表里(COUNT_STAR=47),证明 agent 采集循环一直在跑。

**修复(测试侧,非代码)**:流量循环指定默认库,让 SCHEMA_NAME 非空:
```bash
for i in $(seq 1 300); do docker exec fault-mysql mysql -uroot -proot orders -N -e "SELECT COUNT(*) FROM order_line WHERE status='new';" >/dev/null 2>&1; sleep 1; done &
```
(mysql 命令第 3 个参数 = 默认库,SQL 用不带 schema 的表名)

**结果**:修复后 `profile_windows` 出现 `db_snapshot|8|2026-08-23 01:03:02+00|01:04:22+00`,**链路打通**。

**踩坑记录**:排查时先用 `LIKE '%COUNT(*) FROM%'` 查 digest 表返回空,误以为 SELECT 没进表——实际是 MySQL 归一化文本带空格(`SELECT COUNT ( * ) FROM ...`),LIKE 模式不匹配,需放宽条件(`LIKE '%COUNT%'`)。

### 3.4.5 三处代码缺陷修复(2026-08-24)

上一轮代码审查发现的三条,今天修完:

1. **digest 假尖峰**([ContinuousSampler.cpp:3711-3727](../drop/common/ContinuousSampler.cpp:3711) `compute_digest_delta`)——digest 查询只取 `LIMIT 50`,某条 digest 掉出前 50 又重新进来时,旧代码会把缺席期间攒的全部调用当成一个窗口的增量。修法:`DigestCounterState` 加 `lastSeenMs`,距上次刷新超过 `1.5 × aggregationWindowSec` 就强制重新计基线(`DigestDeltaKind::Reset`),不当增量上报。思路借鉴 Prometheus `rate()`/`increase()` 处理累计计数器的方式——先判断采样间隔是否正常连续,不是无脑做减法。
2. **`g_digestState` 内存泄漏**——复用上面新加的 `lastSeenMs`:`DBSnapshotSampler::Stop()`([3948](../drop/common/ContinuousSampler.cpp:3948))里按 `sessionSID` 前缀确定性清理(`clear_digest_state_for_session`);另外每次 digest 采集顺手做一次兜底扫描(`sweep_stale_digest_state_locked`,阈值 20 倍窗口时长),覆盖 Session 没走正常 Stop 路径的情况。
3. **临时密码配置文件命名碰撞 / 路径穿越 / ini 注入**——三处 `"/tmp/mini_drop_db_" + instanceLabel + 毫秒时间戳` 的拼接方式统一收敛成 `stage_mysql_defaults_file()`([3483](../drop/common/ContinuousSampler.cpp:3483)),用 POSIX 标准的 `mkstemp()` 生成唯一文件名(不再依赖 `instanceLabel` 拼路径,天然消除路径穿越和撞名风险),并且写入前校验 `user`/`password` 不含换行符,含了就拒绝该目标(防止 ini 配置注入)。

**未编译验证**(本地无 Linux 工具链),需要 `docker compose build drop_agent` 确认;g_digestState 相关改动涉及锁的使用需要重点复核(`sweep_stale_digest_state_locked`/`clear_digest_state_for_session_locked` 都要求调用方已持锁,已在代码里用 `_locked` 后缀标注调用约定,但没有单测覆盖这个约定)。

### 3.5 已知未做的功能缺口(设计上明确推迟,不是 bug)

- PostgreSQL 分支未实现(阶段一、阶段二都只有 MySQL)。
- 冷热分层(`ContinuousWindowSummary` 7 天摘要)只覆盖 `cpu_profile`,`db_snapshot` 没有对应的冷层摘要方案。
- `pollIntervalSec` 字段设计了但没有真正按 target 粒度生效,节流修复统一用 `aggregationWindowSec`,是已知简化。
- ~~db 标量指标无查询/展示接口~~ —— **2026-08-24 已解决**,见下方"已完成内容"。
- ~~阶段二异常检测/预警~~ —— **部分已解决**:已发现有人把 `db_snapshot` 接进了现成的 `SentinelRule` 机制(`DBSentinelEvents`,前端 `sentinelRules.events({signal:'db_snapshot',...})`),具体判异规则覆盖到什么程度还需要单独核实,不算完全空白了。
- ~~前端建 Session 表单没有 db_targets 输入框 / 会话列表页看不出哪个 Session 配了数据库巡检~~ —— **已解决**,见下方"已完成内容"(数据库账号页面 + 会话列表 chip)。

---

## 5. 已完成内容(2026-08-24 更新)

- **数据库账号管理页面**:顶部头像 → `/db-accounts`,选一个已有 Session 编辑 `db_targets`(实例标签/主机/端口/用户名/密码文件路径,密码不经过页面明文)。后端新增 `PATCH /api/v1/continuous/sessions/:sid/labels`([continuous.go](../apiserver/server/continuous.go) `UpdateContinuousSessionLabels`)。**发现并纠正了一处认知错误**:最初以为改 `db_targets` 必须停止重建 Session 才生效,后来确认 `ReconcileDBSampler` 只在"已经在跑"时才跳过——给**原本 db_targets 为空**的 Session 后补配置,下一次 reconcile(约 5 秒)会自动启动,不需要重建;只有改一个**已经在跑**的 db_targets 才需要重建。
- **会话列表数据库标识**:`ContinuousSessionList.js` 信号列加"数据库 · N"chip,不用点进详情页就能看出哪些 Session 配了数据库巡检。
- **digest 假尖峰修复**:`DigestCounterState` 加 `lastSeenMs`,`compute_digest_delta` 按距上次刷新的时间间隔(1.5× `aggregationWindowSec`)判断是否要重新计基线,避免 digest 掉出 TOP 50 又重新进来时把缺席期间的调用全算成一个窗口的增量。思路借鉴 Prometheus `rate()`/`increase()` 处理累计计数器的方式。
- **`g_digestState` 内存泄漏修复**:`DBSnapshotSampler::Stop()` 时按 sessionSID 前缀确定性清理 + 每次采集顺手做一次过期扫描兜底(阈值 20 倍窗口时长)。
- **临时密码配置文件安全加固**:三处拼路径的地方统一收敛成 `stage_mysql_defaults_file()`,用 `mkstemp()` 消除撞名/路径穿越风险,写入前拒绝含换行符的账号密码(防 ini 注入)。
- **标量指标可视化补全**:发现 `queryNativeContinuousDBSnapshot` 里已经有人写了从 `window.metrics` 扫 `db_` 前缀指标、按 metric+runtime 分组成时间序列的逻辑,但收集了却没放进最终返回值——补上排序+序列化,加进响应的 `metrics` 字段。前端 `DBSnapshotPanel` 新增"标量指标趋势"区块(连接数/QPS/缓冲池命中率三张小折线图),QPS 是前端用相邻窗口累计值差分换算出来的瞬时速率。同时修了一处判空逻辑漏洞:原来"digests 和 lock_waits 都空就显示空态"会把"只有标量指标、没有慢查询/锁等待"的正常情况误判成空。

**验收清单更新**:第 2 节的 7 类故障场景里,连接风暴、QPS 骤降(现在有专门的折线图可以直接看拐点)、buffer pool 命中率骤降 三项现在有界面可以直接验收,不用再翻 minio 原始 JSON;慢查询堆积、锁等待、死锁、数据库不可达 四项仍按原表验收方式(digest 表 / 锁等待表 / 日志确认)。

## 6. 采集层(Agent, C++)待办优先级

| 优先级 | 任务 | 理由 |
|---|---|---|
| **P0** | **编译验证 2026-08-24 三处修复**(digest 假尖峰 / `g_digestState` 清理 / 临时文件安全) | 这三处改动已经在跑的代码路径上,`sweep_stale_digest_state_locked`/`clear_digest_state_for_session_locked` 的"调用方必须已持锁"约定只靠命名和注释保证,没有单测覆盖。本地无 Linux 工具链没法自己验,但这是当前唯一"已合并但正确性未经验证"的代码,必须排在最前面——不确认不死锁/不数据竞争,后面所有基于这份代码的验收结果都不可信。**动作**:`docker compose build drop_agent` 编译通过 + 起一个真实场景跑几轮 digest 采集,观察 agent 进程有没有卡死或 CPU 异常(死锁的典型表现是某个线程再也不产出新数据)。 |
| **P1** | **死锁场景可观测性缺口** | 不是简单 bug,是采样频率和故障时长的根本性错配:10 秒轮询周期 vs 1-2 秒的死锁生命周期。直接调低 `aggregationWindowSec` 治标不治本(死锁可能更短,而且会显著加大对生产库的轮询压力)。**正确方向**:MySQL 的 `SHOW ENGINE INNODB STATUS` 输出里有一段 `LATEST DETECTED DEADLOCK`,记录的是"最近一次检测到的死锁"，不依赖轮询时机去撞见死锁瞬间——这是 MySQL 自己解决"死锁转瞬即逝"这个问题的标准方案,行业里 Percona Toolkit 的 `pt-deadlock-logger` 就是靠定期解析这段输出实现死锁记录的。可以在阶段二里加一个新的 `kind="deadlock"` 采集分支,复用同样的连接建立流程。 |
| **P1** | **PostgreSQL 分支未实现** | 功能完整性缺口,MySQL 这条路径走通、模式验证过之后,理论上是照抄阶段一/阶段二的思路换一套查询(`pg_stat_database`/`pg_stat_statements`/`pg_blocking_pids()`),工作量可预估,但目前还没排期动手。 |
| **P2** | `pollIntervalSec` 未按 target 粒度生效 | 已知简化,不影响正确性,只是没做到设计文档里"每个目标可以配不同轮询间隔"这个粒度。多个数据库目标轮询频率需求差异明显之前,不算紧迫。 |

**建议顺序**:P0 必须先做且优先级不可协商(正确性风险);P1 的两项可以并行讨论方案(死锁可观测性需要先定"要不要加新的 deadlock 采集分支"这个方案,PostgreSQL 需要先确认要不要投入);P2 排最后。

---

## 进度记录

**2026-08-22(联调)**:阶段一/阶段二采集代码 + 配置下发链路 + 前端展示全部写完,`docker compose build drop_agent`/`go build`/`npm run build` 均编译通过。联调中发现 `startMs==endMs` 导致 batch 被拒收,已提交节流修复(commit `6221a34`),故障注入验收(第 2 节表格)全部处于未开始状态。

**2026-08-23 凌晨(联调续)**:根因链最终澄清——节流修复本身有效,"修复后问题依旧"的真实原因是 **VM 磁盘 94% 满触发 spool 背压,agent 暂停了全部持续采集**(见 3.1)。清理磁盘后背压解除、db 采集恢复。阶段二 lock_wait 因采集账号缺 MySQL 权限报 `ERROR 1356`,已补齐 `EXECUTE ON sys.*` + `performance_schema` 逐表授权修复(见 3.3)。

**2026-08-23 09:00(链路打通)**:digest 采不到的最终根因 = 测试流量不带默认库导致 `SCHEMA_NAME=NULL`,被 agent 的 `WHERE SCHEMA_NAME IS NOT NULL` 过滤(见 3.4,测试侧修复非代码)。修复后 `profile_windows` 出现 `db_snapshot` 窗口,**数据库采集完整链路打通**。下一步:四类故障注入验收(QPS 骤降/连接风暴/慢查询堆积/锁等待死锁)+ 端到端开销对比测试。

**2026-08-23 14:50(7 场景验收 + 开销对比全部完成)**:四类故障 7 场景验收(见第 2 节表格,锁等待/QPS 骤降/连接风暴/buffer pool 骤降/慢查询堆积/不可达全部 PASS,死锁部分验证属时序限制)。**端到端开销对比测试完成**(见 2.5):db 采集额外开销 CPU +3.6pp、内存 +7.7MiB,不挤占主链路。剩余已知缺口:db 标量指标无查询接口(3.5)、PostgreSQL 分支未实现(3.5)。

**2026-08-24(前端补全 + 三处修复 + 标量指标可视化)**:数据库账号管理页面(创建/编辑 `db_targets`,顶部头像入口)+ 会话列表数据库 chip 上线,后端加了对应的 `PATCH /continuous/sessions/:sid/labels`。digest 假尖峰、`g_digestState` 内存泄漏、临时密码文件安全三处代码缺陷修复完成(见第 5 节)。补全了标量指标(连接数/QPS/命中率)的查询与可视化,发现 `queryNativeContinuousDBSnapshot` 已经有半成品聚合逻辑但从没接进返回值,补完即可,前端加了三张趋势折线图。**下一步(见第 6 节优先级)**:P0 是把今天三处修复过一遍 Linux 编译 + 实跑验证有没有死锁/竞态;P1 是死锁可观测性方案(借鉴 `SHOW ENGINE INNODB STATUS`/`pt-deadlock-logger` 思路)和 PostgreSQL 分支要不要排期。

---

## 4. 链路验收踩坑与操作注意事项（附录）

### 4.1 问题链总览

```
① batch 被拒"时间范围不合法"
   └→ ② "修复后问题依旧"= 磁盘背压暂停采集
         └→ ③ 背压解除后暴露 lock_wait ERROR 1356
               └→ ④ 权限修好后 digest 仍采不到（SCHEMA_NAME NULL）
```

### 4.2 逐个问题:现象 → 根因 → 解决

#### 问题 ①:batch 被服务端拒收 `"batch 时间范围不合法"`
- **现象**:`profile_windows` 0 条 `db_snapshot`,日志刷屏拒收。
- **根因**:`collect_db_window` 三条 SQL 毫秒级返回,窗口 `startMs==endMs`,服务端 `!StartTime.Before(EndTime)`([continuous.go:698](../apiserver/server/continuous.go:698))校验拒收。
- **解决**:节流修复 commit `6221a34`——采集完补 sleep 到 `aggregationWindowSec` + `endMs>startMs` 兜底([ContinuousSampler.cpp:3506-3524](../drop/common/ContinuousSampler.cpp:3506))。

#### 问题 ②:修复部署后"问题依旧"——其实是磁盘背压
- **现象**:修复后 `profile_windows` 仍空,Session 显示 `degraded`。
- **根因**:VM 磁盘 94% 满(25G/29G),`spool_free_bytes`([ContinuousSampler.cpp:296](../drop/common/ContinuousSampler.cpp:296))用 `statvfs(spoolDirectory)` 统计,低于 `spoolMinFreeBytes`(1GiB)时 agent **主动暂停全部持续采集**(防 spool 写爆崩盘,设计内自我保护,不是 bug)。
- **解决**:`docker builder prune -af` + `docker system prune -f` 清出 3.4G,背压自动解除。
- **教训**:磁盘不足时 agent **静默暂停**,表现成"代码没生效";排查顺序永远先看 Session 的 `degradation_reason`。

#### 问题 ③:lock_wait 查询 `ERROR 1356`
- **现象**:agent 日志刷 `db target=orders-mysql lock wait query failed rc=1`。
- **根因**:`sys.innodb_lock_waits` 是 `SQL SECURITY INVOKER` 视图,底层引用 `performance_schema.data_locks`/`data_lock_waits` + `sys.quote_identifier()`/`sys.format_statement()` 函数。`GRANT SELECT ON sys.*` 通配授权不够——函数需 `EXECUTE`,特殊表需逐表 SELECT。
- **解决**:补授权(见 3.3)。手动查询不再报错。

#### 问题 ④:digest 采不到(最终根因,卡最久)
- **现象**:权限全修好、流量在跑,但 minio batch 里 `db_snapshots` 恒为 0。
- **根因**:测试流量 `mysql -uroot -proot -e "SELECT COUNT(*) FROM orders.order_line WHERE status='new'"` **不带默认库** → `events_statements_summary_by_digest.SCHEMA_NAME` = **NULL** → agent 查询 `WHERE SCHEMA_NAME IS NOT NULL`([ContinuousSampler.cpp:3363](../drop/common/ContinuousSampler.cpp:3363))把这些语句全过滤 → digest 表对 agent 恒空。
- **解决**:流量循环指定默认库 `mysql -uroot -proot orders -N -e "SELECT COUNT(*) FROM order_line WHERE status='new';"`(第 3 个参数=默认库,SQL 用不带 schema 的表名)。
- **验证**:digest 表证据 `NULL | SELECT COUNT ( * ) FROM orders . order_line WHERE STATUS = ? | 303`。

#### 附属问题(中途踩坑速查)

| 问题 | 根因 | 解决 |
|---|---|---|
| 构建 `CACHED` 用了旧二进制 | Docker 层缓存命中 | 确认 `git pull` 到位;必要时 `--no-cache`(注意磁盘空间) |
| `create mini_drop account` 失败 | `mysqladmin ping` 不校验密码,初始化临时阶段误判就绪 | 就绪检测改 `mysql -e "SELECT 1"`(真认证) |
| 建 Session 报 409 | 同主机最多 1 个 host scope Session | 先停残留 running Session |
| 密码文件读不到 | 容器重建后 overlay 层清空 | 每次重建后重写密码文件 |
| 毒丸批次无限重试 | `drain_one_spooled_batch` 不读 `retryable:false` | 手动清 `spool/<sid>/` 下 `.json` |
| 查 digest 表误判"没进表" | 归一化文本带空格(`COUNT ( * )`),LIKE 模式不匹配 | 放宽条件 `LIKE '%COUNT%'` |

### 4.3 用户操作注意事项(验收/复跑必读)

**环境层**
1. 每次 VM 重启 / 容器重建后,三样东西会丢或变,必须重做:
   - `fault-mysql` 容器 → 账号、orders 库、lock_wait 权限**全套重建**(新容器是全新 MySQL)。
   - agent 密码文件 `/etc/mini-drop/db-credentials.d/orders-mysql.env`(overlay 层清空)。
2. 先清盘再验收:`df -h /` 可用需 >= 1GiB,否则背压暂停一切采集。
3. 确认二进制是新版:`docker exec mini-drop-drop_agent-1 ls -la /app/drop_agent` 时间应 ≥ `Aug 22 17:03`(UTC)。

**测试流量层(最容易栽)**
4. 流量循环**必须带默认库**:`mysql ... orders -N -e "SELECT COUNT(*) FROM order_line ..."`——不带则 SCHEMA_NAME=NULL,digest 永远采不到。
5. 流量**必须持续跑**:digest 是增量 diff,流量一停 `deltaCalls=0` 不上报。用长循环(如 `seq 1 300`),别用 60 秒。
6. 验证**要等够时间**:digest 首窗口记基线不上报(需 ≥2 窗口)+ batch 攒批 30s → 造流量后等 **90 秒**再查。

**故障场景层**
7. lock_wait 是瞬时的:`sys.innodb_lock_waits` 只在"正在阻塞"那一刻可见,注入锁等待要掐在 poll 窗口内(阻塞持续 30s,poll 周期 10s)。
8. 建 Session 前先停残留:`SELECT sid, name FROM continuous_sessions WHERE desired_state='running'`。

**判断标准**
9. 链路通的标准:`profile_windows` 出现 `signal_type='db_snapshot'` 且 `window_end > window_start`(实测 `db_snapshot|8|01:03:02|01:04:22`)。
10. 排查顺序:Session `degradation_reason` → agent 日志 db 活动 → minio batch 的 `db_snapshots` 数量 → 分 agent 端/服务端定位。
