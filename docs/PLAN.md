# 新 Drop 系统完整复刻实施大计划

## Summary

默认按 **4-6 周完整复刻**推进，目标是把当前可演示 Mini-Drop 升级为符合《新drop系统复刻指南》的生产级复刻版：统一契约、可靠调度、独立采集/分析状态机、可恢复任务、受控 Agent、权限安全、SSE 实时展示、稳定测试与部署运维。

当前优先级：先修验收硬伤，再做契约收敛，然后补可靠性、安全和扩展采样器。全程保持 `make verify`、`make e2e` 可作为主验收入口。

## Key Changes

### 阶段 0：修复当前交付硬伤

- 修正任务状态机：采集回调到达时如任务仍是 `RUNNING`，必须先写入 `UPLOADING/RAW_ARTIFACT_UPLOADED` 事件，再进入 `DONE/COLLECTED`，保证 e2e 不因跳过上传阶段失败。
- 修正测试与文档不一致：要么补 Go 单测把 `make coverage` 提升到 50% 以上，要么暂时把设计文档中的覆盖率承诺改成真实值；完整复刻目标选择前者。
- 固定 `make verify`：包含 C++ build、Go test、Python analysis test、React build、coverage、e2e 和 diff check。
- 清理“mock 成功”风险：默认真实 perf/eBPF 失败必须进入 FAILED，只有显式开发开关才允许 mock，并在 UI/日志中标记。

### 阶段 1：统一领域契约与数据库

- 建立单一真源：新增 TaskKind 元数据定义，覆盖 `id/name/display_name/runner/analysis_pipeline/capabilities/schema/default/max_duration/max_concurrency/supported_os`。
- 由 TaskKind 元数据驱动三端：API 校验、Web 表单、Agent capability 匹配都从同一份定义生成或加载，前端不再手写数字枚举含义。
- 重构 Proto：`TaskDesc` 增加 `task_id/request_id/attempt_id/deadline_unix_ms/resource_budget/task_kind/error_code`，采样参数改为 `oneof payload`。
- 完整落库对象：补齐 `TaskEvent.sequence`、`attempt_id`、`source_module`、`payload`；Artifact 增加 `etag/hash/manifest_key/retention/status`；AnalysisJob 增加 `max_attempts/last_error/input_artifact_ids/output_artifact_ids`。
- 引入真实 migration：停止依赖仅 AutoMigrate 做生产 schema，增加可重复执行的 SQL migration 和 schema version 检查。

### 阶段 2：API 编排层生产化

- 拆出 TaskService：Handler 只解析请求和返回响应；TaskService 负责权限、TaskKind、Schema、配额、幂等、Outbox、状态事件和 gRPC 任务转换。
- 统一响应格式：所有 REST 返回 `{request_id,data,error}`；错误使用稳定错误码、retryable、stage，不向用户暴露 DSN、内部 object key、堆栈或凭据。
- 完成权限模型：实现 Viewer、Operator、GroupAdmin、PlatformAdmin；Agent、Task、Artifact、Schedule、Suggestion 都按用户/组/管理员范围校验。
- 完成 Artifact API：新增任务级 Artifact 列表与下载接口，下载前重新鉴权，返回 1-5 分钟短期签名 URL，并记录下载审计。
- 完善 Outbox Dispatcher：支持 `FOR UPDATE SKIP LOCKED`、指数退避、最大重试、死信状态、进程重启恢复；下游调用保持幂等。
- 增加取消接口：`POST /api/v1/tasks/:tid/cancel` 幂等实现，终态任务返回当前状态，运行中任务写取消事件并下发取消指令。
- Readiness/liveness 分离：liveness 只看进程，readiness 检查 DB、storage、gRPC、migration version、队列恢复状态。

完成记录（2026-08-05）：阶段 2 已按兼容方式落地。后端新增 TaskService、统一 `{request_id,data,error}` 响应并保留旧 `code/message`，接入 Header/Cookie 开发期 RBAC、任务级 Artifact 列表与短期下载、幂等取消接口、Outbox claim/retry/dead-letter 元数据、`/livez`/`/readyz`/`/healthz` 健康检查。旧 `/cosfiles` 和前端 API 保持兼容但补充鉴权。验收通过 `make verify`，覆盖 e2e、C++ build、Go/Python/React 测试、coverage 55.8% 和 `git diff --check`。

### 阶段 3：C++ Server 与 Agent 可靠执行

