# ============================================================
# test_analysis.py — analysis 模块单元测试 (修正版)
# ============================================================
import json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

def t_observability_json_log_fields():
    import contextlib
    import io
    from observability import log_event

    buf = io.StringIO()
    with contextlib.redirect_stderr(buf):
        log_event("analysis_started", task_tid="tid-1",
                  analysis_duration_seconds=1.25, ignored_none=None)
    line = buf.getvalue().strip()
    assert line.startswith("[analysis_observe] ")
    payload = json.loads(line.replace("[analysis_observe] ", "", 1))
    assert payload["component"] == "analysis"
    assert payload["stage"] == "analysis_started"
    assert payload["task_tid"] == "tid-1"
    assert payload["analysis_duration_seconds"] == 1.25
    assert "ignored_none" not in payload

def t_observability_redacts_secrets():
    import contextlib
    import io
    from observability import log_event

    buf = io.StringIO()
    with contextlib.redirect_stderr(buf):
        log_event(
            "analysis_failed",
            dsn="host=db user=drop password=dev dbname=drop",
            profile_url="http://app/profile?token=abc123&debug=1",
            headers={"Authorization": "Bearer top-secret", "secret_key": "dropdrop"},
        )
    line = buf.getvalue().strip().replace("[analysis_observe] ", "", 1)
    payload = json.loads(line)
    raw = json.dumps(payload, ensure_ascii=False)
    for forbidden in ["password=dev", "token=abc123", "top-secret", "dropdrop"]:
        assert forbidden not in raw
    assert "password=***" in raw and "token=***" in raw

def t_parse_collapsed_basic():
    from collapsed_data_parser import parse_collapsed
    folded = "func1;func2;func3 10\nfunc1;func2;func4 5\nfunc1;func5 3\n"
    result = parse_collapsed(folded)
    assert len(result) > 0 and "func3" in result and result["func3"] == 10

def t_parse_collapsed_empty():
    from collapsed_data_parser import parse_collapsed
    assert parse_collapsed("") == {}

def t_get_top_functions():
    from collapsed_data_parser import parse_collapsed, get_top_functions
    folded = "a;b;c 100\na;b;d 50\na;e 25\na;f 20\na;g 5\n"
    parsed = parse_collapsed(folded)
    top = get_top_functions(parsed, n=3)
    assert len(top) == 3 and top[0]["function"] == "c" and top[0]["samples"] == 100

def t_analyze_collapsed():
    from collapsed_data_parser import analyze_collapsed
    result = analyze_collapsed("main;worker;process 200\nmain;worker;io_wait 100\nmain;gc 50\n", top_n=10)
    assert result["total_samples"] == 350
    assert result["sample_unit"] == "samples"
    assert result["sample_kind"] == "event_records"

def t_analyze_collapsed_minimal_schema():
    from collapsed_data_parser import analyze_collapsed
    result = analyze_collapsed("main;hot 1\n", top_n=5)
    assert result["total_samples"] == 1
    assert "self_time_top" in result and isinstance(result["self_time_top"], list)
    assert result["self_time_top"][0]["function"] == "hot"
    assert {"function", "samples", "percentage"}.issubset(result["self_time_top"][0].keys())

def t_parse_collapsed_truncated_lines_are_ignored():
    from collapsed_data_parser import parse_collapsed
    parsed = parse_collapsed("main;ok 3\nbroken-without-count\nmain;bad nope\n")
    assert parsed == {"ok": 3}

def t_flamegraph_detects_single_frame_folded_stacks():
    from flamegraph import _looks_like_folded_stacks
    assert _looks_like_folded_stacks([
        "0x769d56fbf527 1",
        "main;worker;hot 3",
        "0x5bbf4625fb6a 1",
    ])

def t_flamegraph_does_not_treat_perf_script_as_folded():
    from flamegraph import _looks_like_folded_stacks
    assert not _looks_like_folded_stacks([
        "python 1234 123.456: cycles:",
        "        ffffffff81000000 native_write_msr ([kernel.kallsyms])",
        "        7f0000000000 PyEval_EvalFrame (/usr/bin/python)",
    ])

