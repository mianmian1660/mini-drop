# ============================================================
# analysis_advisor.py — 规则建议引擎 小白版注释
# ============================================================
# 这个模块根据预置的优化规则，匹配热点函数名，生成中文优化建议。
#
# 工作原理：
#   1. 加载 rules.yaml 中的正则规则列表
#   2. 遍历 TopN 热点函数名
#   3. 用正则匹配函数名 → 命中则输出对应的优化建议
#   4. 将匹配结果格式化为 Markdown 报告 (suggestions.md)
#
# Python 语法小课堂：
#   re.match(pattern, string)  = 正则匹配（从开头匹配）
#   re.search(pattern, string) = 正则搜索（任意位置）
#   yaml.safe_load(f)          = 安全加载 YAML 文件
# ============================================================

import os
import re
import sys
from typing import List, Dict, Optional


# ----------------------------------------------------------
# 默认规则（内置，不依赖外部 yaml 文件）
# ----------------------------------------------------------
DEFAULT_RULES = [
    # --- 内存分配 ---
    {
        "regex": r".*malloc.*|.*calloc.*|.*realloc.*|.*free.*|.*mmap.*",
        "advice": "检测到频繁内存分配/释放，建议：(1) 使用 jemalloc 或 tcmalloc 替代默认 malloc；(2) 引入对象池减少分配次数；(3) 检查是否存在内存泄漏",
    },
    # --- 锁竞争 ---
    {
        "regex": r".*pthread_mutex_lock.*|.*pthread_mutex_unlock.*|.*futex.*|.*__lll_lock.*",
        "advice": "检测到锁竞争热点，建议：(1) 缩小临界区范围；(2) 使用无锁数据结构（lock-free queue/stack）；(3) 考虑读写锁替代互斥锁",
    },
    # --- Python 解释器 ---
    {
        "regex": r".*_PyEval_EvalFrameDefault.*|.*_PyEval_EvalCode.*|.*PyEval_EvalCode.*",
        "advice": "Python 解释器主循环耗时较高，建议：(1) 将热点代码改用 Cython 或 C 扩展实现；(2) 使用 PyPy 替代 CPython；(3) 减少动态属性访问，使用 __slots__",
    },
    # --- JSON 处理 ---
    {
        "regex": r".*[Jj][Ss][Oo][Nn].*|.*json.*|.*Json.*|.*JSON.*|.*_json_.*",
        "advice": "JSON 序列化/反序列化开销较大，建议：(1) 使用 orjson 或 ujson 替代标准 json 库；(2) 考虑使用 protobuf/msgpack 等二进制格式；(3) 缓存序列化结果",
    },
    # --- 加密/哈希 ---
    {
        "regex": r".*SHA.*|.*MD5.*|.*AES.*|.*[Ss][Hh][Aa].*|.*[Mm][Dd]5.*|.*[Aa][Ee][Ss].*|.*hashlib.*|.*_hashlib.*|.*EVP_.*|.*EVP_Digest.*|.*EVP_Encrypt.*",
        "advice": "加密/哈希运算占用较高 CPU，建议：(1) 检查是否可以减少加密轮次或使用更快的算法；(2) 启用硬件加速指令（AES-NI/SHA-NI）；(3) 使用连接池复用 TLS 会话",
    },
    # --- 字符串操作 ---
    {
        "regex": r".*strcmp.*|.*strcpy.*|.*strcat.*|.*strlen.*|.*memcpy.*|.*memset.*|.*memmove.*|.*sprintf.*|.*snprintf.*",
        "advice": "字符串/内存操作频繁，建议：(1) 使用编译器优化的内置版本（-O2以上）；(2) 对已知长度的字符串使用 memcpy 替代 strcpy；(3) 避免在循环内重复计算 strlen",
    },
    # --- 系统调用 ---
    {
        "regex": r".*syscall.*|.*__x64_sys_.*|.*do_syscall.*|.*entry_SYSCALL.*",
        "advice": "系统调用频率较高，建议：(1) 使用 readv/writev 批量 I/O；(2) 考虑 io_uring 替代传统 read/write；(3) 减少不必要的 stat/open 调用",
    },
    # --- 网络 I/O ---
    {
        "regex": r".*tcp_.*|.*udp_.*|.*sendmsg.*|.*recvmsg.*|.*sendto.*|.*recvfrom.*|.*tcp_sendmsg.*|.*tcp_recvmsg.*|.*__netif_.*",
        "advice": "网络 I/O 占用较高，建议：(1) 使用零拷贝技术（sendfile/splice）；(2) 启用 TCP_NODELAY 或 TCP_CORK；(3) 增大 socket buffer 减少系统调用次数",
    },
    # --- 正则表达式 ---
    {
        "regex": r".*[Rr]egex.*|.*regex.*|.*[Rr]e[Gg]exp.*|.*pcre_.*|.*re_.*",
        "advice": "正则表达式开销较大，建议：(1) 预编译正则表达式避免重复解析；(2) 检查是否存在灾难性回溯（catastrophic backtracking）；(3) 对简单匹配使用 str.find/startswith 替代",
    },
    # --- 日志 ---
    {
        "regex": r".*[Ll]og.*|.*printf.*|.*fprintf.*|.*write.*|.*syslog.*|.*__printf.*|.*vfprintf.*",
        "advice": "日志输出占用 CPU，建议：(1) 使用异步日志（spdlog/glog）；(2) 降低非关键路径的日志级别；(3) 使用采样日志代替全量日志",
    },
    # --- GC / 内存管理 ---
    {
        "regex": r".*[Gg][Cc].*|.*gc.*|.*garbage.*|.*collect.*|.*mark_sweep.*|.*_PyGC_.*|.*PyGC_.*",
        "advice": "GC/内存回收耗时较高，建议：(1) 调大 GC 触发阈值减少 GC 频率；(2) 使用对象池复用对象；(3) 检查是否有循环引用导致 GC 无法回收",
    },
    # --- 数据库 ---
    {
        "regex": r".*[Ss][Qq][Ll].*|.*mysql.*|.*postgres.*|.*sqlite.*|.*pqsql.*|.*PQexec.*|.*mysql_query.*",
        "advice": "数据库操作可能成为瓶颈，建议：(1) 添加索引优化查询；(2) 使用连接池减少连接开销；(3) 使用批量操作替代逐条操作",
    },
    # --- 序列化 ---
    {
        "regex": r".*[Pp]rotobuf.*|.*[Mm]sgpack.*|.*[Pp]ickle.*|.*[Mm]arshal.*|.*encode.*|.*decode.*|.*serialize.*|.*deserialize.*",
        "advice": "序列化/反序列化开销较大，建议：(1) 使用更快的序列化库（protobuf/msgpack）；(2) 避免重复序列化相同数据；(3) 考虑直接传递内存中的对象引用",
    },
    # --- 排序 / 查找 ---
    {
        "regex": r".*[Ss]ort.*|.*qsort.*|.*bsearch.*|.*[Bb]inary[Ss]earch.*|.*lfind.*",
        "advice": "排序/查找算法耗时较高，建议：(1) 确认数据规模是否适合当前算法；(2) 对已有序数据使用二分查找替代线性查找；(3) 考虑使用哈希表（O(1)）替代排序（O(n log n)）",
    },
    # --- 文件 I/O ---
    {
        "regex": r".*vfs_.*|.*ext4_.*|.*xfs_.*|.*btrfs_.*|.*filemap_.*|.*generic_file_.*|.*do_sys_open.*",
        "advice": "文件 I/O 占用较高，建议：(1) 使用内存映射（mmap）替代 read/write；(2) 增大 readahead 缓冲区；(3) 使用 O_DIRECT 绕过页缓存（适用于大文件顺序读）",
    },
]


