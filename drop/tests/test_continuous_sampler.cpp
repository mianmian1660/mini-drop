// ============================================================
// tests/test_continuous_sampler.cpp — ContinuousSampler 窗口 JSON / 后端聚合单测
// ============================================================
// 通过 include ContinuousSampler.cpp 直接测试匿名命名空间里的纯函数
// （build_batch_json / backend 状态聚合），不 fork 真实 perf/bpftrace。
// 覆盖（修复计划 Step 2 要求的单测）：
//   - 窗口 JSON 输出 symbol_refs（runtime map 诊断）
//   - 后端状态聚合（ok / degraded / failed）
//   - signal_types 与 sample_count 计算（含 histogram eventCount 累加）
//   - 无 symbolRefs 的旧窗口保持兼容（不输出 symbol_refs）
// ============================================================

#include "../common/ContinuousSampler.cpp"

#include <gtest/gtest.h>

#include <chrono>
#include <string>
#include <thread>

using namespace drop;

namespace
{

ContinuousSamplerConfig make_cfg()
{
    ContinuousSamplerConfig cfg;
    cfg.sampleRateHz = 19;
    cfg.aggregationWindowSec = 10;
    cfg.uploadBatchSec = 60;
    cfg.sessionSID = "test-session";
    cfg.targetIP = "127.0.0.1";
    cfg.hostname = "test-host";
    cfg.apiBaseURL = "http://127.0.0.1:8191";
    cfg.authUID = "test-uid";
    return cfg;
}

AggregatedSample make_sample(const std::string &comm, int pid, uint64_t count)
{
    AggregatedSample s;
    s.comm = comm;
    s.pid = pid;
    s.exe = "/usr/bin/test";
    s.stack = {"funcA", "funcB"};
    s.backend = "perf";
    s.count = count;
    return s;
}

} // namespace

// ---- 窗口 JSON 输出 symbol_refs ----
TEST(ContinuousSampler, WindowJsonEmitsSymbolRefs)
{
    ContinuousSamplerConfig cfg = make_cfg();

    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    window.samples.push_back(make_sample("testcomm", 100, 5));
    window.backendStatus = "ok";
    window.selectedBackend = "perf";
    window.attemptedBackends = {"perf"};
    window.symbolRefsJson = "{\"symbol_status\":\"complete\",\"runtime_maps\":{\"java\":{\"detected\":false}}}";

    std::vector<WindowPayload> windows = {window};
    std::string json = build_batch_json(cfg, "cpb-test", windows);

    EXPECT_NE(json.find("\"symbol_refs\":{\"symbol_status\":\"complete\""), std::string::npos);
    EXPECT_NE(json.find("\"signal_types\":[\"cpu_profile\"]"), std::string::npos);
    EXPECT_NE(json.find("\"backend\":\"perf\""), std::string::npos);
    EXPECT_NE(json.find("\"selected_backend\":\"perf\""), std::string::npos);
    EXPECT_NE(json.find("\"backend_status\":\"ok\""), std::string::npos);
    // sample_count 由 cpu samples 构成
    EXPECT_NE(json.find("\"sample_count\":5"), std::string::npos);
}

// ---- 无 symbolRefs 的旧窗口保持兼容 ----
TEST(ContinuousSampler, WindowJsonWithoutSymbolRefsCompatible)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    window.samples.push_back(make_sample("a", 1, 3));
    window.backendStatus = "ok";
    window.selectedBackend = "perf";

    std::string json = build_batch_json(cfg, "cpb-test", {window});
    // 不输出 symbol_refs，且整体 JSON 仍合法可解析（关键字段仍在）
    EXPECT_EQ(json.find("symbol_refs"), std::string::npos);
    EXPECT_NE(json.find("\"sample_count\":3"), std::string::npos);
    EXPECT_NE(json.find("\"windows\":["), std::string::npos);
}

// ---- 后端状态聚合：全部 ok → ok ----
TEST(ContinuousSampler, BackendAggregationAllOk)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload w1;
    w1.startMs = 1000;
    w1.endMs = 11000;
    w1.samples.push_back(make_sample("a", 1, 1));
    w1.backendStatus = "ok";
    w1.selectedBackend = "perf";
    w1.attemptedBackends = {"perf"};

    std::string json = build_batch_json(cfg, "cpb-test", {w1});
    EXPECT_NE(json.find("\"backend_status\":\"ok\""), std::string::npos);
}

