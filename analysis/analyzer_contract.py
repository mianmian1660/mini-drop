#!/usr/bin/env python3
"""Analyzer contract and shared production guards."""

import hashlib
import json
import os
import threading
import time
from dataclasses import dataclass, field
from typing import Callable, Dict, List, Optional


class AnalyzerError(Exception):
    """Base analyzer error with retry semantics."""

    retryable = True
    code = "ANALYZER_ERROR"


class AnalyzerInputError(AnalyzerError):
    """Invalid or missing RAW input; retrying the same artifact will not help."""

    retryable = False
    code = "ANALYZER_INPUT_INVALID"


class AnalyzerTemporaryError(AnalyzerError):
    """Temporary infrastructure or tool failure."""

    retryable = True
    code = "ANALYZER_TEMPORARY_FAILURE"


@dataclass(frozen=True)
class InputSpec:
    name: str
    formats: List[str]
    max_size: int = 256 * 1024 * 1024
    required: bool = True


@dataclass(frozen=True)
class OutputSpec:
    name: str
    kind: str = "RESULT"
    content_type: str = "application/octet-stream"


@dataclass
class AnalyzerResult:
    outputs: List[str] = field(default_factory=list)
    presigned_urls: Dict[str, str] = field(default_factory=dict)
    local_files: List[object] = field(default_factory=list)
    input_artifacts: List[dict] = field(default_factory=list)
    manifest: Dict[str, object] = field(default_factory=dict)
    source: str = "object_storage"

    @classmethod
    def from_legacy(cls, value: dict) -> "AnalyzerResult":
        return cls(
            outputs=list((value or {}).get("outputs") or []),
            presigned_urls=dict((value or {}).get("presigned_urls") or {}),
            local_files=list((value or {}).get("local_files") or []),
            source=(value or {}).get("source") or "object_storage",
        )

    def to_legacy(self) -> dict:
        return {
            "outputs": self.outputs,
            "presigned_urls": self.presigned_urls,
            "local_files": self.local_files,
            "manifest": self.manifest,
            "source": self.source,
        }


class BaseAnalyzer:
    name = "base"
    pipeline = "unknown"
    input_spec = InputSpec("perf.data", ["perf"])
    output_specs: List[OutputSpec] = []
    timeout_seconds = 300
    retryable = True
    analyzer_version = "stage4-contract"

    def validate(self, context: dict) -> dict:
        return validate_raw_input(context, self.input_spec)

    def prepare(self, context: dict, validated_input: dict) -> dict:
        prepared = dict(context)
        prepared["validated_input"] = validated_input
        return prepared

    def analyze(self, prepared: dict) -> AnalyzerResult:
        raise NotImplementedError

    def build_manifest(self, prepared: dict, result: AnalyzerResult) -> dict:
        return build_manifest(
            task_id=prepared["tid"],
            job_id=getattr(prepared.get("job"), "id", None),
            pipeline=self.pipeline,
            analyzer_name=self.name,
            analyzer_version=self.analyzer_version,
            input_artifacts=result.input_artifacts,
            outputs=result.outputs,
        )

    def cleanup(self, prepared: dict, result: Optional[AnalyzerResult] = None):
        return None

    def run(self, conn, storage_cfg, task, bucket, tid, local_dir="", job=None) -> dict:
        context = {
            "conn": conn,
            "storage_cfg": storage_cfg,
            "task": task,
            "bucket": bucket,
            "tid": tid,
            "local_dir": local_dir,
            "job": job,
        }
        validated = self.validate(context)
        prepared = self.prepare(context, validated)
        result = None
        try:
            result = self.analyze(prepared)
            result.input_artifacts = validated.get("input_artifacts", [])
            result.manifest = self.build_manifest(prepared, result)
            return result.to_legacy()
        finally:
            self.cleanup(prepared, result)

    def __call__(self, conn, storage_cfg, task, bucket, tid, local_dir="", job=None):
        return self.run(conn, storage_cfg, task, bucket, tid, local_dir, job=job)