def t_perf_script_omits_period_field():
    import subprocess
    import flamegraph

    captured = {}
    original_run = flamegraph.subprocess.run
    try:
        def fake_run(args, **kwargs):
            captured["args"] = args
            return subprocess.CompletedProcess(args, 0, stdout="python 1/1 [000] 1.0: cpu-clock:\n", stderr="")
        flamegraph.subprocess.run = fake_run
        flamegraph.run_perf_script("/tmp/demo.perf")
    finally:
        flamegraph.subprocess.run = original_run

    fields = captured["args"][captured["args"].index("-F") + 1]
    assert "period" not in fields.split(",")
    assert fields == flamegraph.PERF_SCRIPT_FIELDS

def t_parse_bpf_histogram():
    from bpf_analyzer import parse_bpf_histogram
    text = "@io_lat_us:\n[1, 2)        42 |@@@@@\n[2, 4)        88 |@@@@@@@@@@\n[4, 8)       156 |@@@@@@@@@@@@\n# Total IO: 286\n"
    r = parse_bpf_histogram(text)
    assert r["type"]=="io_latency" and len(r["buckets"])==3 and r["buckets"][0]["count"]==42 and r["total_events"]==286

def t_parse_bpf_histogram_sched():
    from bpf_analyzer import parse_bpf_histogram
    r = parse_bpf_histogram("# Mini-Drop eBPF Scheduler\n@sched_lat_us:\n[0, 10) 500\n[10, 50) 200\n[50, 100) 50\n")
    assert r["type"]=="sched_latency" and r["total_events"]==750

def t_parse_bpf_histogram_suffix_buckets():
    from bpf_analyzer import parse_bpf_histogram
    text = "@io_lat_us:\n[0] 5 |@@@@@\n[1K, 2K) 3 |@@@\n[2K, 4K) 2 |@@\n"
    r = parse_bpf_histogram(text)
    assert r["type"]=="io_latency" and len(r["buckets"])==3
    assert r["buckets"][1]["low"]==1024 and r["buckets"][1]["high"]==2048
    assert r["total_events"]==10

def t_parse_bpf_histogram_merges_duplicate_buckets():
    from bpf_analyzer import parse_bpf_histogram
    text = "@io_lat_us:\n[1K, 2K) 3 |@@@\n[2K, 4K) 2 |@@\n\n@io_lat_us:\n[1K, 2K) 1 |@\n"
    r = parse_bpf_histogram(text)
    assert len(r["buckets"]) == 2
    assert r["buckets"][0]["range"] == "[1K, 2K)"
    assert r["buckets"][0]["count"] == 4
    assert r["summary"]["max"] == 4096

def t_parse_bpf_histogram_empty():
    from bpf_analyzer import parse_bpf_histogram
    r = parse_bpf_histogram("")
    assert r["type"]=="unknown" and r["buckets"]==[]

def t_bpf_histogram_to_svg():
    from bpf_analyzer import bpf_histogram_to_svg, parse_bpf_histogram
    svg = bpf_histogram_to_svg(parse_bpf_histogram("@io_lat_us:\n[1, 4) 10\n[4, 16) 20\n"), title="Test IO")
    assert svg.startswith('<svg') and 'Test IO' in svg and 'rect' in svg

def t_analyze_bpf_output_auto():
    from bpf_analyzer import analyze_bpf_output
    r = analyze_bpf_output("@io_lat_us:\n[1, 2) 5\n[2, 4) 10\n")
    assert r["type"]=="io_latency"

def t_analyze_bpf_output_cpu_schema():
    from bpf_analyzer import analyze_bpf_output
    r = analyze_bpf_output("bash;main;hot 2\nbash;main;cold 1\n", data_type="collapsed")
    assert r["sample_unit"] == "samples"
    assert r["self_time_top"][0]["function"] == "hot"

def t_analyze_bpf_prefers_raw_bpf_artifact():
    import hotmethod_analyzer as hm

    class Cursor:
        def execute(self, sql, args):
            self.args = args
        def fetchall(self):
            return [("tid-bpf/raw.bpf",), ("tid-bpf/perf.data",)]
        def close(self):
            pass

    class Conn:
        def cursor(self):
            return Cursor()

    class Storage:
        def __init__(self):
            self.read_keys = []
            self.writes = {}
        def object_exists(self, bucket, key):
            return key == "tid-bpf/raw.bpf"
        def get_object(self, bucket, key):
            self.read_keys.append(key)
            if key == "tid-bpf/raw.bpf":
                return b"@io_lat_us:\n[1, 2) 5\n[2, 4) 10\n"
            return None
        def put_object(self, bucket, key, data, content_type):
            self.writes[key] = content_type
        def presigned_get_url(self, bucket, key):
            return "http://example/" + key

    storage = Storage()
    original_connect = hm._connect_storage
    try:
        hm._connect_storage = lambda cfg: (storage, True)
        result = hm._analyze_bpf(Conn(), {}, {"name": "bpf", "request_params": {"event": "io"}}, "drop-data", "tid-bpf", "/tmp")
    finally:
        hm._connect_storage = original_connect

    assert storage.read_keys[0] == "tid-bpf/raw.bpf"
    assert "tid-bpf/bpf_data.json" in storage.writes
    assert "tid-bpf/bpf_raw.txt" in storage.writes
    assert any(item["name"] == "bpf_histogram.svg" for item in result["local_files"])

