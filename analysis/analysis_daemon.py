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
import re
import shutil
import sys
import threading
import time

from analyzer_contract import AnalyzerError, AnalyzerInputError, AnalyzerTemporaryError
from analyzer_registry import build_default_registry
from lease import AnalysisLeaseClient
from observability import elapsed_seconds, log_event, now_seconds
from job_context import get as job_context_get, use as use_job_context


POLL_INTERVAL = 5
LEASE_SECONDS = 300
MAX_ANALYSIS_ATTEMPTS = int(os.environ.get("MAX_ANALYSIS_ATTEMPTS", "3"))
ANALYZER_VERSION = os.environ.get("ANALYZER_VERSION", "b1-lease")
PG_DSN = os.environ.get(
    "PG_DSN", "host=localhost user=postgres password=dev dbname=drop sslmode=disable"
)
WORK_ROOT = os.environ.get("ANALYSIS_WORK_ROOT", "/tmp/mini-drop-analysis")
STALE_WORKSPACE_SECONDS = 2 * 60 * 60


def cleanup_stale_workspaces():
    """Remove only abandoned job directories; active jobs always own their directory."""
    try:
        os.makedirs(WORK_ROOT, exist_ok=True)
        cutoff = time.time() - STALE_WORKSPACE_SECONDS
        for entry in os.scandir(WORK_ROOT):
            if entry.is_dir(follow_symlinks=False) and entry.stat().st_mtime < cutoff:
                shutil.rmtree(entry.path, ignore_errors=True)
                log_event("analysis_workspace_stale_removed", workspace=entry.name)
    except Exception as exc:
        print(f"[analysis_daemon] 清理遗留工作区失败: {exc}", file=sys.stderr)


def _upsert_blob_row(conn, descriptor: dict) -> int:
    """按内容唯一键 (logical_sha256, format, compression) upsert storage_blobs。

    内容寻址去重：同内容复用同一 blob。登记方与 cleaner 共用行锁协议：
    deleting Blob 不允许新增引用，等待分析任务重试；deleted/failed Blob 只有在
    本次 CAS 上传完成后才复活。返回 blob_id。
    """
    cur = conn.cursor()
    cur.execute(
        """
        INSERT INTO storage_blobs (object_key, logical_sha256, stored_sha256,
            stored_size, logical_size, format, schema_version, compression,
            content_encoding, content_type, status, created_at, updated_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, 'ready', NOW(), NOW())
        ON CONFLICT (logical_sha256, format, compression)
          WHERE logical_sha256 IS NOT NULL
        DO NOTHING
        RETURNING id
        """,
        (
            descriptor["blob_key"],
            descriptor.get("logical_sha256"),
            descriptor.get("stored_sha256"),
            descriptor.get("stored_size", 0),
            descriptor.get("logical_size", 0),
            descriptor.get("format"),
            descriptor.get("schema_version"),
            descriptor.get("compression"),
            descriptor.get("content_encoding"),
            descriptor.get("content_type"),
        ),
    )
    row = cur.fetchone()
    if row:
        return row[0]

    cur.execute(
        """
        SELECT id, status
        FROM storage_blobs
        WHERE logical_sha256 = %s AND format = %s AND compression = %s
        FOR UPDATE
        """,
        (descriptor.get("logical_sha256"), descriptor.get("format"),
         descriptor.get("compression")),
    )
    row = cur.fetchone()
    if not row:
        raise AnalyzerTemporaryError("CAS Blob 并发登记失败，请重试")
    blob_id, status = row[0], row[1]
    if status in ("deleting", "uploading"):
        raise AnalyzerTemporaryError("CAS Blob 正在变更状态，请重试")
    if status != "ready":
        cur.execute(
            """
            UPDATE storage_blobs
            SET object_key = %s, stored_sha256 = %s, stored_size = %s,
                logical_size = %s, schema_version = %s,
                content_encoding = %s, content_type = %s,
                status = 'ready', deleted_at = NULL, delete_reason = NULL,
                delete_attempts = 0, next_delete_attempt_at = NULL,
                last_delete_error = '', updated_at = NOW()
            WHERE id = %s AND status <> 'deleting'
            """,
            (descriptor["blob_key"], descriptor.get("stored_sha256"),
             descriptor.get("stored_size", 0), descriptor.get("logical_size", 0),
             descriptor.get("schema_version"), descriptor.get("content_encoding"),
             descriptor.get("content_type"), blob_id),
        )
        if getattr(cur, "rowcount", 1) != 1:
            raise AnalyzerTemporaryError("CAS Blob 状态发生变化，请重试")
    return blob_id


