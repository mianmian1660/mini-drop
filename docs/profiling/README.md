# 性能剖析后端

这个目录用于保存可选的性能剖析后端方案，主要用于把 Mini-Drop 和现有持续性能剖析系统进行对比。

- [Mini-Drop 持续性能剖析第一阶段方案：Pyroscope + Grafana Alloy](pyroscope-first-stage.md)
- [Pyroscope eBPF 性能剖析接入说明](pyroscope.md)

这些集成有意放在 Mini-Drop 主 `docker-compose.yml` 和主应用代码之外，避免第一阶段实验影响现有按需采样链路。
