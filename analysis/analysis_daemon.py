#!/usr/bin/env python3
# ============================================================
# analysis_daemon.py — 分析守护进程
# ============================================================
# 从 analysis_jobs 表领取租约，调用注册表中的分析器，并更新 AnalysisJob
# 与 hotmethod_tasks.analysis_status。
# ============================================================

import argparse
import json
import os
import sys
import threading
import time

from analyzer_contract import AnalyzerError, AnalyzerInputError
from analyzer_registry import build_default_registry
from lease import AnalysisLeaseClient
from observability import elapsed_seconds, log_event, now_seconds


POLL_INTERVAL = 5
LEASE_SECONDS = 300
MAX_ANALYSIS_ATTEMPTS = int(os.environ.get("MAX_ANALYSIS_ATTEMPTS", "3"))
ANALYZER_VERSION = os.environ.get("ANALYZER_VERSION", "b1-lease")
PG_DSN = os.environ.get(
    "PG_DSN", "host=localhost user=postgres password=dev dbname=drop sslmode=disable"
)


def record_result_artifacts(conn, tid: str, outputs, manifest=None, storage=None, bucket=None):
    """Persist analyzer outputs as Artifact metadata without storing URLs."""
    keys = [value for value in (outputs or []) if isinstance(value, str) and value.startswith(tid + "/")]
    if manifest and f"{tid}/manifest.json" not in keys:
        keys.append(f"{tid}/manifest.json")
    if not keys:
        return []
    cur = conn.cursor()
    cur.execute(
        "SELECT id FROM task_attempts WHERE task_tid = %s ORDER BY attempt_seq DESC LIMIT 1", (tid,)
    )
    row = cur.fetchone()
    attempt_id = row[0] if row else 0
    artifact_ids = []
    for key in keys:
        name = key.rsplit("/", 1)[-1]
        kind = "MANIFEST" if name == "manifest.json" else "RESULT" if name.endswith((".svg", ".json", ".md", ".html")) else "INTERMEDIATE"
        content_type = "application/json" if name.endswith(".json") else "image/svg+xml" if name.endswith(".svg") else "text/markdown" if name.endswith(".md") else "application/octet-stream"
        size = 0
        if storage is not None and bucket:
            size = storage.stat_object(bucket, key) or 0
        cur.execute(
            """
            INSERT INTO artifacts (task_tid, attempt_id, kind, object_key, content_type, status, size, created_at)
            VALUES (%s, %s, %s, %s, %s, 'ready', %s, NOW())
            ON CONFLICT (task_tid, kind, object_key) DO UPDATE
            SET attempt_id = EXCLUDED.attempt_id, status = EXCLUDED.status, size = EXCLUDED.size
            RETURNING id
            """,
            (tid, attempt_id, kind, key, content_type, size),
        )
        row = cur.fetchone()
        if row:
            artifact_ids.append(row[0])
    cur.close()
    return artifact_ids


def update_analysis_status(dsn: str, tid: str, status: int, status_info: str = ""):
    """兼容旧前端/旧接口：同步更新 hotmethod_tasks.analysis_status。"""
    try:
        import psycopg2

        conn = psycopg2.connect(dsn)
        cur = conn.cursor()
        if status_info:
            cur.execute(
                """
                UPDATE hotmethod_tasks
                SET analysis_status = %s,
                    status_info = CASE
                        WHEN status_info = '' THEN %s
                        ELSE status_info || '; ' || %s
                    END
                WHERE tid = %s
                """,
                (status, status_info, status_info, tid),
            )
        else:
            cur.execute(
                "UPDATE hotmethod_tasks SET analysis_status = %s WHERE tid = %s",
                (status, tid),
            )
        conn.commit()
        cur.close()
        conn.close()
    except Exception as e:
        print(f"[analysis_daemon] 更新 analysis_status 失败: {e}", file=sys.stderr)