- Server 队列持久化/可恢复：从 DB queued/outbox 重建目标队列，支持 priority、deadline、去重、最大队列长度、取消标记。
- Agent Registry 完整化：使用稳定 `agent_id`，记录 version、platform、capabilities、labels、resource_budget、last_seen、status；离线/恢复写审计和事件。
- 心跳对账：Agent 心跳携带 running/completed attempts；Server 根据 `attempt_id` 修复 uncertain/running/done，不重复执行已完成采样。
- Runner 抽象落地：统一 `Validate/Prepare/Start/Monitor/Collect/Upload/Report/Cleanup` 生命周期；所有外部命令使用 argv API，不拼 shell。
- 超时与取消：Runner 使用进程组，两阶段终止，日志长度上限，失败写稳定错误码，partial artifact 明确标记。
- Artifact 上传可靠性：Agent 上传 raw 文件和 manifest，计算 size、sha256、etag；上传失败只重传文件，不重跑采样；NotifyResult 可重放不重复写记录。
- 对象存储最小权限：Agent 不持有长期 MinIO/COS secret，改为任务前缀短期上传凭证或受控上传代理。
- 容器目标解析：支持容器名/ID 到宿主 PID、namespace、路径映射的二次校验，防止 PID 重用和越权采样。

完成记录（2026-08-05）：阶段 3 已按现有 demo 架构兼容落地。C++ Server 队列升级为带 `attempt_id`、priority、deadline、入队时间和取消标记的元数据队列，支持快照恢复、重复下发去重、过期/取消/已完成 attempt 派发过滤，并新增 `CancelTask` gRPC。Agent 注册和心跳上报稳定 `agent_id`、platform、capabilities、labels、resource_budget、running/completed attempts；Runner 生命周期统一输出 Validate/Prepare/Start/Monitor/Collect/Upload 日志，产物上传改为 argv API，采集成功后生成 raw artifact 的 size、sha256 和 manifest。apiserver 兼容接收扩展后的 `NotifyResult`，按 `task_id + attempt_id + object_key` 幂等登记 Artifact、Attempt 和 AnalysisJob，并在取消时同步跳过未发布 outbox、尽力通知 drop_server。

### 阶段 4：Analyzer 生产化

- 统一 Analyzer 接口：每个分析器实现 `validate/prepare/analyze/build_manifest/cleanup`，声明输入格式、最大大小、超时、输出 artifact 和可重试性。
- 租约严格化：claim、heartbeat、complete、fail 全部校验 `lease_owner`；迟到 worker 不能覆盖新 owner；崩溃后 lease 到期可恢复。
- 输入校验：下载 RAW artifact 后校验 hash、大小、格式、压缩上限和 manifest；损坏输入进入不可重试失败。
- 结果事务一致性：结果文件上传成功后，在同一事务写 Artifact、AnalysisJob success、TaskEvent、AnalysisStatus；失败时保留可诊断错误。
- 完成核心流水线：perf flamegraph/topN、pprof CPU/heap、async-profiler Java、eBPF IO/sched/cpu 至少都有黄金样本和结果产物。
- 增强建议系统：规则建议输出 evidence、threshold、severity、action、rule_version；AI 建议只基于脱敏结果生成，并记录模型/规则版本。

完成记录（2026-08-05）：阶段 4 已按兼容方式落地。Python Analyzer 新增统一契约层，默认注册表改为注册 analyzer 实例并保留旧函数调用兼容；perf、pprof、Java async-profiler、eBPF、memleak 均声明输入格式、大小上限、超时、输出产物和重试语义。RAW artifact 分析前会校验元数据、大小、sha256/hash、manifest 和格式，缺失/损坏输入进入不可重试失败，本地 fallback 仅在显式输出目录场景保留。AnalysisJob 租约完成/失败继续校验 `lease_owner`，写入 `last_error`、`output_artifact_ids` 和 analyzer version；daemon 在结果上传后同一事务写 Artifact、AnalysisJob success、TaskEvent 与 `analysis_status`。每次分析生成 manifest，规则建议补齐 evidence、threshold、severity、action、rule_version，AI 归因仅接收脱敏参数和 TopN 摘要并记录模型/规则版本。新增 Python 单测覆盖生命周期、RAW 校验、owner guard、事务完成和建议字段。

### 阶段 5：Web 展示模块升级

