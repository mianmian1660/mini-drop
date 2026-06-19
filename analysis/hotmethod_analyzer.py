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
# 退出码约定（与 apiserver 的契约）：
#   0   = 全部成功
#   非0 = 失败，stderr 中有 ErrorInfo JSON
#
# Python 语法小课堂：
#   def xxx():          = 定义函数
#   if __name__ == ...  = 判断是直接运行还是被 import
#   sys.exit(0)         = 正常退出（0=成功，非0=失败）
#   f"xxx {var}"        = f-string，字符串里嵌入变量
# ============================================================

import argparse
import configparser
import json
import os
import sys
import traceback

# 引入同目录下的模块
from storage import MinIOStorage, create_storage
from error import ErrorCode, ErrorInfo, exit_ok, exit_error


# ============================================================
# 任务类型常量（与 drop/hotmethod.proto 保持一致）
# ============================================================
TASK_TYPE_GENERIC   = 0   # 通用 CPU 采样
TASK_TYPE_JAVA      = 1   # Java 分析
TASK_TYPE_TRACING   = 2   # Tracing
TASK_TYPE_MEMCHECK  = 4   # 内存泄漏
TASK_TYPE_JAVA_HEAP = 6   # Java 堆 dump


def load_config(config_path: str) -> dict:
    """
    加载配置文件（ini 格式）并支持环境变量覆盖

    配置文件格式示例：
        [database]
        dsn = host=localhost user=postgres password=dev dbname=drop sslmode=disable

        [storage]
        endpoint = localhost:9000
        access_key = drop
        secret_key = dropdrop
        use_ssl = false
        bucket = drop-data

    环境变量可覆盖（优先级更高）：
        PG_DSN       → database.dsn
        S3_ENDPOINT  → storage.endpoint
        S3_ACCESS_KEY → storage.access_key
        S3_SECRET_KEY → storage.secret_key
        S3_BUCKET     → storage.bucket
    """
    config = {
        "database": {
            "dsn": "host=localhost user=postgres password=dev dbname=drop sslmode=disable",
        },
        "storage": {
            "endpoint": "localhost:9000",
            "access_key": "drop",
            "secret_key": "dropdrop",
            "use_ssl": "false",
            "bucket": "drop-data",
        },
    }

    # 尝试读取配置文件
    if os.path.exists(config_path):
        print(f"[analysis] 加载配置文件: {config_path}", file=sys.stderr)
        cp = configparser.ConfigParser()
        cp.read(config_path, encoding="utf-8")

        # 读取 [database]
        if cp.has_section("database"):
            for key in ["dsn"]:
                if cp.has_option("database", key):
                    config["database"][key] = cp.get("database", key)

        # 读取 [storage]
        if cp.has_section("storage"):
            for key in ["endpoint", "access_key", "secret_key", "use_ssl", "bucket"]:
                if cp.has_option("storage", key):
                    config["storage"][key] = cp.get("storage", key)
    else:
        print(f"[analysis] 配置文件不存在，使用默认值: {config_path}", file=sys.stderr)

    # 环境变量覆盖
    if os.environ.get("PG_DSN"):
        config["database"]["dsn"] = os.environ["PG_DSN"]
    if os.environ.get("S3_ENDPOINT"):
        config["storage"]["endpoint"] = os.environ["S3_ENDPOINT"]
    if os.environ.get("S3_ACCESS_KEY"):
        config["storage"]["access_key"] = os.environ["S3_ACCESS_KEY"]
    if os.environ.get("S3_SECRET_KEY"):
        config["storage"]["secret_key"] = os.environ["S3_SECRET_KEY"]
    if os.environ.get("S3_BUCKET"):
        config["storage"]["bucket"] = os.environ["S3_BUCKET"]

    # use_ssl 转 bool
    use_ssl_val = config["storage"].get("use_ssl", "false")
    config["storage"]["use_ssl"] = use_ssl_val.lower() in ("true", "1", "yes")

    return config


def connect_db(dsn: str):
    """
    连接 PostgreSQL 数据库

    返回: psycopg2 connection 对象
    失败则调用 exit_error 退出
    """
    try:
        import psycopg2
        conn = psycopg2.connect(dsn)
        print(f"[analysis] PostgreSQL 连接成功", file=sys.stderr)
        return conn
    except Exception as e:
        exit_error(ErrorCode.ERR_DB_CONNECT,
                   f"数据库连接失败: {e}",
                   traceback.format_exc())


