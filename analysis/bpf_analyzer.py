# ============================================================
# bpf_analyzer.py — eBPF 数据解析器
# ============================================================
# 解析 bpftrace 输出（直方图 + 折叠栈）生成前端可视化数据
#
# 输入格式（bpftrace hist 输出）：
#   @io_lat_us:
#   [1, 2)        42 |@@@@@
#   [2, 4)        88 |@@@@@@@@@@
#   [4, 8)       156 |@@@@@@@@@@@@@@@@
#   ...
#   # Total IO: 665
#
# 输出：JSON 格式，包含直方图 buckets + 统计信息
# ============================================================

import re
import json
import logging

logger = logging.getLogger(__name__)


def parse_bpf_histogram(text: str) -> dict:
    """
    解析 bpftrace hist() 输出。
    
    Args:
        text: bpftrace 脚本的 stdout 输出
    
    Returns:
        {
            "type": "io_latency" | "sched_latency" | "cpu",
            "buckets": [{"range": "[1, 2)", "count": 42, "bar": "@@@@@"}, ...],
            "total_events": 665,
            "unit": "us",
            "summary": {"min": 1, "max": 256, "p50": 8, "p95": 64, "p99": 128}
        }
    """
    result = {
        "type": "unknown",
        "buckets": [],
        "total_events": 0,
        "unit": "us",
        "summary": {},
    }
    
    lines = text.strip().split("\n")
    
    # 检测类型
    for line in lines:
        if "io_lat" in line or "IO Latency" in line or "blk" in line.lower():
            result["type"] = "io_latency"
            break
        if "sched_lat" in line or "Scheduler Latency" in line or "wakeup" in line.lower():
            result["type"] = "sched_latency"
            break
        if "@samples" in line or "CPU Profiler" in line:
            result["type"] = "cpu"
            break
    
    # 解析直方图行。bpftrace 不同版本可能输出:
    #   [1, 2)        42 |@@@@@
    #   [1K, 2K)       3 |@@
    #   [0]            7 |@@@@
    #   (..., 0)       1 |@
    number = r'-?\d+(?:\.\d+)?(?:[KMGTP])?'
    range_pattern = re.compile(
        rf'[\[\(]\s*({number}|-inf|\.\.\.)\s*(?:,\s*({number}|inf|\.\.\.)\s*)?[\]\)]\s+(\d+)\s*\|?(.*)',
        re.IGNORECASE,
    )
    
    buckets = []
    total_count = 0
    
    for line in lines:
        # 匹配直方图行
        m = range_pattern.match(line.strip())
        if m:
            low = parse_hist_value(m.group(1), default=0.0)
            high_raw = m.group(2)
            high = parse_hist_value(high_raw, default=low) if high_raw else low
            count = int(m.group(3))
            bar = m.group(4).strip() if m.group(4) else ""
            buckets.append({
                "range": format_range_label(m.group(1), high_raw),
                "low": low,
                "high": high,
                "count": count,
                "bar": bar,
            })
            total_count += count
        
        # 匹配 total IO 行
        total_match = re.search(r'Total IO\w*\s*:\s*(\d+)', line, re.IGNORECASE)
        if total_match:
            result["total_events"] = int(total_match.group(1))
    
    result["buckets"] = buckets
    
    # 如果没有显式 total，用 bucket 计数和
    if result["total_events"] == 0:
        result["total_events"] = total_count
    
    # 计算统计摘要
    if buckets:
        all_values = []
        for b in buckets:
            for _ in range(b["count"]):
                all_values.append((b["low"] + b["high"]) / 2)
        
        if all_values:
            all_values.sort()
            n = len(all_values)
            result["summary"] = {
                "min": buckets[0]["low"],
                "max": buckets[-1]["high"],
                "p50": all_values[n // 2],
                "p95": all_values[int(n * 0.95)] if n > 20 else all_values[-1],
                "p99": all_values[int(n * 0.99)] if n > 100 else all_values[-1],
                "total_samples": n,
            }
    
    return result


def parse_hist_value(value: str, default: float = 0.0) -> float:
    """把 bpftrace 桶边界中的 K/M/G 后缀转成数值。"""
    if value is None:
        return default
    value = value.strip()
    if value in ("", "-inf", "inf", "..."):
        return default

    multiplier = 1
    suffix = value[-1].upper()
    if suffix in {"K", "M", "G", "T", "P"}:
        value = value[:-1]
        multiplier = {
            "K": 1024,
            "M": 1024 ** 2,
            "G": 1024 ** 3,
            "T": 1024 ** 4,
            "P": 1024 ** 5,
        }[suffix]

    try:
        return float(value) * multiplier
    except ValueError:
        return default


def format_range_label(low_raw: str, high_raw: str = None) -> str:
    """保留 bpftrace 原始桶标签，便于前端和报告对照原始输出。"""
    low = (low_raw or "").strip()
    high = (high_raw or "").strip() if high_raw else ""
    if high:
        return f"[{low}, {high})"
    return f"[{low}]"


def parse_bpf_collapsed(text: str) -> dict:
    """
    解析 bpftrace CPU profiling 折叠栈输出。
    复用 collapsed_data_parser 的逻辑。
    
    Args:
        text: bpftrace -f folded 的输出
    
    Returns:
        {"type": "cpu", "top_functions": [...], "total_samples": N}
    """
    from collapsed_data_parser import analyze_collapsed
    return analyze_collapsed(text)


def analyze_bpf_output(text: str, data_type: str = "auto") -> dict:
    """
    自动检测 bpftrace 输出类型并解析。
    
    Args:
        text: bpftrace 原始输出
        data_type: "auto" | "histogram" | "collapsed"
    
    Returns:
        解析后的 dict，可直接 JSON 序列化
    """
    if data_type == "auto":
        # 自动检测
        if "@io_lat" in text or "@sched_lat" in text or "[1," in text or "[2," in text:
            data_type = "histogram"
        elif ";" in text:
            data_type = "collapsed"
        else:
            data_type = "histogram"  # 默认
    
    if data_type == "histogram":
        return parse_bpf_histogram(text)
    else:
        return parse_bpf_collapsed(text)


def bpf_histogram_to_svg(hist_data: dict, title: str = "eBPF IO Latency") -> str:
    """
    将直方图数据渲染为简单的 SVG 柱状图。
    
    Args:
        hist_data: parse_bpf_histogram 的输出
        title: 图表标题
    
    Returns:
        SVG 字符串
    """
    buckets = hist_data.get("buckets", [])
    if not buckets:
        return '<svg xmlns="http://www.w3.org/2000/svg" width="400" height="100"><text x="10" y="30">No data</text></svg>'
    
    max_count = max(b["count"] for b in buckets) if buckets else 1
    
    bar_width = 40
    bar_gap = 5
    chart_width = max(len(buckets) * (bar_width + bar_gap) + 100, 400)
    chart_height = 300
    margin_left = 80
    margin_bottom = 80
    plot_width = chart_width - margin_left - 40
    plot_height = chart_height - margin_bottom - 40
    
    svg_parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{chart_width}" height="{chart_height}">',
        f'<rect width="100%" height="100%" fill="#f5f5fa"/>',
        f'<text x="{chart_width//2}" y="25" text-anchor="middle" font-size="16" font-weight="bold" fill="#333">{title}</text>',
        f'<text x="{chart_width//2}" y="42" text-anchor="middle" font-size="11" fill="#888">Total: {hist_data.get("total_events", 0)} events | Unit: {hist_data.get("unit", "us")}</text>',
    ]
    
    # Y 轴
    y_steps = 5
    for i in range(y_steps + 1):
        y = margin_bottom + plot_height - (plot_height * i // y_steps)
        val = max_count * i // y_steps
        svg_parts.append(f'<line x1="{margin_left - 5}" y1="{y}" x2="{margin_left + plot_width}" y2="{y}" stroke="#e0e0e0" stroke-width="0.5"/>')
        svg_parts.append(f'<text x="{margin_left - 10}" y="{y + 4}" text-anchor="end" font-size="10" fill="#888">{val}</text>')
    
    # 柱状图
    for i, b in enumerate(buckets):
        x = margin_left + i * (bar_width + bar_gap)
        bar_h = int(plot_height * b["count"] / max_count) if max_count > 0 else 0
        y = margin_bottom + plot_height - bar_h
        
        # 根据延迟范围着色
        if b["high"] <= 10:
            color = "#4caf50"  # 绿色：低延迟
        elif b["high"] <= 100:
            color = "#ff9800"  # 橙色：中延迟
        else:
            color = "#f44336"  # 红色：高延迟
        
        svg_parts.append(f'<rect x="{x}" y="{y}" width="{bar_width}" height="{bar_h}" fill="{color}" rx="2">')
        svg_parts.append(f'<title>{b["range"]}: {b["count"]}</title>')
        svg_parts.append(f'</rect>')
        
        # 标签
        svg_parts.append(f'<text x="{x + bar_width//2}" y="{margin_bottom + plot_height + 15}" text-anchor="middle" font-size="9" fill="#666" transform="rotate(-30, {x + bar_width//2}, {margin_bottom + plot_height + 15})">{b["range"]}</text>')
        
        # 数值
        if bar_h > 15:
            svg_parts.append(f'<text x="{x + bar_width//2}" y="{y + bar_h//2 + 4}" text-anchor="middle" font-size="9" fill="#fff">{b["count"]}</text>')
    
    # 摘要
    summary = hist_data.get("summary", {})
    if summary:
        summary_text = f'P50={summary.get("p50", "?")}us P95={summary.get("p95", "?")}us P99={summary.get("p99", "?")}us'
        svg_parts.append(f'<text x="{chart_width//2}" y="{chart_height - 10}" text-anchor="middle" font-size="11" fill="#555">{summary_text}</text>')
    
    svg_parts.append('</svg>')
    return "\n".join(svg_parts)