def t_analyze_bpf_falls_back_to_legacy_perf_data():
    import hotmethod_analyzer as hm

    class Cursor:
        def execute(self, sql, args):
            pass
        def fetchall(self):
            return []
        def close(self):
            pass

    class Conn:
        def cursor(self):
            return Cursor()

    class Storage:
        def __init__(self):
            self.read_keys = []
            self.writes = {}
        def object_exists(self, bucket, key):
            return key == "tid-old/perf.data"
        def get_object(self, bucket, key):
            self.read_keys.append(key)
            if key == "tid-old/perf.data":
                return b"@sched_lat_us:\n[0, 10) 3\n"
            return None
        def put_object(self, bucket, key, data, content_type):
            self.writes[key] = content_type
        def presigned_get_url(self, bucket, key):
            return "http://example/" + key

    storage = Storage()
    original_connect = hm._connect_storage
    try:
        hm._connect_storage = lambda cfg: (storage, True)
        hm._analyze_bpf(Conn(), {}, {"name": "old", "request_params": {"event": "sched"}}, "drop-data", "tid-old", "/tmp")
    finally:
        hm._connect_storage = original_connect

    assert "tid-old/perf.data" in storage.read_keys
    assert "tid-old/bpf_data.json" in storage.writes

def t_parse_memtrace():
    from memleak_analyzer import parse_memtrace
    allocs, free_lines = parse_memtrace("alloc:main;worker;my_malloc 0x1000 1024\nfree:main;my_free 0x1000\nalloc:main;leak 0x2000 4096\n")
    assert len(allocs)==2 and len(free_lines)==1
    assert allocs[0].address==0x1000 and allocs[0].size==1024

def t_detect_leaks():
    from memleak_analyzer import parse_memtrace, detect_leaks
    allocs, free_lines = parse_memtrace("alloc:main;leak 0xA 100\nalloc:main;ok 0xB 200\nfree:main;ok 0xB\n")
    leaks = detect_leaks(allocs, free_lines)
    assert len(leaks) >= 1 and leaks[0].address == 0xA

def t_detect_leaks_none():
    from memleak_analyzer import parse_memtrace, detect_leaks
    allocs, free_lines = parse_memtrace("alloc:main 0x1 100\nfree:main 0x1\n")
    leaks = detect_leaks(allocs, free_lines)
    assert len(leaks) == 0

def t_generate_mock_memtrace():
    from memleak_analyzer import generate_mock_memtrace
    trace = generate_mock_memtrace()
    assert "alloc:" in trace and len(trace) > 100

def t_advisor_load_rules():
    from analysis_advisor import AnalysisAdvisor
    a = AnalysisAdvisor()
    a.load_rules(None)
    assert len(a.rules) > 0

def t_advisor_match():
    from analysis_advisor import AnalysisAdvisor, Rule
    a = AnalysisAdvisor()
    a.rules = [Rule(regex=r".*malloc.*", advice="使用 jemalloc")]
    s = a.match([{"function":"my_malloc","samples":100,"percentage":50.0}])
    assert len(s)>0 and any("jemalloc" in x["advice"] for x in s)

def t_advisor_no_match():
    from analysis_advisor import AnalysisAdvisor, Rule
    a = AnalysisAdvisor()
    a.rules = [Rule(regex=r".*malloc.*", advice="使用 jemalloc")]
    assert len(a.match([{"function":"normal","samples":100,"percentage":50.0}])) == 0

def t_generate_suggestions():
    from analysis_advisor import generate_suggestions
    top_json = {"self_time_top":[
        {"rank":1,"function":"my_malloc","samples":100,"percentage":50.0},
        {"rank":2,"function":"fast_func","samples":50,"percentage":25.0}]}
    r = generate_suggestions(top_json, "test_task")
    assert isinstance(r, dict) and "suggestions" in r

