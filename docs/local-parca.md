# 本机 Parca 持续 Profiling 验证

Mini-Drop 当前先支持单机闭环：`parca`、`parca-agent`、`drop_agent` 和 Mini-Drop 服务都在本机启动。前端仍通过 Mini-Drop apiserver 查询，不直接访问 Parca UI。

## 启动

```bash
docker compose up -d --build
```

关键端口：

- Mini-Drop Web: `http://localhost`
- Mini-Drop API: `http://localhost:8191`
- Parca UI/API: `http://localhost:7070`

`docker-compose.yml` 中的 `parca_agent` 使用 host network、host PID 和 eBPF/perf 所需 capabilities，并给所有 profile 附加这些本机标签：

```text
job=hotmethod
env=development
instance=127.0.0.1
node=mini-drop-local
```

Mini-Drop 的持续 profiling 查询会用这些标签构造 Parca selector。

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
