# Mini-Drop 设计文档

本文档对应 Mini-Drop 复刻项目的工程设计说明，覆盖架构、状态机、关键取舍、AI 协作、性能自证以及后续演进计划。目标是让评审可以从本文快速理解系统如何从 Web 下发一次采样任务，并最终在浏览器里看到火焰图、eBPF 直方图、热点 TopN、优化建议和可下载产物。

## 1. 系统目标

Mini-Drop 复刻的是一套分布式性能采集与分析平台。用户在 Web 页面指定目标 Agent、PID、采样时长、采样率和采集器类型，Server 将任务下发给 Agent。Agent 在目标机器上执行 perf、eBPF/bpftrace、async-profiler 或 pprof 等采集器，把原始产物上传到对象存储。Analyzer 轮询已完成任务，将原始数据转换成火焰图、热点 TopN、eBPF 直方图和规则建议。Web 最终展示任务状态流转、可视化结果和文件下载入口。

系统重点满足这些硬性要求：

- 多组件架构：Web UI、Server、Agent、Analyzer、PostgreSQL、MinIO。
- 任务状态机：`PENDING -> RUNNING -> UPLOADING -> DONE / FAILED`，每次迁移落库并带 `reason`。
- Agent 心跳：Agent 周期心跳，Server 30s 无心跳判离线，Web 展示 Agent 列表和审计日志。
- 多采集器：perf CPU 火焰图、eBPF 内核探针、pprof/async-profiler 用户态采集器入口。
- Continuous Profiling：定时任务与时间轴回溯。
- 工程基线：结构化日志、显式错误处理、单测覆盖率超过 50%、端到端集成测试覆盖正常路径和异常路径。

## 2. 总体架构

```text
                      HTTP/JSON
┌──────────────────┐  REST API   ┌─────────────────────────┐
│  web_frontend    │ ───────────> │  apiserver              │
│  React + Nginx   │ <─────────── │  Go + Gin + GORM        │
│                  │             │                         │
│  - 新建采样       │             │  - 任务编排              │
│  - Agent 列表     │             │  - 状态迁移审计           │
│  - 结果页         │             │  - Agent 审计             │
│  - 时间轴         │             │  - 对象存储签名/下载       │
└──────────────────┘             └───────────┬─────────────┘
                                             │ gRPC Control
                                             ▼
                                  ┌─────────────────────────┐
                                  │  drop_server            │
                                  │  C++ gRPC 调度中心       │
                                  │                         │
                                  │  - 接收 CreateTask       │
                                  │  - 维护按 IP 分组队列     │
                                  │  - 接收 Agent 心跳        │
                                  │  - 接收 NotifyResult      │
                                  └───────────┬─────────────┘
                                              │ HealthCheck / TaskDesc
                                              ▼
                                  ┌─────────────────────────┐
                                  │  drop_agent             │
                                  │  C++ 采集探针            │
                                  │                         │
                                  │  - perf                 │
                                  │  - eBPF / bpftrace      │
                                  │  - async-profiler        │
                                  │  - pprof                │
                                  │  - 结果上传 MinIO         │
                                  └───────────┬─────────────┘
                                              │ artifacts
                                              ▼
┌──────────────────┐             ┌─────────────────────────┐
│  PostgreSQL      │             │  MinIO                  │
│                  │             │                         │
│  - task          │             │  - perf.data             │
│  - status_event  │             │  - folded.txt            │
│  - agent         │             │  - flamegraph.svg        │
│  - agent_audit   │             │  - top.json              │
│  - schedule      │             │  - bpf_data.json         │
└──────────────────┘             └───────────┬─────────────┘
                                             │ poll + analyze
                                             ▼
                                  ┌─────────────────────────┐
                                  │  analysis               │
                                  │  Python 分析引擎         │
                                  │                         │
                                  │  - 火焰图生成             │
                                  │  - 热点 TopN              │
                                  │  - eBPF 直方图解析         │
                                  │  - 规则建议               │
                                  └─────────────────────────┘
```

## 3. 核心数据流

一次 CPU 火焰图采样的端到端流程如下：

