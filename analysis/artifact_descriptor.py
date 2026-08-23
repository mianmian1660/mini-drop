#!/usr/bin/env python3
# ============================================================
# artifact_descriptor.py — 分析结果登记描述（阶段二）
# ============================================================
# Analyzer 的结果登记从"字符串 key"扩展为 Artifact descriptor：
#
#   descriptor = {
#     "object_key":  逻辑 key（如 "tid/flamegraph.svg"，前端访问不变）
#     "blob_key":    物理 CAS key（blobs/sha256/...）
#     "kind":        RAW / RESULT / INTERMEDIATE / LOG / MANIFEST
#     "format":      svg / folded / pprof / json / markdown ...
#     "schema_version": "1"
#     "compression": gzip / ""（空表示未压缩）
#     "content_encoding": gzip / ""（浏览器透明解码；pprof 为空保持 .gz 原样）
#     "logical_sha256": 未压缩规范内容哈希
#     "stored_sha256":  实际存储字节哈希
#     "logical_size":   解压后字节数
#     "stored_size":    存储字节数（压缩后）
#     "content_type":  MIME
#   }
#
# 新结果统一走 Blob：压缩收益大于成本时 gzip（SVG/folded/≥4KiB 文本/pprof），
# 小 JSON/Markdown 不强制压缩（走无压缩 CAS key，仍按内容去重）。
# ============================================================

import hashlib
import json
import os

from pprof_builder import blob_cas_key, gzip_deterministic
from job_context import get as job_context_get

# 透明 gzip 解码的格式（浏览器资源；pprof 作为文件格式本身保持 .gz）
_TRANSPARENT_GZIP_FORMATS = {"svg", "folded", "json", "markdown"}

# 建议压缩的格式
_COMPRESSIBLE_FORMATS = {"svg", "folded", "pprof"}

# 压缩阈值：大于等于该字节数才压缩（小 JSON/Markdown 不强制压缩）
DEFAULT_MIN_COMPRESS_BYTES = 4096


def current_output_prefix(tid: str) -> str:
    """返回当前分析作业的输出前缀。

    阶段 4：daemon 为每个作业设置 ANALYSIS_OUTPUT_PREFIX 环境变量
    （tasks/{tid}/analysis/{pipeline}/{analyzer_version}/g{generation}）；
    旧链路（未设置）回退到 {tid}，保持历史 key 形态。
    """
    prefix = str(job_context_get("output_prefix", "") or "").strip()
    if not prefix:
        prefix = os.environ.get("ANALYSIS_OUTPUT_PREFIX", "").strip()
    if prefix:
        return prefix
    return tid or ""


def format_from_filename(filename: str) -> str:
    """按文件名推断内容格式（best effort，与 apiserver blobFormatFromKey 对齐）。"""
    lower = filename.lower()
    if "kallsyms" in lower:
        return "kallsyms"
    if lower.endswith(".svg"):
        return "svg"
    if lower.endswith("folded.txt") or lower.endswith(".collapsed"):
        return "folded"
    if lower.endswith(".pb.gz"):
        return "pprof"
    if lower.endswith("perf.data"):
        return "perf.data"
    if lower.endswith(".json"):
        return "json"
    if lower.endswith(".md"):
        return "markdown"
    return ""


def should_compress(descriptor_format: str, logical_size: int,
                    min_compress_bytes: int = DEFAULT_MIN_COMPRESS_BYTES) -> bool:
    """压缩决策：svg/folded/pprof 一律压缩；其它 ≥min_compress_bytes 压缩。"""
    if descriptor_format in _COMPRESSIBLE_FORMATS:
        return True
    if descriptor_format in ("json", "markdown"):
        return logical_size >= min_compress_bytes
    return False


def content_encoding_for(descriptor_format: str, compression: str) -> str:
    """透明 HTTP 编码：浏览器资源 gzip；pprof 不透明解压。"""
    if compression == "gzip" and descriptor_format in _TRANSPARENT_GZIP_FORMATS:
        return "gzip"
    return ""


