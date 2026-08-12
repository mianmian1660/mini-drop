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
import shutil
import subprocess
import sys
import traceback
import re
from datetime import datetime, timezone

# 引入同目录下的模块
from storage import MinIOStorage, create_storage
from error import ErrorCode, ErrorInfo, exit_ok, exit_error
from flamegraph import generate_flamegraph, get_folded_stacks
from collapsed_data_parser import analyze_collapsed
from analysis_advisor import generate_suggestions as advisor_generate_suggestions
from memleak_analyzer import analyze_memtrace, generate_mock_memtrace
from bpf_analyzer import analyze_bpf_output, bpf_histogram_to_svg
from java_analyzer import analyze_java_profile, generate_java_suggestions
from attribution import run_attribution
from observability import elapsed_seconds, log_event, now_seconds


# ============================================================
# 任务类型常量（与 drop/hotmethod.proto 保持一致）
# ============================================================
TASK_TYPE_GENERIC   = 0   # 通用 CPU 采样
TASK_TYPE_JAVA      = 1   # Java 分析
TASK_TYPE_TRACING   = 2   # Tracing
TASK_TYPE_MEMCHECK  = 4   # 内存泄漏
TASK_TYPE_BPF       = 5   # eBPF 内核探针 (IO/调度延迟)
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


def persist_attribution(conn, tid: str, attribution: dict):
    """将一份可验证的归因 JSON 附加到该任务已有建议上。"""
    try:
        value = json.dumps(attribution, ensure_ascii=False)
        cur = conn.cursor()
        cur.execute(
            "UPDATE analysis_suggestions SET ai_suggestion = %s "
            "WHERE tid = %s AND (ai_suggestion = '' OR ai_suggestion IS NULL)",
            (value, tid),
        )
        if cur.rowcount == 0:
            cur.execute(
                "INSERT INTO analysis_suggestions (tid, func, suggestion, ai_suggestion, status) "
                "VALUES (%s, %s, %s, %s, 0)",
                (tid, "智能归因", attribution.get("suggestion", ""), value),
            )
        conn.commit()
        cur.close()
    except Exception as e:
        print(f"[analysis] 写入智能归因失败: {e}", file=sys.stderr)


def _save_attribution(conn, storage, bucket: str, tid: str, task: dict,
                      top_json: dict, folded_text: str, local_dir: str,
                      outputs: list, presigned_urls: dict, local_files: list):
    """归因失败不影响主分析；其状态同样保留给前端展示。"""
    started_at = now_seconds()
    try:
        attribution = run_attribution(conn, task, top_json, folded_text)
    except Exception as e:
        attribution = {
            "status": "error",
            "reasoning_summary": f"智能归因异常但主分析已继续: {e}",
            "suggestion": "",
            "evidence": [],
            "done": False,
            "engine": "",
            "generated_at": datetime.now(timezone.utc).isoformat(),
        }
    log_event(
        "attribution_succeeded" if attribution.get("status") == "completed"
        else "attribution_skipped" if attribution.get("status") == "skipped"
        else "attribution_failed",
        task_tid=tid,
        status=attribution.get("status"),
        duration_seconds=elapsed_seconds(started_at),
        error=attribution.get("reasoning_summary") if attribution.get("status") == "error" else None,
    )
    persist_attribution(conn, tid, attribution)
    key = _upload_output(storage, bucket, tid, "attribution.json", attribution, "application/json")
    if key:
        outputs.append(key)
        presigned_urls["attribution.json"] = _get_presigned_url(storage, bucket, key)
    else:
        local_path = _save_local_output(local_dir, f"{tid}_attribution.json", attribution)
        if local_path:
            local_files.append(local_path)
            outputs.append(local_path)
    return attribution


def _connect_storage(storage_cfg: dict):
    """
    尝试连接 MinIO，返回 (MinIOStorage, bool)
    bool 表示是否连接成功
    """
    try:
        storage = MinIOStorage(
            endpoint=storage_cfg["endpoint"],
            access_key=storage_cfg["access_key"],
            secret_key=storage_cfg["secret_key"],
            use_ssl=storage_cfg["use_ssl"],
        )
        if storage.ensure_bucket(storage_cfg.get("bucket", "drop-data")):
            return storage, True
        return storage, False
    except Exception as e:
        print(f"[analysis] MinIO 不可用: {e}", file=sys.stderr)
        return None, False


def _download_perf_data(storage, bucket: str, tid: str,
                        local_path: str) -> bool:
    """
    从 MinIO 下载 perf.data 到本地

    返回: True=下载成功, False=失败
    """
    if storage is None:
        return False

    perf_key = f"{tid}/perf.data"
    if not storage.object_exists(bucket, perf_key):
        print(f"[analysis] MinIO 上不存在 {perf_key}", file=sys.stderr)
        return False

    try:
        data = storage.get_object(bucket, perf_key)
        if data is None:
            return False
        with open(local_path, "wb") as f:
            f.write(data)
        print(f"[analysis] 下载 perf.data → {local_path} ({len(data)} bytes)",
              file=sys.stderr)
        return True
    except Exception as e:
        print(f"[analysis] 下载 perf.data 失败: {e}", file=sys.stderr)
        return False