def _insert_artifact_from_descriptor(conn, tid: str, attempt_id: int, job_id: int,
                                     descriptor: dict, blob_id: int) -> int:
    """按 descriptor 登记 artifact（带 blob_id / 双大小 / 双哈希语义）。

    阶段 4：登记 analysis_job_id 与 logical_name；object_key 已是
    generation 前缀的逻辑路径，跨代不冲突，同代重试由唯一键覆盖。
    """
    cur = conn.cursor()
    cur.execute(
        """
        INSERT INTO artifacts (task_tid, attempt_id, analysis_job_id, logical_name,
            kind, object_key, format,
            schema_version, blob_id, content_type, status, size, logical_size,
            compression, sha256, created_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, 'ready', %s, %s, %s, %s, NOW())
        ON CONFLICT (task_tid, kind, object_key) DO UPDATE
        SET attempt_id = EXCLUDED.attempt_id,
            analysis_job_id = EXCLUDED.analysis_job_id,
            logical_name = EXCLUDED.logical_name,
            status = EXCLUDED.status,
            size = EXCLUDED.size,
            logical_size = EXCLUDED.logical_size,
            compression = EXCLUDED.compression,
            blob_id = EXCLUDED.blob_id,
            format = EXCLUDED.format,
            schema_version = EXCLUDED.schema_version,
            sha256 = EXCLUDED.sha256
        WHERE artifacts.deleted_at IS NULL
        RETURNING id
        """,
        (
            tid,
            attempt_id,
            job_id or None,
            descriptor.get("logical_name") or descriptor["object_key"].rsplit("/", 1)[-1],
            descriptor["kind"],
            descriptor["object_key"],
            descriptor.get("format"),
            descriptor.get("schema_version"),
            blob_id or None,
            descriptor.get("content_type") or "application/octet-stream",
            descriptor.get("stored_size", 0),
            descriptor.get("logical_size", 0),
            descriptor.get("compression"),
            descriptor.get("stored_sha256"),
        ),
    )
    row = cur.fetchone()
    return row[0] if row else 0


def _insert_artifact_from_key(conn, tid: str, attempt_id: int, job_id: int, key: str,
                              storage=None, bucket=None) -> int:
    """兼容路径：无 descriptor 的历史 analyzer 产物（字符串 key）。"""
    name = key.rsplit("/", 1)[-1]
    kind = "MANIFEST" if name == "manifest.json" else "RESULT" if name.endswith((".svg", ".json", ".md", ".html")) else "INTERMEDIATE"
    content_type = "application/json" if name.endswith(".json") else "image/svg+xml" if name.endswith(".svg") else "text/markdown" if name.endswith(".md") else "application/octet-stream"
    size = 0
    if storage is not None and bucket:
        size = storage.stat_object(bucket, key) or 0
    cur = conn.cursor()
    cur.execute(
        """
        INSERT INTO artifacts (task_tid, attempt_id, analysis_job_id, logical_name,
            kind, object_key, content_type, status, size, created_at)
        VALUES (%s, %s, %s, %s, %s, %s, %s, 'ready', %s, NOW())
        ON CONFLICT (task_tid, kind, object_key) DO UPDATE
        SET attempt_id = EXCLUDED.attempt_id,
            analysis_job_id = EXCLUDED.analysis_job_id,
            logical_name = EXCLUDED.logical_name,
            status = EXCLUDED.status,
            size = EXCLUDED.size
        WHERE artifacts.deleted_at IS NULL
        RETURNING id
        """,
        (tid, attempt_id, job_id or None, name, kind, key, content_type, size),
    )
    row = cur.fetchone()
    if row:
        return row[0]
    if storage is not None and bucket:
        # 唯一键命中了 deleted tombstone：分析产物已先上传，同步删除同 key 对象，
        # 避免留下不可见孤儿。
        try:
            storage.delete_object(bucket, key)
            log_event("artifact_tombstone_upload_removed", task_tid=tid, object_key=key)
        except Exception as exc:
            log_event("artifact_tombstone_upload_remove_failed", task_tid=tid,
                      object_key=key, error=str(exc))
    return 0


