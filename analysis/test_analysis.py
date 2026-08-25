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

def t_perf_script_keeps_partial_output_on_nonzero_exit():
    import subprocess
    import flamegraph

    original_run = flamegraph.subprocess.run
    try:
        flamegraph.subprocess.run = lambda args, **kwargs: subprocess.CompletedProcess(
            args, 1,
            stdout="python 1/1 [000] 1.0: cpu-clock:\n        abc hot (/tmp/app)\n",
            stderr="corrupted tail detected",
        )
        output = flamegraph.run_perf_script("/tmp/partial.perf")
    finally:
        flamegraph.subprocess.run = original_run
    assert "hot" in output

def t_perf_script_rejects_nonzero_exit_without_samples():
    import subprocess
    import flamegraph

    original_run = flamegraph.subprocess.run
    try:
        flamegraph.subprocess.run = lambda args, **kwargs: subprocess.CompletedProcess(
            args, 1, stdout="", stderr="invalid perf.data"
        )
        try:
            flamegraph.run_perf_script("/tmp/broken.perf")
            assert False, "没有任何样本的非零退出必须失败"
        except subprocess.CalledProcessError as exc:
            assert "invalid perf.data" in (exc.stderr or "")
    finally:
        flamegraph.subprocess.run = original_run

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
        def put_object(self, bucket, key, data, content_type, content_encoding=""):
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
        def put_object(self, bucket, key, data, content_type, content_encoding=""):
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

class _FakeMemleakCursor:
    def execute(self, sql, args=None):
        pass
    def close(self):
        pass

class _FakeMemleakConn:
    def cursor(self):
        return _FakeMemleakCursor()

class _FakeMemleakStorage:
    def __init__(self, has_object=False):
        self.has_object = has_object
        self.writes = {}
    def object_exists(self, bucket, key):
        return self.has_object
    def get_object(self, bucket, key):
        return None
    def put_object(self, bucket, key, data, content_type, content_encoding=""):
        self.writes[key] = content_type
    def presigned_get_url(self, bucket, key):
        return "http://example/" + key

def t_analyze_memleak_missing_data_fails_without_mock_flag():
    import hotmethod_analyzer as hm
    original_connect = hm._connect_storage
    original_env = os.environ.pop("DROP_ALLOW_EBPF_MOCK", None)
    try:
        hm._connect_storage = lambda cfg: (_FakeMemleakStorage(has_object=False), True)
        try:
            hm._analyze_memleak(_FakeMemleakConn(), {}, {"name": "leak"}, "drop-data", "tid-leak", "/tmp")
            assert False, "缺数据且未开 mock 开关时应该报错退出"
        except SystemExit as e:
            assert e.code != 0
    finally:
        hm._connect_storage = original_connect
        if original_env is not None:
            os.environ["DROP_ALLOW_EBPF_MOCK"] = original_env

def t_analyze_memleak_uses_mock_when_flag_enabled():
    import hotmethod_analyzer as hm
    original_connect = hm._connect_storage
    original_env = os.environ.get("DROP_ALLOW_EBPF_MOCK")
    try:
        hm._connect_storage = lambda cfg: (_FakeMemleakStorage(has_object=False), True)
        os.environ["DROP_ALLOW_EBPF_MOCK"] = "1"
        result = hm._analyze_memleak(_FakeMemleakConn(), {}, {"name": "leak"}, "drop-data", "tid-leak", "/tmp")
        assert "memleak.json" in "".join(result["outputs"]) or any("memleak" in o for o in result["outputs"])
    finally:
        hm._connect_storage = original_connect
        if original_env is None:
            os.environ.pop("DROP_ALLOW_EBPF_MOCK", None)
        else:
            os.environ["DROP_ALLOW_EBPF_MOCK"] = original_env

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
    for task_type in [0, 1, 2, 4, 5]:
        assert r.get(task_type) is not None
    # task_type=6 (java_heap) 只在 collector_declarations() 里声明为
    # enabled=False，真实分析器未实现，不应注册进 build_default_registry()。
    assert r.get(6) is None
    try:
        r.require(6)
        assert False, "未注册的 task_type=6 应该抛错"
    except KeyError:
        pass
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


