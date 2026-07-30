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
