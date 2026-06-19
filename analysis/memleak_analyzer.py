# ============================================================
# memleak_analyzer.py — 内存泄漏分析器 小白版注释
# ============================================================
# 这个模块分析内存分配/释放追踪数据，识别未被释放的内存泄漏。
#
# 核心原理：
#   1. 解析 alloc/free 事件（地址配对）
#   2. alloc 没有对应 free → 泄漏
#   3. 从调用链倒序找到"第一个用户函数"作为责任人
#
# 输入格式（memtrace.txt，每行一个事件）：
#   alloc:<调用链> <地址(hex)> <size(字节)>
#   free:<调用链> <地址(hex)>
#
# 示例：
#   alloc:main;process;do_work;malloc 0x7f000100 1024
#   free:main;process;cleanup;free 0x7f000100
#   alloc:main;process;do_work;malloc 0x7f000200 2048
#   （地址 0x7f000200 没有 free → 泄漏！责任人: do_work）
#
# Python 语法小课堂：
#   int(x, 16)     = 将十六进制字符串转为整数
#   dict.get(k, v) = 取字典值，不存在返回默认值 v
#   sorted(...)    = 排序
# ============================================================

import re
import sys
from typing import Dict, List, Optional, Tuple


# ----------------------------------------------------------
# 数据结构
# ----------------------------------------------------------
class AllocEvent:
    """一次内存分配事件"""

    def __init__(self, address: int, size: int, callchain: List[str], line_no: int = 0):
        self.address = address       # 分配的内存地址（十六进制整数）
        self.size = size              # 分配大小（字节）
        self.callchain = callchain    # 调用链（从栈底到栈顶）
        self.line_no = line_no        # 在输入文件中的行号

    @property
    def top_function(self) -> str:
        """栈顶函数（实际执行分配的代码）"""
        return self.callchain[-1] if self.callchain else "unknown"

    @property
    def first_user_function(self) -> str:
        """
        "第一个用户函数" — 调用链上从栈顶往上找，
        跳过标准库函数（malloc/calloc/realloc/operator new 等），
        返回第一个非分配器函数作为责任人
        """
        allocator_funcs = {
            "malloc", "calloc", "realloc", "free",
            "__libc_malloc", "__libc_calloc", "__libc_free",
            "operator new", "operator new[]",
            "operator delete", "operator delete[]",
            "_Znwm", "_Znam", "_ZdlPv", "_ZdaPv",  # C++ mangled names
            "PyMem_Malloc", "PyMem_RawMalloc", "PyObject_Malloc",
            "g_malloc", "g_malloc0", "g_free",
        }
        # 从栈顶向下找第一个非分配器函数
        for func in reversed(self.callchain):
            func_clean = func.strip()
            if func_clean not in allocator_funcs:
                return func_clean
        return self.top_function  # 全部是分配器函数则返回栈顶

    def __repr__(self):
        return (f"AllocEvent(addr=0x{self.address:x}, size={self.size}, "
                f"top={self.top_function})")


class LeakInfo:
    """一条泄漏信息"""

    def __init__(self, alloc: AllocEvent):
        self.address = alloc.address
        self.size = alloc.size
        self.callchain = alloc.callchain
        self.responsible = alloc.first_user_function  # 责任人
        self.top_function = alloc.top_function
        self.line_no = alloc.line_no

    def to_dict(self) -> dict:
        return {
            "address": f"0x{self.address:x}",
            "size": self.size,
            "size_human": _format_bytes(self.size),
            "callchain": ";".join(self.callchain),
            "responsible_function": self.responsible,
            "top_function": self.top_function,
        }