```text
1. 用户在 Web 点击“新建采样”，填写目标 Agent、PID、时长、频率。
2. Web POST /api/v1/tasks 到 apiserver。
3. apiserver 写入 hotmethod_task，初始状态为 PENDING。
4. apiserver 通过 gRPC Control.CreateTask 下发 TaskDesc 到 drop_server。
5. 下发成功后，apiserver 写入 RUNNING 状态事件，并记录 reason。
6. drop_agent 周期心跳到 drop_server，拉取属于自己 IP 的任务。
7. drop_agent 执行对应采集器，例如 perf record 或 bpftrace。
8. 采集窗口结束后，apiserver 轮询器将任务推进到 UPLOADING。
9. drop_agent 将产物上传到 MinIO，并 NotifyResult 给 drop_server。
10. apiserver 发现产物可见或上传等待窗口结束，将任务推进到 DONE。
11. analysis daemon 轮询 DONE 且待分析的任务，生成 flamegraph.svg、top.json、suggestions.json 等产物。
12. Web 任务详情页轮询任务详情，展示火焰图、TopN、建议和下载按钮。
```

这个流程里，apiserver 是控制面，drop_server/drop_agent 是采集面，analysis 是离线分析面，MinIO/PostgreSQL 是状态和产物的持久化层。

## 4. 状态机设计

任务主状态保存在 `hotmethod_task.status`，状态迁移审计保存在 `task_status_events`。

```text
             create task
                 │
                 ▼
          ┌─────────────┐
          │ PENDING  0  │
          └──────┬──────┘
                 │ gRPC 下发成功
                 ▼
          ┌─────────────┐
          │ RUNNING  1  │
          └──────┬──────┘
                 │ 采集窗口结束
                 ▼
          ┌─────────────┐
          │ UPLOADING 4 │
          └──────┬──────┘
                 │ 产物可见 / 等待窗口结束
                 ▼
          ┌─────────────┐
          │  DONE    2  │
          └─────────────┘

任意关键阶段出错：

PENDING / RUNNING / UPLOADING ───────────────> FAILED 3
```

每次迁移都会写入：

- `tid`: 任务 ID
- `from_status`: 迁移前状态
- `to_status`: 迁移后状态
- `reason`: 人可读原因，例如“采集窗口结束，等待 Agent 上传产物”
- `source`: 迁移来源，例如 `apiserver`、`task_poller`
- `created_at`: 迁移时间

Web 任务详情页会展示完整状态迁移 reason。这样做的好处是，任务卡住时不需要猜测发生在哪一步，可以直接从状态事件表看到下发、采集、上传、完成或失败的原因。

分析状态独立放在 `analysis_status`，避免把“采集完成”和“分析完成”混在一起：

```text
0 待分析 -> 1 分析中 -> 2 分析完成
                  └──> 3 分析失败
```

## 5. Agent 心跳与审计

Agent 通过 drop_server 的 HealthCheck 服务周期上报自身信息和资源统计。apiserver 通过 gRPC `StatAgent` 与后台自动发现逻辑维护 Agent 在线状态。核心策略是：

- Agent 约每 5s 心跳或被 Server 探测。
- Server/apiserver 超过 30s 未看到有效心跳，判定离线。
- Agent 首次发现、离线、恢复在线都会写入 `agent_audit_logs`。
- Web 首页展示 Agent 列表、在线状态、最后心跳时间、Agent 审计日志。
- 点击某个 Agent 可以打开详情面板，查看 CPU、内存、读写吞吐，以及最近 10 条审计流。

Agent 审计日志包含：

- `ip_addr`
- `hostname`
- `event`: `registered` / `offline` / `recovered`
- `reason`
- `created_at`

这保证了“离线/恢复必须有审计日志”不是只在前端显示，而是落库可查。

## 6. 采集器设计

drop_agent 按 `profiler_type` 分发采集器：

```text
0 perf            CPU 采样，生成 perf.data / folded.txt / flamegraph.svg
1 async-profiler  Java 用户态采集器
2 pprof           Go pprof HTTP profile
3 eBPF/bpftrace   内核探针，支持 IO 延迟、调度延迟、CPU 折叠栈
```

eBPF 采集器使用 bpftrace 生成内核态观测数据。当前重点演示路径是：

- IO 延迟直方图：现场用 `dd` 制造 IO 写入。
- 调度延迟直方图：现场用短 CPU 忙等制造调度样本。

Analyzer 解析 bpftrace 输出，将直方图桶转换为：

- `bpf_histogram.svg`
- `bpf_data.json`
- 摘要指标和桶列表
- 基于 P95/P99 和热点桶的建议

用户态语言级采集器保留 async-profiler 与 pprof 两条路径。pprof 通过 HTTP `/debug/pprof/profile` 拉取 Go profile；async-profiler 面向 Java 进程。Web 创建任务时可以选择采集器类型，结果页会按采集器展示对应产物。

