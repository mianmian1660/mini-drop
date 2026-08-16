#!/usr/bin/env python3
# ============================================================
# analyzer_registry.py — task_type 到分析器的注册表
# ============================================================

from typing import Dict

from analyzer_contract import InputSpec, LegacyFunctionAnalyzer, OutputSpec


class AnalyzerRegistry:
    """维护 task_type -> analyzer 实例的映射。"""

    def __init__(self):
        self._analyzers: Dict[int, object] = {}

    def register(self, task_type: int, analyzer):
        self._analyzers[int(task_type)] = analyzer

    def get(self, task_type: int):
        return self._analyzers.get(int(task_type))

    def require(self, task_type: int):
        analyzer = self.get(task_type)
        if analyzer is None:
            known = ", ".join(str(k) for k in sorted(self._analyzers))
            raise KeyError(f"未注册 task_type={task_type} 的分析器，已注册: {known}")
        return analyzer

    def task_types(self):
        return sorted(self._analyzers)


def collector_declarations():
    """
    阶段 6 扩展采样器声明。

    enabled=False 表示契约、能力名和目标分析流水线已经固定，但 Agent Runner
    尚未接入，API/Web 不应默认展示或允许创建这些采样任务。
    """
    return [
        {"task_kind": "go_pprof", "pipeline": "pprof", "capabilities": ["pprof"], "enabled": True},
        {"task_kind": "go_pprof_heap", "pipeline": "pprof_heap", "capabilities": ["pprof"], "enabled": True},
        {"task_kind": "async_profiler_java", "pipeline": "java_async_profiler", "capabilities": ["async-profiler", "java"], "enabled": True},
        {"task_kind": "java_heap", "pipeline": "java_heap", "capabilities": ["java", "java-heap"], "enabled": False},
        {"task_kind": "python_py_spy", "pipeline": "perf_flamegraph", "capabilities": ["py-spy", "python"], "enabled": False},
        {"task_kind": "python_memray", "pipeline": "memleak", "capabilities": ["memray", "python"], "enabled": False},
        {"task_kind": "gperftools_cpu", "pipeline": "perf_flamegraph", "capabilities": ["gperftools"], "enabled": False},
        {"task_kind": "bolt_profile", "pipeline": "perf_flamegraph", "capabilities": ["bolt"], "enabled": False},
        {"task_kind": "script_diagnostic", "pipeline": "script_diagnostic", "capabilities": ["restricted-script"], "enabled": False},
    ]


def build_default_registry() -> AnalyzerRegistry:
    """
    构造当前项目默认注册表。

    这里复用 hotmethod_analyzer.run_analysis_for_type，先把 B1 的领取/调度链路
    和已有分析能力接上；后续 B2/B3 可以把 Java/归因等具体分析器逐步拆出。
    """
    import hotmethod_analyzer as hm

    registry = AnalyzerRegistry()

    def make_legacy_func(task_type: int):
        def run(conn, storage_cfg, task, bucket, tid, local_dir=""):
            return hm.run_analysis_for_type(
                conn, storage_cfg, task, bucket, tid, task_type, local_dir=local_dir
            )

        return run

    registry.register(
        hm.TASK_TYPE_GENERIC,
        LegacyFunctionAnalyzer(
            name="perf_flamegraph",
            pipeline="perf_flamegraph",
            input_spec=InputSpec("perf.data", ["perf"], max_size=1024 * 1024 * 1024),
            output_specs=[
                OutputSpec("flamegraph.svg", "RESULT", "image/svg+xml"),
                OutputSpec("top.json", "RESULT", "application/json"),
                OutputSpec("suggestions.json", "RESULT", "application/json"),
            ],
            analyze_func=make_legacy_func(hm.TASK_TYPE_GENERIC),
            timeout_seconds=600,
        ),
    )
    registry.register(
        hm.TASK_TYPE_JAVA,
        LegacyFunctionAnalyzer(
            name="java_async_profiler",
            pipeline="java_async_profiler",
            input_spec=InputSpec("perf.data", ["java"], max_size=512 * 1024 * 1024),
            output_specs=[
                OutputSpec("java_flamegraph.svg", "RESULT", "image/svg+xml"),
                OutputSpec("java_top.json", "RESULT", "application/json"),
            ],
            analyze_func=make_legacy_func(hm.TASK_TYPE_JAVA),
            timeout_seconds=600,
        ),
    )
    registry.register(
        hm.TASK_TYPE_TRACING,
        LegacyFunctionAnalyzer(
            name="go_pprof",
            pipeline="pprof",
            input_spec=InputSpec("perf.data", ["pprof"], max_size=256 * 1024 * 1024),
            output_specs=[
                OutputSpec("flamegraph.svg", "RESULT", "image/svg+xml"),
                OutputSpec("top.json", "RESULT", "application/json"),
            ],
            analyze_func=make_legacy_func(hm.TASK_TYPE_TRACING),
            timeout_seconds=300,
        ),
    )
    registry.register(
        hm.TASK_TYPE_MEMCHECK,
        LegacyFunctionAnalyzer(
            name="memleak",
            pipeline="memleak",
            input_spec=InputSpec("memtrace.txt", ["memtrace"], max_size=128 * 1024 * 1024),
            output_specs=[
                OutputSpec("memleak_report.md", "RESULT", "text/markdown"),
                OutputSpec("memleak.json", "RESULT", "application/json"),
            ],
            analyze_func=make_legacy_func(hm.TASK_TYPE_MEMCHECK),
            timeout_seconds=300,
        ),
    )
    registry.register(
        hm.TASK_TYPE_BPF,
        LegacyFunctionAnalyzer(
            name="ebpf",
            pipeline="bpf_histogram",
            input_spec=InputSpec("raw.bpf", ["bpf"], max_size=256 * 1024 * 1024),
            output_specs=[
                OutputSpec("bpf_histogram.svg", "RESULT", "image/svg+xml"),
                OutputSpec("bpf_data.json", "RESULT", "application/json"),
            ],
            analyze_func=make_legacy_func(hm.TASK_TYPE_BPF),
            timeout_seconds=300,
        ),
    )
    registry.register(
        hm.TASK_TYPE_JAVA_HEAP,
        LegacyFunctionAnalyzer(
            name="java_heap_declared",
            pipeline="java_heap",
            input_spec=InputSpec("heapdump.hprof", ["java"], max_size=2 * 1024 * 1024 * 1024),
            output_specs=[OutputSpec("heap_summary.json", "RESULT", "application/json")],
            analyze_func=make_legacy_func(hm.TASK_TYPE_JAVA_HEAP),
            timeout_seconds=900,
        ),
    )

    return registry
