// ============================================================
// tests/test_continuous_segment_processor.cpp — 统一 strict/degraded 段处理流水线单测
// ============================================================
// 阶段 2：验证同一个 perf fixture 经 strict/degraded adapter 处理后，samples、
// structured frames、runtime 分类与 symbol diagnostics 完全一致（去掉 backend
// 名称与处理时钟后）。同时覆盖 Python 路径清理、py-spy 防双计数合并、
// Session 诊断分流（rebuild_filtered_symbol_refs）与 enrichment 语义。
// 测试直接驱动 ContinuousSegmentProcessor::ProcessScript（纯逻辑），不经真实
// perf 命令，与服务器端 E2E（真实 perf.data）互补。
// ============================================================

#include "../common/ContinuousSegmentProcessor.h"

#include <gtest/gtest.h>

#include <string>
#include <vector>

using namespace drop;

namespace
{

ContinuousSamplerConfig make_cfg()
{
    ContinuousSamplerConfig cfg;
    cfg.sampleRateHz = 19;
    cfg.aggregationWindowSec = 10;
    cfg.uploadBatchSec = 60;
    cfg.sessionSID = "proc-test";
    cfg.targetIP = "127.0.0.1";
    cfg.hostname = "test-host";
    cfg.apiBaseURL = "http://127.0.0.1:8191";
    cfg.authUID = "test-uid";
    return cfg;
}

// 覆盖 native 符号帧、libc 帧、内核帧与 Python perf 帧的典型 perf script 输出。
const char *kPerfFixture =
    "api 42/42 [001] 123.456789: cpu-clock:\n"
    "        0000000000001000 worker (/opt/api/api)\n"
    "        0000000000000800 main (/opt/api/api)\n"
    "\n"
    "api 42 42 [001] 124.000000: cpu-clock:\n"
    "        7f1234abcdef do_something (/usr/lib/x86_64-linux-gnu/libc.so.6)\n"
    "\n"
    "burn 9001 [001] 124.500000: cpu-clock:\n"
    "        py::worker:/srv/app/jobs/worker.py+0x1a\n"
    "        py::main:/srv/app/main.py+0x33\n"
    "\n"
    "kworker 0 [000] 125.000001: cpu-clock:\n"
    "        ffffffff81000000 native_cp_safe_halt ([kernel.kallsyms])\n"
    "\n";

PerfSegment make_segment(const std::string &backend)
{
    PerfSegment segment;
    segment.path = "/tmp/mini_drop_test_fixture.data";
    segment.sourceBackend = backend;
    segment.collectorGeneration = "gen-test";
    segment.targetFingerprint = "fp-test";
    segment.wallStartMs = 1700000000000;
    segment.monotonicStartMs = 123000;
    return segment;
}

// 单元测试环境没有真实 /proc/<pid>/exe，靠 configured_process_exe 从
// targetProcesses 补齐 exe，runtime 分类才能工作。
void add_fixture_targets(ContinuousSamplerConfig *cfg)
{
    ContinuousTargetProcess nativeTarget;
    nativeTarget.pid = 42;
    nativeTarget.processStartMs = 1000;
    nativeTarget.exe = "/opt/api/api";
    nativeTarget.comm = "api";
    cfg->targetProcesses.push_back(nativeTarget);
    ContinuousTargetProcess pythonTarget;
    pythonTarget.pid = 9001;
    pythonTarget.processStartMs = 2000;
    pythonTarget.exe = "/usr/bin/python3.12";
    pythonTarget.comm = "burn";
    cfg->targetProcesses.push_back(pythonTarget);
}

std::string json_field(const std::string &body, const std::string &field)
{
    const std::string needle = "\"" + field + "\":\"";
    const size_t begin = body.find(needle);
    if (begin == std::string::npos)
        return {};
    const size_t valueBegin = begin + needle.size();
    const size_t valueEnd = body.find('"', valueBegin);
    if (valueEnd == std::string::npos)
        return {};
    return body.substr(valueBegin, valueEnd - valueBegin);
}

} // namespace

