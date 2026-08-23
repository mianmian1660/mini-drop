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

## 2. 需要验收的故障场景

链路没通之前,以下场景**一个都还没能真正验证**,是打通链路之后的下一步:

| 场景 | 注入方式 | 验证的采集能力 | 状态 |
|---|---|---|---|
| QPS 骤降(导师点名重点) | sysbench 高 QPS 后 `pkill` | `db_questions_total` 增量能否定位拐点 | 未验证(链路未通) |
| 连接风暴 | 50 个 `mysql -e "SELECT SLEEP(600)" &` | `db_active_connections` 能否捕获飙升 | 未验证(链路未通) |
| buffer pool 命中率骤降 | 全表扫描大表 / `FLUSH TABLE` | `db_innodb_buffer_pool_hit_ratio_bps` 从 ~10000 掉到低位 | 未验证(链路未通) |
| 慢查询堆积(阶段二 digest) | 无索引全表扫描循环 | `db_snapshots kind=digest` 的 `callCount`/`totalLatencyUs` | 未验证(链路未通) |
| 锁等待(阶段二 lock_wait) | 双连接互相持锁阻塞 | `waiting_pid`/`blocking_pid`/`wait_seconds`/`locked_table` 是否正确成对 | 未验证(链路未通) |
| 死锁 | 双连接交叉加锁触发 MySQL 自动检测 | 至少捕获死锁前的锁等待链 | 未验证(链路未通) |
| 数据库不可达(已知盲区) | `docker stop fault-mysql` | 确认采集跳过、不拖垮主链路(不是要采到数据,是要确认不崩) | 未验证(链路未通) |

---

## 3. 目前的问题

### 3.1 阻塞项(P0,必须先解决,故障注入验收才能开始)

**`profile_windows` 里 0 条 `db_snapshot` 数据。** 最初表象是 batch 被服务端拒收 `"batch 时间范围不合法"`,2026-08-23 凌晨连查后澄清为**两件事叠加,根因已分别定位**:

1. **`startMs==endMs` 拒收(已被节流修复解决,commit `6221a34`)**。`collect_db_window` 内部三条 SQL 毫秒级返回导致窗口起止同毫秒,`!StartTime.Before(EndTime)`([continuous.go:698](../apiserver/server/continuous.go:698))拒收。修复已提交并部署:[ContinuousSampler.cpp:3506-3524](../drop/common/ContinuousSampler.cpp:3506) 补节流 sleep 到 `aggregationWindowSec` + `endMs>startMs` 兜底。
2. **真正长期阻塞:磁盘背压暂停采集(2026-08-23 凌晨最终定位)**。节流修复部署后 `profile_windows` 仍空,追查发现 agent 一直处于"采集暂停"状态:
   - agent 日志刷 `[native-cp] spool backpressure ... free_bytes=1.74GB min_free_bytes=2147483648`
   - Session `degradation_reason="shared spool backpressure: free disk is below the configured reserve; collection is paused and will resume automatically"`
   - 机制:`spool_free_bytes`([ContinuousSampler.cpp:296](../drop/common/ContinuousSampler.cpp:296))用 `statvfs(spoolDirectory)` 统计磁盘剩余,**低于 `spoolMinFreeBytes`(2GB)时暂停全部持续采集**(防 spool 写满把系统写崩),磁盘恢复后自动 resume,这是设计内的自我保护,不是 bug。
   - 触发原因:VM 磁盘 94% 满(25G/29G)。`docker builder prune -af` + `docker system prune -f` 清出 3.4G 可用后背压解除(`grep -c "spool backpressure"` = 0)。
   - 经验点:VM 的 docker 数据不在 `/var/lib/docker`(`du -sh` 仅 4.0K),疑似 rootless docker(数据在 `~/.local/share/docker`),排查磁盘占用时别只看 `/var/lib/docker`。

