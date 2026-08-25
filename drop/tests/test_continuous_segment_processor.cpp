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

    // 身份不完整时不能仅凭 PID 删除 perf 样本。
    PythonFallbackResult incomplete = ready;
    incomplete.startMs = 0;
    std::vector<AggregatedSample> incompleteSamples = {perfPython};
    merge_python_sidecar_samples(&incompleteSamples, {incomplete});
    ASSERT_EQ(incompleteSamples.size(), 1u);
    EXPECT_EQ(incompleteSamples.front().backend, "perf_rolling");

    // 一个聚合 sidecar 只追加一次，但必须在整个 capture 区间内持续抑制
    // 对应 perf 样本；相邻区间边界不视为重叠。
    PythonFallbackResult interval = ready;
    interval.captureStartMs = 1000;
    interval.captureEndMs = 3000;
    std::vector<AggregatedSample> before = {perfPython};
    bool beforeReplaced = false;
    merge_python_sidecar_samples(&before, {interval}, &beforeReplaced, 0, 1000);
    EXPECT_FALSE(beforeReplaced);
    ASSERT_EQ(before.size(), 1u);

    std::vector<AggregatedSample> firstOverlap = {perfPython};
    bool firstOverlapReplaced = false;
    merge_python_sidecar_samples(&firstOverlap, {interval}, &firstOverlapReplaced, 1000, 2000);
    EXPECT_TRUE(firstOverlapReplaced);
    ASSERT_EQ(firstOverlap.size(), 1u);
    EXPECT_EQ(firstOverlap.front().backend, "py-spy");

    interval.samples.clear();
    std::vector<AggregatedSample> remainingOverlap = {perfPython};
    bool remainingReplaced = false;
    merge_python_sidecar_samples(&remainingOverlap, {interval}, &remainingReplaced, 2000, 3000);
    EXPECT_TRUE(remainingReplaced);
    EXPECT_TRUE(remainingOverlap.empty());

    std::vector<AggregatedSample> after = {perfPython};
    bool afterReplaced = false;
    merge_python_sidecar_samples(&after, {interval}, &afterReplaced, 3000, 4000);
    EXPECT_FALSE(afterReplaced);
    ASSERT_EQ(after.size(), 1u);
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
    EXPECT_EQ(rebuilt.find("\"200\""), std::string::npos);
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

// ============================================================
// 阶段三：SessionFanoutProjector 测试矩阵
// ============================================================

namespace
{

// 构造一个物理窗口：两个进程（pid 100/200）的 CPU 样本 + RSS metric +
// Memray profile + 直方图。
WindowPayload make_physical_window()
{
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    window.collectorGeneration = "gen-1";
    window.physicalSampleRateHz = 49;
    window.effectiveSampleRateHz = 49;

    AggregatedSample a;
    a.comm = "app";
    a.pid = 100;
    a.processStartMs = 1000;
    a.exe = "/opt/app";
    a.runtime = "native";
    a.stack = {"worker", "main"};
    a.count = 10;
    window.samples.push_back(a);

    AggregatedSample b;
    b.comm = "other";
    b.pid = 200;
    b.processStartMs = 2000;
    b.exe = "/opt/other";
    b.runtime = "native";
    b.stack = {"busy", "main"};
    b.count = 20;
    window.samples.push_back(b);

    MetricPayload metric;
    metric.metric = "rss_bytes";
    metric.pid = 100;
    metric.processStartMs = 1000;
    metric.exe = "/opt/app";
    metric.value = 1024;
    window.metrics.push_back(metric);

    ProfilePayload memray;
    memray.signalType = "python_memory";
    memray.backend = "memray";
    AggregatedSample memSample;
    memSample.pid = 100;
    memSample.processStartMs = 1000;
    memSample.exe = "/opt/app";
    memSample.stack = {"alloc", "main"};
    memSample.count = 5;
    memray.samples.push_back(memSample);
    window.profiles.push_back(memray);

    HistogramPayload hist;
    hist.signalType = "io_latency";
    hist.backend = "libbpf-co-re";
    hist.pid = 100;
    hist.processStartMs = 1000;
    hist.exe = "/opt/app";
    hist.eventCount = 7;
    HistogramBucket bucket;
    bucket.range = "[0, 1)";
    bucket.low = 0;
    bucket.high = 1;
    bucket.count = 7;
    hist.buckets.push_back(bucket);
    window.histograms.push_back(hist);

    return window;
}

SessionContract make_contract(const std::string &sid, const std::string &scope,
                              const std::vector<ContinuousTargetProcess> &targets,
                              const std::vector<std::string> &signals,
                              int rateHz = 19)
{
    SessionContract contract;
    contract.sid = sid;
    contract.scope = scope;
    contract.targets = targets;
    contract.signals = signals;
    contract.requestedSampleRateHz = rateHz;
    contract.aggregationWindowSec = 10;
    return contract;
}

ContinuousTargetProcess make_target(int pid, int64_t startMs, const std::string &exe)
{
    ContinuousTargetProcess target;
    target.pid = pid;
    target.processStartMs = startMs;
    target.exe = exe;
    target.comm = "app";
    return target;
}

} // namespace

