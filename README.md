# Mini-Drop

Mini-Drop 是一个按需性能采集与分析平台复刻项目，包含四个主要组件：

- `web_frontend`: React Web UI，负责创建任务、查看 Agent、展示火焰图/eBPF 直方图/TopN/产物下载。
- `apiserver`: Go + Gin 编排层，负责任务状态、REST API、PostgreSQL、MinIO 对象存储和 gRPC 下发。
- `drop`: C++ gRPC 调度与 Agent，负责心跳、任务拉取、perf/eBPF/用户态采集器执行和结果上传。
- `analysis`: Python 分析引擎，负责 perf 火焰图、热点 TopN、eBPF 直方图和规则建议产物生成。

## 环境要求

建议环境：

- Ubuntu 22.04 或同类 Linux 发行版
- Docker Engine + Docker Compose v2
- Linux kernel 5.8+，eBPF 演示需要 `bpftrace`/tracepoint 能力
- `perf` 可用，CPU 火焰图建议允许 perf 采样
- 机器允许容器使用 `privileged`、`pid: host`、`SYS_ADMIN`、`SYS_PTRACE`、`PERFMON`

如果 perf 或 eBPF 权限受限，请在宿主机上确认：

```bash
cat /proc/sys/kernel/perf_event_paranoid
cat /proc/sys/kernel/kptr_restrict
```

开发或演示环境可临时放宽：

```bash
sudo sysctl kernel.perf_event_paranoid=1
sudo sysctl kernel.kptr_restrict=0
```

部分发行版还需要：

```bash
sudo mount -t debugfs debugfs /sys/kernel/debug || true
```

## 一键启动

```bash
docker compose up -d --build
```

服务默认地址：

- Web UI: http://localhost/
- API: http://localhost:8191
- MinIO Console: http://localhost:9001
- gRPC drop_server: `localhost:50051`

健康检查：

```bash
make health
docker compose ps
```

## Demo

题目要求的 `make demo` 会创建三条端到端任务：

- perf CPU 火焰图
- eBPF IO 延迟直方图
- eBPF 调度延迟直方图

```bash
make demo
```

命令会输出每个任务的结果页 URL。Web 端进入任务详情后可查看：

- 任务状态和 reason
- 火焰图或 eBPF 直方图
- CPU 热点 TopN 或直方图摘要/桶列表
- `perf.data`、`flamegraph.svg`、`top.json`、`bpf_histogram.svg`、`bpf_data.json` 等产物下载按钮

也可以单独触发：

```bash
make demo-cpu
make demo-ebpf-io
make demo-ebpf-sched
```

`demo-ebpf-io` 会用 `dd` 在 `/tmp/mini-drop-demo-io.dat` 制造一次短 IO 写入；`demo-ebpf-sched` 会用短 CPU 忙等制造调度样本。

## 端到端验证

按《drop系统复刻指南.md》的端到端链路验证：

1. 启动全部组件：

   ```bash
   docker compose up -d --build
   ```

2. 确认 Agent 在线：

   ```bash
   curl -s http://localhost:8191/api/v1/agents
   ```

3. 创建 CPU 任务并查看结果页：

   ```bash
   make demo-cpu
   ```

   期望产物包含 `perf.data`、`flamegraph.svg`、`folded.txt`、`top.json`，页面展示火焰图和热点 TopN。

4. 创建 eBPF IO 任务并查看结果页：

   ```bash
   make demo-ebpf-io
   ```

   期望产物包含 `perf.data`、`bpf_histogram.svg`、`bpf_data.json`、`bpf_raw.txt`，页面展示直方图、摘要、桶列表和下载入口。

5. 创建 eBPF 调度任务并查看结果页：

   ```bash
   make demo-ebpf-sched
   ```

   期望页面展示调度延迟直方图，并能下载对应原始/分析文件。

## 本地测试

```bash
make test
```

该命令会执行：

- `make -C drop/build`
- `go test ./...`
- `python3 analysis/test_analysis.py`
- `npm run build`

提交前建议执行：

```bash
make verify
```

## 状态机说明

Web/API 使用以下状态表达任务主流程：

- `PENDING`: 任务已创建，等待下发
- `RUNNING`: 已下发到 drop_server，等待 Agent 采集/上传
- `UPLOADING`: 当前实现中与 `RUNNING` 共用数据库状态，通过 `status_info` reason 展示上传/下发阶段
- `DONE`: 采集完成，等待或已完成分析
- `FAILED`: 下发、采集或分析失败，`status_info` 记录原因

`analysis_status` 独立表达分析流程：

- `0`: 待分析
- `1`: 分析中
- `2`: 分析完成
- `3`: 分析失败

每次关键迁移都会更新 `status_info` 或分析 reason，Web 任务详情页会实时展示。

## 提交规范

每个 commit message 必须说明本次改动的目的，例如：

- `Render profiling artifacts in task results`
- `Add demo targets for CPU and eBPF verification`
- `Parse BPF histogram buckets for browser display`

不要使用 `update`、`fix`、`wip` 这类无法解释改动原因的提交信息。