def _raw_artifact_keys(conn, tid: str, suffixes=None) -> list:
    suffixes = suffixes or []
    keys = []
    if conn is None:
        return keys
    try:
        cur = conn.cursor()
        cur.execute(
            "SELECT object_key FROM artifacts WHERE task_tid = %s AND kind = 'RAW' ORDER BY created_at DESC, id DESC",
            (tid,),
        )
        for row in cur.fetchall():
            key = row[0]
            if not key:
                continue
            lowered = key.lower()
            if not suffixes or any(lowered.endswith(suffix) for suffix in suffixes):
                keys.append(key)
        cur.close()
    except Exception as e:
        print(f"[analysis] 读取 RAW artifact 元数据失败: {e}", file=sys.stderr)
    return keys


def _download_first_existing(storage, bucket: str, keys: list, local_path: str, label: str) -> bool:
    if storage is None:
        return False
    seen = set()
    for key in keys:
        if not key or key in seen:
            continue
        seen.add(key)
        try:
            if hasattr(storage, "object_exists") and not storage.object_exists(bucket, key):
                print(f"[analysis] MinIO 上不存在 {key}", file=sys.stderr)
                continue
            data = storage.get_object(bucket, key)
            if not data:
                continue
            with open(local_path, "wb") as f:
                f.write(data)
            print(f"[analysis] 下载 {label}: {key} → {local_path} ({len(data)} bytes)", file=sys.stderr)
            return True
        except Exception as e:
            print(f"[analysis] 下载 {label} 失败 key={key}: {e}", file=sys.stderr)
    return False


def _download_kallsyms(storage, bucket: str, tid: str,
                       local_path: str, conn=None):
    """
    下载 Agent 采集时快照的 /proc/kallsyms。

    优先读取 artifacts 表中记录的 RAW 产物，兼容后续对象 key 调整；如果元数据缺失，
    再回退到早期固定路径 {tid}/kallsyms。
    """
    keys = _raw_artifact_keys(conn, tid, suffixes=["/kallsyms", ".kallsyms", "kallsyms"])
    keys.append(f"{tid}/kallsyms")
    if _download_first_existing(storage, bucket, keys, local_path, "kallsyms"):
        return local_path
    print(f"[analysis] 无 kallsyms 快照，内核符号将无法解析", file=sys.stderr)
    return None

def _upload_output(storage, bucket: str, tid: str,
                   filename: str, content, content_type: str = "application/octet-stream") -> str:
    """
    上传分析产物到 MinIO

    返回: MinIO key，失败返回空字符串
    """
    if storage is None:
        log_event("artifact_upload_skipped", task_tid=tid, filename=filename,
                  reason="storage_unavailable")
        return ""

    key = f"{tid}/{filename}"
    started_at = now_seconds()
    try:
        if isinstance(content, str):
            data = content.encode("utf-8")
        elif isinstance(content, bytes):
            data = content
        else:
            import json
            data = json.dumps(content, ensure_ascii=False).encode("utf-8")

        if storage.put_object(bucket, key, data, content_type):
            log_event("artifact_upload_succeeded", task_tid=tid, filename=filename,
                      object_key=key, size_bytes=len(data),
                      duration_seconds=elapsed_seconds(started_at))
            return key
        log_event("artifact_upload_failed", task_tid=tid, filename=filename,
                  object_key=key, duration_seconds=elapsed_seconds(started_at),
                  error="put_object returned false")
        return ""
    except Exception as e:
        log_event("artifact_upload_failed", task_tid=tid, filename=filename,
                  object_key=key, duration_seconds=elapsed_seconds(started_at),
                  error=str(e))
        print(f"[analysis] 上传 {filename} 失败: {e}", file=sys.stderr)
        return ""


def _save_local_output(local_dir: str, filename: str, content) -> str:
    """
    保存分析产物到本地目录（MinIO 不可用时的降级方案）

    返回: 本地文件路径，失败返回空字符串
    """
    if not local_dir:
        return ""

    try:
        os.makedirs(local_dir, exist_ok=True)
        filepath = os.path.join(local_dir, filename)

        if isinstance(content, str):
            with open(filepath, "w", encoding="utf-8") as f:
                f.write(content)
        elif isinstance(content, bytes):
            with open(filepath, "wb") as f:
                f.write(content)
        else:
            import json
            with open(filepath, "w", encoding="utf-8") as f:
                json.dump(content, f, ensure_ascii=False, indent=2)

        print(f"[analysis] 本地保存: {filepath}", file=sys.stderr)
        return filepath
    except Exception as e:
        print(f"[analysis] 本地保存 {filename} 失败: {e}", file=sys.stderr)
        return ""


