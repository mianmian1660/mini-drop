# db_snapshot 哨兵深挖动作：借鉴 pt-stalk 实现 script_diagnostic

> 承接 `docs/detection-trigger-pipeline-design.md` §10.1（db_snapshot 判异已实现，命中后
> 只写 `DetectionEvent{status: fired_no_action}`，不建诊断任务）。本文档解决的是"命中之后
> 该做什么"，不涉及判异算法本身——判异这一层 mini-drop 已经比参考对象更严谨（滚动基线+
> 持续性判断），本文档只借鉴"触发后自动抓现场"这一个动作设计。

## 1. 参考项目：Percona `pt-stalk`

Percona Toolkit（开源 MySQL/MariaDB 运维工具集）里的一个工具，设计哲学：

- 触发条件很朴素（轮询一个状态变量连续超阈值 N 次），**这一层 mini-drop 已用滚动中位数+MAD
  和持续性判断实现得更严谨，不借鉴**。
- 真正值钱的是命中后的动作：在接下来 `--run-time`（默认30秒）内，每秒跑一套**固定的**诊断
  命令，把结果打包成文本文件归档：
  - `SHOW GLOBAL STATUS` / `SHOW GLOBAL VARIABLES`
  - `SHOW FULL PROCESSLIST`
  - `SHOW ENGINE INNODB STATUS`（锁等待/死锁详情就在这里）
  - `vmstat`/`iostat`/`mpstat`/`top`（操作系统层面辅助佐证）
  - 可选（默认关闭，更重）：`strace`/`gdb` 堆栈、抓包
- **命令清单是写死在脚本里的固定配方，不是通用脚本执行引擎**——用户不能传入任意命令，只能
  触发"跑哪一套预设好的诊断"。这一点是本文档要借鉴的关键设计，直接化解了
  `detection-trigger-pipeline-design.md` §10.1 里提到的"允许自动执行脚本"的安全顾虑。
- 不做实时分析，只负责"完整地拍下现场"，留给人事后用配套工具 `pt-sift` 翻看。

## 2. mini-drop 现状与缺口

- `TaskKindScriptDiagnostic`（`apiserver/server/task_kind.go:199-205`）目前只声明了契约，
  `Enabled: false`，Agent 侧没有真正的执行器。
- `evaluateDBSnapshotRule`（`apiserver/server/detection.go`）命中后只调用
  `recordDetectionEvent(rule, observed, "fired_no_action", "")`，`ChildTID` 恒为空，前端
  "近期哨兵触发"列表（`ContinuousProfilingPanel.js` 的 `DBSentinelEvents`）因此只能展示纯
  文案，不能做成可点击链接。
- 调度延迟/IO延迟类信号命中后走 `triggerDetectionDiagnosis`，直接复用 `perf`/`eBPF` 现成的
  采集器，Agent 侧本来就有这个能力；数据库这条路缺的正是"Agent 侧要有个东西能真正执行"。

## 3. 方案：固定配方 + 复用现有触发/存储管线

### 3.1 两套固定配方（对应 `SentinelRule.Metric` 的两个取值）

| Metric | 配方 | 依据 |
|---|---|---|
| `lock_wait` | `SHOW ENGINE INNODB STATUS`；`SHOW FULL PROCESSLIST` | InnoDB 引擎状态自带最近一次死锁详情和锁等待信息，Processlist 看清楚谁在等谁 |
| `digest` | `EXPLAIN <该digest对应的代表性SQL>`；`SHOW ENGINE INNODB STATUS` | EXPLAIN 直接给执行计划（索引失效/全表扫一眼看出），InnoDB 状态辅助判断是否伴随锁/IO争抢 |

两套配方都是**只读命令**，不修改任何数据，不需要 `EXECUTE`/写权限，风险面比 pt-stalk 原版
（它还会话选性抓 strace/gdb）更小——mini-drop 版本从一开始就不做那两项更重的可选采集。

`digest` 配方的 EXPLAIN 目标 SQL 从哪来：`events_statements_summary_by_digest` 存的是归一化
后的占位符文本（`SELECT * FROM t WHERE id = ?`），EXPLAIN 需要一条真实语句。取
`performance_schema.events_statements_current`（或短窗口的 `events_statements_history`）里
匹配该 `DIGEST` 值的最近一条真实记录，EXPLAIN 那一条；找不到真实语句时，配方跳过 EXPLAIN，
只跑 `SHOW ENGINE INNODB STATUS`（比完全不跑强，且诚实反映"这次没能拿到具体语句"）。