def record_result_artifacts(conn, tid: str, attempt_id: int, job_id: int,
                            generation: int, outputs, manifest=None,
                            storage=None, bucket=None):
    """Persist analyzer outputs as Artifact metadata without storing URLs.

    阶段二：outputs 元素既可以是字符串 key（兼容旧 analyzer），也可以是
    Artifact descriptor（dict，见 artifact_descriptor.build_descriptor）。
    descriptor 会同时登记 storage_blobs（内容去重）与 artifacts（blob_id 引用）。

    阶段 4：全部登记带 analysis_job_id 与 logical_name；attempt_id 取作业的
    attempt_id（不再猜测"最新 attempt"）。
    """
    descriptors = []
    keys = []
    for value in (outputs or []):
        if isinstance(value, dict) and value.get("object_key") and value.get("blob_key"):
            descriptors.append(value)
        elif isinstance(value, str) and ("/" in value):
            keys.append(value)
    if manifest:
        prefix = str(job_context_get("output_prefix", "") or "").strip() or tid
        manifest_key = f"{prefix}/manifest.json"
        if manifest_key not in keys:
            keys.append(manifest_key)
    if not descriptors and not keys:
        return []
    cur = conn.cursor()
    artifact_ids = []
    for descriptor in descriptors:
        blob_id = _upsert_blob_row(conn, descriptor)
        aid = _insert_artifact_from_descriptor(conn, tid, attempt_id, job_id, descriptor, blob_id)
        if aid:
            artifact_ids.append(aid)
        else:
            # 唯一键命中 deleted tombstone：只拒绝逻辑引用。blob_key 是共享 CAS
            # 对象，绝不能在未检查其它引用时直接删除；服务端孤儿对账会在安全
            # 宽限期后回收真正的零引用 Blob。
            log_event("artifact_tombstone_blob_unreferenced", task_tid=tid,
                      object_key=descriptor["object_key"], blob_key=descriptor["blob_key"])
    for key in keys:
        aid = _insert_artifact_from_key(conn, tid, attempt_id, job_id, key, storage, bucket)
        if aid:
            artifact_ids.append(aid)
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
            json.dumps(_json_safe(payload or {}), ensure_ascii=False),
            tid,
        ),
    )
    cur.close()


def _json_safe(value):
    """递归移除不可 JSON 序列化的内容（descriptor 的 _payload 是 bytes）。"""
    if isinstance(value, dict):
        return {k: _json_safe(v) for k, v in value.items() if k != "_payload"}
    if isinstance(value, (list, tuple)):
        return [_json_safe(v) for v in value]
    if isinstance(value, bytes):
        return "<bytes:%d>" % len(value)
    return value


def _upload_manifest(storage_cfg: dict, bucket: str, tid: str, manifest: dict) -> str:
    import hotmethod_analyzer as hm

    try:
        storage, storage_ok = hm._connect_storage(storage_cfg)
        if not storage_ok:
            raise RuntimeError("storage unavailable")
        return hm._upload_output(storage, bucket, tid, "manifest.json", manifest, "application/json")
    except Exception as exc:
        raise RuntimeError(f"manifest 上传失败: {exc}") from exc


