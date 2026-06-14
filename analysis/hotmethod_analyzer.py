#!/usr/bin/env python3
# ============================================================
# hotmethod_analyzer.py — 分析引擎入口 小白版注释
# ============================================================
# 这个脚本被 API 后台调用，负责：
#   1. 从数据库获取任务参数
#   2. 从 MinIO/COS 下载 Agent 采集的原始数据（perf.data 等）
#   3. 调用分析工具生成火焰图、热点 TopN、优化建议
#   4. 把分析结果上传回 MinIO，更新数据库状态
#
# 用法（在终端里运行）：
#   python3 hotmethod_analyzer.py --task-id abc123 --task-type 0
#
# Python 语法小课堂：
#   def xxx():          = 定义函数
#   if __name__ == ...  = 判断是直接运行还是被 import
#   sys.exit(0)         = 正常退出（0=成功，非0=失败）
#   f"xxx {var}"        = f-string，字符串里嵌入变量
# ============================================================

import argparse  # 解析命令行参数（--task-id 这种）
import json      # JSON 格式处理
import sys       # 系统相关（退出码、标准输出）
import os        # 文件路径等


def main():
    """
    主函数：程序入口
    """
    # ---------- 1. 解析命令行参数 ----------
    parser = argparse.ArgumentParser(description="Mini-Drop 性能分析引擎")
    parser.add_argument("--task-id", required=True, help="任务 ID")
    parser.add_argument("--task-type", type=int, default=0, help="任务类型")
    parser.add_argument("--config", default="/etc/analysis/config.ini", help="配置文件路径")
    args = parser.parse_args()

    # 打印到 stderr（标准错误输出），不会干扰 stdout 的结果 JSON
    print(f"[analysis] 开始分析任务: {args.task_id}, 类型: {args.task_type}", file=sys.stderr)

    # ---------- 2. 分析流程（TODO：需要逐步实现） ----------
    # TODO: 连接 PostgreSQL，获取任务参数
    # TODO: 连接 MinIO，下载 perf.data 等原始文件
    # TODO: 根据 task_type 选择分析器：
    #       0 = CPU火焰图 → perf script + flamegraph.pl
    #       1 = Java分析 → async-profiler 解析
    #       4 = 内存泄漏 → memleak_analyzer
    # TODO: 生成火焰图 SVG、热点 JSON、建议 Markdown
    # TODO: 上传分析产物到 MinIO
    # TODO: 更新 PostgreSQL 中任务的 analysis_status

    # ---------- 3. 输出结果 ----------
    result = {
        "task_id": args.task_id,
        "status": "success",
        "outputs": [],  # 分析产物的 URL 列表
    }
    print(json.dumps(result, ensure_ascii=False))
    return 0  # 0 = 成功


# Python 的惯用写法：只有直接运行这个文件时才执行 main()
# 如果是被 import 的，则不会执行
if __name__ == "__main__":
    sys.exit(main())