### 3.2 Agent 侧最小执行器

不是通用脚本引擎，是一个只认内置配方 ID 的小函数：

```cpp
// 伪代码，接口形态
void run_script_diagnostic_recipe(const std::string& recipeId, const DBTargetConfig& target);
// recipeId 只能是 "lock_wait_recipe" / "digest_recipe" 两个硬编码值之一，
// 不接受从服务端下发的自由文本命令 —— 这是安全边界的核心。
```

复用现有 `DBSnapshotSampler` 已经建立的数据库连接方式（凭据文件、`mysql_defaults_file`），
不新增一套连接管理。

### 3.3 触发路径改动

`evaluateDBSnapshotRule` 命中时，不再只调用 `recordDetectionEvent(..., "fired_no_action", "")`，
改为：

```go
childTID, err := s.triggerScriptDiagnosis(rule, recipeIDForMetric(rule.Metric), observed)
status := "fired"
if err != nil {
    status = "fired_no_action" // Agent 执行失败时降级，不是静默假装成功
    childTID = ""
}
s.recordDetectionEvent(rule, observed, status, childTID)
```

`triggerScriptDiagnosis` 复用 `triggerDetectionDiagnosis`（detection.go:322）几乎相同的建任务
逻辑，只是 `TaskKind = TaskKindScriptDiagnostic`，`TriggerContext` 里带 `recipe_id`。

### 3.4 产出物存储

诊断命令的文本输出按现有 Artifact 机制存对象存储（和火焰图产物走同一条路径，只是
`content_type` 是 `text/plain` 不是 profile 二进制），`HotmethodTask` 的分析管线新增
`script_diagnostic` 这一种 `AnalysisPipeline`（`task_kind.go` 里已经声明了这个字段值，
`Runner: "restricted-script", AnalysisPipeline: "script_diagnostic"`），落地时只需要接上
"读文本文件、渲染成简单的多命令输出页面"，不需要像火焰图那样做复杂的可视化。

### 3.5 前端影响

`TaskKindScriptDiagnostic` 从 `Enabled: false` 改为 `true` 后：
- `DBSentinelEvents`（`ContinuousProfilingPanel.js`）里 `child_tid` 不再恒为空，"已记录异常，
  当前无自动诊断"这行文案需要按 `status`/`child_tid` 分支，`fired` 且有 `child_tid` 时改成
  可点击的"查看诊断"，跳转到一个新的简单文本展示页（不是火焰图组件）。
- 这是唯一需要新增前端页面的地方——展示的是几段命令输出文本，不需要复用 `HistogramTrendChart`
  或火焰图组件，做一个最简单的"分命令区块 + 等宽字体文本"页面即可。

## 4. 分阶段落地路径

| 阶段 | 范围 | 验收标准 |
|---|---|---|
| 第1步 | Agent 侧执行器：只接两个硬编码 recipeId，跑固定命令，结果写文件 | 手动调用能在本地生成正确的诊断文本文件 |
| 第2步 | 服务端触发路径改动 + `TaskKindScriptDiagnostic` 启用 | 人为制造一次锁等待/慢SQL，能看到诊断任务从"创建"到"完成"跑通，`DetectionEvent.status=fired`、`child_tid` 非空 |
| 第3步 | 产出物落 Artifact + 简单文本展示页 | 点击"近期哨兵触发"列表里的记录，能看到实际的 `SHOW ENGINE INNODB STATUS`/`EXPLAIN` 输出文本 |
| 第4步 | 前端 `DBSentinelEvents` 按 status/child_tid 分支展示 | 全链路人工验收：从触发到能在界面上看到诊断文本，中间不需要人手动登录数据库执行命令 |

## 5. 与 pt-stalk 的差异说明（避免过度对标）

- pt-stalk 是独立守护进程，对被监控的 MySQL 本身没有"判异"能力上限（阈值单一朴素）；
  mini-drop 版本的判异能力已经更强，只是缺"抓现场"这个动作，所以本文档只借鉴这一个动作，
  不是照抄整个工具。
- pt-stalk 默认还会抓 `strace`/`gdb`/抓包这类更重、风险更高的诊断手段；mini-drop 版本明确
  不做这两项，只保留纯只读 SQL 命令，降低误用/性能影响风险。
- pt-stalk 产出是留给人事后用 `pt-sift` 翻看的原始文本堆；mini-drop 版本产出直接挂在诊断
  任务详情页，复用现有的任务时间轴查看方式，不需要额外的浏览工具。
