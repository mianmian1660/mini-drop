# Mini-Drop 持续性能剖析第一阶段方案：Pyroscope + Grafana Alloy

## 目标

第一阶段先在 Mini-Drop 周围接入一套可复现的持续性能剖析能力，不改核心任务协议，也不自研多语言 eBPF 栈展开器。

本阶段范围：

- 使用 Grafana Alloy 作为具备特权权限的 eBPF 性能剖析器，持续采集本机 Docker 容器和进程的 CPU 性能剖析数据。
- 使用 Pyroscope 作为持续性能剖析后端，负责存储、查询和火焰图展示。
- 保持 Mini-Drop 现有按需性能剖析链路不变。
- 提供 Compose、Makefile 命令、文档和检查脚本，保证方案可以稳定启动和验证。

本阶段暂不做：

- 不把 Pyroscope 性能剖析数据导入 Mini-Drop 数据库。
- 不在 Mini-Drop 前端内嵌完整 Pyroscope 查询 UI。
- 不在 Mini-Drop 内部实现 C/C++、Go、Rust、Python、Java 的原生 eBPF 栈展开。
- 不接入 Parca；Parca 由其他分支单独调研。

## 架构

```text
Mini-Drop 容器
  ├─ drop_server
  ├─ drop_agent
  ├─ apiserver
  ├─ analysis
  └─ web_frontend

Docker socket + host PID namespace
  ↓
Grafana Alloy pyroscope.ebpf
  ↓
Pyroscope
  ↓
Pyroscope UI / API
```

现有 Mini-Drop 链路仍然是按需采样：

```text
Web -> apiserver -> drop_server -> drop_agent -> MinIO -> analysis -> Web
```

Pyroscope 链路作为旁路持续采样：

```text
Alloy -> eBPF -> Pyroscope -> Pyroscope UI
```

## 为什么第一阶段选 Pyroscope

- 仓库里已经有 Pyroscope/Alloy 实验目录：`deploy/profiling/pyroscope`。
- 部署规模小：一个 Pyroscope 容器加一个 Alloy 容器即可启动。
- 适合作为多语言性能剖析的第一阶段基线。eBPF 可以覆盖 C/C++、Go、Rust 等原生进程；Python、Java 先观察进程级 CPU 热点，后续再比较 SDK 或运行时感知性能剖析器的符号质量。
- 演示直观：Pyroscope UI 可以看到性能剖析序列、火焰图、时间范围和标签过滤。

## 运行方式

先启动 Mini-Drop：

```bash
docker compose up -d --build
make demo
```

启动持续性能剖析栈：

```bash
make profiling-pyroscope-up
```

验证：

```bash
make profiling-pyroscope-check
```

打开：

```text
Mini-Drop Web: http://localhost/
Pyroscope UI:  http://localhost:4040
Alloy UI:      http://localhost:12345
```

在 Pyroscope 中选择 `process_cpu`，再用 `container`、`service_name`、`compose_project`、`project`、`profiler` 等 label 过滤 Mini-Drop 相关容器。

停止持续性能剖析栈：

```bash
make profiling-pyroscope-down
```

如果要删除 Pyroscope 已存数据：

```bash
docker compose -f deploy/profiling/pyroscope/docker-compose.yml down -v
```

## 验收标准

1. `make profiling-pyroscope-up` 能启动 `mini-drop-pyroscope` 和 `mini-drop-pyroscope-alloy`。
2. `make profiling-pyroscope-check` 能看到 Pyroscope ready 和 Alloy ready。
3. Drop 支持创建 `continuous_cpu` 元任务：
   - `task_type=7`
   - `profiler_type=4`
   - `backend_type=pyroscope`
   - 支持目标 Agent、目标 PID、目标容器、采样频率、持续时间和 labels。
4. 后端创建 `continuous_cpu` 时会检查 Pyroscope `/ready` 和 Alloy `/-/ready`，并把检查结果写入任务状态。
5. 执行 `make demo` 或创建多语言负载后，Pyroscope 能看到 Mini-Drop 相关容器的 `process_cpu` 性能剖析序列。
6. Mini-Drop Web 仍能正常展示现有按需性能剖析任务结果。
7. Drop 任务详情页能展示持续 profiling backend、collector、profile 类型、profile 时间范围、查询表达式、Pyroscope 查询链接、labels 和运行状态。
8. 汇报时能解释清楚边界：Mini-Drop 当前是按需诊断系统，Pyroscope 提供第一阶段持续性能剖析基线。

任务详情页中的 Pyroscope 查询上下文来自 Drop 任务时间窗和固定 label selector：

```text
query: {project="mini-drop"}
from/until: 任务开始/结束时间，或创建时间加采样时长
```

第一阶段不会把 Pyroscope profile 按 `tid` 写回 Drop 数据库；页面展示的是“该任务时间窗口对应的旁路持续 profiling 查询入口”。第二阶段再做 profile export、MinIO artifact 和 Analyzer 回灌。

如果在 WSL2 中只能看到 `service_name=pyroscope`，且 Alloy metrics 中 `pyroscope_forwarded_entries_total` 仍为 0，这通常说明 Alloy 已启动、Docker discovery 也可能已发现目标，但 eBPF 容器 profile 没有成功写入 Pyroscope。此时第一阶段方案、部署和检查链路可以算完成；“采到 Mini-Drop 容器持续 profile”的运行验收需要换真实 Linux 环境复测。

## 多语言测试状态

第一阶段的多语言验证目标是“制造 C/C++、Go、Rust、Python、Java 负载，并在 Pyroscope UI 中看到对应容器或进程的 `process_cpu` profile”。这一步依赖宿主机 eBPF 能力。

当前 WSL2 环境中可以运行多语言程序来制造 CPU 负载，但不能可靠证明 Pyroscope/Alloy 能采到这些进程。最终多语言截图和验收建议放到真实 Linux 主机、云主机或支持 eBPF 的虚拟机中完成。

## 后续演进

第二阶段可以把持续性能剖析能力显式接入 Mini-Drop：

- 新增 `continuous_cpu` 任务类型。
- 在 `request_params` 中记录 Pyroscope label selector 和时间窗口。
- 在任务结果页展示 Pyroscope 查询链接。
- 周期性导出性能剖析数据为 pprof 或 folded stack，并上传 MinIO。
- 复用现有 Analyzer 生成火焰图和 TopN 视图。

第三阶段再抽象 backend：

```text
ProfilerBackend
  ├─ Start(session)
  ├─ Stop(session)
  ├─ Query(labels, time_range)
  └─ Export(format)
```

之后 Pyroscope、Parca、OpenTelemetry eBPF Profiler 都可以挂在同一接口后面做对比。

## 风险

- Alloy eBPF 需要 Linux、host PID namespace、privileged 权限和可用的 eBPF 内核能力。
- macOS/Windows Docker Desktop 和 WSL2 不是可靠演示环境，建议使用 Linux 主机、云主机或支持 eBPF 的虚拟机。
- Python、Java 的符号质量可能弱于专用 SDK 或运行时感知性能剖析器。第一阶段应表述为 eBPF 基线，不是最终多语言精细栈展开实现。

## 参考

- Pyroscope eBPF on Docker with Grafana Alloy: https://grafana.com/docs/pyroscope/latest/configure-client/grafana-alloy/ebpf/setup-docker/
- Alloy `pyroscope.ebpf`: https://grafana.com/docs/alloy/latest/reference/components/pyroscope/pyroscope.ebpf/
- Pyroscope 支持平台文档: https://grafana.com/docs/pyroscope/latest/configure-client/supported-platforms/