- TaskKind 驱动表单：选择 Agent 后只展示该 Agent 支持的 TaskKind，参数控件由 schema/metadata 生成，提交仍以后端校验为准。
- SSE 事件流：新增任务状态 SSE 和建议 SSE；支持 `Last-Event-ID` 断点恢复，失败时退避轮询。
- 完整状态时间线：展示 Created、Queued、Delivered、Running、Uploading、Collected、Analyzing、Success/Failed/Canceled，每阶段显示 reason、时间、来源。
- Artifact 体验：签名 URL 过期后自动刷新；下载失败显示 request_id；终态后停止普通轮询。
- 大火焰图优化：火焰图解析/布局移入 Web Worker，限制节点数和深度，避免主线程长期阻塞。
- 安全展示：Markdown/HTML 建议经过清洗；错误页和失败面板显示稳定错误码、retryable、request_id 和建议动作。

### 阶段 6：复合任务、计划任务与高级能力

- 复合任务：主任务只做编排，展开 CPU/内存/I/O 等子任务 DAG；支持 `ALL_REQUIRED/BEST_EFFORT/QUORUM/DAG` 聚合策略。
- 计划任务：`schedule_id + scheduled_at` 唯一约束防重复触发；多 API 实例用 DB advisory lock 或 scheduler leader。
- Continuous Profiling：时间轴支持子任务窗口回溯、状态筛选、结果跳转和基础趋势对比。
- 扩展采样器顺序：先 pprof 和 async-profiler 稳定化，再补 Python py-spy/memray、Java Heap、gperftools、BOLT、受限脚本诊断。
- 每新增采样器必须同步补 TaskKind、Schema、Agent capability、Runner、Analyzer、黄金样本、错误码和 Web 展示。

### 阶段 7：安全、可观测性与部署

- 安全边界：API 负责用户权限，Server 负责 Agent 身份和调度约束，Agent 负责本地执行约束，Artifact 下载入口二次鉴权。
- 传输安全：生产配置支持 mTLS 或等价可信通道；开发 Compose 可继续使用 insecure，但必须显式标注。
- Secret 管理：配置中禁止硬编码长期凭据；日志脱敏 access key、secret、profile URL token、object key 中敏感部分。
- 指标与日志：统一 `request_id/task_id/attempt_id/agent_id/job_id/error_code`；暴露任务创建、队列长度、分析耗时、上传失败、SSE 连接数等指标。
- 告警与 Runbook：覆盖 Agent 离线、任务积压、分析失败率、存储不可用、Outbox 堆积、lease 过期过多。
- 部署交付：Compose 保持本地一键启动；补 Kubernetes manifests、配置模板、备份恢复说明、数据保留策略、镜像 SBOM 和安全扫描。

## Test Plan

- 契约测试：TaskKind、Status、ErrorCode、Proto 字段生成结果在 Go/C++/Python/Web 中一致。
- API 单测：创建任务幂等、参数 Schema、Agent capability、权限越权、Artifact 下载鉴权、取消/重试、Outbox 重试和死信。
- C++ 单测/集成：队列 deadline/去重/取消、Runner argv 构造、超时清理、NotifyResult 重放幂等、Agent 离线恢复。
- Analyzer 单测：lease 并发领取、owner guard、损坏输入、格式不支持、重试上限、黄金样本输出稳定。
- Web 测试：TaskKind 表单生成、重复提交防抖、SSE 重连、URL 过期刷新、失败面板、火焰图大数据不阻塞。
- E2E 矩阵：正常 CPU/eBPF/pprof 任务、Agent 离线、Server/API/Analyzer 重启、Storage 5xx、NotifyResult 重放、越权访问、取消运行中任务。
- 验收命令：`make verify` 必须稳定通过；`make e2e` 必须覆盖创建、状态事件、Artifact、分析结果、权限、异常路径；`make coverage` 总覆盖率达到 50% 以上。
- 性能基线：记录 100/1000/10000 任务列表查询、单 Agent 队列延迟、Analyzer 处理耗时、SSE 连接数、火焰图加载时间。

## Assumptions

- 默认目标是 **完整复刻指南**，周期按 **4-6 周** 排，不按两周极限压缩。
- 当前代码作为基础继续演进，不推倒重写；先保持现有 Demo 可用，再逐步替换内部契约。
- 本地 Compose 仍支持开发演示；真实 perf/eBPF 验收必须在具备 PMU、bpftrace、tracefs 权限的 Linux 环境。
- MinIO 继续作为本地对象存储实现，但接口按 COS/S3 可替换方式设计。
- 生产级安全能力允许分阶段启用：开发环境可 insecure，正式验收配置必须支持短期凭证、权限校验和日志脱敏。
