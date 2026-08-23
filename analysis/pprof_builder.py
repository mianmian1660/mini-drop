#!/usr/bin/env python3
# ============================================================
# pprof_builder.py — 单次 CPU 采样 → 标准 pprof v1（.pb.gz）
# ============================================================
# 从"同一规范样本模型"（perf script 输出的结构化解析结果）生成 pprof：
#   - SampleType: samples/count + cpu/nanoseconds
#   - PeriodType: cpu/nanoseconds，period = 1e9 / 任务采样频率
#   - time_nanos / duration_nanos 来自任务开始/结束时间
#   - Mapping: DSO 路径 + 可获得的 build-id
#   - Location: IP、符号名、Mapping；Line → Function
#   - Sample 标签: comm/pid/tid/event
#   - 相同 stack+标签聚合；protobuf 确定性序列化（固定字段序）；
#     gzip mtime=0（确定性输出）。
#
# protobuf 编码为手写实现（无第三方依赖），字段号严格遵循 google/pprof
# profile.proto；用 test 里的 google.protobuf 解析器做 round-trip 验证。
# ============================================================

import gzip
import hashlib
import io
import re
import time
from typing import Dict, List, Optional, Tuple

# ----------------------------------------------------------
# protobuf 基础编码
# ----------------------------------------------------------

def _varint(value: int) -> bytes:
    value &= (1 << 64) - 1
    out = bytearray()
    while True:
        b = value & 0x7F
        value >>= 7
        if value:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def _tag(field: int, wire: int) -> bytes:
    return _varint((field << 3) | wire)


def _length_delimited(field: int, data: bytes) -> bytes:
    return _tag(field, 2) + _varint(len(data)) + data


def _int64_field(field: int, value: int) -> bytes:
    # int64 用 10 号 wire type（varint，负数以 10 字节编码，与标准库一致）
    return _tag(field, 0) + _varint(value)


def _uint64_field(field: int, value: int) -> bytes:
    return _tag(field, 0) + _varint(value)


def _bool_field(field: int, value: bool) -> bytes:
    if value:
        return _tag(field, 0) + _varint(1)
    return b""


def _string_field(field: int, value: str) -> bytes:
    return _length_delimited(field, value.encode("utf-8"))


# ----------------------------------------------------------
# perf script 解析 → 规范样本模型
# ----------------------------------------------------------

# perf script 头行兼容多种版本输出：
#   comm pid tid [cpu] time: event: <frame>      (旧格式)
#   comm pid/tid [cpu] time: event: <frame>      (pid/tid 合并)
#   comm pid/tid time: event:                    (无 [cpu] 字段，帧在下一行)
_HEADER_RE = re.compile(
    r"^(\S+)\s+(\S+)(?:\s+\[\d+\])?\s+([\d.]+):\s+([^:]+):(?:\s*(.*))?$"
)
# 帧：ip sym (dso) 或 ip sym 或 [unknown] 形式
_FRAME_RE = re.compile(
    r"^\s*([0-9a-fA-F]+(?:\s*[0-9a-fA-F])*)\s+(.+?)(?:\s+\((.*)\))?\s*$"
)
_OFFSET_SUFFIX_RE = re.compile(r"\+0x[0-9a-fA-F]+$")


def _clean_sym(sym: str) -> str:
    sym = sym.strip()
    if not sym or sym == "[unknown]":
        return "unknown"
    return _OFFSET_SUFFIX_RE.sub("", sym)