def t_error_codes():
    from error import ErrorCode
    assert ErrorCode.OK==0 and ErrorCode.ERR_DB_CONNECT==1001 and ErrorCode.ERR_STORAGE_CONNECT==2001

def t_error_info():
    from error import ErrorCode, ErrorInfo
    d = ErrorInfo(ErrorCode.OK, "一切正常").to_dict()
    assert d["code"]==0 and d["message"]=="一切正常"

def t_analyzer_registry_defaults():
    from analyzer_registry import build_default_registry
    r = build_default_registry()
    for task_type in [0, 1, 2, 4, 5, 6]:
        assert r.get(task_type) is not None
    try:
        r.require(999)
        assert False, "未知 task_type 应该抛错"
    except KeyError:
        pass

def t_lease_job_dataclass():
    from lease import AnalysisJob, STATUS_PENDING, STATUS_RUNNING
    job = AnalysisJob(id=1, task_tid="tid-test", pipeline="perf", status=STATUS_PENDING, attempt=0)
    assert job.task_tid == "tid-test"
    assert STATUS_RUNNING == "running"

def t_java_normalize_collapsed():
    from java_analyzer import normalize_java_profile
    folded, fmt = normalize_java_profile(
        b"java.lang.Thread.run;com.example.App.handle;com.example.Foo.work 7\n"
        b"java.lang.Thread.run;com.example.App.handle;com.example.Bar.query 3\n"
    )
    assert fmt == "collapsed"
    assert "com.example.Foo.work 7" in folded

def t_java_analyze_profile_topn():
    from java_analyzer import analyze_java_profile
    result = analyze_java_profile(
        b"java.lang.Thread.run;com.example.App.handle;com.example.Foo.work 7\n"
        b"java.lang.Thread.run;com.example.App.handle;com.example.Bar.query 3\n",
        task_name="java-test",
        top_n=2,
    )
    assert result["source_format"] == "collapsed"
    assert result["top_json"]["language"] == "java"
    assert result["top_json"]["self_time_top"][0]["function"] == "com.example.Foo.work"
    assert "<svg" in result["svg"]

def t_java_unknown_profile_rejected():
    from java_analyzer import analyze_java_profile
    try:
        analyze_java_profile(b"\x00\x01not a collapsed profile", task_name="bad")
        assert False, "损坏 Java profile 应该被拒绝"
    except ValueError as e:
        assert "无法识别" in str(e)

def t_java_collapsed_topn_schema():
    from java_analyzer import analyze_java_profile
    result = analyze_java_profile(
        b"root;com.example.A.run 2\nroot;com.example.B.wait 1\n",
        task_name="java-minimal",
        top_n=10,
    )
    top = result["top_json"]["self_time_top"]
    assert result["top_json"]["total_samples"] == 3
    assert top[0]["function"] == "com.example.A.run"
    assert {"function", "samples", "percentage"}.issubset(top[0].keys())

def t_parse_pprof_top_schema():
    from hotmethod_analyzer import _parse_pprof_top
    result = _parse_pprof_top("Showing nodes accounting for 30ms, 100% of 30ms total\n"
                               "      flat  flat%   sum%        cum   cum%\n"
                               "     20ms 66.67% 66.67%      20ms 66.67%  main.work\n"
                               "     10ms 33.33%   100%      10ms 33.33%  main.read\n")
    assert result["language"] == "go"
    assert result["sample_unit"] == "seconds"
    assert result["self_time_top"][0]["function"] == "main.work"
    assert result["self_time_top"][0]["percentage"] == 66.67

def t_lease_owner_guard_sql():
    from lease import AnalysisLeaseClient

    class FakeCursor:
        def __init__(self):
            self.rowcount = 0
            self.params = None
        def execute(self, query, params):
            self.query = query
            self.params = params
            self.rowcount = 1 if params[-1] == "owner-a" else 0
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.cursor_obj = FakeCursor()
        def cursor(self):
            return self.cursor_obj
        def commit(self):
            pass
        def close(self):
            pass

    conn = FakeConn()
    client = AnalysisLeaseClient("fake", worker_id="owner-a")
    client.connect = lambda: conn
    assert client.heartbeat(7) is True
    assert conn.cursor_obj.params[-1] == "owner-a"

    late = FakeConn()
    late_client = AnalysisLeaseClient("fake", worker_id="late-owner")
    late_client.connect = lambda: late
    assert late_client.complete(7, "v-test") is False
    assert late.cursor_obj.params[-1] == "late-owner"