// ---- 后端状态聚合：任一窗口 failed → degraded ----
TEST(ContinuousSampler, BackendAggregationAnyFailedDegraded)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload w1;
    w1.startMs = 1000;
    w1.endMs = 11000;
    w1.samples.push_back(make_sample("a", 1, 1));
    w1.backendStatus = "ok";
    w1.selectedBackend = "perf";
    w1.attemptedBackends = {"perf"};

    WindowPayload w2;
    w2.startMs = 12000;
    w2.endMs = 22000;
    w2.backendStatus = "failed";
    w2.backendReason = "no CPU samples collected by any backend";
    w2.attemptedBackends = {"perf"};

    std::string json = build_batch_json(cfg, "cpb-test", {w1, w2});
    EXPECT_NE(json.find("\"backend_status\":\"degraded\""), std::string::npos);
    // signal_types 仍应包含 cpu_profile（只要有窗口有样本）
    EXPECT_NE(json.find("\"cpu_profile\""), std::string::npos);
}

// ---- sample_count 包含 histogram eventCount（向后兼容） ----
TEST(ContinuousSampler, SampleCountIncludesHistogramEvents)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    window.samples.push_back(make_sample("a", 1, 4));

    HistogramPayload hist;
    hist.signalType = "io_latency";
    hist.backend = "bpftrace";
    hist.eventCount = 7;
    window.histograms.push_back(hist);

    std::string json = build_batch_json(cfg, "cpb-test", {window});
    // cpu samples(4) + histogram events(7) = 11（保持向后兼容）
    EXPECT_NE(json.find("\"sample_count\":11"), std::string::npos);
    EXPECT_NE(json.find("\"io_latency\""), std::string::npos);
}

TEST(ContinuousSampler, SerializesRuntimeRSSAndProfileUnit)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    auto cpu = make_sample("python3", 42, 3);
    cpu.runtime = "python";
    window.samples.push_back(cpu);

    MetricPayload rss;
    rss.metric = "rss_bytes";
    rss.unit = "bytes";
    rss.runtime = "python";
    rss.comm = "python3";
    rss.pid = 42;
    rss.processStartMs = 123400;
    rss.exe = "/usr/bin/python3";
    rss.timestampMs = 5000;
    rss.value = 4096;
    window.metrics.push_back(rss);

    ProfilePayload memory;
    memory.signalType = "python_memory";
    memory.backend = "memray";
    memory.profileID = "profile-1";
    memory.unit = "bytes";
    memory.samples.push_back(cpu);
    window.profiles.push_back(memory);

    std::string json = build_batch_json(cfg, "cpb-test", {window});
    EXPECT_NE(json.find("\"runtime\":\"python\""), std::string::npos);
    EXPECT_NE(json.find("\"metric\":\"rss_bytes\""), std::string::npos);
    EXPECT_NE(json.find("\"process_start_ms\":123400"), std::string::npos);
    EXPECT_NE(json.find("\"profile_id\":\"profile-1\""), std::string::npos);
    EXPECT_NE(json.find("\"unit\":\"bytes\""), std::string::npos);
}

TEST(ContinuousSampler, ClassifiesGoBeforeSymbolExtractionIsReady)
{
    AggregatedSample sample;
    sample.pid = 123;
    sample.exe = "/tmp/go-stripped";
    drop::GoSymbolReport emptyReport;

    EXPECT_EQ(sample_runtime_with_go_hint(sample, emptyReport, true), "go");
    EXPECT_EQ(sample_runtime_with_go_hint(sample, emptyReport, false), "native");
}

TEST(ContinuousSampler, SanitizesPythonPerfMapPaths)
{
    EXPECT_EQ(sanitize_python_perf_frame("py::worker:/srv/app/jobs/worker.py+0x1a"),
              "py::worker:worker.py+0x1a");
    EXPECT_EQ(sanitize_python_perf_frame("py::worker:C:\\app\\jobs\\worker.py:42"),
              "py::worker:worker.py:42");
    EXPECT_EQ(sanitize_python_perf_frame("py::worker:worker.py+0x1a"),
              "py::worker:worker.py+0x1a");
    EXPECT_EQ(sanitize_python_perf_frame("nativeWorker+0x4"), "nativeWorker+0x4");
}

