# Mini-Drop 新复刻指南实施分工执行清单

> 依据：仅参考《新drop系统复刻指南.md》。目标：把新指南里描述的领域模型、可靠性、分析、展示能力落到当前代码库。
> 团队：2 人。Track A = 控制面与可靠性；Track B = 分析、展示与结果可信度。拆分依据见指南第 1.2/1.4/1.5 节的四模块依赖链。
>
> **重要提醒（铁律）**：本文件只是规划。表格里标注为"新建"的文件，在真正动手写之前，仍需按项目约定单独向用户确认一次，不会自动创建。标注"⚠️需确认目录"的项，涉及新增目录或拆分现有单文件，动手前必须先专门讨论。

---

## 总览

```
Web ──HTTP──> API ──gRPC──> drop_server <──gRPC── drop_agent
                   │              │                  │
                   ├──SQL─────────┴──────────────────┤
                   │                                 │
                   └──对象存储 <── Analyzer <────────┘

        └───────── Track A ─────────┘  └────── Track B ──────┘
```

- **Track A（控制面与可靠性）**：drop（C++）+ apiserver（Go）。对应新指南第 2、4、5、6、9、10 章。
- **Track B（分析、展示与结果可信度）**：analysis（Python）+ web_frontend（React）。对应新指南第 3.6/3.7/3.10、7、8 章。
- **共同部分**：第 11 章（安全与可观测性，按模块各自落地）、第 12 章（测试，12.6/12.7 需联调）、第 13 章（分阶段路线，两人按阶段同步推进，不是线性接力）。

建议分支命名（供参考，可自定）：`feature/track-a-reliability`、`feature/track-b-analysis-web`。两人各自分支开发，最后合并前先跑一遍第 12.6/12.7 的集成矩阵。

---

## Track A：控制面与可靠性

### A1. 领域模型扩展（对应第 2 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `apiserver/model/model.go` | 修改 | `AutoMigrate` 里注册下面 3 张新表 |
| `apiserver/model/task_attempt.go` ⚠️需确认目录 | 新建 | `TaskAttempt` 表：`attempt_id`/`task_tid`/`agent_ip`/`runner_version`/`exit_code`/`error_code`/`resource_snapshot`/`artifact_keys`/起止时间。区分"首次下发/掉线重下发/用户重试/子任务执行"这几种场景，重试不再覆盖之前的证据（对应 2.3 节） |
| `apiserver/model/artifact.go` ⚠️需确认目录 | 新建 | `Artifact` 表：`task_tid`/`attempt_id`/`kind`(RAW/INTERMEDIATE/RESULT/LOG/MANIFEST)/`object_key`/`size`/`sha256`/`status`。数据库只存 key，不存长期 URL（对应 2.4 节） |
| `apiserver/model/analysis_job.go` ⚠️需确认目录 | 新建 | `AnalysisJob` 表：`task_tid`/`pipeline`/`status`(pending/running/success/failed)/`lease_owner`/`lease_expires_at`/`attempt`/`analyzer_version`（对应 2.5 节，供 Track B 的分析 worker 使用） |

> ⚠️ 说明：`apiserver/model/model.go` 目前是单文件承载全部 10 张表。把新表拆到独立文件是否要顺带把老表也拆开，需要和你确认后再定，避免结构不一致。

### A2. 异常流与恢复（对应第 4 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `apiserver/server/task.go` | 修改 | `CreateTask` 支持 `Idempotency-Key` 请求头去重（4.2 节：重复请求只建一条 Task） |
| `apiserver/server/server.go` | 修改 | `startTaskPoller` 增加"Server 重启后从 DB 恢复 queued 任务"的对账逻辑（4.6 节） |
| `apiserver/server/error_codes.go` ⚠️需确认目录 | 新建 | 落地第 4.10 节的错误码分层：`AGENT_OFFLINE`/`RUNNER_TIMEOUT`/`ARTIFACT_UPLOAD_FAILED` 等常量 + 是否可重试标记 + HTTP 映射 |

### A3. 核心采集模块（对应第 5 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `drop/server/TaskQueue.cpp` / `.h` | 修改 | 队列从纯内存 `map<ip, deque>` 改为定期快照落盘（简化版持久化，不用引入新中间件），Server 重启后能恢复未派发任务（5.13 节验收项） |
| `drop/server/HotmethodService.cpp` | 修改 | `NotifyResult` 从 log-only 改为真正回调：任务完成后主动 HTTP 通知 apiserver（对应 5.2.4 节"触发后续 AnalysisJob"） |
| `drop/server/ResultNotifier.cpp` / `.h` ⚠️需确认目录 | 新建 | 封装向 apiserver 发 HTTP 回调的逻辑，供 `HotmethodService::NotifyResult` 调用 |
| `drop/agent/main.cpp` | 修改 | perf/async-profiler/pprof 采集失败时的静默 mock（现状：main.cpp:203）改为像 eBPF 一样受 `DROP_ALLOW_EBPF_MOCK` 类环境变量门控，默认显式报错（呼应 5.10 节"安全执行规则"，也是演示风险项） |

