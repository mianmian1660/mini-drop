# ============================================================
# collapsed_data_parser.py — 折叠栈解析器 小白版注释
# ============================================================
# 这个模块解析 stackcollapse-perf.pl 输出的折叠栈格式：
#   func1;func2;func3 1234
# 计算每个函数的 self 时间、inclusive 时间、出现次数，
# 输出 TopN 热点函数 JSON
#
# 折叠栈格式说明：
#   每行格式: "函数A;函数B;函数C 采样次数"
#   分号分隔表示调用链（从左到右是从栈顶到栈底）
#   最后的数字是该调用链的采样次数（可视为 CPU 时间占比）
#
# Python 语法小课堂：
#   dict.get(key, default)  = 取字典值，不存在时返回默认值
#   sorted(d, key=..., reverse=True) = 按指定规则排序
#   str.split(sep)          = 按分隔符切分字符串
# ============================================================

import json
import sys
from typing import Dict, List, Tuple


# ----------------------------------------------------------
# 解析折叠栈文本
# ----------------------------------------------------------
def parse_collapsed(collapsed_text: str) -> Dict[str, int]:
    """
    解析折叠栈文本，统计每个函数的总采样次数（self 时间）

    输入格式:
        func1;func2;func3 1234
        func1;func4 567

    处理逻辑:
        - 每一行是一个调用链 + 采样次数
        - 栈顶（最右边的函数）获得 self 时间（采样次数）
        - 返回 {函数名: self采样次数} 的字典

    参数:
        collapsed_text: 折叠栈文本

    返回:
        {函数名: self采样次数} 字典
    """
    func_counts: Dict[str, int] = {}

    for line in collapsed_text.strip().split("\n"):
        line = line.strip()
        if not line:
            continue

        # 格式: "func1;func2;func3 count"
        # 最后一个空格后是 count
        parts = line.rsplit(" ", 1)
        if len(parts) != 2:
            continue

        stack_str, count_str = parts
        try:
            count = int(count_str)
        except ValueError:
            continue

        if count <= 0:
            continue

        # 分号分隔调用链
        functions = stack_str.split(";")

        # 栈顶（最后一个函数）获得 self 时间
        if functions:
            top_func = functions[-1].strip()
            if top_func:
                func_counts[top_func] = func_counts.get(top_func, 0) + count

    print(f"[parser] 解析完成: {len(func_counts)} 个唯一函数", file=sys.stderr)
    return func_counts


# ----------------------------------------------------------
# 计算 inclusive 时间（含子函数调用的累计时间）
# ----------------------------------------------------------
def compute_inclusive_time(collapsed_text: str) -> Dict[str, int]:
    """
    计算每个函数的 inclusive 时间（含所有子调用的累计采样次数）

    inclusive 含义:
        一个函数在整个调用链中"出现"的所有采样次数之和
        包括它自身执行 + 调用子函数期间的所有采样

    例如:
        A;B 100
        A;C 200
      → A 的 inclusive = 100+200 = 300
        B 的 inclusive = 100
        C 的 inclusive = 200

    参数:
        collapsed_text: 折叠栈文本

    返回:
        {函数名: inclusive采样次数} 字典
    """
    inclusive: Dict[str, int] = {}

    for line in collapsed_text.strip().split("\n"):
        line = line.strip()
        if not line:
            continue

        parts = line.rsplit(" ", 1)
        if len(parts) != 2:
            continue

        stack_str, count_str = parts
        try:
            count = int(count_str)
        except ValueError:
            continue

        if count <= 0:
            continue

        functions = stack_str.split(";")
        # 调用链上的每个函数都累加 inclusive 时间
        seen = set()  # 同一调用链中重复的函数只算一次
        for func in functions:
            func = func.strip()
            if func and func not in seen:
                inclusive[func] = inclusive.get(func, 0) + count
                seen.add(func)

    return inclusive


# ----------------------------------------------------------
# 获取 TopN 热点函数
# ----------------------------------------------------------
def get_top_functions(func_counts: Dict[str, int],
                      n: int = 20,
                      total_samples: int = 0) -> List[dict]:
    """
    从函数计数中取出 TopN，返回结构化的热点列表

    返回格式:
        [
            {
                "rank": 1,
                "function": "_PyEval_EvalFrameDefault",
                "samples": 2292929270,
                "percentage": 58.3
            },
            ...
        ]

    参数:
        func_counts:     {函数名: 采样次数} 字典
        n:              取前 N 个
        total_samples:  总采样次数（用于计算百分比），为 0 则自动计算

    返回:
        TopN 热点函数列表
    """
    # 按采样次数降序排列
    sorted_funcs = sorted(func_counts.items(), key=lambda x: x[1], reverse=True)

    # 自动计算总采样次数
    if total_samples <= 0:
        total_samples = sum(count for _, count in sorted_funcs)

    top_list = []
    for i, (func_name, count) in enumerate(sorted_funcs[:n], start=1):
        pct = (count / total_samples * 100) if total_samples > 0 else 0.0
        top_list.append({
            "rank": i,
            "function": func_name,
            "samples": count,
            "percentage": round(pct, 2),
        })

    print(f"[parser] Top{len(top_list)} 热点函数计算完成", file=sys.stderr)
    return top_list


# ----------------------------------------------------------
# 一站式分析：折叠栈 → TopN JSON
# ----------------------------------------------------------
def analyze_collapsed(collapsed_text: str, top_n: int = 20) -> dict:
    """
    一站式分析折叠栈：计算 self 时间、inclusive 时间、TopN

    返回:
        {
            "total_functions": 唯一函数总数,
            "total_samples": 总采样次数,
            "self_time_top": [...],       # self 时间 TopN
            "inclusive_time_top": [...]   # inclusive 时间 TopN
        }
    """
    print(f"[parser] ===== 开始折叠栈分析 =====", file=sys.stderr)

    # self 时间
    self_counts = parse_collapsed(collapsed_text)
    total_samples = sum(self_counts.values())

    # inclusive 时间
    inclusive_counts = compute_inclusive_time(collapsed_text)

    # TopN
    self_top = get_top_functions(self_counts, n=top_n, total_samples=total_samples)
    inclusive_top = get_top_functions(inclusive_counts, n=top_n, total_samples=total_samples)

    result = {
        "total_functions": len(self_counts),
        "total_samples": total_samples,
        "self_time_top": self_top,
        "inclusive_time_top": inclusive_top,
    }

    print(f"[parser] ===== 分析完毕: {len(self_counts)} 函数, "
          f"{total_samples} 采样 =====", file=sys.stderr)
    return result