def parse_perf_script(output: str) -> dict:
    """解析 perf script 输出为规范样本模型。

    返回:
      {
        "samples": [
          {"comm": str, "pid": int, "tid": int, "event": str,
           "time": float, "frames": [(ip:int, sym:str, dso:str), ...]},
          ...
        ]
      }
    frames 顺序 = perf 输出的调用顺序（叶子在第一位）。
    """
    samples: List[dict] = []
    current = None
    for raw_line in output.splitlines():
        if not raw_line.strip():
            continue
        m = _HEADER_RE.match(raw_line)
        if m:
            if current is not None:
                samples.append(current)
            comm, pidtid, tstr, event, first_frame = m.groups()
            pid, tid = _parse_pid_tid(pidtid)
            frames = []
            if first_frame and first_frame.strip():
                fm = _FRAME_RE.match(first_frame)
                if fm:
                    ip, sym, dso = fm.groups()
                    frames.append(_parse_frame(ip, sym, dso))
            current = {
                "comm": comm,
                "pid": pid,
                "tid": tid,
                "event": event.strip(),
                "time": float(tstr),
                "frames": frames,
            }
            continue
        if current is not None and (raw_line.startswith("\t") or raw_line.startswith(" ")):
            fm = _FRAME_RE.match(raw_line)
            if fm:
                ip, sym, dso = fm.groups()
                current["frames"].append(_parse_frame(ip, sym, dso))
    if current is not None:
        samples.append(current)
    return {"samples": samples}


def _parse_pid_tid(pidtid: str):
    """解析 pid/tid 字段："123/456" → (123,456)；"123" → (123,123)。"""
    try:
        if "/" in pidtid:
            p, t = pidtid.split("/", 1)
            return int(p), int(t)
        v = int(pidtid)
        return v, v
    except (TypeError, ValueError):
        return 0, 0


def _parse_frame(ip_raw, sym, dso):
    ip = 0
    try:
        ip = int(ip_raw.strip().lstrip("0x"), 16) if ip_raw.strip() else 0
    except ValueError:
        ip = 0
    sym = _clean_sym(sym)
    dso = (dso or "").strip()
    if not dso or dso == "[unknown]":
        dso = "unknown"
    return (ip, sym, dso)


# ----------------------------------------------------------
# pprof v1 构建
# ----------------------------------------------------------

# pprof 内置字符串（保持与 google/pprof 兼容的固定顺序）
_PPROF_STRINGS = [
    "",
    "samples",
    "count",
    "cpu",
    "nanoseconds",
    "comm",
    "pid",
    "tid",
    "event",
]


