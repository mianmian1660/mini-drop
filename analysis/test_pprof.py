#!/usr/bin/env python3
# ============================================================
# test_pprof.py — 阶段二：pprof 构建 / descriptor / 登记 单元测试
# ============================================================
# 运行：python3 -m pytest test_pprof.py -q
# ============================================================

import gzip
import hashlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import artifact_descriptor as ad
import pprof_builder as pb

SAMPLE_SCRIPT = """python3 12345 12345 [002] 158665.570607: cpu-clock: 7f8c12345678 main (/usr/bin/python3)
\t7f8c12345679 foo (/lib/x86_64-linux-gnu/libc.so.6)
\t7f8c1234567a bar (/usr/bin/python3)
python3 12345 12345 [002] 158665.570708: cpu-clock: 7f8c12345678 main (/usr/bin/python3)
\t7f8c12345679 foo (/lib/x86_64-linux-gnu/libc.so.6)
\t7f8c1234567a bar (/usr/bin/python3)
python3 12345 12345 [002] 158665.570809: cpu-clock: 7f8c12345678 main (/usr/bin/python3)
\t7f8c12345679 baz (/usr/bin/python3)
"""


def _profile():
    from google.protobuf import descriptor_pool
    from google.protobuf import message_factory
    from google.protobuf import descriptor_pb2, text_format
    pool = descriptor_pool.DescriptorPool()
    fd = descriptor_pb2.FileDescriptorProto()
    text_format.Parse(pb._PROFILE_PROTO_TEXT, fd)
    pool.Add(fd)
    return message_factory.GetMessageClass(pool.FindMessageTypeByName("perftools.profiles.Profile"))


def test_parse_perf_script_counts_samples():
    model = pb.parse_perf_script(SAMPLE_SCRIPT)
    assert len(model["samples"]) == 3
    s = model["samples"][0]
    assert s["comm"] == "python3" and s["pid"] == 12345 and s["tid"] == 12345
    assert s["event"] == "cpu-clock"
    assert s["frames"][0] == (0x7F8C12345678, "main", "/usr/bin/python3")


def test_parse_perf_script_comm_with_spaces():
    output = "worker pool 123/456 [001] 10.25: cpu-clock: 7f main (/bin/app)\n"
    model = pb.parse_perf_script(output)
    assert len(model["samples"]) == 1
    assert model["samples"][0]["comm"] == "worker pool"
    assert model["samples"][0]["pid"] == 123
    assert model["samples"][0]["tid"] == 456


def test_pprof_roundtrip_references_valid():
    model = pb.parse_perf_script(SAMPLE_SCRIPT)
    raw = pb.build_pprof(model, period_ns=52631579,
                         time_nanos=158665570607000000, duration_nanos=202000000,
                         build_ids={"/usr/bin/python3": "deadbeef"})
    gz = pb.gzip_deterministic(raw)
    check = pb.validate_pprof_proto(gz)
    assert check["ok"], check["error"]
    assert check["samples"] == 2  # main;foo;bar 聚合为 1 条 ×2，main;baz 1 条
    assert check["total_samples"] == 3
    Profile = _profile()
    p = Profile()
    p.ParseFromString(gzip.decompress(gz))
    # Mapping/Location/Function 引用合法
    loc_ids = set()
    for loc in p.location:
        assert 1 <= loc.id
        assert loc.mapping_id >= 1
        loc_ids.add(loc.id)
        assert len(loc.line) == 1
        assert loc.line[0].function_id >= 1
    func_ids = {f.id for f in p.function}
    for loc in p.location:
        assert loc.line[0].function_id in func_ids
    # 每个 sample 的 location_id 都必须存在
    for sample in p.sample:
        for lid in sample.location_id:
            assert lid in loc_ids
    # sample_type / period
    assert len(p.sample_type) == 2
    assert p.period_type.type and p.period_type.unit
    assert p.period == 52631579


def test_pprof_total_samples_and_cpu_ns_match_folded():
    model = pb.parse_perf_script(SAMPLE_SCRIPT)
    # folded 等价性：同一脚本走 stackcollapse 语义 = 3 条样本
    # pprof 聚合后 total count 必须等于折叠栈总计数。
    raw = pb.build_pprof(model, period_ns=10000000)  # 100Hz
    Profile = _profile()
    p = Profile()
    p.ParseFromString(raw)
    total_samples = sum(s.value[0] for s in p.sample)
    total_cpu_ns = sum(s.value[1] for s in p.sample)
    assert total_samples == 3
    # CPU 纳秒误差不超过一个采样周期：total = samples * period
    assert total_cpu_ns == total_samples * 10000000
    # 与 analyze_collapsed 的 TopN 一致（self/inclusive）
    from collapsed_data_parser import analyze_collapsed
    top = analyze_collapsed("main;foo;bar 2\nmain;baz 1", top_n=20)
    self_top = {t["function"]: t["samples"] for t in top["self_time_top"]}
    # pprof 中 main 的 self = 0（main 总在叶子之后），baz self=1
    # 这里仅验证样本计数总和与 folded 一致
    assert top["total_samples"] == 3