### A4. API 编排模块（对应第 6 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `apiserver/server/auth.go` | 修改 | `AuthCheck`/`CheckLogin` 实现简化版鉴权（哪怕只是校验 Cookie/Header 一致性），不再无条件放通（6.9 节） |
| `apiserver/server/task.go` | 修改 | `ListTasks`/`GetTaskDetail`/`DeleteTask` 加 `uid`/`gid` 权限过滤，防止越权访问他人任务（6.9/6.10 节） |
| `apiserver/middleware/middleware.go` | 修改 | 补 `RequestID` 中间件（6.4 节推荐顺序里贯穿全链路的 `request_id`） |

### A5. 数据库与部署（对应第 9、10 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `apiserver/model/outbox.go` ⚠️需确认目录 | 新建 | `Outbox` 表（9.6 节 Transactional Outbox），`CreateTask` 写 Task 时同事务写一条 outbox 记录，替代当前"写库后立刻同步调 gRPC"的非事务模式 |
| `apiserver/model/model.go` | 修改 | 给 `HotmethodTask`/`TaskStatusEvent` 补索引 tag（9.4 节：`creator+created_at`、`target+status+created_at` 等） |

---

## Track B：分析、展示与结果可信度

### B1. 分析任务领取与租约（对应第 3.6、7.2、7.3 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `analysis/lease.py` | 新建 | 封装 `AnalysisJob` 的租约领取 SQL（`FOR UPDATE SKIP LOCKED`，7.3 节原文 SQL 可直接照抄）、续租、崩溃后被其他 worker 接管的逻辑 |
| `analysis/analysis_daemon.py` | 修改 | 主循环从"直接轮询 `hotmethod_tasks.analysis_status`"改为"通过 `lease.py` 领取 `AnalysisJob`"，对接 A1 新建的 `analysis_job` 表 |
| `analysis/analyzer_registry.py` | 新建 | 把 `task_type → analyzer 函数` 的映射抽成注册表（7.4 节），替代 `hotmethod_analyzer.py` 里现在的 `if/elif` 分发 |

**完成记录（B1，提交 `30df930`）**：新增 `AnalysisLeaseClient` 与 `AnalysisJob`，使用 `FOR UPDATE SKIP LOCKED` 领取待处理作业，并实现续租、成功完成、失败重试和租约归属校验。分析守护进程已改为从 `analysis_jobs` 领取作业，处理期间启动心跳续租线程，并在执行结束后同步更新 `hotmethod_tasks.analysis_status`。新增注册表统一处理任务类型到分析函数的分发。

**验证（B1）**：`python3 -m py_compile analysis/analysis_daemon.py analysis/analyzer_registry.py analysis/lease.py` 通过；当时 `python3 analysis/test_analysis.py` 为 23 项通过。

### B2. 用户态语言级采集器补全（对应第 5.9、7.7 章 + 题目扩展硬性要求）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `analysis/hotmethod_analyzer.py` | 修改 | `task_type==1`（Java）分支从"打印占位"接上真实解析逻辑 |
| `analysis/java_analyzer.py` | 新建 | 解析 async-profiler 折叠栈/JFR 输出，产出火焰图 SVG + TopN（7.7 节 pprof 流水线的姊妹实现） |
| `web_frontend/src/components/JavaFlamegraphPanel.js` | 新建 | Java 采集器专属的可视化面板（题目要求"用户态采集器必须在 Web 有自己的可视化形态"，参照现有 `BPFHistogramPanel` 的写法） |

**完成记录（B2，提交 `a0253e1`）**：新增 Java async-profiler 分析器，支持 collapsed 折叠栈与文本 JFR 解析；二进制 JFR 在本机存在 `jfr` 工具时先转换为文本。Java 任务会生成火焰图、folded stacks、TopN 与建议产物，并保留 `top.json`、`suggestions.json` 以兼容已有结果接口。结果页新增 Java 专属面板，展示 async-profiler 采集信息、热点方法和 Java 产物入口。

**验证（B2）**：`python3 -m py_compile analysis/java_analyzer.py analysis/hotmethod_analyzer.py` 通过；当时 `python3 analysis/test_analysis.py` 为 25 项通过；`cd web_frontend && npm run build` 构建成功。

### B3. 智能归因（对应第 3.10、8.9 章 + 题目加分项）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `analysis/attribution.py` | 新建 | 工具调用式归因主逻辑：定义可供 LLM 调用的工具（查历史 baseline、对比两次采样、读调用栈上下文），循环调用直到产出可验证结论 |
| `analysis/llm_client.py` | 新建 | LLM API 封装（请求/重试/限速退避），供 `attribution.py` 调用 |
| `analysis/hotmethod_analyzer.py` | 修改 | 分析成功后调用 `attribution.py`，把结果写入 `AnalysisSuggestion.AISuggestion`（该字段已存在，目前从未被写入） |
| `web_frontend/src/components/AICard.js` | 新建 | 归因结果展示卡片：区分 `status`/`reasoning_summary`/`suggestion`/`evidence`/`done`/`error` 几类内容（8.9 节事件分类），Markdown 经清洗后渲染 |
| `web_frontend/src/pages/TaskResultPage.js` | 修改 | 接入 `AICard`，展示归因结论 + 支撑证据 |