def t_parse_pprof_top_heap_bytes():
    from hotmethod_analyzer import _parse_pprof_top
    result = _parse_pprof_top("Showing nodes accounting for 1048576, 100% of 1048576 total\n"
                              "      flat  flat%   sum%        cum   cum%\n"
                              "  524288 50.00% 50.00%   524288 50.00%  main.allocBig\n"
                              "  524288 50.00%   100%  1048576   100%  main.run\n",
                              sample_unit="bytes")
    assert result["sample_unit"] == "bytes"
    assert result["self_time_top"][0]["function"] == "main.allocBig"
    assert result["self_time_top"][0]["samples"] == 524288


def t_parse_pprof_top_converts_units():
    from hotmethod_analyzer import _parse_pprof_top
    cpu = _parse_pprof_top("     20ms 66.67% 66.67%      20ms 66.67%  main.work\n")
    assert abs(cpu["self_time_top"][0]["samples"] - 0.02) < 0.000001
    heap = _parse_pprof_top("     1.5MB 75.00% 75.00%      1.5MB 75.00%  main.alloc\n",
                            sample_unit="bytes")
    assert heap["self_time_top"][0]["samples"] == 1.5 * 1024 * 1024


def t_analyze_pprof_dispatches_heap_by_task_kind():
    """_analyze_pprof should dispatch to heap analyzer when task_kind=go_pprof_heap."""
    import inspect
    import hotmethod_analyzer as hm
    src = inspect.getsource(hm._analyze_pprof)
    assert "go_pprof_heap" in src
    assert "_analyze_pprof_heap" in src
    # heap analyzer exists and references bytes sample_unit
    heap_src = inspect.getsource(hm._analyze_pprof_heap)
    assert "bytes" in heap_src
    assert "inuse_space" in heap_src


def t_analyze_pprof_dispatches_heap_by_url():
    """_analyze_pprof should detect heap by /heap in pprof_url."""
    import inspect
    import hotmethod_analyzer as hm
    src = inspect.getsource(hm._analyze_pprof)
    assert "/heap" in src

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

def t_switch_active_manual_reanalysis_wins():
    """阶段 4：人工重分析（manual）成功后总是切换 active，并降级旧代产物。"""
    from lease import AnalysisJob
    from analysis_daemon import _switch_active_job_tx

    class FakeCursor:
        def __init__(self, conn):
            self.conn = conn
            self.rowcount = 1
        def execute(self, query, params=None):
            self.conn.queries.append(query)
            if "SELECT active_analysis_job_id" in query:
                self.fetchone_value = (3,)
            elif "SELECT attempt_id FROM analysis_jobs" in query:
                self.fetchone_value = (10,)
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.queries = []
        def cursor(self):
            return FakeCursor(self)

    conn = FakeConn()
    job = AnalysisJob(id=9, task_tid="tid", pipeline="perf", status="running",
                      attempt=1, attempt_id=7, generation=2, trigger="manual")
    switched = _switch_active_job_tx(conn, job, "tid")
    assert switched is True
    sql = "\n".join(conn.queries)
    assert "superseded_at = NOW()" in sql
    assert "result_superseded" in sql and "hours" in sql
    assert "active_analysis_job_id = %s" in sql

def t_switch_active_initial_late_attempt_kept():
    """阶段 4：迟到的旧 attempt 自动分析（initial）成功时不覆盖更新 attempt 的结果。"""
    from lease import AnalysisJob
    from analysis_daemon import _switch_active_job_tx

    class FakeCursor:
        def __init__(self, conn):
            self.conn = conn
            self.rowcount = 1
        def execute(self, query, params=None):
            self.conn.queries.append(query)
            if "SELECT active_analysis_job_id" in query:
                self.fetchone_value = (3,)
            elif "SELECT attempt_id FROM analysis_jobs" in query:
                self.fetchone_value = (10,)  # active job 的 attempt 更新（10 > 7）
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.queries = []
        def cursor(self):
            return FakeCursor(self)

    conn = FakeConn()
    job = AnalysisJob(id=9, task_tid="tid", pipeline="perf", status="running",
                      attempt=1, attempt_id=7, generation=2, trigger="initial")
    switched = _switch_active_job_tx(conn, job, "tid")
    assert switched is False
    sql = "\n".join(conn.queries)
    assert "superseded_at = NOW()" in sql
    assert "result_superseded" in sql
    assert "active_analysis_job_id = %s" not in sql

