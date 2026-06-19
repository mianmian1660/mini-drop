#!/usr/bin/env python3
# ============================================================
# analysis_daemon.py — 分析守护进程
# ============================================================
# 轮询 PostgreSQL，找到 status=2(已完成) 且 analysis_status=0(待分析) 的任务，
# 调用 hotmethod_analyzer.py 进行分析，并更新 analysis_status。
#
# 用于 docker-compose 中替代直接运行 hotmethod_analyzer.py，
# 使 analysis 容器作为常驻服务运行。
# ============================================================

import os
import sys
import time
import subprocess
import argparse

# 轮询间隔（秒）
POLL_INTERVAL = 5

# 数据库连接（从环境变量或默认值）
PG_DSN = os.environ.get(
    "PG_DSN", "host=localhost user=postgres password=dev dbname=drop sslmode=disable"
)


def get_pending_tasks():
    """查询所有待分析的任务 (status=2, analysis_status=0)"""
    try:
        import psycopg2
    except ImportError:
        print("[analysis_daemon] psycopg2 未安装，尝试 pip install psycopg2-binary",
              file=sys.stderr)
        return []

    try:
        conn = psycopg2.connect(PG_DSN)
        cur = conn.cursor()
        cur.execute(
            "SELECT tid, type FROM hotmethod_tasks "
            "WHERE status = 2 AND analysis_status = 0 AND deleted_at IS NULL "
            "ORDER BY create_time ASC LIMIT 5"
        )
        rows = cur.fetchall()
        cur.close()
        conn.close()
        return rows
    except Exception as e:
        print(f"[analysis_daemon] 数据库查询失败: {e}", file=sys.stderr)
        return []


def update_analysis_status(tid: str, status: int):
    """更新任务的 analysis_status"""
    try:
        import psycopg2
        conn = psycopg2.connect(PG_DSN)
        cur = conn.cursor()
        cur.execute(
            "UPDATE hotmethod_tasks SET analysis_status = %s WHERE tid = %s",
            (status, tid),
        )
        conn.commit()
        cur.close()
        conn.close()
    except Exception as e:
        print(f"[analysis_daemon] 更新 analysis_status 失败: {e}", file=sys.stderr)


def run_analysis(tid: str, task_type: int) -> bool:
    """调用 hotmethod_analyzer.py 分析单个任务"""
    print(f"[analysis_daemon] 开始分析: tid={tid} type={task_type}", file=sys.stderr)

    # 标记为分析中
    update_analysis_status(tid, 1)

    script_dir = os.path.dirname(os.path.abspath(__file__))
    analyzer = os.path.join(script_dir, "hotmethod_analyzer.py")
    config = os.environ.get("ANALYSIS_CONFIG", os.path.join(script_dir, "config.ini"))

    cmd = [
        sys.executable, analyzer,
        "--task-id", tid,
        "--task-type", str(task_type),
    ]
    if os.path.exists(config):
        cmd.extend(["--config", config])

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        if result.returncode == 0:
            print(f"[analysis_daemon] ✅ 分析成功: tid={tid}", file=sys.stderr)
            update_analysis_status(tid, 2)  # 分析成功
            return True
        else:
            print(f"[analysis_daemon] ❌ 分析失败: tid={tid} rc={result.returncode}",
                  file=sys.stderr)
            print(f"[analysis_daemon]   stderr: {result.stderr[:500]}", file=sys.stderr)
            update_analysis_status(tid, 3)  # 分析失败
            return False
    except subprocess.TimeoutExpired:
        print(f"[analysis_daemon] ⏰ 分析超时: tid={tid}", file=sys.stderr)
        update_analysis_status(tid, 3)
        return False
    except Exception as e:
        print(f"[analysis_daemon] ❌ 分析异常: tid={tid} error={e}", file=sys.stderr)
        update_analysis_status(tid, 3)
        return False


def main():
    parser = argparse.ArgumentParser(description="Analysis Daemon - 轮询并分析已完成任务")
    parser.add_argument("--interval", type=int, default=POLL_INTERVAL,
                        help=f"轮询间隔秒数 (默认: {POLL_INTERVAL})")
    parser.add_argument("--once", action="store_true",
                        help="只运行一次，处理完所有待分析任务后退出")
    args = parser.parse_args()

    print(f"[analysis_daemon] 启动 (间隔={args.interval}s, dsn={PG_DSN[:50]}...)",
          file=sys.stderr)

    while True:
        tasks = get_pending_tasks()

        if tasks:
            print(f"[analysis_daemon] 发现 {len(tasks)} 个待分析任务", file=sys.stderr)
            for tid, task_type in tasks:
                run_analysis(tid, task_type)

        if args.once:
            print("[analysis_daemon] --once 模式，退出", file=sys.stderr)
            break

        time.sleep(args.interval)


if __name__ == "__main__":
    main()
