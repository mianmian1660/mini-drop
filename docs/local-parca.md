# 本机 Parca 持续 Profiling 验证

Mini-Drop 默认启动中心 `parca`、`drop_agent` 和 Mini-Drop 服务。本机 `parca-agent` 依赖宿主内核 eBPF/perf 能力，在 WSL 或受限云主机上经常不可用，因此被放到可选 compose profile 中。前端仍通过 Mini-Drop apiserver 查询，并在 Parca JSON 网关不兼容时提供可直接打开的 Parca UI 查询链接。

## 启动

```bash
docker compose up -d --build
```

关键端口：

- Mini-Drop Web: `http://localhost`
- Mini-Drop API: `http://localhost:8191`
- Parca UI/API: `http://localhost:7070`

如需在真实 Linux 本机开启 host parca-agent，可额外运行：

```bash
docker compose --profile host-parca-agent up -d parca_agent
```

`docker-compose.yml` 中的可选 `parca_agent` 使用 host network、host PID 和 eBPF/perf 所需 capabilities，并给所有 profile 附加这些本机标签：

```text
job=hotmethod
env=development
instance=127.0.0.1
node=mini-drop-local
```

Mini-Drop 的持续 profiling 查询会用 `job` 和 `instance` 构造 Parca selector，并保留 `env/node` 用于页面展示和排查。

## 远端主机上报到中心 Parca

中心机器继续启动 `parca`、`apiserver`、`web_frontend` 等服务，并确保远端主机能访问中心的 `7070` 端口。远端机器需要单独启动与中心 Parca Server 版本一致的 `parca-agent`，把 profile 写入中心 Parca：

```bash
docker run -d --name mini-drop-parca-agent \
  --restart unless-stopped \
  --privileged \
  --pid host \
  --network host \
  --security-opt apparmor=unconfined \
  --cap-add SYS_ADMIN \
  --cap-add SYS_PTRACE \
  --cap-add PERFMON \
  --cap-add BPF \
  --cap-add SYS_RESOURCE \
  --ulimit memlock=-1:-1 \
  -v /sys/kernel/debug:/sys/kernel/debug:rw \
  -v /sys/kernel/tracing:/sys/kernel/tracing:rw \
  -v /sys/fs/bpf:/sys/fs/bpf:rw \
  -v /proc:/host/proc:ro \
  ghcr.io/parca-dev/parca-agent:v0.49.0 \
  --node="$(hostname)" \
  --remote-store-address=<中心IP>:7070 \
  --remote-store-insecure \
  --remote-store-use-v2-schema=false \
  --metadata-external-labels='job=hotmethod;env=development;instance=<远端IP>;node='"$(hostname)" \
  --log-level=info
```

或者使用仓库脚本在远端替换/启动 `parca-agent`：

```bash
REMOTE_HOST=ubuntu@<远端IP> \
CENTER_GRPC_ADDR=<中心IP>:7070 \
REMOTE_AGENT_IP=<远端IP> \
bash scripts/deploy_remote_parca_agent.sh
```

同时远端 `drop_agent` 应使用同一个 `<远端IP>` 注册，例如设置 `DROP_AGENT_IP=<远端IP>`。Mini-Drop 会优先使用 Agent 上报的 labels；实际 Parca 查询使用稳定的 `job` 和 `instance`，因此远端 `parca-agent --metadata-external-labels` 至少要包含 `job=hotmethod;instance=<远端IP>`；v0.49 的 labels 分隔符必须是分号。

远端 `drop_agent` 必须和中心仓库代码同步，否则 eBPF 修复不会生效。可以使用仓库脚本更新远端容器：

```bash
REMOTE_HOST=ubuntu@<远端IP> \
CENTER_ADDR=<中心IP>:50051 \
REMOTE_AGENT_IP=<远端IP> \
bash scripts/deploy_remote_drop_agent.sh
```

WSL 下 `parca_agent` 可能因为 eBPF 能力不足退出。为了仍然有真实 profile 可看，Parca Server 会通过 `deploy/parca/parca.yaml` 主动 scrape 这些 pprof 端点：

```text
parca:7070
pprof_demo:6060
```

在 Parca UI 中可以先查询：

```text
{job="pprof_demo"}
{job="parca"}
```

## 验证

等待服务启动后运行：

```bash
scripts/parca_local_smoke.sh
```

然后打开 Mini-Drop：

1. 进入首页主机列表。
2. 打开本机主机详情。
3. 进入“持续 profiling”tab。
4. 在“Parca 可用数据”区域打开或查看内嵌 Parca UI。
5. 选择最近 30 分钟，查询 `{job="pprof_demo"}` 或 `{job="parca"}`。

## WSL 说明

WSL 环境可能无法完整暴露 eBPF/perf、BTF、符号化或内核采样能力。此时 Mini-Drop 应显示明确状态：

- `unconfigured`: Mini-Drop 未启用 Parca 配置。
- `unknown`: Parca 可访问性或状态探测不确定。
- `offline`: Parca 有服务但当前 selector 没有样本。
- 页面空态提示 WSL/eBPF 权限不足或当前时间段无样本。

完整火焰图验收建议在真实 Linux 或云服务器上进行。