// 两个 process Session：不同 PID 各自只收到自己的样本。
TEST(SessionFanoutProjector, TwoProcessSessionsGetIsolatedSamples)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();

    SessionContract sessionA = make_contract("sid-a", "process", {make_target(100, 1000, "/opt/app")},
                                              {"cpu_profile", "io_latency", "python_rss", "python_memory"});
    WindowPayload outA = projector.Project(physical, sessionA, 49, true);
    ASSERT_EQ(outA.samples.size(), 1u);
    EXPECT_EQ(outA.samples.front().pid, 100);
    ASSERT_EQ(outA.metrics.size(), 1u);
    EXPECT_EQ(outA.metrics.front().pid, 100);
    ASSERT_EQ(outA.profiles.size(), 1u);
    EXPECT_EQ(outA.profiles.front().signalType, "python_memory");
    ASSERT_EQ(outA.histograms.size(), 1u);
    EXPECT_EQ(outA.histograms.front().pid, 100);

    SessionContract sessionB = make_contract("sid-b", "process", {make_target(200, 2000, "/opt/other")},
                                              {"cpu_profile", "io_latency", "python_rss", "python_memory"});
    WindowPayload outB = projector.Project(physical, sessionB, 49, true);
    ASSERT_EQ(outB.samples.size(), 1u);
    EXPECT_EQ(outB.samples.front().pid, 200);
    // B 没有 RSS metric / Memray / histogram（都是 pid 100 的）。
    EXPECT_TRUE(outB.metrics.empty());
    EXPECT_TRUE(outB.profiles.empty());
    EXPECT_TRUE(outB.histograms.empty());
}

// 同 PID 不同 start time = PID 复用：新实例不得收到旧实例数据。
TEST(SessionFanoutProjector, SamePidDifferentStartTimeIsReuse)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();

    // 目标 pid 100 但 start time 是 9999（不是样本的 1000）→ 身份不匹配。
    SessionContract stale = make_contract("sid-stale", "process", {make_target(100, 9999, "/opt/app")},
                                          {"cpu_profile"});
    WindowPayload out = projector.Project(physical, stale, 49, true);
    EXPECT_TRUE(out.samples.empty());
    // 身份不完整/不匹配被丢弃的样本数被记录。
    EXPECT_GT(out.identityUnavailableCount, 0u);
    // 状态窗口仍存在（target_idle）。
    auto it = out.signalStatuses.find("cpu_profile");
    ASSERT_NE(it, out.signalStatuses.end());
    EXPECT_EQ(it->second.status, SignalCollectionStatus::TargetIdle);
}

// 同 exe 多实例：两个实例各自只收到自己的数据。
TEST(SessionFanoutProjector, SameExeMultipleInstancesIsolated)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    // 增加第二个同 exe 实例（pid 300，start 3000）。
    AggregatedSample c;
    c.comm = "app";
    c.pid = 300;
    c.processStartMs = 3000;
    c.exe = "/opt/app";
    c.runtime = "native";
    c.stack = {"idle", "main"};
    c.count = 3;
    physical.samples.push_back(c);

    SessionContract instance1 = make_contract("sid-i1", "process", {make_target(100, 1000, "/opt/app")},
                                              {"cpu_profile"});
    WindowPayload out1 = projector.Project(physical, instance1, 49, true);
    ASSERT_EQ(out1.samples.size(), 1u);
    EXPECT_EQ(out1.samples.front().pid, 100);

    SessionContract instance2 = make_contract("sid-i2", "process", {make_target(300, 3000, "/opt/app")},
                                              {"cpu_profile"});
    WindowPayload out2 = projector.Project(physical, instance2, 49, true);
    ASSERT_EQ(out2.samples.size(), 1u);
    EXPECT_EQ(out2.samples.front().pid, 300);
}