def _get_presigned_url(storage, bucket: str, key: str,
                       expire_sec: int = 900) -> str:
    """
    获取预签名下载 URL

    返回: URL 字符串，失败返回空字符串
    """
    if storage is None or not key:
        return ""

    try:
        url = storage.presigned_get_url(bucket, key, expire_sec)
        if url:
            print(f"[analysis] 预签名 URL: {key}", file=sys.stderr)
            return url
        return ""
    except Exception as e:
        print(f"[analysis] 生成预签名 URL 失败 ({key}): {e}", file=sys.stderr)
        return ""


def _analyze_cpu_flamegraph(conn, storage_cfg: dict, task: dict,
                            bucket: str, tid: str,
                            local_dir: str = "") -> dict:
    """
    CPU 火焰图分析（task_type=0）

    完整流水线:
      1. 从 MinIO 下载 perf.data
      2. perf script → stackcollapse → flamegraph.pl → SVG
      3. 解析折叠栈 → TopN 热点 JSON
      4. 规则建议引擎 → suggestions.md
      5. 上传产物到 MinIO（或保存到本地 local_dir）
      6. 生成预签名 URL（MinIO 可用时）
      7. 写结果到 analysis_suggestions 表

    返回: {"outputs": [...], "presigned_urls": {...}, "local_files": [...]}
    """
    outputs = []

    # --- 1. 连接 MinIO ---
    storage, storage_ok = _connect_storage(storage_cfg)

    # --- 2. 获取 perf.data ---
    # 优先从 MinIO 下载，其次用本地测试文件
    local_perf = f"/tmp/{tid}_perf.data"
    has_perf = False

    if storage_ok:
        has_perf = _download_perf_data(storage, bucket, tid, local_perf)

    if not has_perf:
        # MinIO 不可用或文件不存在时，尝试用本地 perf.data（仅本地测试用）
        test_files = [
            f"/tmp/{tid}_perf.data",
            "/tmp/test_perf3.data",
            "/tmp/test_perf.data",
        ]
        for tf in test_files:
            if os.path.exists(tf) and os.path.getsize(tf) > 0:
                local_perf = tf
                has_perf = True
                print(f"[analysis] 使用本地测试文件: {local_perf}", file=sys.stderr)
                break

    if not has_perf:
        print(f"[analysis] 错误: 找不到 perf.data，无法生成火焰图", file=sys.stderr)
        return outputs

    # --- 2b. 获取 kallsyms 快照（缺失则降级，内核符号显示为 [unknown]）---
    local_kallsyms = None
    if storage_ok:
        local_kallsyms = _download_kallsyms(
            storage, bucket, tid, f"/tmp/{tid}_kallsyms", conn)

    # --- 3. 生成火焰图 SVG ---
    task_name = task.get("name", tid)
    title = f"CPU Flame Graph: {task_name}"

    print(f"[analysis] 开始生成火焰图 ...", file=sys.stderr)
    try:
        svg_content = generate_flamegraph(local_perf, title=title,
                                          kallsyms_path=local_kallsyms)
    except Exception as e:
        exit_error(ErrorCode.ERR_ANALYSIS_FAILED,
                   f"火焰图生成失败: {e}",
                   traceback.format_exc())

    # --- 4. 获取折叠栈 ---
    try:
        folded_text = get_folded_stacks(local_perf, local_kallsyms)
    except Exception as e:
        print(f"[analysis] 折叠栈生成失败: {e}", file=sys.stderr)
        folded_text = ""

    # --- 5. 解析折叠栈 → TopN 热点 ---
    top_json = {}
    if folded_text:
        try:
            top_json = analyze_collapsed(folded_text, top_n=20)
        except Exception as e:
            print(f"[analysis] 热点分析失败: {e}", file=sys.stderr)

    # --- 6. 上传产物到 MinIO / 保存到本地 ---
    presigned_urls = {}
    local_files = []

    # 火焰图 SVG
    svg_key = _upload_output(storage, bucket, tid,
                             "flamegraph.svg", svg_content, "image/svg+xml")
    if svg_key:
        outputs.append(svg_key)
        presigned_urls["flamegraph.svg"] = _get_presigned_url(storage, bucket, svg_key)
    else:
        local_path = _save_local_output(local_dir, f"{tid}_flamegraph.svg", svg_content)
        if local_path:
            local_files.append(local_path)
            outputs.append(local_path)

    # 折叠栈
    folded_key = _upload_output(storage, bucket, tid,
                                "folded.txt", folded_text, "text/plain")
    if folded_key:
        outputs.append(folded_key)
        presigned_urls["folded.txt"] = _get_presigned_url(storage, bucket, folded_key)
    else:
        local_path = _save_local_output(local_dir, f"{tid}_folded.txt", folded_text)
        if local_path:
            local_files.append(local_path)
            outputs.append(local_path)

    # TopN JSON
    top_key = _upload_output(storage, bucket, tid,
                             "top.json", top_json, "application/json")
    if top_key:
        outputs.append(top_key)
        presigned_urls["top.json"] = _get_presigned_url(storage, bucket, top_key)
    else:
        local_path = _save_local_output(local_dir, f"{tid}_top.json", top_json)
        if local_path:
            local_files.append(local_path)
            outputs.append(local_path)

    # --- 7. 规则建议引擎：匹配热点函数 → 生成优化建议 ---
    suggestions_result = {}
    if top_json and top_json.get("self_time_top"):
        try:
            # 确定规则文件路径
            rules_file = os.path.join(
                os.path.dirname(os.path.abspath(__file__)),
                "rules.yaml"
            )
            suggestions_result = advisor_generate_suggestions(
                top_json,
                task_name=task_name,
                rules_file=rules_file,
            )
            print(f"[analysis] 规则引擎匹配到 "
                  f"{len(suggestions_result.get('suggestions', []))} 条建议",
                  file=sys.stderr)

            # 上传/保存 suggestions.md
            md_content = suggestions_result.get("suggestions_md", "")
            if md_content:
                md_key = _upload_output(storage, bucket, tid,
                                        "suggestions.md", md_content,
                                        "text/markdown")
                if md_key:
                    outputs.append(md_key)
                    presigned_urls["suggestions.md"] = _get_presigned_url(
                        storage, bucket, md_key)
                else:
                    local_path = _save_local_output(
                        local_dir, f"{tid}_suggestions.md", md_content)
                    if local_path:
                        local_files.append(local_path)
                        outputs.append(local_path)

            # 上传/保存 suggestions.json
            sugg_json = {
                "suggestions": suggestions_result.get("suggestions", []),
                "rules_loaded": suggestions_result.get("rules_loaded", 0),
                "rule_version": suggestions_result.get("rule_version", ""),
            }
            sugg_key = _upload_output(storage, bucket, tid,
                                      "suggestions.json", sugg_json,
                                      "application/json")
            if sugg_key:
                outputs.append(sugg_key)
                presigned_urls["suggestions.json"] = _get_presigned_url(
                    storage, bucket, sugg_key)
            else:
                local_path = _save_local_output(
                    local_dir, f"{tid}_suggestions.json", sugg_json)
                if local_path:
                    local_files.append(local_path)
                    outputs.append(local_path)

            # 写入 Top5 匹配到的建议到 analysis_suggestions 表
            matched = suggestions_result.get("suggestions", [])
            for item in matched[:5]:
                insert_suggestion(conn, tid,
                                  item["function"],
                                  item["advice"])

        except Exception as e:
            print(f"[analysis] 规则建议生成失败: {e}", file=sys.stderr)
            # 规则引擎失败不阻塞主流程

    # --- 8. 如果没有规则匹配，写 Top5 热点基本信息 ---
    if not suggestions_result.get("suggestions") and top_json.get("self_time_top"):
        for item in top_json["self_time_top"][:5]:
            func_name = item["function"]
            pct = item["percentage"]
            suggestion = (f"函数 '{func_name}' 占 CPU {pct}%，"
                          f"建议人工审查是否存在优化空间")
            insert_suggestion(conn, tid, func_name, suggestion)

    _save_attribution(conn, storage, bucket, tid, task, top_json, folded_text,
                      local_dir, outputs, presigned_urls, local_files)

    print(f"[analysis] CPU 火焰图分析完成: {len(outputs)} 个产物 "
          f"(MinIO: {len(presigned_urls)}, 本地: {len(local_files)})",
          file=sys.stderr)

    return {
        "outputs": outputs,
        "presigned_urls": presigned_urls,
        "local_files": local_files,
    }