TEST(ContinuousSegmentProcessor, StrictAndDegradedAdaptersProduceIdenticalOutput)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.scope = "host";
    add_fixture_targets(&cfg);
    RuntimeCapabilitySet caps;
    caps.pythonFallback = false;
    caps.pythonRss = false;
    caps.memray = false;
    caps.goSymbols = false; // 纯解析路径（enrichment 关闭等价），保证 fixture 可复现

    RuntimeSymbolReport emptyRuntime;
    GoSymbolReport emptyGo;
    std::vector<BuildIdEntry> buildIds;
    std::vector<PythonFallbackResult> noPython;
    std::vector<MemrayProfileResult> noMemray;

    SegmentProcessResult strict = ProcessScript(kPerfFixture, make_segment("perf_rolling"),
                                                cfg, caps, emptyRuntime, emptyGo, buildIds,
                                                noPython, 0, noMemray);
    SegmentProcessResult degraded = ProcessScript(kPerfFixture, make_segment("perf"),
                                                  cfg, caps, emptyRuntime, emptyGo, buildIds,
                                                  noPython, 0, noMemray);
    ASSERT_TRUE(strict.success);
    ASSERT_TRUE(degraded.success);
    ASSERT_EQ(strict.windows.size(), 1u);
    ASSERT_EQ(degraded.windows.size(), 1u);

    const WindowPayload &sw = strict.windows.front();
    const WindowPayload &dw = degraded.windows.front();
    ASSERT_EQ(sw.samples.size(), dw.samples.size());

    // 去掉 backend 名称后，samples / structured frames / runtime 分类必须一致。
    for (size_t i = 0; i < sw.samples.size(); ++i)
    {
        const AggregatedSample &a = sw.samples[i];
        const AggregatedSample &b = dw.samples[i];
        EXPECT_EQ(a.comm, b.comm);
        EXPECT_EQ(a.pid, b.pid);
        EXPECT_EQ(a.exe, b.exe);
        EXPECT_EQ(a.runtime, b.runtime);
        EXPECT_EQ(a.count, b.count);
        EXPECT_EQ(a.stack, b.stack);
        EXPECT_EQ(a.frames.size(), b.frames.size());
        for (size_t f = 0; f < a.frames.size(); ++f)
        {
            EXPECT_EQ(a.frames[f].function, b.frames[f].function);
            EXPECT_EQ(a.frames[f].mappingFile, b.frames[f].mappingFile);
            EXPECT_EQ(a.frames[f].resolved, b.frames[f].resolved);
        }
    }
    // backend 名不同但 symbol diagnostics 完全一致（不因采集模式分叉）。
    EXPECT_NE(sw.samples.front().backend, dw.samples.front().backend);
    EXPECT_EQ(sw.symbolRefsJson, dw.symbolRefsJson);
    EXPECT_EQ(strict.diagnostics.symbolStatus, degraded.diagnostics.symbolStatus);
    EXPECT_EQ(strict.diagnostics.totalFrameWeight, degraded.diagnostics.totalFrameWeight);
    EXPECT_EQ(strict.diagnostics.unresolvedFrameWeight, degraded.diagnostics.unresolvedFrameWeight);
}

TEST(ContinuousSegmentProcessor, ClassifiesRuntimesAndCleansPythonPaths)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.scope = "host";
    add_fixture_targets(&cfg);
    RuntimeCapabilitySet caps;
    caps.pythonFallback = false;
    caps.pythonRss = false;
    caps.memray = false;
    caps.goSymbols = false;
    RuntimeSymbolReport emptyRuntime;
    GoSymbolReport emptyGo;
    std::vector<BuildIdEntry> buildIds;
    std::vector<PythonFallbackResult> noPython;
    std::vector<MemrayProfileResult> noMemray;

    SegmentProcessResult result = ProcessScript(kPerfFixture, make_segment("perf_rolling"),
                                                cfg, caps, emptyRuntime, emptyGo, buildIds,
                                                noPython, 0, noMemray);
    ASSERT_TRUE(result.success);
    const WindowPayload &window = result.windows.front();

    bool sawNative = false;
    bool sawPython = false;
    for (const auto &sample : window.samples)
    {
        if (sample.runtime == "python")
        {
            sawPython = true;
            for (const auto &frame : sample.stack)
            {
                // Python 绝对源码路径必须被清理（不泄漏 /srv/app/...）。
                EXPECT_EQ(frame.find("/srv/"), std::string::npos);
                EXPECT_NE(frame.find("py::"), std::string::npos);
            }
        }
        if (sample.runtime == "native")
            sawNative = true;
    }
    EXPECT_TRUE(sawNative);
    EXPECT_TRUE(sawPython);
    // 结构化帧至少存在（frames 与 stack 并行）。
    bool sawStructured = false;
    for (const auto &sample : window.samples)
        if (!sample.frames.empty())
            sawStructured = true;
    EXPECT_TRUE(sawStructured);
}