def get_task(conn, tid: str) -> dict:
    """
    从数据库获取任务详情

    返回: 任务参数字典（包含 type, profiler_type, target_ip, request_params 等）
    失败则调用 exit_error 退出
    """
    try:
        import psycopg2.extras
        cur = conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor)
        cur.execute(
            "SELECT tid, name, type, profiler_type, target_ip, "
            "request_params, status, analysis_status "
            "FROM hotmethod_tasks WHERE tid = %s AND deleted_at IS NULL",
            (tid,)
        )
        row = cur.fetchone()
        cur.close()

        if row is None:
            exit_error(ErrorCode.ERR_TASK_NOT_FOUND,
                       f"任务不存在: {tid}")

        # RealDictCursor 返回的就是 dict，但 request_params 是 JSONB 需要解析
        task = dict(row)
        if task.get("request_params"):
            if isinstance(task["request_params"], str):
                task["request_params"] = json.loads(task["request_params"])

        print(f"[analysis] 任务详情: name={task.get('name')}, "
              f"type={task.get('type')}, profiler_type={task.get('profiler_type')}, "
              f"target_ip={task.get('target_ip')}",
              file=sys.stderr)
        return task

    except SystemExit:
        raise  # 向上传递 exit_error 的 sys.exit
    except Exception as e:
        exit_error(ErrorCode.ERR_DB_QUERY,
                   f"查询任务失败: {e}",
                   traceback.format_exc())


def update_analysis_status(conn, tid: str, status: int, status_info: str = ""):
    """
    更新任务的 analysis_status 字段

    参数：
        conn:       数据库连接
        tid:        任务 ID
        status:     新状态 (1=分析中, 2=成功, 3=失败)
        status_info:状态备注
    """
    try:
        cur = conn.cursor()
        if status_info:
            cur.execute(
                "UPDATE hotmethod_tasks SET analysis_status = %s, "
                "status_info = CASE WHEN status_info = '' THEN %s ELSE status_info || '; ' || %s END "
                "WHERE tid = %s",
                (status, status_info, status_info, tid)
            )
        else:
            cur.execute(
                "UPDATE hotmethod_tasks SET analysis_status = %s WHERE tid = %s",
                (status, tid)
            )
        conn.commit()
        cur.close()
        print(f"[analysis] 更新 analysis_status={status} (tid={tid})", file=sys.stderr)
    except Exception as e:
        print(f"[analysis] 更新 analysis_status 失败: {e}", file=sys.stderr)
        # 不退出，上传/分析的结果比状态更新更重要


def insert_suggestion(conn, tid: str, func_name: str,
                      suggestion: str, ai_suggestion: str = ""):
    """
    往 analysis_suggestion 表插入一条分析建议
    """
    try:
        cur = conn.cursor()
        cur.execute(
            "INSERT INTO analysis_suggestions (tid, func, suggestion, ai_suggestion, status) "
            "VALUES (%s, %s, %s, %s, 0)",
            (tid, func_name, suggestion, ai_suggestion)
        )
        conn.commit()
        cur.close()
        print(f"[analysis] 插入建议: {func_name}", file=sys.stderr)
    except Exception as e:
        print(f"[analysis] 插入建议失败: {e}", file=sys.stderr)