def t_switch_active_initial_newer_attempt_switches():
    """阶段 4：新 attempt 的自动分析（initial）成功时正常切换。"""
    from lease import AnalysisJob
    from analysis_daemon import _switch_active_job_tx

    class FakeCursor:
        def __init__(self, conn):
            self.conn = conn
            self.rowcount = 1
        def execute(self, query, params=None):
            self.conn.queries.append(query)
            if "SELECT active_analysis_job_id" in query:
                self.fetchone_value = (3,)
            elif "SELECT attempt_id FROM analysis_jobs" in query:
                self.fetchone_value = (6,)  # active job 的 attempt 更旧（6 < 12）
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.queries = []
        def cursor(self):
            return FakeCursor(self)

    conn = FakeConn()
    job = AnalysisJob(id=9, task_tid="tid", pipeline="perf", status="running",
                      attempt=1, attempt_id=12, generation=3, trigger="initial")
    switched = _switch_active_job_tx(conn, job, "tid")
    assert switched is True
    sql = "\n".join(conn.queries)
    assert "superseded_at = NOW()" in sql

def t_record_result_artifacts_carries_job_and_logical_name():
    """阶段 4：descriptor 登记携带 analysis_job_id 与 logical_name。"""
    from analysis_daemon import record_result_artifacts

    class FakeCursor:
        rowcount = 1
        def __init__(self, conn):
            self.conn = conn
        def execute(self, query, params=None):
            self.conn.queries.append((query, params))
            if "INSERT INTO storage_blobs" in query:
                self.fetchone_value = (11,)
            elif "RETURNING id" in query:
                self.fetchone_value = (22,)
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def __init__(self):
            self.queries = []
        def cursor(self):
            return FakeCursor(self)

    descriptor = {
        "object_key": "tasks/tid/analysis/perf_flamegraph/v4/g2/flamegraph.svg",
        "logical_name": "flamegraph.svg",
        "blob_key": "blobs/sha256/aa/x/svg-v1.gz",
        "kind": "RESULT", "format": "svg", "schema_version": "1",
        "compression": "gzip", "logical_sha256": "a" * 64,
        "stored_sha256": "b" * 64, "logical_size": 100, "stored_size": 20,
        "content_encoding": "gzip", "content_type": "image/svg+xml",
    }
    conn = FakeConn()
    ids = record_result_artifacts(conn, "tid", 7, 9, 2, [descriptor])
    assert ids == [22]
    # 找到 artifacts INSERT 的参数，校验 analysis_job_id / logical_name / 前缀 key
    inserts = [q for q in conn.queries if "INSERT INTO artifacts" in q[0]]
    assert len(inserts) == 1
    query, params = inserts[0]
    assert "analysis_job_id" in query and "logical_name" in query
    assert 9 in params and "flamegraph.svg" in params
    assert params[params.index("tasks/tid/analysis/perf_flamegraph/v4/g2/flamegraph.svg")] == descriptor["object_key"]

def t_record_result_artifacts_removes_upload_rejected_by_tombstone():
    from analysis_daemon import record_result_artifacts

    class FakeCursor:
        def execute(self, query, params=None):
            self.fetchone_value = (7,) if "SELECT id FROM task_attempts" in query else None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def cursor(self):
            return FakeCursor()

    class FakeStorage:
        def __init__(self):
            self.deleted = []
        def stat_object(self, bucket, key):
            return 123
        def delete_object(self, bucket, key):
            self.deleted.append((bucket, key))

    storage = FakeStorage()
    ids = record_result_artifacts(
        FakeConn(), "tid", 0, 0, 0, ["tid/top.json"], storage=storage, bucket="drop-data"
    )
    assert ids == []
    assert storage.deleted == [("drop-data", "tid/top.json")]