def t_attribution_tool_call_and_evidence_validation():
    from attribution import run_attribution

    class FakeLLM:
        enabled = True
        model = "fake-model"
        def __init__(self):
            self.calls = 0
        def chat(self, messages, tools=None):
            self.calls += 1
            if self.calls == 1:
                return {"tool_calls": [{
                    "id": "tool-1",
                    "function": {"name": "read_stack_context", "arguments": '{"function":"hot.work"}'},
                }]}
            return {"content": json.dumps({
                "reasoning_summary": "hot.work 是当前采样中的主要热点。",
                "suggestion": "优先检查 hot.work 的循环与锁竞争。",
                "evidence": [{"function": "hot.work", "detail": "占 CPU 70%", "source": "topn"}],
            })}

    result = run_attribution(
        None,
        {"tid": "t1", "name": "test", "type": 0, "request_params": {}},
        {"total_samples": 10, "self_time_top": [{"function": "hot.work", "samples": 7, "percentage": 70.0}]},
        "main;hot.work 7\n",
        llm_client=FakeLLM(),
    )
    assert result["status"] == "completed"
    assert result["done"] is True
    assert result["evidence"][0]["function"] == "hot.work"

def t_attribution_disabled_is_non_blocking():
    from attribution import run_attribution
    class DisabledLLM:
        enabled = False
        model = "disabled"
    result = run_attribution(
        None, {"tid": "t1", "type": 0},
        {"self_time_top": [{"function": "work", "samples": 1, "percentage": 100.0}]},
        llm_client=DisabledLLM(),
    )
    assert result["status"] == "skipped" and result["done"] is True

def t_analyzer_contract_lifecycle_and_manifest():
    from analyzer_contract import BaseAnalyzer, AnalyzerResult, InputSpec

    calls = []

    class DemoAnalyzer(BaseAnalyzer):
        name = "demo"
        pipeline = "demo_pipeline"
        input_spec = InputSpec("perf.data", ["perf"])
        def validate(self, context):
            calls.append("validate")
            return {"input_artifacts": [{"id": 1, "object_key": "tid/perf.data", "size": 5}]}
        def prepare(self, context, validated_input):
            calls.append("prepare")
            return super().prepare(context, validated_input)
        def analyze(self, prepared):
            calls.append("analyze")
            return AnalyzerResult(outputs=["tid/top.json"])
        def build_manifest(self, prepared, result):
            calls.append("build_manifest")
            return super().build_manifest(prepared, result)
        def cleanup(self, prepared, result=None):
            calls.append("cleanup")

    result = DemoAnalyzer().run(None, {}, {"tid": "tid"}, "bucket", "tid")
    assert calls == ["validate", "prepare", "analyze", "build_manifest", "cleanup"]
    assert result["manifest"]["task_id"] == "tid"
    assert result["manifest"]["pipeline"] == "demo_pipeline"
    assert result["manifest"]["output_artifacts"][0]["content_type"] == "application/json"

def t_validate_raw_input_checks_hash_size_manifest():
    import analyzer_contract
    from analyzer_contract import InputSpec, validate_raw_input

    data = b"abc123"
    import hashlib
    digest = hashlib.sha256(data).hexdigest()

    class FakeCursor:
        def __init__(self):
            self.rows = [{
                "id": 9,
                "object_key": "tid/perf.data",
                "size": len(data),
                "sha256": digest,
                "hash": "",
                "etag": "",
                "manifest_key": "tid/raw_manifest.json",
            }]
        def execute(self, query, params):
            pass
        def fetchall(self):
            return self.rows
        def close(self):
            pass

    class FakeConn:
        def cursor(self, *args, **kwargs):
            return FakeCursor()

    class FakeStorage:
        def get_object(self, bucket, key):
            if key.endswith("raw_manifest.json"):
                return json.dumps({
                    "object_key": "tid/perf.data",
                    "size": len(data),
                    "sha256": digest,
                }).encode()
            return data

    old_connect = analyzer_contract._connect_storage
    try:
        analyzer_contract._connect_storage = lambda cfg: FakeStorage()
        result = validate_raw_input(
            {"conn": FakeConn(), "tid": "tid", "storage_cfg": {}, "bucket": "b"},
            InputSpec("perf.data", ["perf"], max_size=100),
        )
    finally:
        analyzer_contract._connect_storage = old_connect
    assert result["source"] == "object_storage"
    assert result["input_artifacts"][0]["sha256"] == digest

