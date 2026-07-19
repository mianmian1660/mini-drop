# Pyroscope eBPF profiling

This directory adds a sidecar-style Pyroscope setup for Mini-Drop experiments. It does not change the existing Mini-Drop task path:

```text
Web -> apiserver -> drop_server -> drop_agent -> MinIO -> analysis -> Web
```

Instead, Grafana Alloy runs as a privileged eBPF profiler, discovers local Docker containers, and sends continuous CPU profiles to a standalone Pyroscope backend.

## Files

```text
deploy/profiling/pyroscope/docker-compose.yml
deploy/profiling/pyroscope/alloy.config
scripts/profiling/pyroscope_check.sh
docs/profiling/pyroscope.md
```

## Start Mini-Drop

Run the normal project first if you want Pyroscope to see Mini-Drop containers:

```bash
docker compose up -d --build
make demo
```

## Start Pyroscope

Start the separate Pyroscope stack:

```bash
docker compose -f deploy/profiling/pyroscope/docker-compose.yml up -d
```

Open:

```text
Pyroscope UI: http://localhost:4040
Alloy UI:     http://localhost:12345
```

In Pyroscope, select profile type `process_cpu`, then filter by labels such as `container`, `service_name`, `compose_project`, `project`, or `profiler`.

## Verify

```bash
bash scripts/profiling/pyroscope_check.sh
```

Pyroscope can return temporary `503` responses while its internal metastore, ingester, and segment writer finish their startup grace periods. The check script retries for this reason.

The script also prints the profile series currently visible to Pyroscope. This is the most direct way to confirm whether the backend is only showing Pyroscope's own self-profiles or whether Alloy eBPF profiles from Mini-Drop containers are arriving too.

If profiles do not appear immediately, keep Mini-Drop busy for a short window:

```bash
make demo
```

You can also create CPU load in another terminal and wait 30-60 seconds before refreshing Pyroscope.

## Stop

```bash
docker compose -f deploy/profiling/pyroscope/docker-compose.yml down
```

To remove stored Pyroscope data too:

```bash
docker compose -f deploy/profiling/pyroscope/docker-compose.yml down -v
```

## Notes

- Pyroscope itself listens on port `4040`.
- Alloy listens on port `12345`.
- Alloy must run as root, in the host PID namespace, and with privileged permissions for `pyroscope.ebpf`.
- The eBPF profiler requires Linux kernel `4.9+`.
- The setup uses Docker discovery, so `/var/run/docker.sock` is mounted read-only.
- Symbol cache data is stored in the `alloy-symb-cache` Docker volume at `/tmp/symb-cache`.

## Relationship with Mini-Drop

Use this as the Pyroscope comparison branch:

- Mini-Drop remains the on-demand profiling platform.
- Pyroscope provides continuous profiling over the same running containers.
- Final comparison can focus on deployment complexity, eBPF permissions, UI, label model, resource overhead, and whether each approach fits on-demand diagnosis or long-running observability better.

## References

- Grafana Pyroscope get started: https://grafana.com/docs/pyroscope/latest/get-started/
- Pyroscope eBPF on Docker with Grafana Alloy: https://grafana.com/docs/pyroscope/latest/configure-client/grafana-alloy/ebpf/setup-docker/
- Alloy `pyroscope.ebpf` component: https://grafana.com/docs/alloy/latest/reference/components/pyroscope/pyroscope.ebpf/