TEST(PythonRuntimeProfiler, ParsesAndSanitizesPySpyRaw)
{
    auto samples = parse_pyspy_raw(
        "root (/srv/app/main.py:1);pyWorker (/venv/lib/python3.11/site-packages/pkg/work.py:37) 9\n");
    ASSERT_EQ(samples.size(), 1u);
    ASSERT_EQ(samples[0].count, 9u);
    ASSERT_EQ(samples[0].stack.size(), 2u);
    EXPECT_EQ(samples[0].stack[0], "root (main.py:1)");
    EXPECT_EQ(samples[0].stack[1], "pyWorker (work.py:37)");
    EXPECT_EQ(samples[0].stack[1].find("/venv"), std::string::npos);
}

TEST(PythonRuntimeProfiler, ProcessStartIdentityIsStableAcrossReads)
{
    int64_t first = 0;
    int64_t second = 0;
    ASSERT_TRUE(python_process_start_ms(static_cast<int>(::getpid()), &first));
    std::this_thread::sleep_for(std::chrono::milliseconds(25));
    ASSERT_TRUE(python_process_start_ms(static_cast<int>(::getpid()), &second));
    EXPECT_GT(first, 0);
    EXPECT_EQ(first, second);
    EXPECT_TRUE(python_process_is_same(static_cast<int>(::getpid()), first));
    EXPECT_FALSE(python_process_is_same(static_cast<int>(::getpid()), first + 1));
}

TEST(MemrayProfileIngest, ParsesAndResolvesNamespaceProcessIdentity)
{
    MemrayProfileIdentity profile;
    ASSERT_TRUE(parse_memray_profile_identity(
        "/proc/91/root/tmp/mini-drop-memray/memray-7-12345-acde.ready", &profile));
    EXPECT_EQ(profile.profileID, "memray-7-12345-acde");
    EXPECT_EQ(profile.namespacePid, 7);
    EXPECT_EQ(profile.startTicks, 12345u);

    std::vector<MemrayProcessIdentity> processes = {
        {91, 7, 999, "root-a", "worker-a", "/usr/bin/uwsgi"},
        {92, 7, 12345, "root-b", "worker-b", "/usr/bin/uwsgi"},
        {93, 7, 12345, "root-a", "worker-c", "/usr/bin/uwsgi"},
    };
    EXPECT_EQ(resolve_memray_profile_process(profile, "root-a", processes), 93);
    EXPECT_EQ(resolve_memray_profile_process(profile, "root-c", processes), 0);
}

TEST(MemrayProfileIngest, AcknowledgeRequiresReachableReadyFile)
{
    char directoryTemplate[] = "/tmp/mini-drop-memray-test-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    std::string ready = std::string(directory) + "/memray-7-12345-acde.ready";
    std::string done = std::string(directory) + "/memray-7-12345-acde.done";
    {
        std::ofstream out(ready);
        out << "profile";
    }
    EXPECT_TRUE(acknowledge_memray_profile(ready));
    EXPECT_FALSE(acknowledge_memray_profile(ready));
    EXPECT_EQ(::access(done.c_str(), F_OK), 0);
    ::unlink(done.c_str());
    ::rmdir(directory);
}

TEST(ContinuousSampler, SerializesFailedMemrayProfileForMemoryDiagnostics)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    ProfilePayload failed;
    failed.signalType = "python_memory";
    failed.backend = "memray";
    failed.profileID = "memray-7-12345-acde";
    failed.unit = "bytes";
    window.profiles.push_back(failed);
    window.symbolRefsJson = "{\"python_memory\":{\"ready\":[],\"failed\":[{\"pid\":7,\"reason\":\"version mismatch\"}]}}";

    std::string json = build_batch_json(cfg, "cpb-test", {window});
    EXPECT_NE(json.find("\"signal_types\":[\"python_memory\"]"), std::string::npos);
    EXPECT_NE(json.find("version mismatch"), std::string::npos);
}
