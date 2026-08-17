# Mini-Drop

Mini-Drop 是一个按需性能采集与分析平台复刻项目，包含四个主要组件：

- `web_frontend`: React Web UI，负责创建任务、查看 Agent、展示火焰图/eBPF 直方图/TopN/产物下载。
- `apiserver`: Go + Gin 编排层，负责任务状态、REST API、PostgreSQL、MinIO 对象存储和 gRPC 下发。
- `drop`: C++ gRPC 调度与 Agent，负责心跳、任务拉取、perf/eBPF/用户态采集器执行和结果上传。
- `analysis`: Python 分析引擎，负责 perf 火焰图、热点 TopN、eBPF 直方图和规则建议产物生成。

设计说明见 [docs/design.md](docs/design.md)，其中包含架构图、状态机迁移图、关键取舍、AI 协作说明、性能自证和后续 7 天计划。

## 环境要求

建议环境：

- Ubuntu 22.04 或同类 Linux 发行版
- Docker Engine + Docker Compose v2
- Linux kernel 5.8+，eBPF 演示需要宿主机能真实运行 `bpftrace` tracepoint
- `perf` 可用，CPU 火焰图建议允许 perf 采样
- 机器允许容器使用 `privileged`、`pid: host`、`SYS_ADMIN`、`SYS_PTRACE`、`PERFMON`、`BPF`、`SYS_RESOURCE`

特别说明：`perf record` 的真实 CPU 采样依赖 Linux perf_event 和硬件 PMU/性能计数器。部分 VMware/VirtualBox/WSL 环境无法开启“虚拟化 CPU 性能计数器”，即使容器权限正确，也可能无法真跑 perf 硬件采样。这属于虚拟化环境限制，不是系统链路错误。正式演示建议使用可开启 PMU 的 Linux 虚拟机、Linux 裸机或云主机。

### WSL 的 `perf not found for kernel ...-microsoft` 告警

这是 Ubuntu 的 `/usr/bin/perf` 包装器找不到微软定制 WSL 内核同版本工具包的提示；提示中的 `linux-tools-<WSL 内核版本>` 通常不在 Ubuntu 仓库，不能直接用 apt 修复。Mini-Drop Agent 使用镜像内的 `perf-real`，启动后可用以下命令确认真实采样能力：

```bash
docker compose exec drop_agent /usr/local/bin/perf-real stat -e task-clock true
```

如还需要让宿主 WSL 命令行的 `perf` 可用，执行：

```bash
bash scripts/setup_wsl_perf.sh
```

脚本会基于当前 WSL 6.6/6.1 内核线编译匹配的 perf，安装到 `/usr/local/bin/perf`，不会替换 `/usr/bin/perf` 或修改内核。完成后用 `perf stat -e task-clock true` 验收。即便该检查通过，WSL 的硬件 PMU 事件仍可能受 Hyper-V 限制；项目会报告真实失败，不会伪造采样成功。