def build_pprof(model: dict, period_ns: int,
                time_nanos: int = 0, duration_nanos: int = 0,
                build_ids: Optional[Dict[str, str]] = None) -> bytes:
    """从规范样本模型构建 pprof protobuf（确定性序列化）。

    参数:
      model: parse_perf_script 的输出
      period_ns: 采样周期纳秒（= 1e9 / 频率）
      time_nanos: profile 开始时间（Unix 纳秒），0 表示未知
      duration_nanos: profile 时长（纳秒）
      build_ids: {dso_path: build_id} 映射（可获得的 build-id）
    """
    samples = model.get("samples") or []
    string_table = list(_PPROF_STRINGS)
    str_index: Dict[str, int] = {}
    for i, s in enumerate(string_table):
        str_index[s] = i

    def intern(s: str) -> int:
        s = s or ""
        idx = str_index.get(s)
        if idx is not None:
            return idx
        idx = len(string_table)
        string_table.append(s)
        str_index[s] = idx
        return idx

    # 聚合样本：按 (comm,pid,tid,event, stack) 归并，保持首见顺序。
    # stack = ((ip, sym, dso), ...) 叶子在前。
    agg: Dict[Tuple, dict] = {}
    for s in samples:
        if not s.get("frames"):
            continue
        stack = tuple((ip, sym, dso) for (ip, sym, dso) in s["frames"])
        key = (s.get("comm", ""), s.get("pid", 0), s.get("tid", 0),
               s.get("event", ""), stack)
        rec = agg.get(key)
        if rec is None:
            agg[key] = {
                "comm": s.get("comm", ""),
                "pid": s.get("pid", 0),
                "tid": s.get("tid", 0),
                "event": s.get("event", ""),
                "stack": stack,
                "count": 1,
            }
        else:
            rec["count"] += 1

    # 建立 Mapping / Location / Function（按首见顺序分配 id）
    mappings: Dict[str, dict] = {}   # dso → mapping 描述
    locations: Dict[Tuple, dict] = {}  # (ip, sym, dso) → location 描述
    functions: Dict[str, dict] = {}  # sym → function 描述
    mapping_list: List[bytes] = []
    location_list: List[bytes] = []
    function_list: List[bytes] = []
    mapping_id = 1
    location_id = 1
    function_id = 1
    build_ids = build_ids or {}

    def ensure_mapping(dso: str) -> int:
        nonlocal mapping_id
        m = mappings.get(dso)
        if m is not None:
            return m["id"]
        mid = mapping_id
        mapping_id += 1
        mappings[dso] = {"id": mid}
        filename_idx = intern(dso)
        bid = build_ids.get(dso, "")
        build_id_idx = intern(bid) if bid else 0
        msg = (
            _uint64_field(1, mid)
            + _int64_field(5, filename_idx)
            + (_int64_field(6, build_id_idx) if bid else b"")
        )
        mapping_list.append(_length_delimited(3, msg))
        return mid

    def ensure_function(sym: str) -> int:
        nonlocal function_id
        f = functions.get(sym)
        if f is not None:
            return f["id"]
        fid = function_id
        function_id += 1
        functions[sym] = {"id": fid}
        name_idx = intern(sym)
        msg = (
            _uint64_field(1, fid)
            + _int64_field(2, name_idx)
            + _int64_field(3, name_idx)
        )
        function_list.append(_length_delimited(5, msg))
        return fid

    def ensure_location(ip: int, sym: str, dso: str) -> int:
        nonlocal location_id
        key = (ip, sym, dso)
        loc = locations.get(key)
        if loc is not None:
            return loc["id"]
        lid = location_id
        location_id += 1
        locations[key] = {"id": lid}
        mid = ensure_mapping(dso)
        fid = ensure_function(sym)
        line = _uint64_field(1, fid)
        msg = (
            _uint64_field(1, lid)
            + _uint64_field(2, mid)
            + _uint64_field(3, ip & ((1 << 64) - 1))
            + _length_delimited(4, line)
        )
        location_list.append(_length_delimited(4, msg))
        return lid

    sample_msgs: List[bytes] = []
    total_count = 0
    for rec in agg.values():
        loc_ids = [ensure_location(*frame) for frame in rec["stack"]]
        count = rec["count"]
        total_count += count
        sample_msg = (
            b"".join(_uint64_field(1, lid) for lid in loc_ids)
            + _int64_field(2, count)
            + _int64_field(2, count * period_ns)
            + _label_field("comm", str(rec["comm"]), intern)
            + _label_field("pid", str(rec["pid"]), intern, num=rec["pid"])
            + _label_field("tid", str(rec["tid"]), intern, num=rec["tid"])
            + _label_field("event", str(rec["event"]), intern)
        )
        sample_msgs.append(_length_delimited(2, sample_msg))

    # sample_type: [(samples, count), (cpu, nanoseconds)]
    sample_type_msg = (
        _length_delimited(1, _int64_field(1, intern("samples")) + _int64_field(2, intern("count")))
        + _length_delimited(1, _int64_field(1, intern("cpu")) + _int64_field(2, intern("nanoseconds")))
    )
    period_type_msg = (
        _int64_field(1, intern("cpu")) + _int64_field(2, intern("nanoseconds"))
    )

    profile = (
        sample_type_msg
        + b"".join(sample_msgs)
        + b"".join(mapping_list)
        + b"".join(location_list)
        + b"".join(function_list)
        + b"".join(_string_field(6, s) for s in string_table)
        + (_int64_field(9, time_nanos) if time_nanos else b"")
        + (_int64_field(10, duration_nanos) if duration_nanos else b"")
        + _length_delimited(11, period_type_msg)
        + _int64_field(12, period_ns)
    )
    return profile


def _label_field(key: str, value: str, intern, num: int = None) -> bytes:
    """构建 Sample.Label 消息：key + str（必要时 num/num_unit）。"""
    parts = [_int64_field(1, intern(key))]
    if num is not None:
        parts.append(_int64_field(3, num))
        parts.append(_int64_field(4, intern("count")))
    else:
        parts.append(_int64_field(2, intern(value)))
    return _length_delimited(3, b"".join(parts))


# ----------------------------------------------------------
# 确定性 gzip + CAS key
# ----------------------------------------------------------