# ----------------------------------------------------------
# 解析函数
# ----------------------------------------------------------
def parse_memtrace(text: str) -> Tuple[List[AllocEvent], List[int]]:
    """
    解析内存追踪文本，返回 (分配事件列表, 释放地址列表)

    输入格式:
        alloc:<调用链> <地址> <大小>
        free:<调用链> <地址>

    行示例:
        alloc:main;work;malloc 0x7f000100 1024
        free:main;cleanup;free 0x7f000100

    参数:
        text: 内存追踪文本

    返回:
        (allocations, free_addresses)
    """
    allocs: List[AllocEvent] = []
    free_addrs: List[int] = []

    alloc_pattern = re.compile(
        r"^alloc:(.*?)\s+(0x[0-9a-fA-F]+|[0-9]+)\s+(\d+)$"
    )
    free_pattern = re.compile(
        r"^free:(.*?)\s+(0x[0-9a-fA-F]+|[0-9]+)$"
    )

    for line_no, line in enumerate(text.strip().split("\n"), start=1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue

        # 尝试匹配 alloc
        m = alloc_pattern.match(line)
        if m:
            callchain_str = m.group(1)
            addr_str = m.group(2)
            size_str = m.group(3)

            callchain = [f.strip() for f in callchain_str.split(";") if f.strip()]
            address = int(addr_str, 16) if addr_str.startswith("0x") else int(addr_str)
            size = int(size_str)

            allocs.append(AllocEvent(address, size, callchain, line_no))
            continue

        # 尝试匹配 free
        m = free_pattern.match(line)
        if m:
            addr_str = m.group(2)
            address = int(addr_str, 16) if addr_str.startswith("0x") else int(addr_str)
            free_addrs.append(address)
            continue

        print(f"[memleak] 警告: 第{line_no}行格式无法识别: {line[:60]}",
              file=sys.stderr)

    print(f"[memleak] 解析完成: {len(allocs)} alloc, {len(free_addrs)} free",
          file=sys.stderr)
    return allocs, free_addrs


# ----------------------------------------------------------
# 泄漏检测
# ----------------------------------------------------------
def detect_leaks(allocs: List[AllocEvent],
                 free_addrs: List[int]) -> List[LeakInfo]:
    """
    检测内存泄漏：alloc 没有对应 free 的即为泄漏

    算法:
      对每个分配地址，检查 free_addrs 中是否有对应释放。
      如果有，移除一对（alloc+free）。
      剩下的 alloc 即为泄漏。

    参数:
        allocs:      分配事件列表
        free_addrs:  释放地址列表

    返回:
        泄漏信息列表（按泄漏大小降序排列）
    """
    # 构建释放地址的多重集（一个地址可能被分配多次）
    free_counts: Dict[int, int] = {}
    for addr in free_addrs:
        free_counts[addr] = free_counts.get(addr, 0) + 1

    leaks: List[LeakInfo] = []

    for alloc in allocs:
        addr = alloc.address
        if free_counts.get(addr, 0) > 0:
            # 有匹配的 free，消费一次
            free_counts[addr] -= 1
        else:
            # 没有 free → 泄漏
            leaks.append(LeakInfo(alloc))

    # 按泄漏大小降序
    leaks.sort(key=lambda l: l.size, reverse=True)

    total_leaked = sum(l.size for l in leaks)
    print(f"[memleak] 检测到 {len(leaks)} 处泄漏，"
          f"总计 {_format_bytes(total_leaked)}",
          file=sys.stderr)
    return leaks


# ----------------------------------------------------------
# 责任人分析
# ----------------------------------------------------------
def analyze_responsible(leaks: List[LeakInfo]) -> Dict[str, dict]:
    """
    按责任人函数汇总泄漏信息

    返回:
        {
            "do_work": {
                "leak_count": 3,
                "total_bytes": 10240,
                "total_human": "10.00 KB",
                "leaks": [...]  # 属于该责任人的泄漏详情
            },
            ...
        }
    """
    responsible_map: Dict[str, dict] = {}

    for leak in leaks:
        func = leak.responsible
        if func not in responsible_map:
            responsible_map[func] = {
                "function": func,
                "leak_count": 0,
                "total_bytes": 0,
                "total_human": "",
                "leaks": [],
            }
        responsible_map[func]["leak_count"] += 1
        responsible_map[func]["total_bytes"] += leak.size
        responsible_map[func]["leaks"].append(leak.to_dict())

    # 格式化
    for func_info in responsible_map.values():
        func_info["total_human"] = _format_bytes(func_info["total_bytes"])
        # 只保留前 10 条泄漏详情，避免数据过大
        if len(func_info["leaks"]) > 10:
            func_info["leaks"] = func_info["leaks"][:10]
            func_info["leaks_truncated"] = True

    # 按总泄漏量排序
    sorted_funcs = sorted(
        responsible_map.values(),
        key=lambda x: x["total_bytes"],
        reverse=True
    )

    return sorted_funcs


# ----------------------------------------------------------
# 生成 Markdown 报告
# ----------------------------------------------------------
def generate_report(leaks: List[LeakInfo],
                    responsible_list: List[dict],
                    task_name: str = "",
                    total_allocs: int = 0) -> str:
    """
    生成内存泄漏分析报告 (memleak_report.md)
    """
    total_leaked = sum(l.size for l in leaks)

    lines = []
    lines.append("# 内存泄漏分析报告")
    lines.append("")

    if task_name:
        lines.append(f"**任务**: {task_name}")
        lines.append("")

    lines.append(f"**生成时间**: {_now()}")
    lines.append(f"**总分配次数**: {total_allocs}")
    lines.append(f"**泄漏次数**: {len(leaks)}")
    lines.append(f"**泄漏总量**: {_format_bytes(total_leaked)}")
    lines.append("")

    # ---- 责任人排名 ----
    lines.append("## 1. 泄漏责任人排名")
    lines.append("")
    lines.append("| 排名 | 责任人函数 | 泄漏次数 | 泄漏总量 | 占比 |")
    lines.append("|------|-----------|----------|----------|------|")

    for i, func_info in enumerate(responsible_list[:20], start=1):
        pct = (func_info["total_bytes"] / total_leaked * 100) if total_leaked > 0 else 0
        lines.append(
            f"| {i} | `{func_info['function']}` | {func_info['leak_count']} | "
            f"{func_info['total_human']} | {pct:.1f}% |"
        )
    lines.append("")

    # ---- 泄漏详情 ----
    lines.append("## 2. 泄漏详情")
    lines.append("")

    if not leaks:
        lines.append("> ✅ 未检测到内存泄漏。")
    else:
        for i, leak in enumerate(leaks[:30], start=1):
            d = leak.to_dict()
            lines.append(f"### 2.{i}. 泄漏地址 `{d['address']}` ({d['size_human']})")
            lines.append("")
            lines.append(f"- **责任人**: `{d['responsible_function']}`")
            lines.append(f"- **分配函数**: `{d['top_function']}`")
            lines.append(f"- **调用链**: `{d['callchain']}`")
            lines.append("")

        if len(leaks) > 30:
            lines.append(f"> *... 还有 {len(leaks) - 30} 处泄漏未列出*")
            lines.append("")

    # ---- 优化建议 ----
    lines.append("## 3. 优化建议")
    lines.append("")

    if responsible_list:
        top_func = responsible_list[0]
        lines.append(
            f"最大泄漏责任人是 `{top_func['function']}`，"
            f"泄漏 {top_func['leak_count']} 次共 {top_func['total_human']}。"
        )
    lines.append("")
    lines.append("通用内存泄漏排查建议：")
    lines.append("1. 检查责任人函数中是否有 **提前 return / 异常路径** 遗漏了 free/delete")
    lines.append("2. 使用 **RAII**（智能指针 `unique_ptr/shared_ptr`）自动管理内存")
    lines.append("3. 对 C 代码使用 `__attribute__((cleanup))` 或 `goto cleanup` 模式")
    lines.append("4. 用 `valgrind --leak-check=full` 或 `AddressSanitizer` 验证修复")
    lines.append("")

    lines.append("---")
    lines.append(f"*本报告由 memleak_analyzer 生成*")
    lines.append("")

    return "\n".join(lines)


# ----------------------------------------------------------
# 一站式分析入口
# ----------------------------------------------------------
def analyze_memtrace(memtrace_text: str,
                     task_name: str = "") -> dict:
    """
    一站式内存泄漏分析

    参数:
        memtrace_text: 内存追踪文本
        task_name:     任务名称

    返回:
        {
            "total_allocs": N,
            "total_frees": N,
            "leak_count": N,
            "total_leaked_bytes": N,
            "total_leaked_human": "10.00 KB",
            "responsible_top": [...],   # 责任人排名
            "leaks": [...],             # 所有泄漏详情
            "report_md": "...",         # Markdown 报告
        }
    """
    print(f"[memleak] ===== 开始内存泄漏分析 =====", file=sys.stderr)

    # 1. 解析
    allocs, free_addrs = parse_memtrace(memtrace_text)
    total_allocs = len(allocs)

    # 2. 检测泄漏
    leaks = detect_leaks(allocs, free_addrs)

    # 3. 责任人分析
    responsible_list = analyze_responsible(leaks)

    # 4. 生成报告
    report_md = generate_report(leaks, responsible_list, task_name, total_allocs)

    result = {
        "total_allocs": total_allocs,
        "total_frees": len(free_addrs),
        "leak_count": len(leaks),
        "total_leaked_bytes": sum(l.size for l in leaks),
        "total_leaked_human": _format_bytes(sum(l.size for l in leaks)),
        "responsible_top": [
            {
                "function": r["function"],
                "leak_count": r["leak_count"],
                "total_bytes": r["total_bytes"],
                "total_human": r["total_human"],
            }
            for r in responsible_list[:10]
        ],
        "leaks": [l.to_dict() for l in leaks[:50]],
        "report_md": report_md,
    }

    print(f"[memleak] ===== 分析完毕: {len(leaks)} 处泄漏 =====", file=sys.stderr)
    return result


# ----------------------------------------------------------
# 通用泄漏检测（不依赖特定格式，直接对 alloc/free 配对）
# ----------------------------------------------------------
def detect_leaks_generic(alloc_records: List[dict],
                         free_records: List[dict]) -> dict:
    """
    通用泄漏检测：接受已解析的 alloc/free 记录列表

    参数:
        alloc_records: [{"address": int, "size": int, "function": str}, ...]
        free_records:  [{"address": int, "function": str}, ...]

    返回:
        与 analyze_memtrace 相同格式的结果
    """
    allocs = [
        AllocEvent(r["address"], r["size"],
                   [r.get("function", "unknown")])
        for r in alloc_records
    ]
    free_addrs = [r["address"] for r in free_records]

    leaks = detect_leaks(allocs, free_addrs)
    responsible_list = analyze_responsible(leaks)

    report_md = generate_report(leaks, responsible_list,
                                task_name="", total_allocs=len(allocs))

    return {
        "total_allocs": len(allocs),
        "total_frees": len(free_addrs),
        "leak_count": len(leaks),
        "total_leaked_bytes": sum(l.size for l in leaks),
        "total_leaked_human": _format_bytes(sum(l.size for l in leaks)),
        "responsible_top": [
            {
                "function": r["function"],
                "leak_count": r["leak_count"],
                "total_bytes": r["total_bytes"],
                "total_human": r["total_human"],
            }
            for r in responsible_list[:10]
        ],
        "leaks": [l.to_dict() for l in leaks[:50]],
        "report_md": report_md,
    }


# ----------------------------------------------------------
# 工具函数
# ----------------------------------------------------------
def _format_bytes(size: int) -> str:
    """将字节数格式化为人类可读格式"""
    if size < 1024:
        return f"{size} B"
    elif size < 1024 * 1024:
        return f"{size / 1024:.2f} KB"
    elif size < 1024 * 1024 * 1024:
        return f"{size / (1024 * 1024):.2f} MB"
    else:
        return f"{size / (1024 * 1024 * 1024):.2f} GB"


def _now() -> str:
    """获取当前时间字符串"""
    from datetime import datetime
    return datetime.now().strftime("%Y-%m-%d %H:%M:%S")


# ----------------------------------------------------------
# 生成模拟内存追踪数据（测试用）
# ----------------------------------------------------------
def generate_mock_memtrace() -> str:
    """
    生成模拟的内存追踪数据，包含故意制造的泄漏
    用于无真实数据时测试分析流程
    """
    lines = [
        "# 模拟内存追踪数据 — 包含故意泄漏",
        "# 格式: alloc:<调用链> <地址> <大小>",
        "#       free:<调用链> <地址>",
        "",
        "# --- 正常分配释放（无泄漏）---",
        "alloc:main;init;parse_config;malloc 0x00001 512",
        "free:main;cleanup;free 0x00001",
        "alloc:main;process_request;handle_json;malloc 0x00002 4096",
        "free:main;process_request;handle_json;free 0x00002",
        "alloc:main;process_request;db_query;malloc 0x00003 8192",
        "free:main;process_request;db_query;free 0x00003",
        "",
        "# --- 泄漏 1: 处理请求后忘了释放临时缓冲区 ---",
        "alloc:main;process_request;compute_hash;malloc 0x00100 65536",
        "# （没有 free！模拟忘记释放）",
        "",
        "# --- 泄漏 2: 循环内分配，循环外未释放 ---",
        "alloc:main;event_loop;handle_event;malloc 0x00200 128",
        "alloc:main;event_loop;handle_event;malloc 0x00201 256",
        "alloc:main;event_loop;handle_event;malloc 0x00202 512",
        "# （3 次分配都没有 free）",
        "",
        "# --- 泄漏 3: 大对象分配后异常路径遗漏 free ---",
        "alloc:main;load_dataset;parse_csv;malloc 0x00300 1048576",
        "# （1MB 分配后遇到错误 return，没有 free）",
        "",
        "# --- 正常: 小对象及时释放 ---",
        "alloc:main;helper;format_string;malloc 0x00004 64",
        "free:main;helper;format_string;free 0x00004",
        "alloc:main;helper;format_string;malloc 0x00005 128",
        "free:main;helper;format_string;free 0x00005",
        "",
        "# --- 泄漏 4: 线程本地缓存未清理 ---",
        "alloc:main;thread_worker;cache_data;malloc 0x00400 2048",
        "alloc:main;thread_worker;cache_data;malloc 0x00401 4096",
        "# （线程退出时未清理缓存）",
        "",
        "# --- 正常: 批量操作后释放 ---",
        "alloc:main;batch_process;allocate_buffer;malloc 0x00006 262144",
        "free:main;batch_process;free_buffer;free 0x00006",
    ]
    return "\n".join(lines)