题目要求 eBPF 必须真跑：演示视频里需要现场触发一次 IO 或调度异常，并在 Web 上看到分布变化。因此 eBPF 任务默认不再静默降级成 mock；如果日志里出现 `eBPF 采集失败`，需要更换到能运行 `bpftrace` 的 Linux 环境或修复容器权限。只有本地开发想看页面链路时，才可以显式设置 `DROP_ALLOW_EBPF_MOCK=1`。同一个开关也控制 `analysis` 侧的内存泄漏分析器：MinIO 不可用或 `memtrace.txt` 缺失时默认直接报错，而不是悄悄用内置模拟数据冒充真实检测结果。

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
sudo mount -t tracefs tracefs /sys/kernel/tracing || true
```

排查 perf/eBPF 是否因为环境降级：

```bash
docker compose logs drop_agent --tail=200
docker compose logs apiserver --tail=200
```

如果日志中出现 `mock`、`perf 不可用`、`eBPF 采集失败`、`Operation not permitted`、`perf_event_open` 失败等信息，通常说明当前宿主机或虚拟机没有暴露所需内核能力。VMware 中“虚拟化 CPU 性能计数器”无法勾选时，CPU perf 真采大概率无法完成；eBPF 评分演示则必须确认 `bpftrace` 能真实采到 tracepoint 数据。

## 一键启动

```bash
docker compose up -d --build
```

这条命令的作用是用 Docker Compose 一次性启动 Mini-Drop 的全部服务，包括 PostgreSQL、MinIO、drop_server、drop_agent、apiserver、analysis 和 web_frontend。`--build` 表示如果本地镜像不存在或代码有变化，就先重新构建镜像；`-d` 表示在后台运行。

服务默认地址：

- Web UI: http://localhost/
- API: http://localhost:8191
- MinIO Console: http://localhost:9001
- gRPC drop_server: `localhost:50051`
- Metrics: http://localhost:8191/metrics

健康检查：

```bash
make health
docker compose ps
```

如果 `docker compose ps` 里主要服务都是 `Up` 或 `healthy`，说明平台已经启动。此时再运行 `make demo`，才会真正创建采样任务。

本地 Compose 明确以 `MINI_DROP_ENV=development` 和 `ALLOW_INSECURE_TRANSPORT=true` 运行，方便一键演示。生产环境必须改用可信通道配置，至少替换 PostgreSQL、MinIO/S3、gRPC mTLS 证书和所有 Secret。

## Demo

`make demo` 不是启动系统，而是在系统已经启动后，通过 HTTP API 自动创建几条演示采样任务。可以把它理解成“自动帮你在网页里点了三次新建采样”。

题目要求的 `make demo` 会创建三条端到端任务：

- perf CPU 火焰图
- eBPF IO 延迟直方图
- eBPF 调度延迟直方图

```bash
make demo
```

命令会输出每个任务的结果页 URL。创建任务后需要等待采集、上传和分析完成，通常几十秒内会在结果页看到产物。Web 端进入任务详情后可查看：

- 任务状态和 reason
- 火焰图或 eBPF 直方图
- CPU 热点 TopN 或直方图摘要/桶列表
- `perf.data`、`flamegraph.svg`、`top.json`、`bpf_histogram.svg`、`bpf_data.json` 等产物下载按钮

也可以单独触发：

```bash
make demo-cpu
make demo-ebpf-io
make demo-ebpf-sched
make demo-pprof
```

`demo-pprof` 使用 Compose 内置的开发示例服务，并采集 `http://127.0.0.1:6060/debug/pprof/profile`。生产环境创建 Go pprof 任务时，必须在页面填写 Agent 网络命名空间可访问的完整 Profile URL；它可以是任意主机、端口或容器地址，不能只填写 PID，也不会再自动猜测 Agent 的 6060 端口。

`demo-ebpf-io` 会用 `dd` 在 `/tmp/mini-drop-demo-io.dat` 持续制造 IO 写入；`demo-ebpf-sched` 会用 4 个忙等循环制造 CPU 竞争/调度样本。

如果 `make demo-cpu` 在 VMware 中无法生成真实 perf 火焰图，请先确认虚拟机是否支持虚拟化 CPU 性能计数器。若该选项无法勾选，建议不要把它作为机器故障处理，而是在演示说明中标注“当前 VMware 环境无法暴露 PMU，perf 真采需要换可用 Linux 环境”。但 eBPF 是评分硬项，必须在可运行 `bpftrace` 的 Linux 环境中完成 IO/调度现场采集。

## Native Dual-Track Continuous Profiling（可选）

Native Continuous Profiling 是独立于旧 schedule 时间轴的整机低频持续采集链路。当前是双轨设计：

- CPU profile 轨：优先尝试 eBPF backend，fallback 顺序为 `CO-RE -> bpftrace -> perf`。当前版本会探测 CO-RE/BTF 能力并记录不可用原因，真实 eBPF CPU 样本先由 bpftrace backend 产出；再失败则退到现有 `perf record -a -F 19 -g`。
- 延迟 histogram 轨：使用 bpftrace 持续采集整机 IO 延迟和调度延迟 histogram，按 10 秒窗口、60 秒 batch 上传。

前端主机页的 Native Profiling 标签页提供 CPU、IO 延迟、调度延迟三类视图。CPU 视图可切换用户栈/内核栈；IO/调度视图展示合并 histogram、P50/P95/P99 趋势和当前 backend。