def content_type_for(filename: str) -> str:
    lower = filename.lower()
    if lower.endswith(".json"):
        return "application/json"
    if lower.endswith(".svg"):
        return "image/svg+xml"
    if lower.endswith(".md"):
        return "text/markdown"
    if lower.endswith(".txt") or lower.endswith(".collapsed"):
        return "text/plain"
    if lower.endswith(".pb.gz"):
        return "application/gzip"
    return "application/octet-stream"


def kind_for(filename: str, kind: str = "") -> str:
    """默认 kind 推断（与 analysis_daemon.record_result_artifacts 对齐）。"""
    if kind:
        return kind
    name = filename.rsplit("/", 1)[-1]
    if name == "manifest.json":
        return "MANIFEST"
    if name.endswith((".svg", ".json", ".md", ".html")):
        return "RESULT"
    return "INTERMEDIATE"


def build_descriptor(tid: str, filename: str, content,
                     kind: str = "", fmt: str = "",
                     schema_version: str = "1",
                     min_compress_bytes: int = DEFAULT_MIN_COMPRESS_BYTES,
                     compression: str = None,
                     logical_name: str = "") -> dict:
    """把一份新生成的结果转成 descriptor，并完成压缩与双哈希计算。

    compression=None 时按规则自动决定（svg/folded/pprof 压缩、大文本压缩）；
    显式传 "" / "gzip" 强制不压缩/压缩。pprof 调用方应传 compression=""
    （内容本身就是 .pb.gz 文件格式，不再二次压缩）。

    阶段 4：object_key 使用当前作业输出前缀（ANALYSIS_OUTPUT_PREFIX 环境变量，
    未设置时回退 {tid}）；logical_name 为稳定角色名（filename）。

    不负责上传；调用方用 descriptor["blob_key"] 上传后，
    把 descriptor 交给 analysis_daemon.record_result_artifacts 登记。

    返回: descriptor dict；内容既可以是 str 也可以是 bytes/dict。
    """
    object_key = f"{current_output_prefix(tid)}/{filename}"
    if isinstance(content, str):
        raw = content.encode("utf-8")
    elif isinstance(content, bytes):
        raw = content
    else:
        raw = json.dumps(content, ensure_ascii=False).encode("utf-8")

    if not fmt:
        fmt = format_from_filename(filename)
    if not kind:
        kind = kind_for(filename)

    logical_sha256 = hashlib.sha256(raw).hexdigest()
    logical_size = len(raw)
    if compression is None:
        compress = should_compress(fmt, logical_size, min_compress_bytes)
        compression = "gzip" if compress else ""
    else:
        compress = (compression == "gzip")
    stored = gzip_deterministic(raw) if compress else raw
    stored_sha256 = hashlib.sha256(stored).hexdigest()
    stored_size = len(stored)
    blob_key = blob_cas_key(logical_sha256, fmt or "unknown", schema_version, compression)
    content_encoding = content_encoding_for(fmt, compression)
    content_type = content_type_for(filename)

    return {
        "object_key": object_key,
        "logical_name": logical_name or filename,
        "blob_key": blob_key,
        "kind": kind,
        "format": fmt,
        "schema_version": schema_version,
        "compression": compression,
        "content_encoding": content_encoding,
        "logical_sha256": logical_sha256,
        "stored_sha256": stored_sha256,
        "logical_size": logical_size,
        "stored_size": stored_size,
        "content_type": content_type,
        "_payload": stored,  # 实际要上传的字节（压缩后）
    }


def upload_descriptor(storage, bucket: str, descriptor: dict) -> bool:
    """按 descriptor 上传物理对象（CAS key；带 content_encoding）。"""
    if storage is None or not descriptor:
        return False
    payload = descriptor.get("_payload")
    if payload is None:
        return False
    return storage.put_object(
        bucket,
        descriptor["blob_key"],
        payload,
        descriptor.get("content_type") or "application/octet-stream",
        descriptor.get("content_encoding") or "",
    )