def t_record_result_artifacts_never_deletes_shared_cas_on_tombstone():
    from analysis_daemon import record_result_artifacts

    class FakeCursor:
        rowcount = 1
        def execute(self, query, params=None):
            if "SELECT id FROM task_attempts" in query:
                self.fetchone_value = (7,)
            elif "INSERT INTO storage_blobs" in query:
                self.fetchone_value = (11,)
            elif "INSERT INTO artifacts" in query:
                self.fetchone_value = None
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value
        def close(self):
            pass

    class FakeConn:
        def cursor(self):
            return FakeCursor()

    class FakeStorage:
        def __init__(self):
            self.deleted = []
        def delete_object(self, bucket, key):
            self.deleted.append((bucket, key))

    descriptor = {
        "object_key": "tid/flamegraph.svg", "blob_key": "blobs/sha256/aa/x/svg-v1.gz",
        "kind": "RESULT", "format": "svg", "schema_version": "1",
        "compression": "gzip", "logical_sha256": "a" * 64,
        "stored_sha256": "b" * 64, "logical_size": 100, "stored_size": 20,
        "content_encoding": "gzip", "content_type": "image/svg+xml",
    }
    storage = FakeStorage()
    ids = record_result_artifacts(FakeConn(), "tid", 0, 0, 0, [descriptor],
                                  storage=storage, bucket="drop-data")
    assert ids == []
    assert storage.deleted == []

def t_blob_registration_rejects_deleting_blob():
    from analysis_daemon import _upsert_blob_row
    from analyzer_contract import AnalyzerTemporaryError

    class FakeCursor:
        rowcount = 0
        def execute(self, query, params=None):
            if "INSERT INTO storage_blobs" in query:
                self.fetchone_value = None
            elif "SELECT id, status" in query:
                self.fetchone_value = (11, "deleting")
            else:
                self.fetchone_value = None
        def fetchone(self):
            return self.fetchone_value

    class FakeConn:
        def cursor(self):
            return FakeCursor()

    descriptor = {
        "blob_key": "blobs/sha256/aa/x/svg-v1.gz", "format": "svg",
        "schema_version": "1", "compression": "gzip",
        "logical_sha256": "a" * 64, "stored_sha256": "b" * 64,
        "logical_size": 100, "stored_size": 20,
        "content_encoding": "gzip", "content_type": "image/svg+xml",
    }
    try:
        _upsert_blob_row(FakeConn(), descriptor)
        raise AssertionError("deleting blob must reject registration")
    except AnalyzerTemporaryError:
        pass

def t_manifest_includes_descriptor_outputs_without_payload():
    from analyzer_contract import build_manifest
    descriptor = {
        "object_key": "tid/flamegraph.svg", "blob_key": "blobs/sha256/aa/x/svg-v1.gz",
        "kind": "RESULT", "format": "svg", "schema_version": "1",
        "compression": "gzip", "logical_sha256": "a" * 64,
        "stored_sha256": "b" * 64, "logical_size": 100, "stored_size": 20,
        "content_encoding": "gzip", "content_type": "image/svg+xml", "_payload": b"gzip",
    }
    manifest = build_manifest(task_id="tid", job_id=1, pipeline="perf",
                              analyzer_name="cpu", analyzer_version="v1",
                              input_artifacts=[], outputs=[descriptor, "tid/suggestions.md"])
    outputs = manifest["output_artifacts"]
    assert [item["object_key"] for item in outputs] == [
        "tid/flamegraph.svg", "tid/suggestions.md"
    ]
    assert outputs[0]["logical_sha256"] == "a" * 64
    assert "_payload" not in outputs[0]

def t_pprof_enforce_rejects_upload_failure():
    import hotmethod_analyzer as hm
    from analyzer_contract import AnalyzerTemporaryError

    old = os.environ.get("PORTABLE_PROFILE_MODE")
    os.environ["PORTABLE_PROFILE_MODE"] = "enforce"
    try:
        try:
            hm._upload_cpu_pprof(False, None, "drop-data", {"_payload": b"x"}, "tid")
            raise AssertionError("enforce upload failure must raise")
        except AnalyzerTemporaryError:
            pass
    finally:
        if old is None:
            os.environ.pop("PORTABLE_PROFILE_MODE", None)
        else:
            os.environ["PORTABLE_PROFILE_MODE"] = old

def t_task_time_nanos_uses_database_timestamps():
    from datetime import datetime, timezone
    from hotmethod_analyzer import _task_time_nanos
    start = datetime(2026, 8, 23, 1, 2, 3, tzinfo=timezone.utc)
    end = datetime(2026, 8, 23, 1, 2, 5, tzinfo=timezone.utc)
    start_ns, duration_ns = _task_time_nanos({"create_time": start, "end_time": end})
    assert start_ns == int(start.timestamp() * 1e9)
    assert duration_ns == 2_000_000_000