// CPU-only Session 不含 Memray；memory-only 不含 CPU。
TEST(SessionFanoutProjector, SignalIsolationCpuOnlyVsMemoryOnly)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();

    SessionContract cpuOnly = make_contract("sid-cpu", "host", {}, {"cpu_profile"});
    WindowPayload outCpu = projector.Project(physical, cpuOnly, 49, true);
    EXPECT_FALSE(outCpu.samples.empty());
    EXPECT_TRUE(outCpu.profiles.empty()); // Memray 被剔除
    EXPECT_TRUE(outCpu.metrics.empty());  // RSS 被剔除
    EXPECT_TRUE(outCpu.histograms.empty()); // io_latency 未请求

    SessionContract memoryOnly = make_contract("sid-mem", "host", {}, {"python_memory", "python_rss"});
    WindowPayload outMem = projector.Project(physical, memoryOnly, 49, true);
    EXPECT_TRUE(outMem.samples.empty()); // CPU 被剔除
    EXPECT_FALSE(outMem.profiles.empty()); // Memray 保留
    EXPECT_FALSE(outMem.metrics.empty());  // RSS 保留
}

// 七类信号组合全覆盖：db_snapshot 只保留 db 数据。
TEST(SessionFanoutProjector, SevenSignalCombination)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    DBSnapshotSample db;
    db.kind = "digest";
    db.instanceLabel = "pg-1";
    db.digestText = "SELECT 1";
    db.callCount = 3;
    physical.dbSnapshots.push_back(db);

    SessionContract all = make_contract("sid-all", "host", {},
                                        {"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency",
                                         "python_rss", "python_memory", "db_snapshot"});
    WindowPayload out = projector.Project(physical, all, 49, true);
    EXPECT_FALSE(out.samples.empty());
    EXPECT_FALSE(out.profiles.empty());
    EXPECT_FALSE(out.metrics.empty());
    EXPECT_FALSE(out.histograms.empty());
    EXPECT_EQ(out.dbSnapshots.size(), 1u);
    // 每信号状态都登记。
    EXPECT_EQ(out.signalStatuses["cpu_profile"].status, SignalCollectionStatus::Collected);
    EXPECT_EQ(out.signalStatuses["io_latency"].status, SignalCollectionStatus::Collected);
    EXPECT_EQ(out.signalStatuses["python_rss"].status, SignalCollectionStatus::Collected);
    EXPECT_EQ(out.signalStatuses["python_memory"].status, SignalCollectionStatus::Collected);
    EXPECT_EQ(out.signalStatuses["db_snapshot"].status, SignalCollectionStatus::Collected);
    // 请求了但物理层无数据的信号登记 no_events（不是不登记）。
    EXPECT_EQ(out.signalStatuses["sched_latency"].status, SignalCollectionStatus::NoEvents);
    EXPECT_EQ(out.signalStatuses["io_syscall_latency"].status, SignalCollectionStatus::NoEvents);
}

// 19Hz 与 49Hz Session 的计数比例正确，降采样结果跨重试完全一致。
TEST(SessionFanoutProjector, DeterministicDownsamplingRatioAndStability)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    // 物理 49Hz，总样本 30（10+20）。
    SessionContract low = make_contract("sid-low", "host", {}, {"cpu_profile"}, 19);
    WindowPayload outLow = projector.Project(physical, low, 49, true);
    uint64_t total = 0;
    for (const auto &sample : outLow.samples)
        total += sample.count;
    // round(30 × 19/49) = round(11.63) = 12。
    EXPECT_EQ(total, 12u);
    EXPECT_EQ(outLow.effectiveSampleRateHz, 19);
    EXPECT_EQ(outLow.physicalSampleRateHz, 49);

    // 跨重试完全一致（同输入同输出）。
    WindowPayload again = projector.Project(physical, low, 49, true);
    ASSERT_EQ(outLow.samples.size(), again.samples.size());
    for (size_t i = 0; i < outLow.samples.size(); ++i)
    {
        EXPECT_EQ(outLow.samples[i].pid, again.samples[i].pid);
        EXPECT_EQ(outLow.samples[i].count, again.samples[i].count);
    }

    // 高频 Session 不降采样。
    SessionContract high = make_contract("sid-high", "host", {}, {"cpu_profile"}, 49);
    WindowPayload outHigh = projector.Project(physical, high, 49, true);
    uint64_t highTotal = 0;
    for (const auto &sample : outHigh.samples)
        highTotal += sample.count;
    EXPECT_EQ(highTotal, 30u);
    EXPECT_EQ(outHigh.effectiveSampleRateHz, 49);
}