它默认关闭，原因是 WSL、部分 VMware/VirtualBox 和权限受限云主机经常无法运行 host perf 采样；默认关闭可以避免平台启动后持续刷 perf 权限错误。确认当前机器具备 perf 权限后，可显式启用：

```bash
DROP_NATIVE_CP_ENABLED=true docker compose up -d --build apiserver drop_agent
```

也可以运行 smoke 脚本完成一轮启用、等待上传和 API 查询：

```bash
bash scripts/native_cp_smoke.sh
```

可选环境变量：

- `DROP_NATIVE_CP_ENABLED=true`：启用 Agent 内置 Native CP sampler。
- `DROP_NATIVE_CP_EBPF_ENABLED=true`：启用持续 eBPF backend；关闭后 CPU 退回 perf，IO/sched histogram 显示不可用。
- `DROP_NATIVE_CP_SIGNALS=cpu,io,sched`：选择持续采集信号。
- `DROP_NATIVE_CP_CPU_BACKENDS=core,bpftrace,perf`：CPU backend fallback 顺序。当前 CO-RE 是能力探测/预留位，实际 eBPF CPU 数据由 bpftrace backend 产生。
- `DROP_BTF_PATH=/path/to/vmlinux.btf`：显式指定 CO-RE BTF 文件；未设置时默认探测 `/sys/kernel/btf/vmlinux`。
- `DROP_NATIVE_CP_API_BASE_URL=http://127.0.0.1:8191`：Agent 访问 apiserver 的地址，host network compose 默认使用本机地址。
- `DROP_NATIVE_CP_SESSION_ID=cps-...`：复用已有 session；不设置时 Agent 会自动创建。
- `WAIT_SECONDS=75`：smoke 等待上传 batch 的时间。

如果 smoke 返回空结果或日志出现 `perf record failed`、`bpftrace failed`、`tracepoints unavailable`、`CO-RE BTF unavailable`，先看：

```bash
docker compose logs --tail=120 drop_agent apiserver
```

常见原因仍是 `perf_event_paranoid`、容器 capability、host PID namespace 或虚拟化 PMU 不可用。此时 on-demand 任务和 Web/API 启动不受影响。

最常用的完整演示顺序是：

```bash
docker compose up -d --build
make demo
```

第一条命令启动平台，第二条命令创建演示任务。然后打开 http://localhost/ 查看任务列表和结果页。

## 关闭和结束流程

演示或开发结束后，需要区分“停止当前运行的服务”和“彻底清理本次运行产生的数据”。通常建议先用普通停止方式保留数据，确认不再需要历史任务和产物后，再执行彻底清理。

### 1. 确认演示任务已经结束

`make demo` 创建任务后，CPU/eBPF 采集会按照任务的 `duration` 自动结束；`demo-ebpf-io` 和 `demo-ebpf-sched` 为了制造现场负载而启动的临时进程也使用 `timeout` 控制，默认会在采样窗口结束后自动退出。

关闭平台前，建议先在 Web UI 的任务列表或任务详情页确认任务状态已经进入 `DONE` 或 `FAILED`。也可以通过下面命令查看容器和服务状态：

```bash
docker compose ps
docker compose logs apiserver --tail=100
docker compose logs analysis --tail=100
```

如果任务仍处于 `RUNNING`、`UPLOADING` 或分析中，直接关闭不会破坏 Docker 本身，但可能导致该任务本轮采集、上传或分析没有完整完成。演示录屏时建议等结果页产物可见后再关闭。

### 2. 普通停止：保留数据库和 MinIO 产物

如果只是结束本次运行，并希望下次启动后仍能看到历史任务、Agent 记录和已经上传的火焰图/eBPF 产物，请执行：

```bash
docker compose down
```

这会停止并删除本项目启动的容器和默认网络，但不会删除 Docker volume。PostgreSQL 的任务数据和 MinIO 的对象文件会继续保留在 `pgdata`、`miniodata` 两个数据卷中。下次重新执行：

```bash
docker compose up -d --build
```

平台会复用这些数据卷，历史任务和产物仍然可以查看。

### 3. 彻底清理：删除容器、网络和持久化数据

如果需要把环境恢复到“第一次启动前”的干净状态，或者演示数据已经不再需要，可以执行：

```bash
docker compose down -v
```