## 7. Continuous Profiling

Continuous Profiling 通过 `schedule_tasks` 与 robfig/cron 实现。用户在新建采样时勾选“持续采集”，系统会创建一个定时任务：

```text
schedule_task -> cron trigger -> child hotmethod_task -> normal task flow
```

每次 cron 触发都会创建一个普通采集任务，并将 `master_task_tid` 指向定时任务 SID。Web 的时间轴页面通过 `GET /api/v1/tasks/timeline?master_tid=...` 查询某个定时任务下的所有子任务，按时间顺序展示历史采集窗口。用户可以点击任意点进入对应结果页，回看该窗口的火焰图或直方图。

当前实现的重点是让时间轴能验证“常驻低频采样、定时切割、按时间回溯”的基本闭环。后续可以进一步支持任意 5 分钟窗口聚合和跨窗口对比。

## 8. 关键决策与取舍

### 8.1 MinIO 替代 COS

真实 Drop 使用对象存储保存大文件。复刻项目为了可复现性选择 MinIO，原因是评审机器只需要 Docker Compose 即可启动，不依赖云账号和外部网络。apiserver 通过存储接口访问 MinIO，后续替换 COS/S3 时只需要替换实现层。

### 8.2 分离采集状态与分析状态

采集任务的状态机只表达任务是否下发、采集、上传、完成或失败；分析引擎另用 `analysis_status` 表达待分析、分析中、分析完成和失败。这样可以避免一个任务“采集完成但分析仍在跑”时被误判为异常。

### 8.3 用轮询补齐状态，而不是直接依赖回调

drop_server 能收到 Agent 的 NotifyResult，但 apiserver 与 analysis 之间仍使用轮询推进。这个设计简单、稳定、便于演示，代价是实时性不如消息队列。复刻项目更关注端到端可跑通和状态可解释，因此优先选择轮询。

### 8.4 eBPF 提供 mock fallback，但演示必须真跑

在 WSL、macOS Docker Desktop 或权限不足的 Linux 上，bpftrace/perf 可能无法正常工作。为了让开发者仍能看到页面效果，Agent 保留 mock fallback。但评分要求里 eBPF 必须真跑，所以正式演示需要在 Ubuntu 22.04 或类似 Linux VM 上，确认容器具备 `privileged`、`pid: host`、`SYS_ADMIN`、`SYS_PTRACE`、`PERFMON` 等权限。

### 8.5 UI 优先展示可验证结果

结果页优先展示火焰图、eBPF 直方图、TopN、建议和文件下载按钮。相比复杂的筛选器和装饰性页面，当前 UI 更强调“评审能看到任务状态、reason、产物、下载入口和端到端证据”。

## 9. 工程基线

### 9.1 结构化日志

apiserver 使用 Zap 输出结构化日志，关键字段包括：

- `tid`
- `target_ip`
- `from_status`
- `to_status`
- `reason`
- `source`
- `error`

Agent 和 drop_server 输出采集器选择、心跳、任务领取、采集结果和 NotifyResult 信息，便于端到端排障。

### 9.2 显式错误处理

HTTP API 对常见错误返回明确状态码和 message，例如：

- 请求参数错误返回 400
- 任务不存在返回 404
- 对象存储不可用返回 503
- 数据库或存储异常返回 500

任务下发失败、gRPC 不可达、Agent 离线、产物列表失败等都会记录日志，并尽量写入任务或 Agent 审计 reason。

### 9.3 单测覆盖

`make coverage` 会执行 Go 覆盖率统计。当前覆盖率超过题目要求的 50%，重点覆盖：

- 配置加载与环境变量覆盖
- CORS、Recovery、AccessLog 中间件
- MinIO URL 生成与 endpoint 行为
- model/util 基础逻辑

### 9.4 端到端集成测试

`make e2e` 覆盖：

- 健康检查正常路径
- Agent 列表正常路径
- Agent 详情心跳策略
- 正常创建任务
- 任务状态事件落库
- `UPLOADING` 状态迁移落库并带 reason
- 非法创建任务返回 400
- 不存在任务返回 404
- Agent 审计日志存在

这些测试覆盖了“正常路径 + 两类异常路径”的题目要求。

## 10. 性能自证

Mini-Drop 的性能自证主要从三个角度做：

1. 采集链路可观测  
   任务状态迁移落库并展示 reason。出现卡顿时，可以定位是下发、采集、上传、分析还是前端展示问题。