# 阶段 4：被替换旧代 RESULT/INTERMEDIATE 保留时长（小时）。
SUPERSEDED_RESULT_HOURS = int(os.environ.get("ARTIFACT_SUPERSEDED_RESULT_HOURS", "72"))


def _switch_active_job_tx(conn, job, tid: str) -> bool:
    """同一事务内完成 active 切换与旧代降级（阶段 4）。

    - trigger=manual：总是切换（人工明确选择历史 attempt 的重分析成功后允许切换）。
    - trigger=initial：仅当本作业 attempt 不早于当前 active 作业的 attempt 才切换
      （迟到的旧 attempt 自动分析不覆盖更新 attempt 的结果）。
    - 切换时（同一事务）：新 job 已 complete → 旧 job 写 superseded_at →
      旧代 RESULT/INTERMEDIATE 降级为 result_superseded（72h，到期=切换时间+72h）→
      manifest 保持永久（不动）→ 更新 active_analysis_job_id。
      pinned 任务由任务级 pin 机制保护全部代次。
    返回是否切换。
    """
    cur = conn.cursor()
    cur.execute("SELECT active_analysis_job_id FROM hotmethod_tasks WHERE tid = %s FOR UPDATE", (tid,))
    row = cur.fetchone()
    prev_active = int(row[0]) if row and row[0] else 0
    if prev_active == job.id:
        cur.close()
        return True

    should_switch = True
    if getattr(job, "trigger", "initial") == "initial" and prev_active:
        cur.execute("SELECT attempt_id FROM analysis_jobs WHERE id = %s", (prev_active,))
        prow = cur.fetchone()
        prev_attempt = int(prow[0]) if prow and prow[0] else 0
        if prev_attempt > int(getattr(job, "attempt_id", 0) or 0):
            should_switch = False

    if should_switch:
        if prev_active:
            cur.execute(
                "UPDATE analysis_jobs SET superseded_at = NOW(), updated_at = NOW() "
                "WHERE id = %s AND superseded_at IS NULL",
                (prev_active,),
            )
            cur.execute(
                """
                UPDATE artifacts
                SET retention = 'result_superseded',
                    expires_at = NOW() + (%s || ' hours')::interval,
                    retention_task_state = 'done',
                    updated_at = NOW()
                WHERE analysis_job_id = %s AND kind IN ('RESULT', 'INTERMEDIATE')
                  AND deleted_at IS NULL AND status = 'ready'
                """,
                (SUPERSEDED_RESULT_HOURS, prev_active),
            )
        cur.execute(
            "UPDATE hotmethod_tasks SET active_analysis_job_id = %s WHERE tid = %s",
            (job.id, tid),
        )
        log_event("analysis_active_switched", task_tid=tid,
                  job_id=job.id, generation=getattr(job, "generation", 0),
                  attempt_id=getattr(job, "attempt_id", 0),
                  prev_active=prev_active, trigger=getattr(job, "trigger", "initial"))
    else:
        # A successful late initial job that loses the active election is also
        # a superseded generation. Keeping it as a normal 30-day result would
        # silently defeat the small-disk retention policy.
        cur.execute(
            "UPDATE analysis_jobs SET superseded_at = NOW(), updated_at = NOW() "
            "WHERE id = %s AND superseded_at IS NULL",
            (job.id,),
        )
        cur.execute(
            """
            UPDATE artifacts
            SET retention = 'result_superseded',
                expires_at = NOW() + (%s || ' hours')::interval,
                retention_task_state = 'done',
                updated_at = NOW()
            WHERE analysis_job_id = %s AND kind IN ('RESULT', 'INTERMEDIATE')
              AND deleted_at IS NULL AND status = 'ready'
            """,
            (SUPERSEDED_RESULT_HOURS, job.id),
        )
        log_event("analysis_active_kept_late_attempt", task_tid=tid,
                  job_id=job.id, attempt_id=getattr(job, "attempt_id", 0),
                  active_job_id=prev_active)
    cur.close()
    return should_switch