def _parse_pprof_top(output: str) -> dict:
    """Convert `go tool pprof -top` CPU-time rows to the existing TopN schema.

    pprof's ``flat`` value is duration (for example ``41.33s``), not a count
    of sampling events.  It remains in ``samples`` for backwards-compatible
    JSON, while ``sample_unit`` tells the result page how to label it.
    """
    rows = []
    total = 0
    for line in output.splitlines():
        # flat flat% sum% cum cum% function
        m = re.match(r"^\s*([\d.]+)([a-zA-Zµ]+)?\s+([\d.]+)%\s+([\d.]+)%\s+([\d.]+)([a-zA-Zµ]+)?\s+([\d.]+)%\s+(.+)$", line)
        if not m:
            continue
        samples = float(m.group(1))
        pct = float(m.group(3))
        name = m.group(8).strip()
        if not name or name.startswith("..."):
            continue
        total += samples
        rows.append({"function": name, "samples": samples, "percentage": pct})
    return {
        "language": "go", "source_format": "pprof", "sample_unit": "seconds", "total_samples": total,
        "self_time_top": [{**row, "rank": i + 1} for i, row in enumerate(rows[:20])],
    }


def _analyze_pprof(conn, storage_cfg: dict, task: dict, bucket: str, tid: str,
                   local_dir: str = "") -> dict:
    """Analyse a gzip protobuf CPU profile with the official Go pprof CLI."""
    storage, storage_ok = _connect_storage(storage_cfg)
    local_profile = f"/tmp/{tid}_pprof.pb.gz"
    if not storage_ok or not _download_perf_data(storage, bucket, tid, local_profile):
        raise ValueError("找不到 pprof 原始 profile")
    if not shutil.which("go"):
        raise RuntimeError("分析镜像缺少 go tool pprof")

    svg_path = f"/tmp/{tid}_pprof.svg"
    svg_run = subprocess.run(["go", "tool", "pprof", "-svg", "-output", svg_path, local_profile],
                             text=True, capture_output=True)
    if svg_run.returncode != 0 or not os.path.exists(svg_path):
        raise RuntimeError("pprof SVG 生成失败: " + (svg_run.stderr.strip() or svg_run.stdout.strip()))
    top_run = subprocess.run(["go", "tool", "pprof", "-top", local_profile],
                             text=True, capture_output=True)
    if top_run.returncode != 0:
        raise RuntimeError("pprof TopN 生成失败: " + top_run.stderr.strip())
    with open(svg_path, "rb") as f:
        svg = f.read()
    top_json = _parse_pprof_top(top_run.stdout)
    top_json["task_name"] = task.get("name", tid)
    outputs, urls, local_files = [], {}, []
    for name, data, content_type in (("flamegraph.svg", svg, "image/svg+xml"), ("top.json", top_json, "application/json")):
        key = _upload_output(storage, bucket, tid, name, data, content_type)
        if key:
            outputs.append(key)
            urls[name] = _get_presigned_url(storage, bucket, key)
        else:
            path = _save_local_output(local_dir, f"{tid}_{name}", data)
            if path:
                outputs.append(path); local_files.append(path)
    return {"outputs": outputs, "presigned_urls": urls, "local_files": local_files}