def t_stage6_collector_declarations_are_gated():
    from analyzer_registry import collector_declarations
    decls = {item["task_kind"]: item for item in collector_declarations()}
    assert decls["go_pprof"]["enabled"] is True
    assert decls["go_pprof_heap"]["enabled"] is True
    assert decls["go_pprof_heap"]["pipeline"] == "pprof_heap"
    assert decls["go_pprof_heap"]["capabilities"] == ["pprof"]
    assert decls["async_profiler_java"]["pipeline"] == "java_async_profiler"
    for task_kind in ["java_heap", "python_py_spy", "python_memray", "gperftools_cpu", "bolt_profile", "script_diagnostic"]:
        assert task_kind in decls
        assert decls[task_kind]["enabled"] is False
        assert decls[task_kind]["capabilities"]

def t_symbolization_chain_is_wired():
    """
    符号化链路完整性回归测试

    内核符号链路（阶段一）：Agent 快照 kallsyms → analysis 下载 → 透传给
    perf script。用户态符号链路（阶段三）：Agent 按 build-id 去重上传 →
    symbolizer 查 task_build_ids 表逐个下载安装到 perf 默认查找路径。
    缺任一段符号都解析不出来。

    2026-08-13 的一次远端合并丢掉了 _download_symbol_archive 的定义，
    但调用处还在，导致每个 perf CPU 任务的分析都抛 NameError 崩溃，
    且 py_compile 查不出来（语法合法，只是名字没定义）。故加此测试，
    阶段三重构符号化实现后，断言对象换成新的 symbolizer.py 模块。

    用 AST 源码检查而非 import：hotmethod_analyzer/symbolizer 依赖
    minio/psycopg2，在未安装这些模块的环境里 import 会直接失败，
    源码级检查不受影响，任何环境都能跑。
    """
    import ast, os, builtins

    base = os.path.dirname(os.path.abspath(__file__))

    def scan(path):
        tree = ast.parse(open(path, encoding="utf-8").read())
        defined = {n.name for n in ast.walk(tree)
                   if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))}
        imported = set()
        for n in ast.walk(tree):
            if isinstance(n, ast.ImportFrom):
                imported |= {a.asname or a.name for a in n.names}
            elif isinstance(n, ast.Import):
                imported |= {(a.asname or a.name).split(".")[0] for a in n.names}
        called = {n.func.id for n in ast.walk(tree)
                  if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)}
        return tree, defined, imported, called

    _, ana_def, ana_imp, ana_called = scan(os.path.join(base, "hotmethod_analyzer.py"))
    fg_tree, fg_def, _, _ = scan(os.path.join(base, "flamegraph.py"))
    _, sym_def, _, _ = scan(os.path.join(base, "symbolizer.py"))

    # 1) 内核符号链路：下载 + 消费两端都必须存在
    assert "_download_kallsyms" in ana_def, "内核符号下载环节缺失"

    # 2) 用户态符号链路（阶段三）：symbolizer.py 三个函数都必须定义，
    #    且 hotmethod_analyzer 必须真的调用了编排函数，不能只有定义没有调用点。
    for fn_name in ("get_task_build_ids", "install_symbol", "install_symbols_for_task"):
        assert fn_name in sym_def, f"symbolizer.{fn_name} 缺失"
    assert "install_symbols_for_task" in ana_called, \
        "hotmethod_analyzer 没有调用 symbolizer.install_symbols_for_task"

    # 3) 内核符号的两条消费路径都必须接得住 kallsyms_path，否则下载了也传不进去。
    #    SVG(generate_flamegraph) 和 TopN(get_folded_stacks) 各调一次
    #    perf script，漏接任一个，那条路径的符号就会退回 [unknown]——
    #    阶段一的 kallsyms 就是只接了 SVG 漏了 TopN，见设计文档 §9.1。
    for fn_name in ("generate_flamegraph", "get_folded_stacks"):
        fn = next((n for n in ast.walk(fg_tree)
                   if isinstance(n, ast.FunctionDef) and n.name == fn_name), None)
        assert fn is not None, f"{fn_name} 缺失"
        fn_args = {a.arg for a in fn.args.args} | {a.arg for a in fn.args.kwonlyargs}
        assert "kallsyms_path" in fn_args, f"{fn_name} 丢失 kallsyms_path 参数"

    # 4) 阶段三清理：symbol_archive 相关旧代码路径应已彻底删除，不是改了个名字
    #    还留着半条尾巴——留着才是真正的隐患（两套机制并存、行为不可预测）。
    assert "_install_symbol_archive" not in fg_def, \
        "旧的整包符号解包函数应已删除（阶段三改为按 build-id 安装）"
    assert "_download_symbol_archive" not in ana_def, \
        "旧的整包符号下载函数应已删除（阶段三改为查 task_build_ids 按需下载）"

    # 5) 兜底：没有"调用了但没定义"的内部函数（合并丢代码的典型症状）
    missing = sorted(c for c in ana_called
                     if c.startswith("_") and c not in ana_def
                     and c not in ana_imp and not hasattr(builtins, c))
    assert not missing, f"hotmethod_analyzer 调用了未定义的内部函数: {missing}"