# ----------------------------------------------------------
# Rule — 单条规则
# ----------------------------------------------------------
class Rule:
    """单条优化规则"""

    def __init__(self, regex: str, advice: str):
        self.regex = regex
        self.advice = advice
        self._pattern = re.compile(regex)

    def matches(self, func_name: str) -> bool:
        """检查函数名是否匹配此规则"""
        return bool(self._pattern.search(func_name))

    def __repr__(self):
        return f"Rule(regex={self.regex[:40]}...)"


# ----------------------------------------------------------
# AnalysisAdvisor — 规则建议引擎
# ----------------------------------------------------------
class AnalysisAdvisor:
    """
    规则建议引擎

    用法:
        advisor = AnalysisAdvisor()
        advisor.load_rules()                    # 加载默认规则
        suggestions = advisor.match(top_functions)  # 匹配 TopN 热点
        md = advisor.generate_markdown(suggestions, task_name)  # 生成报告
    """

    def __init__(self):
        self.rules: List[Rule] = []

    # ---------- 加载规则 ----------
    def load_rules(self, rules_file: Optional[str] = None):
        """
        加载规则，优先级：
          1. 指定的 YAML 文件
          2. 内置默认规则

        参数:
            rules_file: YAML 规则文件路径，为 None 则使用默认规则
        """
        if rules_file and os.path.exists(rules_file):
            self._load_from_yaml(rules_file)
        else:
            self._load_defaults()

        print(f"[advisor] 加载了 {len(self.rules)} 条优化规则", file=sys.stderr)

    def _load_defaults(self):
        """加载内置默认规则"""
        self.rules = [Rule(**r) for r in DEFAULT_RULES]

    def _load_from_yaml(self, filepath: str):
        """从 YAML 文件加载规则"""
        try:
            import yaml
            with open(filepath, "r", encoding="utf-8") as f:
                data = yaml.safe_load(f)
            rules_list = data.get("rules", [])
            self.rules = [Rule(**r) for r in rules_list]
            print(f"[advisor] 从 {filepath} 加载了 {len(self.rules)} 条规则",
                  file=sys.stderr)
        except ImportError:
            print("[advisor] PyYAML 未安装，使用默认规则", file=sys.stderr)
            self._load_defaults()
        except Exception as e:
            print(f"[advisor] 加载规则文件失败 ({e})，使用默认规则", file=sys.stderr)
            self._load_defaults()

    # ---------- 匹配函数 ----------
    def match(self, top_functions: List[dict]) -> List[dict]:
        """
        匹配 TopN 热点函数与规则

        参数:
            top_functions: [{"function": "xxx", "samples": N, "percentage": P}, ...]

        返回:
            [{"function": "xxx", "percentage": P, "advice": "...", "rule_regex": "..."}, ...]
            每个函数可能匹配多条规则
        """
        suggestions = []

        for func in top_functions:
            func_name = func.get("function", "")
            if not func_name:
                continue

            for rule in self.rules:
                if rule.matches(func_name):
                    suggestions.append({
                        "function": func_name,
                        "percentage": func.get("percentage", 0),
                        "samples": func.get("samples", 0),
                        "advice": rule.advice,
                        "rule_regex": rule.regex,
                    })

        # 去重（同一函数可能匹配多条规则，合并建议）
        merged = self._merge_suggestions(suggestions)

        print(f"[advisor] 匹配到 {len(merged)} 条优化建议 "
              f"(共 {len(suggestions)} 条原始匹配)",
              file=sys.stderr)
        return merged

    def _merge_suggestions(self, suggestions: List[dict]) -> List[dict]:
        """合并同一函数的多条建议"""
        merged: Dict[str, dict] = {}
        for s in suggestions:
            func_name = s["function"]
            if func_name in merged:
                # 追加建议
                merged[func_name]["advice"] += "\n\n" + s["advice"]
                merged[func_name]["rule_regex"] += " | " + s["rule_regex"]
            else:
                merged[func_name] = dict(s)
        return list(merged.values())

    # ---------- 生成 Markdown 报告 ----------
    def generate_markdown(self, suggestions: List[dict],
                          task_name: str = "",
                          top_json: dict = None) -> str:
        """
        生成优化建议 Markdown 报告 (suggestions.md)

        参数:
            suggestions: match() 的返回结果
            task_name:   任务名称
            top_json:    热点分析结果（用于生成概览）

        返回:
            Markdown 格式的报告文本
        """
        lines = []
        lines.append("# 性能优化建议报告")
        lines.append("")

        if task_name:
            lines.append(f"**任务**: {task_name}")
            lines.append("")

        lines.append(f"**生成时间**: {self._now()}")
        lines.append(f"**匹配建议数**: {len(suggestions)}")
        lines.append("")

        # ---- 概览 ----
        if top_json:
            lines.append("## 1. 热点概览")
            lines.append("")
            lines.append("| 排名 | 函数 | CPU 占比 |")
            lines.append("|------|------|----------|")
            for item in (top_json.get("self_time_top") or [])[:10]:
                lines.append(
                    f"| {item['rank']} | `{item['function']}` | {item['percentage']}% |"
                )
            lines.append("")

        # ---- 建议详情 ----
        lines.append("## 2. 优化建议详情")
        lines.append("")

        if not suggestions:
            lines.append("> ✅ 未匹配到已知优化模式，热点函数暂无预置建议。")
            lines.append("> 建议人工审查 TopN 函数，或扩展 `rules.yaml` 规则库。")
        else:
            for i, s in enumerate(suggestions, 1):
                func_name = s["function"]
                pct = s.get("percentage", 0)
                advice = s["advice"]

                lines.append(f"### 2.{i}. `{func_name}` (CPU {pct}%)")
                lines.append("")
                lines.append(advice)
                lines.append("")

        # ---- 规则库信息 ----
        lines.append("---")
        lines.append("")
        lines.append(f"*本报告由 AnalysisAdvisor 规则引擎生成，"
                     f"当前规则库包含 {len(self.rules)} 条规则。*")
        lines.append(f"*规则配置文件: rules.yaml（可自定义扩展）*")
        lines.append("")

        return "\n".join(lines)

    @staticmethod
    def _now() -> str:
        """获取当前时间字符串"""
        from datetime import datetime
        return datetime.now().strftime("%Y-%m-%d %H:%M:%S")


# ----------------------------------------------------------
# 便捷函数：一站式建议生成
# ----------------------------------------------------------
def generate_suggestions(top_json: dict,
                         task_name: str = "",
                         rules_file: str = None) -> dict:
    """
    一站式：加载规则 → 匹配函数 → 生成报告

    参数:
        top_json:   analyze_collapsed() 的输出
        task_name:  任务名称
        rules_file: 规则文件路径（可选）

    返回:
        {
            "suggestions": [...],      # 匹配到的建议列表
            "suggestions_md": "...",   # Markdown 报告
            "rules_loaded": N,         # 加载的规则数
        }
    """
    advisor = AnalysisAdvisor()
    advisor.load_rules(rules_file)

    # 用 self_time TopN 做匹配
    top_funcs = top_json.get("self_time_top", [])

    suggestions = advisor.match(top_funcs)
    md = advisor.generate_markdown(suggestions, task_name, top_json)

    return {
        "suggestions": suggestions,
        "suggestions_md": md,
        "rules_loaded": len(advisor.rules),
    }