TEST(ContinuousSegmentProcessor, PythonSidecarMergesWithoutDoubleCounting)
{
    // perf 段含 PID 100 的 Python 样本；py-spy 就绪后应替换而非叠加。
    std::vector<AggregatedSample> samples;
    AggregatedSample perfPython;
    perfPython.comm = "python";
    perfPython.pid = 100;
    perfPython.processStartMs = 111;
    perfPython.exe = "/usr/bin/python3.12";
    perfPython.backend = "perf_rolling";
    perfPython.runtime = "python";
    perfPython.stack = {"py::worker:worker.py+0x1a", "main"};
    perfPython.count = 5;
    samples.push_back(perfPython);

    PythonFallbackResult ready;
    ready.pid = 100;
    ready.startMs = 111;
    ready.comm = "python";
    ready.exe = "/usr/bin/python3.12";
    ready.ready = true;
    ready.samples.push_back(PythonStackSample{{"py::worker:worker.py+0x1a", "py::main:main.py+0x33"}, 7});

    bool replaced = false;
    merge_python_sidecar_samples(&samples, {ready}, &replaced);
    EXPECT_TRUE(replaced);
    ASSERT_EQ(samples.size(), 1u);
    // 只保留 py-spy 样本（不双计数），backend 切换为 py-spy。
    EXPECT_EQ(samples.front().backend, "py-spy");
    EXPECT_EQ(samples.front().count, 7u);

    // 失败 / PID 复用：保留原 perf 样本。
    std::vector<AggregatedSample> fallbackSamples = {perfPython};
    PythonFallbackResult failed;
    failed.pid = 100;
    failed.startMs = 111;
    failed.ready = false;
    failed.reason = "py-spy failed rc=124";
    bool replaced2 = false;
    merge_python_sidecar_samples(&fallbackSamples, {failed}, &replaced2);
    EXPECT_FALSE(replaced2);
    ASSERT_EQ(fallbackSamples.size(), 1u);
    EXPECT_EQ(fallbackSamples.front().backend, "perf_rolling");

    // 相同 PID、不同 start time 是 PID 复用，不能删除新进程的 perf 样本。
    std::vector<AggregatedSample> reusedSamples = {perfPython};
    PythonFallbackResult stale = ready;
    stale.startMs = 999;
    bool replaced3 = false;
    merge_python_sidecar_samples(&reusedSamples, {stale}, &replaced3);
    EXPECT_FALSE(replaced3);
    ASSERT_EQ(reusedSamples.size(), 1u);
    EXPECT_EQ(reusedSamples.front().backend, "perf_rolling");
}

TEST(ContinuousSegmentProcessor, AsyncPythonSidecarUsesRealBoundsAndOnlyReplacesOverlap)
{
    WindowPayload before;
    before.startMs = 1000;
    before.endMs = 2000;
    WindowPayload overlap;
    overlap.startMs = 2000;
    overlap.endMs = 3000;
    AggregatedSample python;
    python.pid = 100;
    python.processStartMs = 111;
    python.runtime = "python";
    python.backend = "perf_rolling";
    python.stack = {"[unknown]"};
    python.count = 5;
    before.samples.push_back(python);
    overlap.samples.push_back(python);

    PythonFallbackResult ready;
    ready.pid = 100;
    ready.startMs = 111;
    ready.captureStartMs = 2200;
    ready.captureEndMs = 2800;
    ready.ready = true;
    ready.samples.push_back(PythonStackSample{{"worker", "main"}, 7});

    std::vector<WindowPayload> windows{before, overlap};
    apply_python_sidecars_to_windows(&windows, {ready});
    ASSERT_EQ(windows.size(), 3u);
    EXPECT_EQ(windows[0].samples.size(), 1u); // 非重叠段保持 perf fallback
    EXPECT_TRUE(windows[1].samples.empty()); // 重叠段由 py-spy 替换
    EXPECT_EQ(windows[2].startMs, 2200);
    EXPECT_EQ(windows[2].endMs, 2800);
    ASSERT_EQ(windows[2].samples.size(), 1u);
    EXPECT_EQ(windows[2].samples.front().backend, "py-spy");
}