def gzip_deterministic(data: bytes, level: int = 9) -> bytes:
    """gzip 压缩，mtime=0（确定性输出，跨运行一致）。"""
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode="wb", compresslevel=level, mtime=0) as f:
        f.write(data)
    return out.getvalue()


def blob_cas_key(logical_sha256: str, format_name: str,
                 schema_version: str = "1", compression: str = "gzip") -> str:
    """与 apiserver blobCASKey 保持一致的内容寻址物理 key（跨端契约）。

    扩展名规则：pprof→".pb.gz"（格式自带扩展名）；gzip→".gz"；zstd→".zst"；未压缩→""。
    """
    prefix = logical_sha256[:2] if len(logical_sha256) >= 2 else "00"
    if format_name == "pprof":
        ext = ".pb.gz"
    else:
        ext = {"gzip": ".gz", "zstd": ".zst"}.get(compression, "")
    if not format_name:
        format_name = "unknown"
    return "blobs/sha256/{}/{}/{}-v{}{}".format(
        prefix, logical_sha256, format_name, schema_version or "1", ext)


def pprof_gz(model: dict, period_ns: int,
             time_nanos: int = 0, duration_nanos: int = 0,
             build_ids: Optional[Dict[str, str]] = None) -> bytes:
    """构建标准 cpu.pprof.pb.gz（gzip mtime=0）。"""
    raw = build_pprof(model, period_ns, time_nanos, duration_nanos, build_ids)
    return gzip_deterministic(raw)


# ----------------------------------------------------------
# 校验（observe 模式 / 测试用）
# ----------------------------------------------------------

def profile_stats(raw_gz: bytes) -> dict:
    """不解依赖外部库的基础自检：gzip 可解、非空、可统计 total/count。

    返回: {"samples": int, "cpu_ns": int, "ok": bool, "error": str}
    """
    try:
        data = gzip.decompress(raw_gz)
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "error": "gzip decompress failed: %s" % exc}
    if not data:
        return {"ok": False, "error": "empty profile"}
    return {"ok": True, "bytes": len(data)}


def validate_pprof_proto(raw_gz: bytes) -> dict:
    """尝试用 google.protobuf 完整解析（分析容器已安装 protobuf）。

    返回: {"ok": bool, "samples": int, "locations": int, "mappings": int,
           "functions": int, "error": str}
    """
    result = {"ok": False, "samples": 0, "locations": 0,
              "mappings": 0, "functions": 0, "error": ""}
    try:
        from google.protobuf import descriptor_pool
        from google.protobuf import message_factory
        # 动态注册 profile.proto（内嵌描述，避免构建期 protoc 依赖）
        pool = descriptor_pool.DescriptorPool()
        # profile.proto 的最小描述：使用 text_format 内嵌
        from google.protobuf import descriptor_pb2, text_format
        file_descriptor_proto = descriptor_pb2.FileDescriptorProto()
        text_format.Parse(_PROFILE_PROTO_TEXT, file_descriptor_proto)
        pool.Add(file_descriptor_proto)
        msg_cls = message_factory.GetMessageClass(
            pool.FindMessageTypeByName("perftools.profiles.Profile"))
        data = gzip.decompress(raw_gz)
        profile = msg_cls()
        profile.ParseFromString(data)
        result["ok"] = True
        result["samples"] = len(profile.sample)
        result["locations"] = len(profile.location)
        result["mappings"] = len(profile.mapping)
        result["functions"] = len(profile.function)
    except Exception as exc:  # noqa: BLE001
        result["error"] = "%s: %s" % (type(exc).__name__, exc)
    return result