def t_validate_raw_input_rejects_hash_mismatch():
    import analyzer_contract
    from analyzer_contract import AnalyzerInputError, InputSpec, validate_raw_input

    class FakeCursor:
        def execute(self, query, params):
            pass
        def fetchall(self):
            return [{"id": 1, "object_key": "tid/perf.data", "size": 3, "sha256": "0" * 64}]
        def close(self):
            pass

    class FakeConn:
        def cursor(self, *args, **kwargs):
            return FakeCursor()

    class FakeStorage:
        def get_object(self, bucket, key):
            return b"abc"

    old_connect = analyzer_contract._connect_storage
    try:
        analyzer_contract._connect_storage = lambda cfg: FakeStorage()
        try:
            validate_raw_input(
                {"conn": FakeConn(), "tid": "tid", "storage_cfg": {}, "bucket": "b"},
                InputSpec("perf.data", ["perf"], max_size=100),
            )
            assert False, "hash mismatch should fail"
        except AnalyzerInputError as e:
            assert "sha256" in str(e)
    finally:
        analyzer_contract._connect_storage = old_connect

def t_advisor_stage4_fields_are_backward_compatible():
    from analysis_advisor import generate_suggestions
    result = generate_suggestions({"self_time_top": [
        {"function": "pthread_mutex_lock", "samples": 10, "percentage": 80.0}
    ]})
    item = result["suggestions"][0]
    assert "advice" in item and "rule_regex" in item
    assert item["evidence"][0]["function"] == "pthread_mutex_lock"
    assert {"threshold", "severity", "action", "rule_version"}.issubset(item.keys())
    assert result["rule_version"]

def t_finalize_success_updates_job_and_task_in_one_connection():
    from lease import AnalysisJob, AnalysisLeaseClient
    from analysis_daemon import finalize_success_tx

    class FakeCursor:
        def __init__(self, conn):
            self.conn = conn
            self.rowcount = 1
        def execute(self, query, params=None):
            self.conn.queries.append((query, params))
            if "SELECT id FROM task_attempts" in query:
                self.fetchone_value = (7,)
            elif "RETURNING id" in query:
                self.fetchone_value = (11,)
            elif "COALESCE(MAX(sequence)" in query:
                self.fetchone_value = (2,)
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.queries = []
        def cursor(self, *args, **kwargs):
            return FakeCursor(self)

    conn = FakeConn()
    client = AnalysisLeaseClient("fake", worker_id="owner-a")
    job = AnalysisJob(id=5, task_tid="tid", pipeline="perf", status="running", attempt=1)
    finalize_success_tx(conn, client, job, "tid", {"outputs": ["tid/top.json"], "manifest": {"ok": True}}, "v4")
    sql = "\n".join(q for q, _ in conn.queries)
    assert "INSERT INTO artifacts" in sql
    assert "UPDATE analysis_jobs" in sql and "output_artifact_ids" in sql
    assert "UPDATE hotmethod_tasks" in sql
    assert "INSERT INTO task_status_events" in sql

def t_stage6_collector_declarations_are_gated():
    from analyzer_registry import collector_declarations
    decls = {item["task_kind"]: item for item in collector_declarations()}
    assert decls["go_pprof"]["enabled"] is True
    assert decls["async_profiler_java"]["pipeline"] == "java_async_profiler"
    for task_kind in ["java_heap", "python_py_spy", "python_memray", "gperftools_cpu", "bolt_profile", "script_diagnostic"]:
        assert task_kind in decls
        assert decls[task_kind]["enabled"] is False
        assert decls[task_kind]["capabilities"]

if __name__ == "__main__":
    tests = [v for k,v in list(globals().items()) if k.startswith("t_") and callable(v)]
    passed = failed = 0
    for test in tests:
        try:
            test()
            print(f"  ✅ {test.__name__}")
            passed += 1
        except Exception as e:
            import traceback
            print(f"  ❌ {test.__name__}: {e}")
            failed += 1
    print(f"\n{'='*50}")
    print(f"结果: {passed} 通过, {failed} 失败, {len(tests)} 总计")
    sys.exit(0 if failed == 0 else 1)