def _analyze_java_async_profiler(conn, storage_cfg: dict, task: dict,
                                 bucket: str, tid: str,
                                 local_dir: str = "") -> dict:
    """
    Java async-profiler 分析（task_type=1）

    支持 async-profiler collapsed 输出；文本化 JFR 会做保守解析；二进制 JFR
    会提示采集侧输出 collapsed 或先用 jfr print 转文本。
    """
    outputs = []
    presigned_urls = {}
    local_files = []

    storage, storage_ok = _connect_storage(storage_cfg)
    local_profile = f"/tmp/{tid}_java_profile.data"
    has_profile = False

    if storage_ok:
        has_profile = _download_perf_data(storage, bucket, tid, local_profile)

    if not has_profile:
        test_files = [
            f"/tmp/{tid}_java_profile.txt",
            f"/tmp/{tid}_perf.data",
            "/tmp/test_java_collapsed.txt",
        ]
        for tf in test_files:
            if os.path.exists(tf) and os.path.getsize(tf) > 0:
                local_profile = tf
                has_profile = True
                print(f"[analysis] 使用本地 Java profile: {local_profile}", file=sys.stderr)
                break

    if not has_profile:
        print("[analysis] 错误: 找不到 Java profile，无法生成 Java 火焰图", file=sys.stderr)
        return {"outputs": outputs, "presigned_urls": presigned_urls, "local_files": local_files}

    with open(local_profile, "rb") as f:
        profile_data = f.read()

    if profile_data.startswith(b"FLR\x00") and shutil.which("jfr"):
        try:
            printed = subprocess.run(
                ["jfr", "print", "--events", "jdk.ExecutionSample", local_profile],
                capture_output=True,
                text=True,
                timeout=120,
            )
            if printed.returncode == 0 and printed.stdout.strip():
                profile_data = printed.stdout.encode("utf-8")
                print("[analysis] 已用 jfr print 转换二进制 JFR", file=sys.stderr)
            else:
                print(f"[analysis] jfr print 未产出可解析文本: {printed.stderr[:300]}",
                      file=sys.stderr)
        except Exception as e:
            print(f"[analysis] jfr print 转换失败: {e}", file=sys.stderr)

    task_name = task.get("name", tid)
    try:
        java_result = analyze_java_profile(profile_data, task_name=task_name, top_n=20)
    except Exception as e:
        exit_error(ErrorCode.ERR_ANALYSIS_FAILED,
                   f"Java profile 解析失败: {e}",
                   traceback.format_exc())

    svg_content = java_result["svg"]
    folded_text = java_result["folded"]
    top_json = java_result["top_json"]

    artifacts = [
        ("java_flamegraph.svg", svg_content, "image/svg+xml"),
        ("java_folded.txt", folded_text, "text/plain"),
        ("java_top.json", top_json, "application/json"),
        # 兼容现有 apiserver: fetchTopFunctions 只读取 top.json。
        ("top.json", top_json, "application/json"),
    ]

    for filename, content, content_type in artifacts:
        key = _upload_output(storage, bucket, tid, filename, content, content_type)
        if key:
            outputs.append(key)
            presigned_urls[filename] = _get_presigned_url(storage, bucket, key)
        else:
            local_name = f"{tid}_{filename}"
            local_path = _save_local_output(local_dir, local_name, content)
            if local_path:
                local_files.append(local_path)
                outputs.append(local_path)

    suggestions_result = generate_java_suggestions(top_json, task_name=task_name)
    if suggestions_result.get("suggestions_md"):
        md_key = _upload_output(storage, bucket, tid,
                                "java_suggestions.md",
                                suggestions_result["suggestions_md"],
                                "text/markdown")
        if md_key:
            outputs.append(md_key)
            presigned_urls["java_suggestions.md"] = _get_presigned_url(storage, bucket, md_key)
        else:
            local_path = _save_local_output(
                local_dir, f"{tid}_java_suggestions.md",
                suggestions_result["suggestions_md"],
            )
            if local_path:
                local_files.append(local_path)
                outputs.append(local_path)

    suggestions_json = {
        "suggestions": suggestions_result.get("suggestions", []),
        "rules_loaded": suggestions_result.get("rules_loaded", 0),
        "rule_version": suggestions_result.get("rule_version", ""),
        "language": "java",
        "source_format": top_json.get("source_format", ""),
    }
    for filename in ("java_suggestions.json", "suggestions.json"):
        key = _upload_output(storage, bucket, tid, filename, suggestions_json, "application/json")
        if key:
            outputs.append(key)
            presigned_urls[filename] = _get_presigned_url(storage, bucket, key)
        else:
            local_path = _save_local_output(local_dir, f"{tid}_{filename}", suggestions_json)
            if local_path:
                local_files.append(local_path)
                outputs.append(local_path)

    matched = suggestions_result.get("suggestions", [])
    if matched:
        for item in matched[:5]:
            insert_suggestion(conn, tid, item["function"], item["advice"])
    elif top_json.get("self_time_top"):
        for item in top_json["self_time_top"][:5]:
            insert_suggestion(
                conn,
                tid,
                item["function"],
                f"Java 方法 '{item['function']}' 占 CPU {item['percentage']}%，建议检查锁竞争、对象分配、JIT 内联和业务热点路径。",
            )

    _save_attribution(conn, storage, bucket, tid, task, top_json, folded_text,
                      local_dir, outputs, presigned_urls, local_files)

    print(f"[analysis] Java async-profiler 分析完成: {len(outputs)} 个产物",
          file=sys.stderr)
    return {
        "outputs": outputs,
        "presigned_urls": presigned_urls,
        "local_files": local_files,
    }


