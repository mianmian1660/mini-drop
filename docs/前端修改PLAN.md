# Mini-Drop 接入持续 Profiling 的产品与技术方案

## Summary

目标是把 Mini-Drop 从“按需采样任务平台”扩展为“统一性能观测平台”：保留现有所有前端和按需采集能力，同时新增基于开源持续 profiling agent 的主机/服务级持续画像能力。

最终形态：

```text
被观测服务器：
  - parca-agent：持续 profiling
  - drop_agent：按需采集 / 特殊诊断

中心服务器：
  - Parca Server：接收、存储、查询持续 profile
  - Mini-Drop：统一 API、任务编排、分析、前端展示
```

用户在 Mini-Drop 页面中先选择主机或服务，再查看该对象的持续火焰图、TopN、时间范围画像、对比结果，也可以继续创建现有的按需采样任务。

## Key Changes

### 1. 架构分层

采用双采集面架构：

```text
Continuous Profiling Plane
  parca-agent + Parca Server
  用于常驻、低开销、长期 CPU/内存 profile 采集

On-demand Diagnostics Plane
  drop_agent + drop_server + analysis
  保留现有 perf/eBPF/pprof/async-profiler 按需任务能力

Unified Visualization Plane
  Mini-Drop apiserver + web_frontend
  统一展示主机、服务、profile、任务、火焰图、TopN、时间轴
```

不让 AI 或 Mini-Drop 重新实现 eBPF 持续 profiling agent。AI 只用于分析解释、热点归因、对比总结和优化建议。

### 2. 部署变化

每台被观测机器部署两个 agent：

```text
parca-agent
  负责持续 CPU/内存 profiling，上报到 Parca Server

drop_agent
  负责现有按需采样任务，上报到 Mini-Drop/drop_server
```

中心机器新增 Parca Server：

```text
central-host
  - parca
  - drop_server
  - apiserver
  - analysis
  - web_frontend
  - postgres
  - minio
```

Mini-Drop 前端不直接访问 Parca UI，统一通过 Mini-Drop apiserver 查询持续 profile 数据。

### 3. 后端接口新增

Mini-Drop apiserver 增加 profiling 查询层，包装 Parca 查询能力。

建议新增 API：

```text
GET /api/v1/profile/targets
GET /api/v1/profile/query
GET /api/v1/profile/flamegraph
GET /api/v1/profile/topn
GET /api/v1/profile/diff
```

核心查询参数：

```text
target_id
host
service
from
to
profile_type
labels
```

其中 `profile_type` v1 默认支持：

```text
cpu
memory
```

v1 可以先重点实现 CPU，memory 作为接口和页面选项预留。

### 4. 前端页面调整

保留现有所有页面和功能：

```text
任务列表
任务详情
新建采样
Agent 列表
现有 Timeline
perf/eBPF/pprof/async-profiler 结果展示
```

新增“观测对象优先”的页面结构：

```text
Dashboard / 首页
  - 主机列表
  - 服务列表
  - drop_agent 状态
  - parca_agent 状态
  - 最近任务入口

Host Detail / 主机详情
  - 当前主机信息
  - 持续 profiling 面板
  - 该主机的按需任务列表
  - 创建按需任务入口

Profiles / 持续画像页
  - 选择 host/service
  - 选择时间范围
  - 选择 profile 类型
  - 展示火焰图、TopN、趋势、AI 分析

Profile Diff / 对比页
  - 对比两个时间段的热点变化
```

所有持续画像页面顶部必须明确展示当前观察对象：

```text
host
ip
service
environment
time range
profile type
agent status
```

避免用户不知道自己正在看哪台机器的数据。

### 5. 数据模型建议

v1 明确新增一个轻量的观测对象抽象，不复用现有 `AgentInfo` 作为持续 profiling 的主模型：

```text
ProfileTarget
  id
  hostname
  ip
  service_name
  environment
  labels
  parca_agent_status
  drop_agent_status
  last_profile_at
  last_seen
```

`ProfileTarget` 作为页面选择主机/服务时的统一对象。它负责表达“用户当前正在观察谁”，并在 API 层聚合 Parca agent 状态、drop_agent 状态和最近 profile 时间。现有 `AgentInfo` 继续服务于 drop_agent 的心跳、按需任务和审计逻辑，不承担持续 profiling 的主模型职责。

## Test Plan

### 后端测试

- `GET /api/v1/profile/targets` 能返回可观测主机/服务列表。
- 当 Parca Server 不可用时，接口返回明确错误，现有任务功能不受影响。
- 查询指定 host、service、时间范围时，apiserver 能正确构造 Parca 查询。
- `profile_type=cpu` 返回可渲染火焰图或 profile 数据。
- 无 profile 数据时返回空状态，不误报失败。
- `diff` 接口能处理两个时间段，其中一个时间段无数据的情况。

### 前端测试

- 首页能显示主机/服务列表。
- 用户选择某个主机后，页面顶部明确显示当前观察对象。
- 持续画像页可以切换时间范围并刷新火焰图。
- 现有任务列表、新建任务、任务详情、Timeline 页面保持可用。
- drop_agent 离线但 parca_agent 在线时，持续画像可看，按需任务不可创建或提示不可用。
- parca_agent 离线但 drop_agent 在线时，持续画像提示无数据，按需任务仍可创建。

### 集成验证

- v1 暂不要求两台机器联调，先完成主机页面和单主机展示闭环。
- 在本机或单台测试机器上部署 `parca-agent + drop_agent`。
- 中心侧部署 `Parca Server + Mini-Drop`，可以与被观测机器暂时同机。
- Mini-Drop 页面能显示当前主机，并在持续画像页面顶部明确展示 host、ip、时间范围和 profile 类型。
- 对当前主机执行压力负载后，持续火焰图能看到热点变化。
- 对当前主机创建现有按需 perf/eBPF 任务，结果仍显示在原有 Mini-Drop 页面中。

## Assumptions

- v1 使用 Parca 作为持续 profiling 后端。
- 每台被观测机器允许同时部署 `parca-agent` 和 `drop_agent`。
- v1 先按单主机/本机展示完成页面和数据闭环，不把多机器联调作为当前验收要求。
- v1 新增 `ProfileTarget` 作为持续 profiling 的观测对象模型，不复用 `AgentInfo` 作为主模型。
- Mini-Drop 现有前端功能全部保留，不做替换式重构。
- Mini-Drop 前端不直接暴露 Parca UI，而是通过 Mini-Drop apiserver 统一访问。
- v1 重点实现 CPU continuous profiling；memory profiling 可以保留接口和 UI 入口，按 Parca 实际能力逐步补齐。
- AI 能力只用于 profile 结果解释、热点变化总结和优化建议，不参与底层采集器实现。
