# ============================================================
# data_parser/__init__.py — 数据解析器包
# ============================================================
# 这个包包含各种性能数据格式的解析器：
#   - collapsed_data_parser: 折叠栈格式（perf 输出）
#   - pprof_data_parser:    pprof profile 格式 (W5)
#   - pprof_heap_parser:    pprof heap profile 格式 (W5)
#
# 当前 W4 阶段：主要提供折叠栈解析能力
# ============================================================

# 从顶层模块导入折叠栈解析函数（保持向后兼容）
import sys
import os

# 确保可以找到顶层的 collapsed_data_parser
_top_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _top_dir not in sys.path:
    sys.path.insert(0, _top_dir)

from collapsed_data_parser import (
    parse_collapsed,
    compute_inclusive_time,
    get_top_functions,
    analyze_collapsed,
)

__all__ = [
    "parse_collapsed",
    "compute_inclusive_time",
    "get_top_functions",
    "analyze_collapsed",
]