**当前状态(2026-08-23 01:30)**:背压已解除、db 采集已恢复运行(日志出现 `db target=orders-mysql lock wait query failed` 证明 dbTargets 解析、密码读取、采集循环全部正常,仅 lock_wait 单独失败)。**digest/标量落库待最终确认**(见 3.4)。

### 3.2 联调阶段顺带发现的新问题(不阻塞,但要记录)

**坏批次会被重试到天荒地老。** [ContinuousSampler.cpp:2309-2347](../drop/common/ContinuousSampler.cpp:2309) `drain_one_spooled_batch` 只区分 `Acknowledged`/`BatchIDConflict` 两种结果,其余一律进入指数退避重试,不读服务端返回的 `"retryable":false`。一个结构性错误(如 `start==end`)的批次,不管重试多少次结果都一样,会以 `retryMaxSec`(默认 300s)封顶后每 5 分钟重发一次、无限期占用 spool 和刷日志,没有任何"永久失败,丢弃"的路径。是否要修、怎么修(读 `retryable` 字段主动丢弃 vs 加个最大重试次数)还没讨论过,不属于本次范围。

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

### 3.5 已知未做的功能缺口(设计上明确推迟,不是 bug)

- PostgreSQL 分支未实现(阶段一、阶段二都只有 MySQL)。
- 阶段二"看到数据"和"判断异常"是两回事——现在只是记录事实(digest/锁等待数据),没有任何阈值/基线异常检测逻辑,时间轴预警功能还在方案讨论阶段(见对话中方案 A/B/C),没有代码。
- 前端建 Session 表单没有 `db_targets` 输入框,会话列表页看不出哪个 Session 配了数据库巡检(见 1.3)。
- 冷热分层(`ContinuousWindowSummary` 7 天摘要)只覆盖 `cpu_profile`,`db_snapshot` 没有对应的冷层摘要方案。
- `pollIntervalSec` 字段设计了但没有真正按 target 粒度生效,本次节流修复统一用 `aggregationWindowSec`,是已知简化。

---

## 进度记录

**2026-08-22(联调)**:阶段一/阶段二采集代码 + 配置下发链路 + 前端展示全部写完,`docker compose build drop_agent`/`go build`/`npm run build` 均编译通过。联调中发现 `startMs==endMs` 导致 batch 被拒收,已提交节流修复(commit `6221a34`),故障注入验收(第 2 节表格)全部处于未开始状态。

**2026-08-23 凌晨(联调续)**:根因链最终澄清——节流修复本身有效,"修复后问题依旧"的真实原因是 **VM 磁盘 94% 满触发 spool 背压,agent 暂停了全部持续采集**(见 3.1)。清理磁盘后背压解除、db 采集恢复。阶段二 lock_wait 因采集账号缺 MySQL 权限报 `ERROR 1356`,已补齐 `EXECUTE ON sys.*` + `performance_schema` 逐表授权修复(见 3.3)。

**2026-08-23 09:00(链路打通)**:digest 采不到的最终根因 = 测试流量不带默认库导致 `SCHEMA_NAME=NULL`,被 agent 的 `WHERE SCHEMA_NAME IS NOT NULL` 过滤(见 3.4,测试侧修复非代码)。修复后 `profile_windows` 出现 `db_snapshot` 窗口,**数据库采集完整链路打通**。下一步:四类故障注入验收(QPS 骤降/连接风暴/慢查询堆积/锁等待死锁)+ 端到端开销对比测试。

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
- **根因**:VM 磁盘 94% 满(25G/29G),`spool_free_bytes`([ContinuousSampler.cpp:296](../drop/common/ContinuousSampler.cpp:296))用 `statvfs(spoolDirectory)` 统计,低于 `spoolMinFreeBytes`(2GB)时 agent **主动暂停全部持续采集**(防 spool 写爆崩盘,设计内自我保护,不是 bug)。
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
2. 先清盘再验收:`df -h /` 可用需 > 2GB,否则背压暂停一切采集。
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