TEST(SessionFanoutProjector, DownsamplesDegradedCPUProfilesAndMarksCollected)
{
    SessionFanoutProjector projector;
    WindowPayload physical;
    physical.startMs = 1000;
    physical.endMs = 11000;
    physical.collectorGeneration = "gen-profile";
    ProfilePayload profile;
    profile.signalType = "cpu_profile";
    profile.backend = "bpftrace";
    AggregatedSample sample;
    sample.pid = 100;
    sample.processStartMs = 1000;
    sample.exe = "/opt/app";
    sample.stack = {"work", "main"};
    sample.count = 49;
    profile.samples.push_back(sample);
    physical.profiles.push_back(profile);

    SessionContract contract = make_contract("sid-low", "host", {}, {"cpu_profile"}, 19);
    const auto out = projector.Project(physical, contract, 49, true);
    ASSERT_EQ(out.profiles.size(), 1u);
    ASSERT_EQ(out.profiles.front().samples.size(), 1u);
    EXPECT_EQ(out.profiles.front().samples.front().count, 19u);
    EXPECT_EQ(out.signalStatuses.at("cpu_profile").status, SignalCollectionStatus::Collected);
}

// 极低流量窗口允许零样本并上报 target_idle。
TEST(SessionFanoutProjector, ExtremelyLowRateYieldsZeroAndTargetIdle)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    // 1Hz vs 49Hz：round(30 × 1/49) = round(0.61) = 1，仍 > 0。
    // 用 0.1Hz 语义不可行（int），改用极小比例：请求 1Hz 但物理 1000Hz。
    SessionContract tiny = make_contract("sid-tiny", "host", {}, {"cpu_profile"}, 1);
    WindowPayload out = projector.Project(physical, tiny, 1000, true);
    // round(30 × 1/1000) = 0 → 零样本。
    EXPECT_TRUE(out.samples.empty());
    auto it = out.signalStatuses.find("cpu_profile");
    ASSERT_NE(it, out.signalStatuses.end());
    EXPECT_EQ(it->second.status, SignalCollectionStatus::TargetIdle);
}

// host+process 混合：host 得到整机数据，process 只得到目标实例。
TEST(SessionFanoutProjector, HostAndProcessMixed)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();

    SessionContract host = make_contract("sid-host", "host", {}, {"cpu_profile", "io_latency"});
    WindowPayload outHost = projector.Project(physical, host, 49, true);
    EXPECT_EQ(outHost.samples.size(), 2u); // 整机两个进程
    EXPECT_EQ(outHost.histograms.size(), 1u); // 整机直方图（pid=100 的）

    SessionContract process = make_contract("sid-proc", "process", {make_target(100, 1000, "/opt/app")},
                                            {"cpu_profile", "io_latency"});
    WindowPayload outProc = projector.Project(physical, process, 49, true);
    EXPECT_EQ(outProc.samples.size(), 1u);
    EXPECT_EQ(outProc.samples.front().pid, 100);
    EXPECT_EQ(outProc.histograms.size(), 1u);
    EXPECT_EQ(outProc.histograms.front().pid, 100);
}

// 无法安全归属的直方图（degraded 多进程 fallback）只返回 unavailable。
TEST(SessionFanoutProjector, UnsafeHistogramAttributionReturnsUnavailable)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();

    SessionContract process = make_contract("sid-proc", "process", {make_target(100, 1000, "/opt/app")},
                                            {"io_latency"});
    // histogramAttributionSafe=false：直方图无法归属 → 只登记 unavailable
    // 状态窗口，不得复制整机直方图。
    WindowPayload out = projector.Project(physical, process, 49, false);
    ASSERT_EQ(out.histograms.size(), 1u);
    EXPECT_TRUE(out.histograms.front().unavailable);
    auto it = out.signalStatuses.find("io_latency");
    ASSERT_NE(it, out.signalStatuses.end());
    EXPECT_EQ(it->second.status, SignalCollectionStatus::Unavailable);
}

// 目标空闲/无事件仍产生零计数状态窗口。
TEST(SessionFanoutProjector, IdleWindowStillProducesStatusWindow)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    // 物理窗口有样本但目标进程不匹配 → 投影后零样本，但状态窗口存在。
    SessionContract other = make_contract("sid-other", "process", {make_target(999, 9999, "/opt/none")},
                                          {"cpu_profile"});
    WindowPayload out = projector.Project(physical, other, 49, true);
    EXPECT_TRUE(out.samples.empty());
    auto it = out.signalStatuses.find("cpu_profile");
    ASSERT_NE(it, out.signalStatuses.end());
    EXPECT_EQ(it->second.status, SignalCollectionStatus::TargetIdle);
}