def run_analysis_for_type(conn, storage_cfg: dict, task: dict,
                          bucket: str, tid: str, task_type: int) -> list:
    """
    根据任务类型执行对应的分析逻辑

    当前 W1 阶段：仅验证文件存在，实际分析留到 W2 实现
    返回: 产物 URL 列表（目前为空）
    """
    outputs = []

    # ----- 连接 MinIO 并列出该任务的文件 -----
    try:
        storage = MinIOStorage(
            endpoint=storage_cfg["endpoint"],
            access_key=storage_cfg["access_key"],
            secret_key=storage_cfg["secret_key"],
            use_ssl=storage_cfg["use_ssl"],
        )

        # 确保 bucket 存在
        if not storage.ensure_bucket(bucket):
            print(f"[analysis] 警告: 无法访问 Bucket '{bucket}'，"
                  f"MinIO 可能未启动，跳过存储操作",
                  file=sys.stderr)
        else:
            # 列出该 tid 下的所有文件
            files = storage.list_objects(bucket, prefix=f"{tid}/")
            if not files:
                print(f"[analysis] 警告: 任务 {tid} 在 MinIO 上没有文件，"
                      f"请确认 Agent 已上传采集数据",
                      file=sys.stderr)
            else:
                print(f"[analysis] 找到 {len(files)} 个文件:", file=sys.stderr)
                for f in files:
                    print(f"[analysis]   - {f.name} ({f.size} bytes)", file=sys.stderr)

    except Exception as e:
        # MinIO 不可用时不致命，W1 阶段允许跳过
        print(f"[analysis] 警告: MinIO 连接失败 ({e})，跳过存储操作",
              file=sys.stderr)

    # ---------- 按 task_type 分发分析 ----------
    try:
        if task_type == TASK_TYPE_GENERIC:
            # W2 将实现: perf script → stackcollapse → flamegraph.pl → SVG
            print(f"[analysis] CPU 火焰图分析 (W2 实现)", file=sys.stderr)

        elif task_type == TASK_TYPE_JAVA:
            # W5 将实现: async-profiler 折叠栈解析
            print(f"[analysis] Java 分析 (W5 实现)", file=sys.stderr)

        elif task_type == TASK_TYPE_MEMCHECK:
            # W5 将实现: 内存泄漏检测
            print(f"[analysis] 内存泄漏分析 (W5 实现)", file=sys.stderr)

        elif task_type == TASK_TYPE_JAVA_HEAP:
            # W5 将实现: Java 堆 dump 分析
            print(f"[analysis] Java 堆分析 (W5 实现)", file=sys.stderr)

        else:
            print(f"[analysis] 未知任务类型 {task_type}，跳过分析", file=sys.stderr)

    except SystemExit:
        raise
    except Exception as e:
        exit_error(ErrorCode.ERR_ANALYSIS_FAILED,
                   f"分析过程异常: {e}",
                   traceback.format_exc())

    # 关闭 MinIO 客户端（MinIO Python SDK 无需显式关闭）
    return outputs


def main():
    """
    主函数：分析引擎入口

    执行流程：
      1. 解析命令行参数
      2. 加载配置（文件 + 环境变量）
      3. 连接 PostgreSQL → 获取任务详情
      4. 连接 MinIO → 下载原始数据
      5. 按 task_type 分发分析器
      6. 上传分析产物
      7. 更新数据库状态
      8. 输出结果 JSON
    """
    # ---------- 1. 解析命令行参数 ----------
    parser = argparse.ArgumentParser(description="Mini-Drop 性能分析引擎")
    parser.add_argument("--task-id", required=True, help="任务 ID（必填）")
    parser.add_argument("--task-type", type=int, default=0,
                        help="任务类型: 0=CPU火焰图 1=Java 2=Tracing 4=内存 6=Java堆")
    parser.add_argument("--config", default="/etc/analysis/config.ini",
                        help="配置文件路径（默认 /etc/analysis/config.ini）")
    args = parser.parse_args()

    print(f"[analysis] ========================================", file=sys.stderr)
    print(f"[analysis] 开始分析: tid={args.task_id}, type={args.task_type}",
          file=sys.stderr)
    print(f"[analysis] ========================================", file=sys.stderr)

    # ---------- 2. 加载配置 ----------
    config = load_config(args.config)
    db_cfg = config["database"]
    storage_cfg = config["storage"]
    bucket = storage_cfg["bucket"]

    # ---------- 3. 连接数据库 ----------
    conn = connect_db(db_cfg["dsn"])

    try:
        # ---------- 4. 获取任务详情 ----------
        task = get_task(conn, args.task_id)

        # ---------- 5. 标记分析开始 ----------
        update_analysis_status(conn, args.task_id, 1, "分析中")

        # ---------- 6. 执行分析 ----------
        outputs = run_analysis_for_type(
            conn, storage_cfg, task, bucket,
            args.task_id, args.task_type
        )

        # ---------- 7. 标记分析完成 ----------
        update_analysis_status(conn, args.task_id, 2, "分析完成")

        # ---------- 8. 输出结果 ----------
        result = {
            "task_id": args.task_id,
            "status": "success",
            "analysis_status": 2,
            "outputs": outputs,
        }
        exit_ok(result)

    except SystemExit:
        raise  # exit_ok / exit_error 的 sys.exit
    except Exception as e:
        # 未预期的异常 → 标记失败
        update_analysis_status(conn, args.task_id, 3, f"分析异常: {e}")
        exit_error(ErrorCode.ERR_ANALYSIS_FAILED,
                   f"未预期的错误: {e}",
                   traceback.format_exc())
    finally:
        # 关闭数据库连接
        try:
            conn.close()
            print(f"[analysis] 数据库连接已关闭", file=sys.stderr)
        except Exception:
            pass


# Python 的惯用写法：只有直接运行这个文件时才执行 main()
# 如果是被 import 的，则不会执行
if __name__ == "__main__":
    main()