def _start_heartbeat(lease_client: AnalysisLeaseClient, job_id: int,
                     stop_event: threading.Event):
    def run():
        while not stop_event.wait(max(1, lease_client.lease_seconds // 3)):
            try:
                if not lease_client.heartbeat(job_id):
                    print(f"[analysis_daemon] 续租失败: job_id={job_id}", file=sys.stderr)
            except Exception as e:
                print(f"[analysis_daemon] 续租异常: job_id={job_id} error={e}",
                      file=sys.stderr)

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    return thread


def should_retry(job) -> bool:
    max_attempts = int(getattr(job, "max_attempts", 0) or MAX_ANALYSIS_ATTEMPTS)
    return int(getattr(job, "attempt", 0) or 0) < max_attempts


def _record_task_event_tx(conn, tid: str, reason: str, source: str, payload: dict = None):
    cur = conn.cursor()
    cur.execute(
        "SELECT COALESCE(MAX(sequence), 0) FROM task_status_events WHERE tid = %s",
        (tid,),
    )
    row = cur.fetchone()
    sequence = int(row[0] or 0) + 1
    cur.execute(
        """
        INSERT INTO task_status_events
            (tid, from_status, to_status, reason, source, sequence,
             source_module, payload, created_at)
        SELECT tid, status, status, %s, %s, %s, %s, %s::jsonb, NOW()
        FROM hotmethod_tasks WHERE tid = %s
        """,
        (
            reason,
            source,
            sequence,
            source,
            json.dumps(payload or {}, ensure_ascii=False),
            tid,
        ),
    )
    cur.close()


def _upload_manifest(storage_cfg: dict, bucket: str, tid: str, manifest: dict) -> str:
    import hotmethod_analyzer as hm

    try:
        storage, storage_ok = hm._connect_storage(storage_cfg)
        if not storage_ok:
            raise RuntimeError("storage unavailable")
        return hm._upload_output(storage, bucket, tid, "manifest.json", manifest, "application/json")
    except Exception as exc:
        raise RuntimeError(f"manifest 上传失败: {exc}") from exc


def finalize_success_tx(conn, lease_client, job, tid: str, result: dict,
                        analyzer_version: str, storage=None, bucket=None):
    manifest = result.get("manifest") or {}
    artifact_ids = record_result_artifacts(conn, tid, result.get("outputs", []), manifest=manifest,
                                           storage=storage, bucket=bucket)
    if not lease_client.complete(
        job.id,
        analyzer_version,
        conn=conn,
        output_artifact_ids=json.dumps(artifact_ids),
    ):
        raise RuntimeError("AnalysisJob 完成失败：lease_owner 已变化")
    cur = conn.cursor()
    cur.execute(
        """
        UPDATE hotmethod_tasks
        SET analysis_status = 2,
            status_info = CASE
                WHEN COALESCE(status_info, '') = '' THEN '分析完成'
                ELSE COALESCE(status_info, '') || '; 分析完成'
            END
        WHERE tid = %s
        """,
        (tid,),
    )
    cur.close()
    _record_task_event_tx(conn, tid, "分析完成", "analysis_daemon",
                          {"job_id": job.id, "outputs": result.get("outputs", [])})


def finalize_failure_tx(conn, lease_client, job, tid: str, error: str, retry: bool,
                        analyzer_version: str):
    if not lease_client.fail(
        job.id,
        retry=retry,
        analyzer_version=analyzer_version,
        last_error=error,
        conn=conn,
    ):
        raise RuntimeError("AnalysisJob 失败标记未生效：lease_owner 已变化")
    cur = conn.cursor()
    cur.execute(
        """
        UPDATE hotmethod_tasks
        SET analysis_status = 3,
            status_info = CASE
                WHEN COALESCE(status_info, '') = '' THEN %s
                ELSE COALESCE(status_info, '') || '; ' || %s
            END
        WHERE tid = %s
        """,
        (error[:1024], error[:1024], tid),
    )
    cur.close()
    _record_task_event_tx(conn, tid, error[:1024], "analysis_daemon",
                          {"job_id": job.id, "retryable": retry})


def run_job(dsn: str, lease_client: AnalysisLeaseClient, job, config_path: str,
            local_output_dir: str, registry) -> bool:
    """执行一个已领取的 AnalysisJob。"""
    import hotmethod_analyzer as hm

    tid = job.task_tid
    started_at = now_seconds()
    common = {
        "task_tid": tid,
        "job_id": job.id,
        "pipeline": job.pipeline,
        "attempt": job.attempt,
        "worker_id": lease_client.worker_id,
    }
    print(f"[analysis_daemon] 开始分析: job_id={job.id} tid={tid} "
          f"pipeline={job.pipeline} attempt={job.attempt}", file=sys.stderr)
    log_event("analysis_started", **common)

    stop_heartbeat = threading.Event()
    heartbeat_thread = _start_heartbeat(lease_client, job.id, stop_heartbeat)

    conn = None
    try:
        config = hm.load_config(config_path)
        db_cfg = config["database"]
        storage_cfg = config["storage"]
        bucket = storage_cfg["bucket"]

        conn = hm.connect_db(db_cfg["dsn"])
        task = hm.get_task(conn, tid)
        task_type = int(task.get("type", 0))
        analyzer = registry.require(task_type)
        analyzer_version = getattr(analyzer, "analyzer_version", ANALYZER_VERSION)

        hm.update_analysis_status(conn, tid, 1, "分析中")
        result = analyzer(
            conn, storage_cfg, task, bucket, tid, local_dir=local_output_dir, job=job
        )
        if result.get("manifest"):
            manifest_key = _upload_manifest(storage_cfg, bucket, tid, result["manifest"])
            if manifest_key:
                result.setdefault("outputs", []).append(manifest_key)

        storage_for_sizes, storage_ok = hm._connect_storage(storage_cfg)
        if not storage_ok:
            storage_for_sizes = None
        finalize_success_tx(conn, lease_client, job, tid, result, analyzer_version,
                            storage=storage_for_sizes, bucket=bucket)
        conn.commit()

        log_event("analysis_succeeded", **common,
                  analysis_duration_seconds=elapsed_seconds(started_at),
                  duration_seconds=elapsed_seconds(started_at),
                  outputs_count=len(result.get("outputs", [])))
        print(f"[analysis_daemon] ✅ 分析成功: tid={tid} "
              f"outputs={len(result.get('outputs', []))}", file=sys.stderr)
        return True

    except KeyError as e:
        error = f"未注册分析器: {e}"
        update_analysis_status(dsn, tid, 3, error)
        lease_client.fail(job.id, retry=False, analyzer_version=ANALYZER_VERSION, last_error=error)
        log_event("analysis_failed", **common,
                  analysis_duration_seconds=elapsed_seconds(started_at),
                  duration_seconds=elapsed_seconds(started_at),
                  error=str(e), retryable=False)
        print(f"[analysis_daemon] ❌ 不可重试失败: tid={tid} error={e}",
              file=sys.stderr)
        return False
    except SystemExit as e:
        code = e.code if isinstance(e.code, int) else 1
        ok = code == 0
        if ok:
            lease_client.complete(job.id, ANALYZER_VERSION)
            log_event("analysis_succeeded", **common,
                      analysis_duration_seconds=elapsed_seconds(started_at),
                      duration_seconds=elapsed_seconds(started_at),
                      exit_code=code)
        else:
            retry = should_retry(job)
            error = f"分析失败: exit={code}"
            update_analysis_status(dsn, tid, 3, error)
            lease_client.fail(job.id, retry=retry, analyzer_version=ANALYZER_VERSION, last_error=error)
            log_event("analysis_failed", **common,
                      analysis_duration_seconds=elapsed_seconds(started_at),
                      duration_seconds=elapsed_seconds(started_at),
                      exit_code=code, retryable=retry)
        return ok
    except Exception as e:
        retry = should_retry(job)
        if isinstance(e, AnalyzerInputError):
            retry = False
        elif isinstance(e, AnalyzerError):
            retry = bool(getattr(e, "retryable", True)) and should_retry(job)
        error = f"分析异常: {e}"
        try:
            if conn is not None:
                conn.rollback()
                finalize_failure_tx(conn, lease_client, job, tid, error, retry, ANALYZER_VERSION)
                conn.commit()
            else:
                update_analysis_status(dsn, tid, 3, error)
                lease_client.fail(job.id, retry=retry, analyzer_version=ANALYZER_VERSION, last_error=error)
        except Exception as finish_error:
            update_analysis_status(dsn, tid, 3, f"{error}; 完成失败: {finish_error}")
            lease_client.fail(job.id, retry=retry, analyzer_version=ANALYZER_VERSION, last_error=error)
        log_event("analysis_failed", **common,
                  analysis_duration_seconds=elapsed_seconds(started_at),
                  duration_seconds=elapsed_seconds(started_at),
                  error=str(e), retryable=retry)
        print(f"[analysis_daemon] ❌ 分析异常: tid={tid} error={e}", file=sys.stderr)
        return False
    finally:
        stop_heartbeat.set()
        heartbeat_thread.join(timeout=1)
        if conn is not None:
            try:
                conn.close()
            except Exception:
                pass


def main():
    parser = argparse.ArgumentParser(description="Analysis Daemon - 租约领取并分析任务")
    parser.add_argument("--interval", type=int, default=POLL_INTERVAL,
                        help=f"轮询间隔秒数 (默认: {POLL_INTERVAL})")
    parser.add_argument("--once", action="store_true",
                        help="只尝试领取一次，处理完已领取任务后退出")
    parser.add_argument("--lease-seconds", type=int, default=LEASE_SECONDS,
                        help=f"租约有效期秒数 (默认: {LEASE_SECONDS})")
    parser.add_argument("--worker-id", default=os.environ.get("ANALYSIS_WORKER_ID", ""),
                        help="当前 worker 标识，默认自动生成")
    parser.add_argument("--config", default=os.environ.get("ANALYSIS_CONFIG", ""),
                        help="配置文件路径")
    parser.add_argument("--local-output-dir", default=os.environ.get("LOCAL_OUTPUT_DIR", ""),
                        help="MinIO 不可用时的本地产物目录")
    args = parser.parse_args()

    script_dir = os.path.dirname(os.path.abspath(__file__))
    config_path = args.config or os.path.join(script_dir, "config.ini")
    lease_client = AnalysisLeaseClient(
        PG_DSN,
        worker_id=args.worker_id or None,
        lease_seconds=args.lease_seconds,
    )
    registry = build_default_registry()

    print(f"[analysis_daemon] 启动 (interval={args.interval}s, "
          f"worker={lease_client.worker_id}, lease={args.lease_seconds}s, "
          f"max_attempts={MAX_ANALYSIS_ATTEMPTS}, "
          f"task_types={registry.task_types()})", file=sys.stderr)

    while True:
        claim_started_at = now_seconds()
        try:
            job = lease_client.claim_one()
            log_event("analysis_claimed" if job is not None else "analysis_claim_empty",
                      worker_id=lease_client.worker_id,
                      job_id=getattr(job, "id", None),
                      task_tid=getattr(job, "task_tid", None),
                      pipeline=getattr(job, "pipeline", None),
                      attempt=getattr(job, "attempt", None),
                      analysis_claim_latency_seconds=elapsed_seconds(claim_started_at),
                      duration_seconds=elapsed_seconds(claim_started_at))
        except Exception as e:
            log_event("analysis_claim_failed",
                      worker_id=lease_client.worker_id,
                      analysis_claim_latency_seconds=elapsed_seconds(claim_started_at),
                      duration_seconds=elapsed_seconds(claim_started_at),
                      error=str(e))
            print(f"[analysis_daemon] 领取 AnalysisJob 失败: {e}", file=sys.stderr)
            job = None

        if job is not None:
            run_job(PG_DSN, lease_client, job, config_path, args.local_output_dir, registry)
        elif args.once:
            print("[analysis_daemon] --once 模式，没有可领取任务，退出", file=sys.stderr)
            break

        if args.once:
            print("[analysis_daemon] --once 模式，退出", file=sys.stderr)
            break

        time.sleep(args.interval)


if __name__ == "__main__":
    main()