**完成记录（B3）**：分析器在 CPU 与 Java 火焰图产出 TopN 后执行受限工具调用式归因，支持查询历史任务、比较采样和读取折叠调用栈。归因结论必须包含当前热点函数的证据；LLM 未配置、请求失败或结论不可验证时，会保存明确状态并继续完成原有分析。结果同步写入 `analysis_suggestions.ai_suggestion`，并生成 `attribution.json`，结果页优先读取数据库字段，必要时读取该产物展示归因摘要、建议和支撑证据。

**验证（B3）**：`python3 analysis/test_analysis.py`（27 项通过）；`cd web_frontend && npm run build`（构建成功）。

### B4. Web 展示模块补强（对应第 8 章）

| 文件 | 状态 | 实现的功能 |
|---|---|---|
| `web_frontend/src/api/index.js` | 修改 | 新增 `suggestions`/`attribution` 相关 API 封装函数 |
| `web_frontend/src/pages/TaskResultPage.js` | 修改 | 状态展示从当前的简单进度条，扩展为 8.6 节"阶段时间线"（已创建→等待下发→Agent已接收→采集中→原始数据已保存→分析中→结果可用），失败态显示错误码+是否可重试 |

**完成记录（B4）**：前端新增 `analysisResults.suggestions` 与 `analysisResults.attribution` 封装，复用现有任务详情接口中的建议、归因和产物数据，不依赖尚未提供的独立后端路由。任务结果页已由五段进度条升级为七阶段时间线，结合状态迁移审计、任务状态、分析状态与产物可用性展示当前进度。失败时会从 `status_info` 和最近审计事件提取错误码及原因，判断是否适合重试；可重试任务可直接调用已有重试接口创建新的采集任务。

**补充修复（B4）**：修复任务详情页在加载态与数据返回态调用 Hook 顺序不一致导致的 React 渲染错误。开发环境取消全路径代理，新增 `setupProxy.js` 仅转发 `/api` 到后端，保证直接访问 `/task/result?tid=...` 时由 React 路由返回页面，而非被 API 误处理为 404。

**验证（B4）**：`cd web_frontend && npm run build`（构建成功）；`python3 analysis/test_analysis.py`（27 项通过）。

---

## 共同部分

### 安全与可观测性（对应第 11 章，各自负责自己模块）

| 归属 | 文件 | 实现的功能 |
|---|---|---|
| Track A | `drop/server/*.cpp`、`apiserver/*` | 结构化日志补 `stage`/`error_code` 字段（11.6 节）；Agent/Server 侧指标（11.7 节 `drop_agents_online`/`drop_task_queue_depth` 等，可先只打日志，不接 Prometheus） |
| Track B | `analysis/*.py` | Analyzer 侧指标（`analysis_claim_latency_seconds`/`analysis_duration_seconds` 等，同样可先只打日志） |

### 测试与验收（对应第 12 章）

| 归属 | 文件 | 实现的功能 |
|---|---|---|
| Track A | `apiserver/server/task_test.go`、`apiserver/server/schedule_test.go` ⚠️需确认目录（当前无同名测试文件） | 12.3 节 API 单测：身份缺失、资源越权、幂等并发、gRPC 超时等场景 |
| Track B | `analysis/test_attribution.py`、`analysis/test_analysis.py`（追加用例） | 12.4 节 Analyzer 单测：最小合法样本、空文件、hash 错误、超时 |
| **两人联合** | `scripts/e2e_smoke.sh`（追加）+ 手动执行 12.6 集成矩阵 | Agent离线/Server重启/Storage失败/重复请求/越权下载等场景的联调验证，这一步必须两条 Track 的改动都合并后才能测 |

---

## 建议推进节奏（对应第 13 章，两人同阶段并行，不是接力）

- [ ] **阶段一**：A 完成 A1（领域模型表）；B 先跑通 B1（对接新表的租约领取），两人在这一步必须对齐表结构再各自开工
- [ ] **阶段二**：A 做 A3/A4 核心链路可靠性；B 做 B2 用户态采集器闭环
- [ ] **阶段三**：A 做 A2 异常恢复；B 做 B3 智能归因（工作量最大，建议提前开始）
- [ ] **阶段四**：A 做 A5 数据库/Outbox；B 做 B4 Web 展示补强
- [ ] **收尾**：两人合并分支 → 共同完成 12.6 集成矩阵 + 12.7 端到端验收清单

---

## 使用说明

1. 这是规划文档，**不是可以直接复制粘贴的代码**。每个文件真正开始写之前，请先告诉我"现在要做 A几/B几"，我会给出具体的函数签名和实现细节，等你确认后再动手（按你们的两条铁律执行）。
2. 标注 ⚠️需确认目录 的条目，涉及新增目录或拆分现有单文件，动手前需要单独讨论放在哪、怎么拆。
3. 两条 Track 都会碰的共享文件（`apiserver/model/model.go`）建议约定谁先改、改完及时同步，避免 merge 冲突。