class LegacyFunctionAnalyzer(BaseAnalyzer):
    def __init__(
        self,
        *,
        name: str,
        pipeline: str,
        input_spec: InputSpec,
        output_specs: List[OutputSpec],
        analyze_func: Callable,
        timeout_seconds: int = 300,
        retryable: bool = True,
        analyzer_version: str = "stage4-contract",
    ):
        self.name = name
        self.pipeline = pipeline
        self.input_spec = input_spec
        self.output_specs = output_specs
        self._analyze_func = analyze_func
        self.timeout_seconds = timeout_seconds
        self.retryable = retryable
        self.analyzer_version = analyzer_version

    def analyze(self, prepared: dict) -> AnalyzerResult:
        # 之前这里是"调用返回之后才比较耗时"，如果 _analyze_func 内部真的
        # 卡死（比如某个子进程调用忘了传 timeout=），这个检查永远等不到
        # 执行的机会——timeout_seconds 全是摆设，daemon 的单线程处理循环会
        # 被永久卡死。现在把调用放进一个后台线程，用 join(timeout) 真正
        # 限定 daemon 愿意等多久：超时就直接判失败、把控制权交还给调用方，
        # 不再无限等待。
        #
        # 代价：Python 没有安全的手段从外部强行杀死一个线程，所以卡住的
        # 后台线程本身还会继续跑到它自己内部的阻塞点解除为止（比如子进程
        # 自己的 timeout= 到期），期间占用的内存/fd 不会立刻释放——这是
        # "daemon 不再永久卡死"和"资源立刻回收"之间的权衡，优先保证前者。
        # 真正杜绝资源泄漏需要把 _analyze_func 整个放进子进程
        # (multiprocessing)，可以用 terminate()/kill()，但那是更大的改动，
        # 这次先解决"永久卡死"这个更紧急的问题。
        result_box = {}
        error_box = {}

        def _run():
            try:
                result_box["legacy"] = self._analyze_func(
                    prepared["conn"],
                    prepared["storage_cfg"],
                    prepared["task"],
                    prepared["bucket"],
                    prepared["tid"],
                    prepared.get("local_dir", ""),
                )
            except BaseException as exc:  # noqa: BLE001 - 需要把任何异常带回主线程
                error_box["exc"] = exc

        worker = threading.Thread(target=_run, daemon=True)
        started = time.time()
        worker.start()
        worker.join(self.timeout_seconds)
        if worker.is_alive():
            raise AnalyzerTemporaryError(
                f"分析超时: 已等待 {self.timeout_seconds}s 仍未返回，放弃等待（后台线程可能仍在运行）"
            )
        if "exc" in error_box:
            exc = error_box["exc"]
            if isinstance(exc, AnalyzerError):
                raise exc
            raise AnalyzerTemporaryError(str(exc)) from exc
        elapsed = time.time() - started
        if elapsed > self.timeout_seconds:
            raise AnalyzerTemporaryError(f"分析超时: {self.timeout_seconds}s")
        legacy = result_box.get("legacy")
        result = AnalyzerResult.from_legacy(legacy)
        if not result.outputs:
            raise AnalyzerInputError(f"{self.name} 未生成任何结果产物")
        if prepared.get("validated_input", {}).get("source") == "local_fallback":
            result.source = "local_fallback"
        return result


def validate_raw_input(context: dict, spec: InputSpec) -> dict:
    """Validate RAW artifact metadata and bytes when object storage is available."""
    tid = context["tid"]
    conn = context.get("conn")
    local_dir = context.get("local_dir") or ""
    rows = _load_raw_artifacts(conn, tid, getattr(context.get("job"), "input_artifact_ids", None))
    if not rows and local_dir:
        return {"source": "local_fallback", "input_artifacts": []}
    if not rows and spec.required:
        raise AnalyzerInputError(f"缺少 RAW artifact: {spec.name}")

    storage = _connect_storage(context.get("storage_cfg") or {})
    bucket = context.get("bucket")
    validated = []
    for row in rows:
        key = row.get("object_key") or ""
        if not key:
            raise AnalyzerInputError("RAW artifact 缺少 object_key")
        size = int(row.get("size") or 0)
        if size < 0 or size > spec.max_size:
            raise AnalyzerInputError(f"RAW artifact 超过大小上限: {size} > {spec.max_size}")
        if not _key_matches_format(key, spec.formats):
            raise AnalyzerInputError(f"RAW artifact 格式不支持: {key}")
        data = None
        if storage is not None and bucket:
            data = storage.get_object(bucket, key)
            if data is None:
                raise AnalyzerTemporaryError(f"RAW artifact 下载失败: {key}")
            if len(data) == 0:
                raise AnalyzerInputError(f"RAW artifact 为空: {key}")
            if len(data) > spec.max_size:
                raise AnalyzerInputError(f"RAW artifact 超过大小上限: {len(data)} > {spec.max_size}")
            expected = _expected_sha(row)
            actual = hashlib.sha256(data).hexdigest()
            if expected and expected.lower() != actual.lower():
                raise AnalyzerInputError(f"RAW artifact sha256 不匹配: {key}")
            if size and size != len(data):
                raise AnalyzerInputError(f"RAW artifact size 不匹配: {key}")
            _validate_manifest(storage, bucket, row.get("manifest_key"), key, actual, len(data))
            size = len(data)
        elif not local_dir:
            raise AnalyzerTemporaryError("对象存储不可用，且未启用本地 fallback")
        validated.append({
            "id": row.get("id"),
            "object_key": key,
            "size": size,
            "sha256": _expected_sha(row),
            "manifest_key": row.get("manifest_key") or "",
        })
    return {"source": "object_storage", "input_artifacts": validated}


def build_manifest(
    *,
    task_id: str,
    job_id: Optional[int],
    pipeline: str,
    analyzer_name: str,
    analyzer_version: str,
    input_artifacts: List[dict],
    outputs: List[str],
) -> dict:
    return {
        "task_id": task_id,
        "job_id": job_id,
        "pipeline": pipeline,
        "analyzer": analyzer_name,
        "analyzer_version": analyzer_version,
        "generated_at_unix": int(time.time()),
        "input_artifacts": input_artifacts,
        "output_artifacts": [
            {
                "object_key": output,
                "name": str(output).rsplit("/", 1)[-1],
                "kind": "RESULT" if str(output).endswith((".svg", ".json", ".md", ".html")) else "INTERMEDIATE",
                "content_type": _content_type(str(output)),
            }
            for output in outputs
            if isinstance(output, str)
        ],
    }


