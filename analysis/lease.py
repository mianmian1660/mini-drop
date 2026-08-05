#!/usr/bin/env python3
# ============================================================
# lease.py — AnalysisJob 租约领取/续租/完成标记
# ============================================================

import socket
import time
import uuid
from dataclasses import dataclass
from typing import Optional


STATUS_PENDING = "pending"
STATUS_RUNNING = "running"
STATUS_SUCCESS = "success"
STATUS_FAILED = "failed"
STATUS_RETRY = "retry"


@dataclass
class AnalysisJob:
    id: int
    task_tid: str
    pipeline: str
    status: str
    attempt: int
    max_attempts: int = 3
    input_artifact_ids: object = None


def default_worker_id() -> str:
    return f"{socket.gethostname()}-{uuid.uuid4().hex[:8]}"


class AnalysisLeaseClient:
    """基于 PostgreSQL 行锁的 AnalysisJob 租约客户端。"""

    def __init__(self, dsn: str, worker_id: Optional[str] = None,
                 lease_seconds: int = 300):
        self.dsn = dsn
        self.worker_id = worker_id or default_worker_id()
        self.lease_seconds = lease_seconds

    def connect(self):
        import psycopg2
        return psycopg2.connect(self.dsn)

    def claim_one(self) -> Optional[AnalysisJob]:
        """
        领取一条可处理作业。

        使用 FOR UPDATE SKIP LOCKED，多个 worker 并发时不会抢同一行。
        retry 和租约过期的 running 作业也会被重新领取。
        """
        conn = self.connect()
        try:
            conn.autocommit = False
            cur = conn.cursor()
            cur.execute(
                """
                WITH picked AS (
                    SELECT id
                    FROM analysis_jobs
                    WHERE
                        status IN (%s, %s)
                        OR (status = %s AND lease_expires_at < NOW())
                    ORDER BY created_at ASC
                    FOR UPDATE SKIP LOCKED
                    LIMIT 1
                )
                UPDATE analysis_jobs AS j
                SET
                    status = %s,
                    lease_owner = %s,
                    lease_expires_at = NOW() + (%s || ' seconds')::interval,
                    attempt = attempt + 1,
                    updated_at = NOW()
                FROM picked
                WHERE j.id = picked.id
                RETURNING j.id, j.task_tid, j.pipeline, j.status, j.attempt,
                          COALESCE(j.max_attempts, 3), j.input_artifact_ids
                """,
                (
                    STATUS_PENDING,
                    STATUS_RETRY,
                    STATUS_RUNNING,
                    STATUS_RUNNING,
                    self.worker_id,
                    self.lease_seconds,
                ),
            )
            row = cur.fetchone()
            conn.commit()
            cur.close()
            if row is None:
                return None
            return AnalysisJob(
                id=row[0],
                task_tid=row[1],
                pipeline=row[2],
                status=row[3],
                attempt=row[4],
                max_attempts=row[5],
                input_artifact_ids=row[6],
            )
        except Exception:
            conn.rollback()
            raise
        finally:
            conn.close()

    def heartbeat(self, job_id: int) -> bool:
        """续租当前 worker 持有的 running 作业。"""
        conn = self.connect()
        try:
            cur = conn.cursor()
            cur.execute(
                """
                UPDATE analysis_jobs
                SET lease_expires_at = NOW() + (%s || ' seconds')::interval,
                    updated_at = NOW()
                WHERE id = %s AND status = %s AND lease_owner = %s
                """,
                (self.lease_seconds, job_id, STATUS_RUNNING, self.worker_id),
            )
            ok = cur.rowcount == 1
            conn.commit()
            cur.close()
            return ok
        finally:
            conn.close()

    def complete(self, job_id: int, analyzer_version: str = "",
                 conn=None, output_artifact_ids=None) -> bool:
        return self._finish(
            job_id,
            STATUS_SUCCESS,
            analyzer_version,
            conn=conn,
            output_artifact_ids=output_artifact_ids,
        )

    def fail(self, job_id: int, retry: bool = True,
             analyzer_version: str = "", last_error: str = "", conn=None) -> bool:
        return self._finish(
            job_id,
            STATUS_RETRY if retry else STATUS_FAILED,
            analyzer_version,
            last_error=last_error,
            conn=conn,
        )

    def _finish(self, job_id: int, status: str, analyzer_version: str,
                last_error: str = "", conn=None, output_artifact_ids=None) -> bool:
        owns_conn = conn is None
        if owns_conn:
            conn = self.connect()
        try:
            cur = conn.cursor()
            cur.execute(
                """
                UPDATE analysis_jobs
                SET status = %s,
                    lease_owner = '',
                    lease_expires_at = NULL,
                    analyzer_version = CASE WHEN %s = '' THEN analyzer_version ELSE %s END,
                    last_error = CASE WHEN %s = '' THEN last_error ELSE %s END,
                    output_artifact_ids = CASE WHEN %s::jsonb IS NULL THEN output_artifact_ids ELSE %s::jsonb END,
                    updated_at = NOW()
                WHERE id = %s AND lease_owner = %s
                """,
                (
                    status,
                    analyzer_version,
                    analyzer_version,
                    last_error[:1024],
                    last_error[:1024],
                    output_artifact_ids,
                    output_artifact_ids,
                    job_id,
                    self.worker_id,
                ),
            )
            ok = cur.rowcount == 1
            cur.close()
            if owns_conn:
                conn.commit()
            return ok
        except Exception:
            if owns_conn:
                conn.rollback()
            raise
        finally:
            if owns_conn:
                conn.close()


class LeaseHeartbeat:
    """长分析过程中按间隔续租；daemon 当前同步执行，先用轻量轮询即可。"""

    def __init__(self, client: AnalysisLeaseClient, job_id: int,
                 interval_seconds: int = 60):
        self.client = client
        self.job_id = job_id
        self.interval_seconds = interval_seconds
        self._last = 0.0

    def maybe_heartbeat(self):
        now = time.time()
        if now - self._last >= self.interval_seconds:
            self.client.heartbeat(self.job_id)
            self._last = now