`-v` 会同时删除 Compose 创建的数据卷，因此会清空：

- PostgreSQL 中的用户、任务、Agent、状态流转记录
- MinIO 中保存的 `perf.data`、`flamegraph.svg`、`top.json`、`bpf_histogram.svg`、`bpf_data.json` 等采集和分析产物

这一步不可恢复。执行前请确认不需要保留页面上的历史任务结果，也不需要继续下载本次演示生成的文件。

### 4. 可选清理镜像和本地临时文件

如果还想释放本项目构建出来的镜像空间，可以在 `docker compose down` 或 `docker compose down -v` 之后查看镜像：

```bash
docker images
```

确认镜像不再需要后，再按镜像名或镜像 ID 删除。不要在不了解镜像用途时批量清理，以免影响其他 Docker 项目。

`make demo-ebpf-io` 会在宿主机 `/tmp` 下临时写入 `/tmp/mini-drop-demo-io.dat`，正常情况下脚本会自动删除；日志可能保留在 `/tmp/mini-drop-demo-io.log`。如需手动清理，可以执行：

```bash
rm -f /tmp/mini-drop-demo-io.dat /tmp/mini-drop-demo-io.log
```

### 5. 确认端口已经释放

关闭后如果想确认 Web、API、MinIO 和 gRPC 端口已经不再被本项目占用，可以查看：

```bash
docker compose ps
ss -ltnp | grep -E ':(80|8191|9000|9001|50051)\b' || true
```

普通结束流程可以概括为：

```bash
docker compose down
```

彻底清空环境则使用：

```bash
docker compose down -v
```

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

   在支持 perf_event/PMU 的 Linux 环境中，期望产物包含 `perf.data`、`flamegraph.svg`、`folded.txt`、`top.json`，页面展示火焰图和热点 TopN。若虚拟机无法暴露 CPU 性能计数器，则这一步可能降级或失败，需要更换演示环境。

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

## 安全、可观测性与部署

阶段 7 增加了生产配置边界和运行可观测性：

- `/metrics` 暴露 Prometheus 文本指标，包括任务状态、Outbox、AnalysisJob、在线 Agent、SSE 连接数和关键计数器。
- Go/Python/C++ 日志会脱敏 DSN password、access key、secret、Bearer token、profile URL token 和敏感对象路径。
- `apiserver` 支持 `MINI_DROP_ENV`、`ALLOW_INSECURE_TRANSPORT`、`GRPC_MTLS_CERT_FILE`、`GRPC_MTLS_KEY_FILE`、`GRPC_MTLS_CA_FILE`、`METRICS_ENABLED` 环境变量。
- `deploy/kubernetes/` 提供基础 Kubernetes 模板；部署前必须替换镜像、Secret、证书和域名。
- `deploy/runbook.md` 覆盖 Agent 离线、任务积压、分析失败、存储不可用、Outbox 堆积和 SSE 异常。
- `deploy/backup-restore.md` 描述 PostgreSQL、MinIO/S3 的备份恢复和数据保留策略。
- `scripts/sbom_scan.sh` 使用 `syft` 生成 SBOM，并用 `grype` 或 `trivy` 扫描镜像。

## 状态机说明

Web/API 使用以下状态表达任务主流程：

- `PENDING`: 任务已创建，等待下发
- `RUNNING`: 已下发到 drop_server，等待 Agent 采集
- `UPLOADING`: 采集窗口结束，等待 Agent 上传产物或等待产物可见
- `DONE`: 采集完成，等待或已完成分析
- `FAILED`: 下发、采集或分析失败，`status_info` 记录原因

`analysis_status` 独立表达分析流程：

- `0`: 待分析
- `1`: 分析中
- `2`: 分析完成
- `3`: 分析失败

每次任务状态迁移都会写入 `task_status_events`，包含 `from_status`、`to_status`、`reason`、`source` 和时间戳；Web 任务详情页会实时展示这些 reason。

## 提交规范

每个 commit message 必须说明本次改动的目的，例如：

- `Render profiling artifacts in task results`
- `Add demo targets for CPU and eBPF verification`
- `Parse BPF histogram buckets for browser display`

不要使用 `update`、`fix`、`wip` 这类无法解释改动原因的提交信息。
