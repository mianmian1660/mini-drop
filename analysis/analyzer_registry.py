#!/usr/bin/env python3
# ============================================================
# analyzer_registry.py — task_type 到分析器的注册表
# ============================================================

from typing import Callable, Dict

AnalyzerFunc = Callable[..., dict]


class AnalyzerRegistry:
    """维护 task_type -> analyzer 函数的映射。"""

    def __init__(self):
        self._analyzers: Dict[int, AnalyzerFunc] = {}

    def register(self, task_type: int, analyzer: AnalyzerFunc):
        self._analyzers[int(task_type)] = analyzer

    def get(self, task_type: int):
        return self._analyzers.get(int(task_type))

    def require(self, task_type: int) -> AnalyzerFunc:
        analyzer = self.get(task_type)
        if analyzer is None:
            known = ", ".join(str(k) for k in sorted(self._analyzers))
            raise KeyError(f"未注册 task_type={task_type} 的分析器，已注册: {known}")
        return analyzer

    def task_types(self):
        return sorted(self._analyzers)


def build_default_registry() -> AnalyzerRegistry:
    """
    构造当前项目默认注册表。

    这里复用 hotmethod_analyzer.run_analysis_for_type，先把 B1 的领取/调度链路
    和已有分析能力接上；后续 B2/B3 可以把 Java/归因等具体分析器逐步拆出。
    """
    import hotmethod_analyzer as hm

    registry = AnalyzerRegistry()

    def make_runner(task_type: int):
        def run(conn, storage_cfg, task, bucket, tid, local_dir=""):
            return hm.run_analysis_for_type(
                conn, storage_cfg, task, bucket, tid, task_type, local_dir=local_dir
            )

        return run

    for task_type in (
        hm.TASK_TYPE_GENERIC,
        hm.TASK_TYPE_JAVA,
        hm.TASK_TYPE_TRACING,
        hm.TASK_TYPE_MEMCHECK,
        hm.TASK_TYPE_BPF,
        hm.TASK_TYPE_JAVA_HEAP,
    ):
        registry.register(task_type, make_runner(task_type))

    return registry
