# Pyroscope eBPF 性能剖析接入说明

这个目录为 Mini-Drop 实验增加了一套旁路运行的 Pyroscope 配置。它不会改变现有 Mini-Drop 任务链路：

```text
Web -> apiserver -> drop_server -> drop_agent -> MinIO -> analysis -> Web
```

旁路链路中，Grafana Alloy 以具备特权权限的 eBPF 性能剖析器方式运行，发现本机 Docker 容器，并把持续采集到的 CPU 性能剖析数据发送到独立的 Pyroscope 后端。

## 文件

```text
deploy/profiling/pyroscope/docker-compose.yml
deploy/profiling/pyroscope/alloy.config
scripts/profiling/pyroscope_check.sh
docs/profiling/pyroscope.md
```

## 启动 Mini-Drop

如果希望 Pyroscope 能看到 Mini-Drop 相关容器，需要先启动正常项目：

```bash
docker compose up -d --build
make demo
```

## 启动 Pyroscope

启动独立的 Pyroscope 持续性能剖析栈：

```bash
make profiling-pyroscope-up
```

打开：

```text
Pyroscope UI: http://localhost:4040
Alloy UI:     http://localhost:12345
```

在 Pyroscope 中选择性能剖析类型 `process_cpu`，然后用 `container`、`service_name`、`compose_project`、`project`、`profiler` 等标签过滤目标容器。

## 验证

```bash
bash scripts/profiling/pyroscope_check.sh
```

也可以通过 Make 执行同样的检查：

```bash
make profiling-pyroscope-check
```

Pyroscope 启动初期可能临时返回 `503`，因为内部的 metastore、ingester、segment writer 还在完成启动等待期。检查脚本会自动重试。

脚本还会打印 Pyroscope 当前可见的性能剖析序列。通过这部分输出可以直接判断：后端目前是否只看到了 Pyroscope 自身性能剖析数据，还是已经收到了 Alloy eBPF 从 Mini-Drop 容器采集到的性能剖析数据。

如果性能剖析数据没有立刻出现，可以让 Mini-Drop 保持一段时间的业务负载：

```bash
make demo
```

也可以在另一个终端制造 CPU 负载，等待 30-60 秒后再刷新 Pyroscope。

## 停止

```bash
make profiling-pyroscope-down
```

如果也要删除 Pyroscope 已存数据：

```bash
docker compose -f deploy/profiling/pyroscope/docker-compose.yml down -v
```

## 注意事项

- Pyroscope 监听端口是 `4040`。
- Alloy 监听端口是 `12345`。
- Alloy 必须以 root 身份运行，并使用 host PID namespace 和 `pyroscope.ebpf` 所需的 privileged 权限。
- eBPF 性能剖析器要求 Linux kernel `4.9+`。
- WSL2 可能出现 Alloy ready、eBPF tracer loaded，但 Pyroscope 里仍只看到 `service_name=pyroscope` 的情况。最终验收建议使用真实 Linux 主机、云主机或支持 eBPF 的虚拟机。
- 这套配置使用 Docker discovery，因此会以只读方式挂载 `/var/run/docker.sock`。
- 符号缓存数据保存在 `alloy-symb-cache` Docker volume 中，对应容器内路径 `/tmp/symb-cache`。

## 与 Mini-Drop 的关系

这一套配置适合作为 Pyroscope 对比基线：

- Mini-Drop 仍然是按需性能剖析平台。
- Pyroscope 对同一批正在运行的容器提供持续性能剖析。
- 最终对比可以重点关注部署复杂度、eBPF 权限、UI、标签模型、资源开销，以及不同方案更适合按需诊断还是长期观测。

## 参考

- Grafana Pyroscope 入门文档: https://grafana.com/docs/pyroscope/latest/get-started/
- Pyroscope eBPF on Docker with Grafana Alloy: https://grafana.com/docs/pyroscope/latest/configure-client/grafana-alloy/ebpf/setup-docker/
- Alloy `pyroscope.ebpf` 组件文档: https://grafana.com/docs/alloy/latest/reference/components/pyroscope/pyroscope.ebpf/