2. Agent 自监控  
   Agent 上报 CPU、内存、读写吞吐。Web Agent 详情页能直接看到 Agent 自身资源开销，避免采集探针对业务造成不可见影响。

3. Demo 可制造可见变化  
   `make demo-ebpf-io` 用 `dd` 制造 IO 写入，`make demo-ebpf-sched` 用短 CPU 忙等制造调度样本。eBPF 直方图应能在真实 Linux 权限环境下看到分布变化，作为采集器有效性的自证。

正式演示时建议同时打开：

- Web 首页 Agent 列表
- 任务详情页状态迁移 reason
- eBPF 直方图页面
- MinIO 对应任务产物目录
- apiserver / drop_agent 日志

这样可以从 UI、数据库状态、对象存储产物和日志四个层面证明链路有效。

## 11. AI 协作说明

本项目在实现过程中使用 AI 辅助完成了需求拆解、代码审查、测试补齐和文档整理。AI 的角色不是替代工程判断，而是作为一个结对助手，帮助快速发现缺口、生成候选实现方案，并把验收项转化为可执行的测试。

具体协作方式包括：

- 对照题目要求逐项检查基础能力、扩展能力和交付物，识别状态机 reason、Agent 审计、覆盖率、E2E 等缺口。
- 阅读现有 Go、C++、Python、React 代码，尽量沿用原项目已有结构，而不是重写系统。
- 辅助实现 Web 结果页、Agent 详情面板、产物下载入口、TopN、建议展示和 eBPF 直方图布局。
- 辅助补充单测和 E2E 脚本，使“满足要求”可以通过命令验证，而不只是口头说明。
- 辅助整理 README 和设计文档，让评审可以复现启动、demo 和验收流程。

在 AI 生成建议后，仍通过以下方式做人工确认：

- 使用 `make test`、`make coverage`、`make verify`、`make e2e` 验证。
- 检查 Git diff，避免引入无关重构。
- 对 eBPF/perf 权限相关能力保留环境说明，避免把 mock fallback 误写成真实生产能力。

## 12. 如果再有 7 天我会做什么

如果再有 7 天，我会优先做这些改进：

1. 把前端页面进一步做得更美观、更统一  
   现在 UI 已经能完成采样、查看状态、看结果和下载文件，但视觉上仍偏工程化。后续会统一颜色、间距、表格、按钮和空状态，让首页、任务详情、时间轴、Agent 详情更像一个完整产品。

2. 增强结果页的交互体验  
   为火焰图、直方图、TopN 增加筛选、排序、复制函数名、下载全部产物、异常桶高亮和 baseline 对比。

3. 做更完整的真实采集演示环境  
   准备一个 Go pprof demo 服务、一个 Java async-profiler demo 进程和一个稳定的 eBPF Linux VM 环境，确保评审现场不依赖 mock fallback。

4. 强化 Continuous Profiling  
   支持任意 5 分钟窗口聚合、跨窗口趋势对比、异常窗口自动标注，以及从时间轴直接比较两次采样差异。

5. 增加更细的失败诊断  
   把 “perf 权限不足”“bpftrace 不存在”“目标 PID 不存在”“pprof 端点不可达”“MinIO 上传失败” 等错误转成结构化错误码，并在 Web 上给出修复建议。

6. 增加真实智能归因  
   将火焰图摘要、TopN、采集参数、历史 baseline 和系统指标结构化，交给 LLM 工具调用式分析，产出可验证的归因结论，而不是泛泛建议。

7. 加强安全与权限模型  
   增加用户/组权限、Agent 归属、任务访问控制、下载链接权限和审计导出能力，使系统更接近生产平台。

## 13. 演示建议

15 分钟演示可以按以下顺序：

1. `docker compose up -d --build` 启动全部组件。
2. 打开 Web 首页，展示 Agent 在线、Agent 详情和心跳审计。
3. 执行 `make demo-cpu`，进入结果页展示状态迁移、火焰图、TopN、下载入口。
4. 执行 `make demo-ebpf-io`，现场制造 IO，展示 eBPF 直方图和桶列表。
5. 执行 `make demo-ebpf-sched`，展示调度延迟直方图。
6. 创建一个持续采集任务，打开时间轴页面展示历史窗口。
7. 简述最得意的设计：状态迁移 reason 与 Agent 审计，使整条链路可解释、可排障、可验收。
8. 说明如果重做，会引入消息队列替代轮询，并建设更完整的真实采集 demo 环境。
