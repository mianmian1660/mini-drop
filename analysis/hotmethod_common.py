# ============================================================
# hotmethod_common.py — 分析公共库 小白版注释
# ============================================================
# 这个模块是 analysis 仓库的统一分析函数库，整合了：
#   - 折叠栈解析（collapsed_data_parser）
#   - 火焰图生成（flamegraph）
#   - 规则建议（analysis_advisor）
#
# 外部使用者（如 hotmethod_analyzer.py）只需 import 这个模块，
# 即可获得所有分析能力。
#
# Python 语法小课堂：
#   from .xxx import *  = 从子模块导入所有公开符号
#   __all__ = [...]      = 控制 from module import * 的行为
# ============================================================

# ----------------------------------------------------------
# 从各子模块导入核心分析函数
# ----------------------------------------------------------

# 折叠栈解析
from collapsed_data_parser import (
    parse_collapsed,
    compute_inclusive_time,
    get_top_functions,
    analyze_collapsed,
)

# 火焰图生成
from flamegraph import (
    generate_flamegraph,
    get_folded_stacks,
    run_perf_script,
    run_stackcollapse,
    run_flamegraph,
)

# 规则建议引擎
from analysis_advisor import (
    AnalysisAdvisor,
    generate_suggestions,
)

# 存储
from storage import (
    Storage,
    MinIOStorage,
    create_storage,
    FileInfo,
)

# 错误处理
from error import (
    ErrorCode,
    ErrorInfo,
    exit_ok,
    exit_error,
)

# eBPF 分析
from bpf_analyzer import (
    parse_bpf_histogram,
    parse_bpf_collapsed,
    analyze_bpf_output,
    bpf_histogram_to_svg,
)


# ----------------------------------------------------------
# 额外工具函数
# ----------------------------------------------------------

def format_percentage(value: float, total: float) -> str:
    """
    格式化百分比字符串

    用法:
        format_percentage(33.3, 100) → "33.30%"
        format_percentage(0, 100) → "0.00%"
    """
    if total == 0:
        return "0.00%"
    return f"{value / total * 100:.2f}%"


def summarize_top_functions(top_list: list, top_n: int = 5) -> str:
    """
    将 TopN 热点函数列表格式化为可读的文本摘要

    用法:
        summary = summarize_top_functions(top_json["self_time_top"])
        print(summary)
        # 输出:
        #   Top 5 热点函数:
        #     1. _PyEval_EvalFrameDefault (58.10%)
        #     2. [python3.10] (26.99%)
        #     ...
    """
    lines = [f"Top {min(len(top_list), top_n)} 热点函数:"]
    for item in top_list[:top_n]:
        lines.append(f"  {item['rank']}. {item['function']} ({item['percentage']}%)")
    return "\n".join(lines)


def is_analysis_complete(task_record: dict) -> bool:
    """
    判断任务分析是否已完成

    参数:
        task_record: 数据库中的任务记录（含 analysis_status 字段）

    返回:
        True 如果 analysis_status >= 2（成功或失败）
    """
    status = task_record.get("analysis_status", 0)
    return status >= 2


def get_analysis_status_text(status: int) -> str:
    """
    将 analysis_status 数值转为可读文本

    0 → "待分析"
    1 → "分析中"
    2 → "分析完成"
    3 → "分析失败"
    """
    status_map = {
        0: "待分析",
        1: "分析中",
        2: "分析完成",
        3: "分析失败",
    }
    return status_map.get(status, f"未知状态({status})")


# ----------------------------------------------------------
# 模块导出控制
# ----------------------------------------------------------
__all__ = [
    # 折叠栈
    "parse_collapsed",
    "compute_inclusive_time",
    "get_top_functions",
    "analyze_collapsed",
    # 火焰图
    "generate_flamegraph",
    "get_folded_stacks",
    # 建议引擎
    "AnalysisAdvisor",
    "generate_suggestions",
    # 存储
    "Storage",
    "MinIOStorage",
    "create_storage",
    "FileInfo",
    # 错误
    "ErrorCode",
    "ErrorInfo",
    "exit_ok",
    "exit_error",
    # 工具
    "format_percentage",
    "summarize_top_functions",
    "is_analysis_complete",
    "get_analysis_status_text",
]