def test_pprof_deterministic_serialization():
    model = pb.parse_perf_script(SAMPLE_SCRIPT)
    r1 = pb.build_pprof(model, period_ns=52631579, time_nanos=1, duration_nanos=2)
    r2 = pb.build_pprof(model, period_ns=52631579, time_nanos=1, duration_nanos=2)
    assert r1 == r2
    g1 = pb.gzip_deterministic(b"x" * 100)
    g2 = pb.gzip_deterministic(b"x" * 100)
    assert g1 == g2
    # mtime=0：解压出的 mtime 为 0
    with gzip.GzipFile(fileobj=__import__("io").BytesIO(g1)) as f:
        f.read()
        assert f.mtime == 0


def test_blob_cas_key_rules():
    sha = "ab" * 32
    # pprof 格式自带 .pb.gz 扩展名
    assert pb.blob_cas_key(sha, "pprof", "1", "gzip") == \
        "blobs/sha256/ab/%s/pprof-v1.pb.gz" % sha
    assert pb.blob_cas_key(sha, "pprof", "1", "") == \
        "blobs/sha256/ab/%s/pprof-v1.pb.gz" % sha
    # 其它格式 gzip → .gz
    assert pb.blob_cas_key(sha, "svg", "1", "gzip") == \
        "blobs/sha256/ab/%s/svg-v1.gz" % sha
    # 未压缩 → 无后缀
    assert pb.blob_cas_key(sha, "json", "1", "") == \
        "blobs/sha256/ab/%s/json-v1" % sha


def test_descriptor_svg_compressed_transparent():
    svg = "<svg>%s</svg>" % ("x" * 5000)
    d = ad.build_descriptor("tid-1", "flamegraph.svg", svg, fmt="svg")
    assert d["compression"] == "gzip"
    assert d["content_encoding"] == "gzip"
    assert d["logical_size"] == len(svg.encode())
    assert d["stored_size"] < d["logical_size"]
    assert d["logical_sha256"] == hashlib.sha256(svg.encode()).hexdigest()
    # stored 字节是 gzip，解压后等于原文
    assert gzip.decompress(d["_payload"]) == svg.encode()
    assert d["blob_key"].endswith("svg-v1.gz")
    assert d["object_key"] == "tid-1/flamegraph.svg"
    assert d["kind"] == "RESULT"


def test_descriptor_small_json_not_compressed():
    payload = {"a": 1}
    d = ad.build_descriptor("tid-1", "top.json", payload, fmt="json")
    assert d["compression"] == ""
    assert d["content_encoding"] == ""
    assert d["stored_size"] == d["logical_size"]
    assert d["_payload"] == b'{"a": 1}'
    assert d["blob_key"].endswith("json-v1") and not d["blob_key"].endswith(".gz")


def test_descriptor_pprof_not_recompressed():
    raw_gz = pb.pprof_gz(pb.parse_perf_script(SAMPLE_SCRIPT), period_ns=10000000)
    d = ad.build_descriptor("tid-1", "cpu.pprof.pb.gz", raw_gz,
                            kind="RAW", fmt="pprof", schema_version="1",
                            compression="")
    assert d["compression"] == ""
    assert d["content_encoding"] == ""
    assert d["kind"] == "RAW"
    assert d["format"] == "pprof"
    assert d["schema_version"] == "1"
    # 不二次压缩：payload 就是 pprof.gz 本身
    assert d["_payload"] == raw_gz
    assert d["stored_sha256"] == d["logical_sha256"]
    assert d["blob_key"].endswith("pprof-v1.pb.gz")


def test_folded_to_model():
    m = pb.folded_to_model("main;foo;bar 5\nmain;baz 2")
    assert len(m["samples"]) == 7
    assert m["samples"][0]["frames"][0][1] == "main"


def test_empty_pprof_is_rejected():
    empty = pb.pprof_gz({"samples": []}, period_ns=10000000)
    check = pb.validate_pprof_proto(empty)
    assert not check["ok"]
    assert "no samples" in check["error"]


if __name__ == "__main__":
    failures = 0
    for k, v in sorted(list(globals().items())):
        if k.startswith("test_") and callable(v):
            try:
                v()
                print(f"  ✅ {k}")
            except Exception as e:
                failures += 1
                print(f"  ❌ {k}: {e}")
    total = sum(1 for k, v in globals().items() if k.startswith("test_") and callable(v))
    print(f"结果: 通过 {total - failures}, 失败 {failures}, 总计 {total}")
    sys.exit(1 if failures else 0)
