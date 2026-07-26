#!/usr/bin/env python3
# ============================================================
# java_analyzer.py — Java async-profiler 产物分析
# ============================================================

import json
import os
import re
import sys
from typing import Tuple

from analysis_advisor import generate_suggestions as advisor_generate_suggestions
from collapsed_data_parser import analyze_collapsed
from flamegraph import run_flamegraph


def detect_java_profile_format(data: bytes) -> str:
    """识别 async-profiler 常见输出格式。"""
    if data.startswith(b"FLR\x00"):
        return "jfr_binary"
    text = data[:4096].decode("utf-8", errors="ignore")
    if _looks_like_collapsed(text):
        return "collapsed"
    if "jdk.ExecutionSample" in text or "stackTrace" in text:
        return "jfr_text"
    return "unknown_text"


def normalize_java_profile(data: bytes) -> Tuple[str, str]:
    """
    将 async-profiler 产物归一化为 folded stacks。

    当前支持：
      - async-profiler collapsed 格式
      - jfr print 文本中的简化 stackTrace/function 行

    二进制 JFR 需要在采集侧输出 collapsed，或先用 jfr 工具转成文本后再进入分析。
    """
    fmt = detect_java_profile_format(data)
    if fmt == "jfr_binary":
        raise ValueError("检测到二进制 JFR；请让 async-profiler 输出 collapsed，或先用 jfr print 转文本")

    text = data.decode("utf-8", errors="replace")
    if fmt == "collapsed":
        return _normalize_collapsed(text), fmt
    if fmt == "jfr_text":
        folded = _fold_jfr_text(text)
        if folded:
            return folded, fmt

    if _looks_like_collapsed(text):
        return _normalize_collapsed(text), "collapsed"
    raise ValueError("无法识别 Java profile 格式：未找到 folded stack 或可解析的 JFR 文本")


def analyze_java_profile(data: bytes, task_name: str = "", top_n: int = 20) -> dict:
    folded, source_format = normalize_java_profile(data)
    top_json = analyze_collapsed(folded, top_n=top_n)
    top_json["language"] = "java"
    top_json["source_format"] = source_format
    top_json["task_name"] = task_name
    svg = run_flamegraph(
        folded,
        title=f"Java Flame Graph: {task_name or 'async-profiler'}",
        width=1200,
        colors="java",
    )
    return {
        "folded": folded,
        "svg": svg,
        "top_json": top_json,
        "source_format": source_format,
    }


def _looks_like_collapsed(text: str) -> bool:
    for line in text.splitlines()[:50]:
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if ";" in line and re.search(r"\s+\d+(\.\d+)?$", line):
            return True
    return False


def _normalize_collapsed(text: str) -> str:
    lines = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.rsplit(None, 1)
        if len(parts) != 2:
            continue
        stack, count = parts
        try:
            n = int(float(count))
        except ValueError:
            continue
        frames = [_clean_java_frame(f) for f in stack.split(";")]
        frames = [f for f in frames if f]
        if frames and n > 0:
            lines.append(";".join(frames) + f" {n}")
    return "\n".join(lines) + ("\n" if lines else "")


def _fold_jfr_text(text: str) -> str:
    """
    解析 jfr print 文本的保守实现。

    只依赖文本里连续出现的 Java 方法帧，把相同调用栈合并计数；不同 JDK
    的 jfr print 格式差异较大，因此这里作为 folded 格式的补充路径。
    """
    counts = {}
    current = []
    for raw in text.splitlines():
        line = raw.strip()
        match = re.search(r"([a-zA-Z_$][\w$]*(?:\.[\w$<>]+){2,})", line)
        if match:
            current.append(_clean_java_frame(match.group(1)))
            continue
        if current:
            stack = ";".join(reversed([f for f in current if f]))
            if stack:
                counts[stack] = counts.get(stack, 0) + 1
            current = []
    if current:
        stack = ";".join(reversed([f for f in current if f]))
        if stack:
            counts[stack] = counts.get(stack, 0) + 1
    return "\n".join(f"{stack} {count}" for stack, count in counts.items())


def _clean_java_frame(frame: str) -> str:
    frame = frame.strip()
    frame = re.sub(r"^\[.*?\]\s*", "", frame)
    frame = re.sub(r"\s+\(.*?\)$", "", frame)
    frame = frame.replace("/", ".")
    return frame or "[unknown]"


def generate_java_suggestions(top_json: dict, task_name: str) -> dict:
    rules_file = os.path.join(os.path.dirname(os.path.abspath(__file__)), "rules.yaml")
    try:
        return advisor_generate_suggestions(top_json, task_name=task_name, rules_file=rules_file)
    except Exception as e:
        print(f"[java_analyzer] 规则建议生成失败: {e}", file=sys.stderr)
        return {"suggestions": [], "rules_loaded": 0, "suggestions_md": ""}


def to_json_bytes(value: dict) -> bytes:
    return json.dumps(value, ensure_ascii=False, indent=2).encode("utf-8")