def t_kallsyms_lookup_ignores_sample_attempt_filter():
    import os
    import hotmethod_analyzer as hm
    from job_context import use

    class Cursor:
        def __init__(self):
            self.sql = ""
            self.params = []
        def execute(self, sql, params):
            self.sql, self.params = sql, params
        def fetchall(self):
            return [("kernel-symbols/abc/kallsyms.gz",)]
        def close(self):
            pass
    class Conn:
        def __init__(self):
            self.last = None
        def cursor(self):
            self.last = Cursor()
            return self.last

    conn = Conn()
    with use({"attempt_id": 42}):
        keys = hm._raw_artifact_keys(
            conn, "tid", suffixes=["kallsyms.gz"], match_attempt=False
        )
    assert keys == ["kernel-symbols/abc/kallsyms.gz"]
    assert "a.attempt_id = %s" not in conn.last.sql
    assert conn.last.params == ["tid"]


def t_timed_out_legacy_thread_keeps_immutable_job_context():
    import threading
    import time
    from analyzer_contract import AnalyzerTemporaryError, InputSpec, LegacyFunctionAnalyzer
    from artifact_descriptor import current_output_prefix
    from job_context import use

    started = threading.Event()
    release = threading.Event()
    observed = []

    def stale_analyzer(*_args):
        started.set()
        release.wait(1)
        observed.append(current_output_prefix("tid-a"))
        return {"outputs": []}

    analyzer = LegacyFunctionAnalyzer(
        name="context-test", pipeline="perf", input_spec=InputSpec("perf.data", ["perf"]),
        output_specs=[], analyze_func=stale_analyzer, timeout_seconds=0.01,
    )
    prepared = {
        "conn": object(), "storage_cfg": {}, "task": {}, "bucket": "b", "tid": "tid-a",
        "job_context": {"output_prefix": "tasks/tid-a/analysis/perf/v/g1", "work_dir": "/tmp/a"},
    }
    try:
        analyzer.analyze(prepared)
        assert False, "legacy analyzer should time out"
    except AnalyzerTemporaryError:
        pass
    assert started.is_set()
    with use({"output_prefix": "tasks/tid-b/analysis/perf/v/g2", "work_dir": "/tmp/b"}):
        release.set()
        for _ in range(100):
            if observed:
                break
            time.sleep(0.005)
    assert observed == ["tasks/tid-a/analysis/perf/v/g1"]


def t_legacy_system_exit_is_not_reported_as_retryable_number():
    from analyzer_contract import AnalyzerInputError, InputSpec, LegacyFunctionAnalyzer

    def exits(*_args):
        raise SystemExit(1)

    analyzer = LegacyFunctionAnalyzer(
        name="exit-test", pipeline="perf", input_spec=InputSpec("perf.data", ["perf"]),
        output_specs=[], analyze_func=exits,
    )
    prepared = {
        "conn": object(), "storage_cfg": {}, "task": {}, "bucket": "b", "tid": "tid-exit",
        "job_context": {"output_prefix": "tid-exit"},
    }
    try:
        analyzer.analyze(prepared)
        assert False, "SystemExit 应转换为确定性输入错误"
    except AnalyzerInputError as exc:
        assert "exit=1" in str(exc)
        assert str(exc) != "1"


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
