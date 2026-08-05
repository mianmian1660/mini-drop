# Mini-Drop 运维 Runbook

## 快速入口

- 存活检查：`curl -fsS http://<api>/livez`
- 就绪检查：`curl -fsS http://<api>/readyz`
- 指标入口：`curl -fsS http://<api>/metrics`
- 本地日志：`docker compose logs apiserver analysis drop_agent drop_server --tail=200`
- Kubernetes 日志：`kubectl -n mini-drop logs deploy/apiserver --tail=200`

## Agent 离线

1. 看指标：`mini_drop_agents_online` 是否下降。
2. 看 API 日志中的 `agent_id`、`agent_metrics`、`offline` 审计事件。
3. 确认 drop_agent 到 drop_server 的网络、gRPC 地址、主机 PID/perf/eBPF 权限。
4. Kubernetes 中优先检查 Node 权限和 DaemonSet/hostPID 版本；本仓库的基础模板不默认部署生产 Agent。
5. Agent 恢复后确认 Agent audit 出现 `recovered`，并新建一条短任务验证下发。

## 任务积压或 Outbox 堆积

1. 看 `/metrics` 的 `mini_drop_outbox_by_status{status="pending"}` 和 `dead_letter`。
2. `/readyz` 如果 `grpc=unavailable`，先恢复 drop_server。
3. dead-letter 出现后查看 apiserver 日志中的 `error_code`、`task_id`、`attempt_id`。
4. 临时恢复方式：修复依赖后对失败任务使用重试接口，避免直接改库。

## 分析失败率升高

1. 看 `mini_drop_analysis_jobs_by_status{status="failed"}` 和 analysis 日志的 `analysis_failed`。
2. 如果错误是输入损坏，检查 RAW artifact 的 size/hash/manifest。
3. 如果 lease 频繁过期，增加 worker 资源或降低并发任务量。
4. 如果存储 5xx，先恢复 MinIO/S3，再重试对应任务。

## 存储不可用

1. `/readyz` 中 `storage=unavailable` 时，确认 MinIO/S3 服务、bucket 和凭据。
2. 检查 apiserver 日志，敏感凭据会被脱敏，只需要核对 endpoint/bucket。
3. 恢复后访问 Artifact 下载接口，确认能重新生成短期签名 URL。

## SSE 连接异常

1. 看 `mini_drop_sse_active_connections` 是否持续异常升高。
2. 检查反向代理是否关闭 buffering，Nginx 需设置 `X-Accel-Buffering: no`。
3. 客户端会退避重连并回退轮询，服务端重点排查 DB 慢查询和连接泄漏。