def _analyze_memleak(conn, storage_cfg: dict, task: dict,
                     bucket: str, tid: str,
                     local_dir: str = "") -> dict:
    """
    内存泄漏分析（task_type=4）

    完整流水线:
      1. 从 MinIO 下载 memtrace.txt（或使用内置模拟数据）
      2. 解析 alloc/free 事件 → 配对检测泄漏
      3. 责任人分析 → 按泄漏量排名
      4. 生成 memleak_report.md + memleak.json
      5. 上传产物到 MinIO（或保存到本地）
      6. 写责任人到 analysis_suggestions 表

    返回: {"outputs": [...], "presigned_urls": {...}, "local_files": [...]}
    """
    outputs = []
    presigned_urls = {}
    local_files = []

    task_name = task.get("name", tid)

    # --- 1. 连接 MinIO ---
    storage, storage_ok = _connect_storage(storage_cfg)

    # --- 2. 获取内存追踪数据 ---
    memtrace_text = ""
    has_data = False

    if storage_ok:
        memtrace_key = f"{tid}/memtrace.txt"
        if storage.object_exists(bucket, memtrace_key):
            try:
                data = storage.get_object(bucket, memtrace_key)
                if data:
                    memtrace_text = data.decode("utf-8", errors="replace")
                    has_data = True
                    print(f"[analysis] 下载 memtrace.txt ({len(data)} bytes)",
                          file=sys.stderr)
            except Exception as e:
                print(f"[analysis] 下载 memtrace.txt 失败: {e}", file=sys.stderr)

    if not has_data:
        # MinIO 不可用或无数据时，使用内置模拟数据
        memtrace_text = generate_mock_memtrace()
        has_data = True
        print(f"[analysis] 使用内置模拟内存追踪数据 "
              f"({len(memtrace_text)} chars)",
              file=sys.stderr)

    if not has_data:
        print(f"[analysis] 错误: 无内存追踪数据", file=sys.stderr)
        return {"outputs": outputs, "presigned_urls": presigned_urls,
                "local_files": local_files}

    # --- 3. 执行内存泄漏分析 ---
    print(f"[analysis] 开始内存泄漏分析 ...", file=sys.stderr)
    try:
        memleak_result = analyze_memtrace(memtrace_text, task_name=task_name)
    except Exception as e:
        exit_error(ErrorCode.ERR_ANALYSIS_FAILED,
                   f"内存泄漏分析失败: {e}",
                   traceback.format_exc())

    # --- 4. 上传/保存产物 ---
    # memleak_report.md
    report_md = memleak_result.get("report_md", "")
    if report_md:
        md_key = _upload_output(storage, bucket, tid,
                                "memleak_report.md", report_md,
                                "text/markdown")
        if md_key:
            outputs.append(md_key)
            presigned_urls["memleak_report.md"] = _get_presigned_url(
                storage, bucket, md_key)
        else:
            local_path = _save_local_output(
                local_dir, f"{tid}_memleak_report.md", report_md)
            if local_path:
                local_files.append(local_path)
                outputs.append(local_path)

    # memleak.json
    memleak_json = {
        "total_allocs": memleak_result.get("total_allocs", 0),
        "total_frees": memleak_result.get("total_frees", 0),
        "leak_count": memleak_result.get("leak_count", 0),
        "total_leaked_human": memleak_result.get("total_leaked_human", "0 B"),
        "responsible_top": memleak_result.get("responsible_top", []),
        "leaks": memleak_result.get("leaks", []),
    }
    json_key = _upload_output(storage, bucket, tid,
                              "memleak.json", memleak_json,
                              "application/json")
    if json_key:
        outputs.append(json_key)
        presigned_urls["memleak.json"] = _get_presigned_url(
            storage, bucket, json_key)
    else:
        local_path = _save_local_output(
            local_dir, f"{tid}_memleak.json", memleak_json)
        if local_path:
            local_files.append(local_path)
            outputs.append(local_path)

    # --- 5. 写入 Top5 责任人到 analysis_suggestions ---
    responsible_top = memleak_result.get("responsible_top", [])
    for item in responsible_top[:5]:
        func_name = item["function"]
        suggestion = (f"函数 '{func_name}' 存在内存泄漏: "
                      f"{item['leak_count']} 处, 泄漏 {item['total_human']}。"
                      f"建议检查该函数中的 alloc/free 配对，"
                      f"确保所有路径都释放了分配的内存。")
        insert_suggestion(conn, tid, func_name, suggestion)

    print(f"[analysis] 内存泄漏分析完成: {len(outputs)} 个产物 "
          f"(MinIO: {len(presigned_urls)}, 本地: {len(local_files)})",
          file=sys.stderr)

    return {
        "outputs": outputs,
        "presigned_urls": presigned_urls,
        "local_files": local_files,
    }