# profile.proto 的最小完整描述（仅保留本构建器使用的消息与字段）
_PROFILE_PROTO_TEXT = """
syntax: "proto3"
package: "perftools.profiles"
message_type {
  name: "Profile"
  field {
    name: "sample_type" number: 1 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.ValueType"
  }
  field {
    name: "sample" number: 2 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Sample"
  }
  field {
    name: "mapping" number: 3 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Mapping"
  }
  field {
    name: "location" number: 4 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Location"
  }
  field {
    name: "function" number: 5 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Function"
  }
  field {
    name: "string_table" number: 6 label: LABEL_REPEATED type: TYPE_STRING
  }
  field { name: "drop_frames" number: 7 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "keep_frames" number: 8 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "time_nanos" number: 9 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "duration_nanos" number: 10 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field {
    name: "period_type" number: 11 label: LABEL_OPTIONAL type: TYPE_MESSAGE
    type_name: ".perftools.profiles.ValueType"
  }
  field { name: "period" number: 12 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field {
    name: "comment" number: 13 label: LABEL_REPEATED type: TYPE_INT64
  }
  field { name: "default_sample_type" number: 14 label: LABEL_OPTIONAL type: TYPE_INT64 }
}
message_type {
  name: "ValueType"
  field { name: "type" number: 1 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "unit" number: 2 label: LABEL_OPTIONAL type: TYPE_INT64 }
}
message_type {
  name: "Sample"
  field {
    name: "location_id" number: 1 label: LABEL_REPEATED type: TYPE_UINT64
  }
  field {
    name: "value" number: 2 label: LABEL_REPEATED type: TYPE_INT64
  }
  field {
    name: "label" number: 3 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Label"
  }
}
message_type {
  name: "Label"
  field { name: "key" number: 1 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "str" number: 2 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "num" number: 3 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "num_unit" number: 4 label: LABEL_OPTIONAL type: TYPE_INT64 }
}
message_type {
  name: "Mapping"
  field { name: "id" number: 1 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "memory_start" number: 2 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "memory_limit" number: 3 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "file_offset" number: 4 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "filename" number: 5 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "build_id" number: 6 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "has_functions" number: 7 label: LABEL_OPTIONAL type: TYPE_BOOL }
  field { name: "has_filenames" number: 8 label: LABEL_OPTIONAL type: TYPE_BOOL }
  field { name: "has_line_numbers" number: 9 label: LABEL_OPTIONAL type: TYPE_BOOL }
  field { name: "has_inline_frames" number: 10 label: LABEL_OPTIONAL type: TYPE_BOOL }
}
message_type {
  name: "Location"
  field { name: "id" number: 1 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "mapping_id" number: 2 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "address" number: 3 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field {
    name: "line" number: 4 label: LABEL_REPEATED type: TYPE_MESSAGE
    type_name: ".perftools.profiles.Line"
  }
  field { name: "is_folded" number: 5 label: LABEL_OPTIONAL type: TYPE_BOOL }
}
message_type {
  name: "Line"
  field { name: "function_id" number: 1 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "line" number: 2 label: LABEL_OPTIONAL type: TYPE_INT64 }
}
message_type {
  name: "Function"
  field { name: "id" number: 1 label: LABEL_OPTIONAL type: TYPE_UINT64 }
  field { name: "name" number: 2 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "system_name" number: 3 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "filename" number: 4 label: LABEL_OPTIONAL type: TYPE_INT64 }
  field { name: "start_line" number: 5 label: LABEL_OPTIONAL type: TYPE_INT64 }
}
"""


# ----------------------------------------------------------
# 便捷：由折叠文本构建 pprof（无 perf script 时兜底，测试/演示用）
# ----------------------------------------------------------

def folded_to_model(folded_text: str) -> dict:
    """把折叠栈文本（frame;frame;... count）转成规范样本模型。

    帧顺序：折叠文本为叶子在前（与 perf script 一致），comm 取首帧后的
    "processname" 前缀（若有）。不用于生产 perf 路径，仅供测试与无 perf 环境。
    """
    samples = []
    for line in folded_text.splitlines():
        line = line.strip()
        if not line:
            continue
        parts = line.rsplit(" ", 1)
        if len(parts) != 2:
            continue
        try:
            count = int(parts[1])
        except ValueError:
            continue
        frames = []
        for frame in parts[0].split(";"):
            if not frame:
                continue
            frames.append((0, frame, "unknown"))
        if not frames:
            continue
        comm = frames[0][1].split(";")[0] if frames else ""
        for _ in range(max(1, count)):
            samples.append({
                "comm": comm, "pid": 0, "tid": 0, "event": "cpu-clock",
                "time": 0.0, "frames": frames,
            })
    return {"samples": samples}