def _load_raw_artifacts(conn, tid: str, input_artifact_ids=None) -> List[dict]:
    if conn is None:
        return []
    try:
        cursor_kwargs = {}
        try:
            import psycopg2.extras
            cursor_kwargs["cursor_factory"] = psycopg2.extras.RealDictCursor
        except Exception:
            cursor_kwargs = {}
        cur = conn.cursor(**cursor_kwargs)
        ids = _json_ids(input_artifact_ids)
        if ids:
            cur.execute(
                """
                SELECT id, object_key, size, sha256, hash, etag, manifest_key
                FROM artifacts
                WHERE task_tid = %s AND kind = 'RAW' AND id = ANY(%s)
                ORDER BY id ASC
                """,
                (tid, ids),
            )
        else:
            cur.execute(
                """
                SELECT id, object_key, size, sha256, hash, etag, manifest_key
                FROM artifacts
                WHERE task_tid = %s AND kind = 'RAW' AND status = 'ready'
                ORDER BY created_at ASC, id ASC
                LIMIT 1
                """,
                (tid,),
            )
        rows = [dict(row) for row in cur.fetchall()]
        cur.close()
        return rows
    except Exception as exc:
        raise AnalyzerTemporaryError(f"读取 RAW artifact 元数据失败: {exc}") from exc


def _json_ids(value) -> List[int]:
    if not value:
        return []
    if isinstance(value, (bytes, bytearray)):
        value = value.decode("utf-8", errors="ignore")
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError:
            return []
    if isinstance(value, list):
        return [int(v) for v in value if str(v).isdigit()]
    return []


def _connect_storage(storage_cfg: dict):
    try:
        from storage import MinIOStorage

        storage = MinIOStorage(
            endpoint=storage_cfg["endpoint"],
            access_key=storage_cfg["access_key"],
            secret_key=storage_cfg["secret_key"],
            use_ssl=storage_cfg.get("use_ssl", False),
        )
        bucket = storage_cfg.get("bucket", "drop-data")
        if storage.ensure_bucket(bucket):
            return storage
    except Exception as exc:
        raise AnalyzerTemporaryError(f"对象存储不可用: {exc}") from exc
    return None


def _expected_sha(row: dict) -> str:
    sha = str(row.get("sha256") or "").strip()
    if sha:
        return sha
    hash_value = str(row.get("hash") or "").strip()
    if hash_value.startswith("sha256:"):
        return hash_value.split(":", 1)[1]
    return ""


def _validate_manifest(storage, bucket: str, manifest_key: str, object_key: str, sha256: str, size: int):
    if not manifest_key:
        return
    data = storage.get_object(bucket, manifest_key)
    if data is None:
        raise AnalyzerInputError(f"RAW manifest 不存在: {manifest_key}")
    if len(data) > 1024 * 1024:
        raise AnalyzerInputError(f"RAW manifest 过大: {manifest_key}")
    try:
        manifest = json.loads(data.decode("utf-8"))
    except Exception as exc:
        raise AnalyzerInputError(f"RAW manifest 不是有效 JSON: {manifest_key}") from exc
    manifest_key_value = manifest.get("object_key") or manifest.get("key")
    if manifest_key_value and manifest_key_value != object_key:
        raise AnalyzerInputError("RAW manifest object_key 不匹配")
    manifest_sha = str(manifest.get("sha256") or "").strip()
    if manifest_sha and manifest_sha.lower() != sha256.lower():
        raise AnalyzerInputError("RAW manifest sha256 不匹配")
    manifest_size = int(manifest.get("size") or manifest.get("size_bytes") or 0)
    if manifest_size and manifest_size != size:
        raise AnalyzerInputError("RAW manifest size 不匹配")


def _key_matches_format(key: str, formats: List[str]) -> bool:
    lowered = key.lower()
    for fmt in formats:
        if fmt in ("perf", "perf.data") and lowered.endswith("perf.data"):
            return True
        if fmt == "pprof" and (lowered.endswith(".pb.gz") or lowered.endswith("perf.data")):
            return True
        if fmt == "java" and lowered.endswith(("perf.data", ".jfr", ".collapsed", ".txt", ".data")):
            return True
        if fmt == "bpf" and lowered.endswith(("perf.data", ".bpf", ".txt")):
            return True
        if fmt == "memtrace" and lowered.endswith(("memtrace.txt", ".memtrace", ".txt")):
            return True
    return False


def _content_type(name: str) -> str:
    if name.endswith(".json"):
        return "application/json"
    if name.endswith(".svg"):
        return "image/svg+xml"
    if name.endswith(".md"):
        return "text/markdown"
    if name.endswith(".txt"):
        return "text/plain"
    if name.endswith(".html"):
        return "text/html"
    return "application/octet-stream"
