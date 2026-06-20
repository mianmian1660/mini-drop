# ============================================================
# test_analysis.py — analysis 模块单元测试 (修正版)
# ============================================================
import json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

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