def _analyze_bpf(conn, storage_cfg: dict, task: dict,
                 bucket: str, tid: str, local_dir: str = "") -> dict:
    """
    eBPF 内核探针分析（task_type=5）
    支持 IO 延迟直方图 / 调度延迟 / CPU 火焰图
    """
    outputs = []
    presigned_urls = {}
    local_files = []

    storage, storage_ok = _connect_storage(storage_cfg)

    local_bpf = f"/tmp/{tid}_bpf.txt"
    has_data = False

    if storage_ok:
        keys = _raw_artifact_keys(conn, tid, suffixes=["raw.bpf", ".bpf", "perf.data", ".txt"])
        keys.extend([f"{tid}/raw.bpf", f"{tid}/perf.data"])
        has_data = _download_first_existing(storage, bucket, keys, local_bpf, "eBPF raw")

    if not has_data:
        print(f"[analysis] 错误: 找不到 eBPF 数据文件", file=sys.stderr)
        return {"outputs": [], "presigned_urls": {}, "local_files": []}

    with open(local_bpf, 'r') as f:
        bpf_text = f.read()

    if not bpf_text.strip():
        return {"outputs": [], "presigned_urls": {}, "local_files": []}

    params = task.get("request_params") or {}
    bpf_mode = str(params.get("event") or "").lower()
    if bpf_mode not in ("cpu", "io", "sched"):
        bpf_mode = "cpu" if (";" in bpf_text and "@" not in bpf_text) else "histogram"

    # 检测格式：CPU 折叠栈 → 火焰图；IO/sched → SVG 直方图。
    is_cpu_profile = bpf_mode == "cpu" or (";" in bpf_text and "@" not in bpf_text)
    if is_cpu_profile:
        print(f"[analysis] eBPF CPU 折叠栈 → 火焰图", file=sys.stderr)
        try:
            svg = generate_flamegraph(local_bpf, title=f"eBPF CPU: {task.get('name', tid)}")
        except:
            svg = ""
        folded = get_folded_stacks(local_bpf) if False else bpf_text
        try:
            top_json = analyze_collapsed(bpf_text, top_n=20)
            top_json["language"] = "native"
            top_json["source_format"] = "bpftrace_folded"
            top_json["collector"] = "ebpf"
        except:
            top_json = {}
    else:
        print(f"[analysis] eBPF 直方图 → SVG 柱状图", file=sys.stderr)
        hist_data = analyze_bpf_output(bpf_text)
        if not hist_data.get("buckets"):
            raise ValueError("eBPF 直方图没有有效桶：请确认采集窗口内存在 IO/调度负载、tracepoint 可用且 Agent 具备 bpftrace/tracefs 权限；必要时加长 duration")
        svg = bpf_histogram_to_svg(hist_data, title=f"eBPF {hist_data.get('type', '')}")
        top_json = hist_data

    # 保存产物。MinIO 不可用时使用 apiserver 的本地降级约定：/tmp/drop-output/{tid}_*
    out_dir = local_dir if local_dir else "/tmp/drop-output"
    os.makedirs(out_dir, exist_ok=True)
    local_prefix = "" if local_dir else f"{tid}_"

    svg_name = f"{local_prefix}{'flamegraph.svg' if is_cpu_profile else 'bpf_histogram.svg'}"
    svg_path = os.path.join(out_dir, svg_name)
    if svg:
        with open(svg_path, 'w') as f:
            f.write(svg)
        local_files.append({"name": svg_name, "path": svg_path})

    json_name = f"{local_prefix}{'top.json' if is_cpu_profile else 'bpf_data.json'}"
    json_path = os.path.join(out_dir, json_name)
    with open(json_path, 'w') as f:
        json.dump(top_json, f, ensure_ascii=False, indent=2)
    local_files.append({"name": json_name, "path": json_path})

    raw_name = f"{local_prefix}bpf_raw.txt"
    raw_path = os.path.join(out_dir, raw_name)
    with open(raw_path, 'w') as f:
        f.write(bpf_text)
    local_files.append({"name": raw_name, "path": raw_path})

    # 上传 MinIO
    if storage_ok:
        for lf in local_files:
            object_name = lf["name"]
            if object_name.startswith(f"{tid}_"):
                object_name = object_name[len(tid) + 1:]
            key = f"{tid}/{object_name}"
            content_type = "application/octet-stream"
            if object_name.endswith(".svg"):
                content_type = "image/svg+xml"
            elif object_name.endswith(".json"):
                content_type = "application/json"
            elif object_name.endswith(".txt"):
                content_type = "text/plain"
            try:
                with open(lf["path"], 'rb') as f:
                    file_data = f.read()
                storage.put_object(bucket, key, file_data, content_type)
                url = storage.presigned_get_url(bucket, key)
                if url:
                    presigned_urls[object_name] = url
                outputs.append(key)
            except Exception as e:
                print(f"[analysis] MinIO 上传 {lf['name']} 失败: {e}", file=sys.stderr)

    print(f"[analysis] eBPF 分析完成: {len(outputs)} 个产物", file=sys.stderr)
    return {"outputs": outputs, "presigned_urls": presigned_urls, "local_files": local_files}