def finalize_success_tx(conn, lease_client, job, tid: str, result: dict,
                        analyzer_version: str, storage=None, bucket=None):
    manifest = result.get("manifest") or {}
    attempt_id = int(getattr(job, "attempt_id", 0) or 0)
    generation = int(getattr(job, "generation", 0) or 0)
    artifact_ids = record_result_artifacts(conn, tid, attempt_id, job.id, generation,
                                           result.get("outputs", []), manifest=manifest,
                                           storage=storage, bucket=bucket)
    if not lease_client.complete(
        job.id,
        analyzer_version,
        conn=conn,
        output_artifact_ids=json.dumps(artifact_ids),
    ):
        raise RuntimeError("AnalysisJob 完成失败：lease_owner 已变化")
    # 阶段 4：active 切换 + 旧代降级（与 job 完成同一事务；失败整体回滚不切 active）。
    _switch_active_job_tx(conn, job, tid)
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
                          {"job_id": job.id, "generation": generation,
                           "attempt_id": attempt_id, "outputs": result.get("outputs", [])})


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


def _safe_token(value: str) -> str:
    """把 pipeline/analyzer_version 规范化为 [A-Za-z0-9._-]（阶段 4 输出前缀）。"""
    return re.sub(r"[^A-Za-z0-9._-]", "_", str(value or ""))


def _build_job_context(job, tid: str, analyzer_version: str, workspace: str):
    """Build immutable per-job context without mutating process environment.

    ANALYSIS_OUTPUT_PREFIX = tasks/{tid}/analysis/{pipeline}/{version}/g{generation}
    （generation>0 且 pipeline 非空时；旧链路回退 {tid} 形态）。
    """
    generation = int(getattr(job, "generation", 0) or 0)
    pipeline = getattr(job, "pipeline", "") or ""
    prefix = ""
    if generation > 0 and pipeline:
        prefix = "tasks/{tid}/analysis/{p}/{v}/g{gen}".format(
            tid=tid,
            p=_safe_token(pipeline),
            v=_safe_token(analyzer_version),
            gen=generation,
        )
    return {
        "job_id": int(getattr(job, "id", 0) or 0),
        "attempt_id": int(getattr(job, "attempt_id", 0) or 0),
        "generation": generation,
        "pipeline": pipeline,
        "analyzer_version": analyzer_version,
        "output_prefix": prefix or tid,
        "work_dir": workspace,
    }


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
        "generation": getattr(job, "generation", 0),
        "attempt_id": getattr(job, "attempt_id", 0),
    }
    print(f"[analysis_daemon] 开始分析: job_id={job.id} tid={tid} "
          f"pipeline={job.pipeline} attempt={job.attempt} "
          f"generation={common['generation']} attempt_id={common['attempt_id']}",
          file=sys.stderr)
    log_event("analysis_started", **common)

    stop_heartbeat = threading.Event()
    heartbeat_thread = _start_heartbeat(lease_client, job.id, stop_heartbeat)

    conn = None
    workspace = os.path.join(WORK_ROOT, f"{job.id}-{tid}-{job.attempt}")
    os.makedirs(workspace, exist_ok=True)
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

        immutable_context = _build_job_context(job, tid, analyzer_version, workspace)
        with use_job_context(immutable_context):
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
        # Successful, failed and cancelled jobs never retain raw local files. A
        # timed-out external worker may leave its directory behind; the next
        # daemon sweep removes it after the two-hour safety window.
        shutil.rmtree(workspace, ignore_errors=True)


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
    cleanup_stale_workspaces()

    print(f"[analysis_daemon] 启动 (interval={args.interval}s, "
          f"worker={lease_client.worker_id}, lease={args.lease_seconds}s, "
          f"max_attempts={MAX_ANALYSIS_ATTEMPTS}, "
          f"task_types={registry.task_types()})", file=sys.stderr)

    while True:
        cleanup_stale_workspaces()
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