TEST(ContinuousSegmentProcessor, RebuildFilteredSymbolRefsIsolatesProcessSession)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.symbolStatus = "partial";
    diagnostics.buildIds = {"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"};
    diagnostics.kallsymsSha256 = "kallsyms-sha-1234";
    RuntimeSymbolReport runtime;
    runtime.python.detected = true;
    runtime.python.readyPids = {100};
    runtime.python.missingPids = {100, 200};
    diagnostics.runtimeReport = runtime;

    const std::string physicalJson =
        "{\"symbol_status\":\"partial\",\"frame_stats\":{\"total_frame_weight\":10,"
        "\"unresolved_frame_weight\":3},\"build_ids\":[\"aaaaaaaaaaaaaaaa\","
        "\"bbbbbbbbbbbbbbbb\"],\"kallsyms_sha256\":\"kallsyms-sha-1234\","
        "\"runtime_maps\":{},\"native_go\":{},\"python_fallback\":{},\"python_memory\":{}}";

    // host Session 复用完整物理诊断。
    ContinuousSamplerConfig host = make_cfg();
    host.scope = "host";
    std::vector<AggregatedSample> filtered;
    EXPECT_EQ(rebuild_filtered_symbol_refs(physicalJson, diagnostics, filtered, host), physicalJson);

    // process Session 只看得到本 selector 的 build-id 与 PID。
    ContinuousSamplerConfig process = make_cfg();
    process.scope = "process";
    ContinuousTargetProcess target;
    target.pid = 100;
    target.processStartMs = 111;
    target.exe = "/usr/bin/python3.12";
    process.targetProcesses.push_back(target);

    AggregatedSample sample;
    sample.pid = 100;
    sample.processStartMs = 111;
    sample.exe = "/usr/bin/python3.12";
    sample.runtime = "python";
    sample.stack = {"py::worker:worker.py+0x1a", "main"};
    sample.count = 4;
    ContinuousStackFrame frame;
    frame.function = "worker";
    frame.buildId = "aaaaaaaaaaaaaaaa";
    sample.frames.push_back(frame);
    filtered.push_back(sample);

    const std::string rebuilt = rebuild_filtered_symbol_refs(physicalJson, diagnostics, filtered, process);
    // 只含本 Session 引用的 build-id，不含另一个 selector 的。
    EXPECT_NE(rebuilt.find("\"aaaaaaaaaaaaaaaa\""), std::string::npos);
    EXPECT_EQ(rebuilt.find("\"bbbbbbbbbbbbbbbb\""), std::string::npos);
    // 整机级 kallsyms SHA 允许复用。
    EXPECT_NE(rebuilt.find("\"kallsyms_sha256\":\"kallsyms-sha-1234\""), std::string::npos);
    // 重算后的 frame 统计反映本 Session 样本。
    EXPECT_NE(rebuilt.find("\"total_frame_weight\":8"), std::string::npos);
    // 不泄漏另一个 selector 的 PID（200 不在本 Session runtime missing 中）。
    EXPECT_EQ(rebuilt.find("200"), std::string::npos);
}

TEST(ContinuousSegmentProcessor, CombinedSymbolRefsComputesFrameStats)
{
    std::vector<AggregatedSample> samples;
    AggregatedSample ok;
    ok.comm = "api";
    ok.pid = 42;
    ok.runtime = "native";
    ok.stack = {"resolved_a", "resolved_b"};
    ok.count = 3;
    samples.push_back(ok);
    AggregatedSample un;
    un.comm = "burn";
    un.pid = 43;
    un.runtime = "native";
    un.stack = {"[unknown]", "0x7f1234 [libc.so.6]"};
    un.count = 2;
    samples.push_back(un);

    RuntimeSymbolReport emptyRuntime;
    GoSymbolReport emptyGo;
    std::vector<PythonFallbackResult> noPython;
    std::vector<MemrayProfileResult> noMemray;
    std::vector<BuildIdEntry> buildIds;
    const std::string json = combined_symbol_refs_json(emptyRuntime, emptyGo, noPython, 0,
                                                       noMemray, samples, buildIds, "sha-xyz");
    EXPECT_EQ(json_field(json, "symbol_status"), "partial");
    EXPECT_NE(json.find("\"total_frame_weight\":10"), std::string::npos);
    // [unknown] 与 0x... [libc.so.6] 均判未解析：2 帧 × count 2 = 4。
    EXPECT_NE(json.find("\"unresolved_frame_weight\":4"), std::string::npos);
}