def run_analysis_for_type(conn, storage_cfg: dict, task: dict,
                          bucket: str, tid: str, task_type: int,
                          local_dir: str = "") -> dict:
    """
    根据任务类型执行对应的分析逻辑

    返回: {"outputs": [...], "presigned_urls": {...}, "local_files": [...]}
    """
    result = {"outputs": [], "presigned_urls": {}, "local_files": []}

    # ---------- 按 task_type 分发分析 ----------
    try:
        if task_type == TASK_TYPE_GENERIC:
            # CPU 火焰图：perf script → stackcollapse → flamegraph.pl → SVG
            result = _analyze_cpu_flamegraph(conn, storage_cfg, task,
                                             bucket, tid, local_dir)

        elif task_type == TASK_TYPE_JAVA:
            # Java async-profiler：collapsed/JFR 文本 → Java 火焰图 + TopN
            result = _analyze_java_async_profiler(conn, storage_cfg, task,
                                                  bucket, tid, local_dir)

        elif task_type == TASK_TYPE_TRACING:
            # task type 2 is the Go HTTP pprof collector, not generic tracing.
            result = _analyze_pprof(conn, storage_cfg, task, bucket, tid, local_dir)

        elif task_type == TASK_TYPE_MEMCHECK:
            # 内存泄漏检测：alloc/free 配对分析 → 责任人定位
            result = _analyze_memleak(conn, storage_cfg, task,
                                      bucket, tid, local_dir)

        elif task_type == TASK_TYPE_BPF:
            # eBPF 内核探针分析：IO延迟直方图 / 调度延迟
            result = _analyze_bpf(conn, storage_cfg, task,
                                  bucket, tid, local_dir)

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

    return result


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
    parser.add_argument("--local-output-dir", default="",
                        help="本地输出目录（MinIO 不可用时将结果保存到此目录）")
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
        analysis_result = run_analysis_for_type(
            conn, storage_cfg, task, bucket,
            args.task_id, args.task_type,
            local_dir=args.local_output_dir,
        )

        # ---------- 7. 标记分析完成 ----------
        update_analysis_status(conn, args.task_id, 2, "分析完成")

        # ---------- 8. 输出结果 ----------
        result = {
            "task_id": args.task_id,
            "status": "success",
            "analysis_status": 2,
            "outputs": analysis_result.get("outputs", []),
            "presigned_urls": analysis_result.get("presigned_urls", {}),
            "local_files": analysis_result.get("local_files", []),
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