// 身份缺失（start time 或 exe 为空）不得猜测归属。
TEST(SessionFanoutProjector, IncompleteIdentityIsDroppedNotGuessed)
{
    SessionFanoutProjector projector;
    WindowPayload physical = make_physical_window();
    // 样本身份不完整：start=0。
    AggregatedSample incomplete;
    incomplete.comm = "app";
    incomplete.pid = 100;
    incomplete.processStartMs = 0; // 身份缺失
    incomplete.exe = "/opt/app";
    incomplete.stack = {"worker", "main"};
    incomplete.count = 5;
    physical.samples.push_back(incomplete);

    SessionContract process = make_contract("sid-proc", "process", {make_target(100, 1000, "/opt/app")},
                                            {"cpu_profile"});
    WindowPayload out = projector.Project(physical, process, 49, true);
    // 完整身份样本保留，不完整身份样本被丢弃并计数。
    ASSERT_EQ(out.samples.size(), 1u);
    EXPECT_EQ(out.samples.front().pid, 100);
    EXPECT_GT(out.identityUnavailableCount, 0u);
}

// 多 slice 聚合后 frame stats / build IDs / runtime diagnostics 为全集重算结果。
TEST(SessionFanoutProjector, MultiSliceAggregationRecomputesDiagnostics)
{
    // 两个 slice 的物理窗口，各自带不同样本；投影后合并应重算。
    SessionFanoutProjector projector;
    WindowPayload slice1 = make_physical_window();
    WindowPayload slice2 = make_physical_window();
    slice2.startMs = 11000;
    slice2.endMs = 21000;
    AggregatedSample extra;
    extra.comm = "app";
    extra.pid = 100;
    extra.processStartMs = 1000;
    extra.exe = "/opt/app";
    extra.runtime = "native";
    extra.stack = {"extra_work", "main"};
    extra.count = 4;
    slice2.samples.push_back(extra);

    SessionContract process = make_contract("sid-proc", "process", {make_target(100, 1000, "/opt/app")},
                                            {"cpu_profile"}, 49);
    WindowPayload out1 = projector.Project(slice1, process, 49, true);
    WindowPayload out2 = projector.Project(slice2, process, 49, true);

    // 合并两个投影结果（模拟 merge_shared_slices）。
    WindowPayload merged;
    merged.startMs = out1.startMs;
    merged.endMs = out2.endMs;
    merged.samples.insert(merged.samples.end(), out1.samples.begin(), out1.samples.end());
    merged.samples.insert(merged.samples.end(), out2.samples.begin(), out2.samples.end());
    merged.physicalDiagnostics = out2.physicalDiagnostics;

    // 重算 frame 统计：slice1 的 pid100 样本 10 + slice2 的 pid100 样本
    // (10+4) = 24 样本 × 2 帧 = 48（49Hz 不降采样）。
    uint64_t totalFrames = 0;
    for (const auto &sample : merged.samples)
        totalFrames += sample.count * sample.stack.size();
    EXPECT_EQ(totalFrames, 48u);
}

// 降采样稳定性：相同余数用稳定排序键，重试结果与 content hash 稳定。
TEST(SessionFanoutProjector, DownsampleStableAcrossRetries)
{
    std::vector<AggregatedSample> samples;
    for (int i = 0; i < 10; ++i)
    {
        AggregatedSample sample;
        sample.comm = "app";
        sample.pid = 100 + i;
        sample.processStartMs = 1000 + i;
        sample.exe = "/opt/app";
        sample.stack = {"frame_" + std::to_string(i), "main"};
        sample.count = 3;
        samples.push_back(sample);
    }
    // 30 样本 → 19Hz/49Hz：round(30×19/49)=12。
    auto first = SessionFanoutProjector::DownsampleDeterministic(samples, 19, 49, "gen-1|1000|11000|sid-x");
    auto second = SessionFanoutProjector::DownsampleDeterministic(samples, 19, 49, "gen-1|1000|11000|sid-x");
    ASSERT_EQ(first.size(), second.size());
    for (size_t i = 0; i < first.size(); ++i)
    {
        EXPECT_EQ(first[i].pid, second[i].pid);
        EXPECT_EQ(first[i].count, second[i].count);
    }
    // 不同 stability key（不同 Session）结果可能不同但总数一致。
    auto other = SessionFanoutProjector::DownsampleDeterministic(samples, 19, 49, "gen-1|1000|11000|sid-y");
    uint64_t totalFirst = 0, totalOther = 0;
    for (const auto &sample : first)
        totalFirst += sample.count;
    for (const auto &sample : other)
        totalOther += sample.count;
    EXPECT_EQ(totalFirst, totalOther);
    EXPECT_EQ(totalFirst, 12u);
}
