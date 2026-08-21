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

std::string json_time_field(const std::string &body, const std::string &field)
{
    const std::string prefix = "\"" + field + "\":\"";
    const size_t begin = body.find(prefix);
    EXPECT_NE(begin, std::string::npos);
    if (begin == std::string::npos)
        return {};
    const size_t valueBegin = begin + prefix.size();
    const size_t valueEnd = body.find('"', valueBegin);
    EXPECT_NE(valueEnd, std::string::npos);
    return valueEnd == std::string::npos ? std::string{} : body.substr(valueBegin, valueEnd - valueBegin);
}

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

class ScopedEnvOverride
{
public:
    ScopedEnvOverride(std::string name, const std::string &value)
        : name_(std::move(name))
    {
        const char *existing = ::getenv(name_.c_str());
        if (existing)
        {
            hadValue_ = true;
            oldValue_ = existing;
        }
        ::setenv(name_.c_str(), value.c_str(), 1);
    }

    ~ScopedEnvOverride()
    {
        if (hadValue_)
            ::setenv(name_.c_str(), oldValue_.c_str(), 1);
        else
            ::unsetenv(name_.c_str());
    }

private:
    std::string name_;
    std::string oldValue_;
    bool hadValue_ = false;
};

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

TEST(ContinuousSampler, SerializesSubSecondWindowWithDistinctRFC3339Bounds)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1700000000123;
    window.endMs = 1700000000456;
    window.samples.push_back(make_sample("testcomm", 100, 1));
    window.backendStatus = "ok";

    const std::string body = build_batch_json(cfg, "cpb-subsecond", {window});
    EXPECT_EQ(json_time_field(body, "start_time"), "2023-11-14T22:13:20.123Z");
    EXPECT_EQ(json_time_field(body, "end_time"), "2023-11-14T22:13:20.456Z");
    EXPECT_NE(body.find("\"window_start\":\"2023-11-14T22:13:20.123Z\""), std::string::npos);
    EXPECT_NE(body.find("\"window_end\":\"2023-11-14T22:13:20.456Z\""), std::string::npos);
}

TEST(ContinuousSampler, ParsesPerfTimestampBoundsForRollingWindows)
{
    // Both header layouts are emitted by perf: the first includes a CPU
    // column, while the second is process-attached and has pid/tid fields.
    const std::string script =
        "api 42/42 [001] 123.456789: cpu-clock:\n"
        "        0000000000001000 first (api)\n"
        "\n"
        "api 42 42 125.000001: cpu-clock:\n"
        "        0000000000002000 second (api)\n\n";
    PerfScriptParseResult parsed = parse_perf_script_result(script);
    ASSERT_TRUE(parsed.hasTimestamp);
    EXPECT_DOUBLE_EQ(parsed.startTimestampSec, 123.456789);
    EXPECT_DOUBLE_EQ(parsed.endTimestampSec, 125.000001);
    ASSERT_EQ(parsed.samples.size(), 2u);

    EXPECT_EQ(perf_timestamp_to_unix_ms(123.456789, 2'000'000, 1'000'000), 1'123'457);
    EXPECT_EQ(perf_timestamp_to_unix_ms(1'700'000'000.123, 2'000'000, 1'000'000), 1'700'000'000'123);
}

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

TEST(ContinuousSampler, ParsesProcessAndSystemPerfHeaders)
{
    std::string comm;
    int pid = 0;
    EXPECT_TRUE(parse_sample_header(
        "yes 1678476 86300.539793: 52631578 cpu-clock:", &comm, &pid));
    EXPECT_EQ(comm, "yes");
    EXPECT_EQ(pid, 1678476);

    EXPECT_TRUE(parse_sample_header(
        "worker 42/43 [003] 86300.539793: cycles:", &comm, &pid));
    EXPECT_EQ(comm, "worker");
    EXPECT_EQ(pid, 42);

    EXPECT_FALSE(parse_sample_header(
        "ffffffff9f6f7d3c vfs_write+0x9c ([kernel.kallsyms])", &comm, &pid));
}

TEST(ContinuousSampler, NormalizesThreadSamplesToProcessTgid)
{
    EXPECT_EQ(process_tgid(static_cast<int>(::getpid())), static_cast<int>(::getpid()));
    EXPECT_EQ(process_tgid(-1), -1);
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
    EXPECT_GT(first, 1577836800000LL); // Unix epoch milliseconds, after 2020-01-01.
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

TEST(ContinuousSpool, PersistsJournalAtomicallyAndRecoversAfterRestart)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-test-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.spoolDirectory = directory;
    const std::string batchID = "cpb-1000-test";
    ASSERT_TRUE(ensure_directory(session_spool_directory(cfg)));
    ASSERT_TRUE(persist_batch(cfg, batchID, "{\"batch_id\":\"cpb-1000-test\"}"));

    struct stat st = {};
    ASSERT_EQ(::stat(journal_path(cfg, batchID).c_str(), &st), 0);
    EXPECT_EQ(st.st_mode & 0777, 0600);
    EXPECT_TRUE(list_spool_files(cfg).empty());

    recover_spool_journals(cfg);
    auto files = list_spool_files(cfg);
    ASSERT_EQ(files.size(), 1u);
    EXPECT_EQ(batch_id_from_spool_path(files[0]), batchID);
    std::string body;
    EXPECT_TRUE(read_file(files[0], &body));
    EXPECT_NE(body.find(batchID), std::string::npos);

    ::unlink(files[0].c_str());
    ::rmdir(session_spool_directory(cfg).c_str());
    ::rmdir(directory);
}

TEST(ContinuousSpool, RequiresExplicitMatchingAck)
{
    EXPECT_TRUE(response_acknowledges_batch(
        "{\"code\":0,\"data\":{\"accepted\":true,\"batch_id\":\"cpb-1\"}}", "cpb-1"));
    EXPECT_FALSE(response_acknowledges_batch(
        "{\"code\":0,\"data\":{\"batch_id\":\"cpb-1\"}}", "cpb-1"));
    EXPECT_FALSE(response_acknowledges_batch(
        "{\"code\":0,\"data\":{\"accepted\":true,\"batch_id\":\"cpb-2\"}}", "cpb-1"));
}

TEST(ContinuousSpool, BatchIDIsStableAndUniqueAcrossSessions)
{
    ContinuousSamplerConfig first = make_cfg();
    ContinuousSamplerConfig second = first;
    second.sessionSID = "another-session";

    const std::string firstID = make_batch_id(first, 1234567890);
    EXPECT_EQ(firstID, make_batch_id(first, 1234567890));
    EXPECT_NE(firstID, make_batch_id(second, 1234567890));
    second = first;
    second.scope = "process";
    second.selectorExe = "/opt/api";
    second.targetProcesses = {{42, 1000, "api", "/opt/api"}};
    EXPECT_NE(firstID, make_batch_id(second, 1234567890));
    EXPECT_LT(firstID.size(), 64u);
}

TEST(ContinuousSpool, RekeysConflictingBatchWithoutDiscardingPayload)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-conflict-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.spoolDirectory = directory;
    cfg.sessionSID = "conflict-session";
    ASSERT_TRUE(ensure_directory(session_spool_directory(cfg)));
    const std::string oldID = "cpb-conflict";
    const std::string body =
        "{\"session_sid\":\"conflict-session\",\"batch_id\":\"cpb-conflict\",\"windows\":[]}";
    ASSERT_TRUE(atomic_write_file(spool_path(cfg, oldID), body));
    ASSERT_TRUE(rekey_conflicted_spooled_batch(cfg, spool_path(cfg, oldID), oldID));

    const auto files = list_session_spool_files(cfg, ".json");
    ASSERT_EQ(files.size(), 1u);
    const std::string newID = batch_id_from_spool_path(files.front());
    EXPECT_NE(newID, oldID);
    std::string rekeyed;
    ASSERT_TRUE(read_file(files.front(), &rekeyed));
    EXPECT_NE(rekeyed.find("\"batch_id\":\"" + newID + "\""), std::string::npos);
    EXPECT_EQ(rekeyed.find("\"batch_id\":\"" + oldID + "\""), std::string::npos);

    ::unlink(files.front().c_str());
    ::rmdir(session_spool_directory(cfg).c_str());
    ::rmdir(directory);
}

TEST(ContinuousSpool, FindsPendingBatchesFromPreviousSession)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-cross-session-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig oldCfg = make_cfg();
    oldCfg.spoolDirectory = directory;
    oldCfg.sessionSID = "old-session";
    ASSERT_TRUE(ensure_directory(session_spool_directory(oldCfg)));
    ASSERT_TRUE(persist_batch(oldCfg, "cpb-100-old", "{}"));
    ASSERT_TRUE(finalize_batch(oldCfg, "cpb-100-old"));

    ContinuousSamplerConfig newCfg = oldCfg;
    newCfg.sessionSID = "new-session";
    ASSERT_TRUE(ensure_directory(session_spool_directory(newCfg)));
    auto files = list_spool_files(newCfg);
    ASSERT_EQ(files.size(), 1u);
    EXPECT_NE(files[0].find("/old-session/"), std::string::npos);

    ::unlink(files[0].c_str());
    ::rmdir(session_spool_directory(oldCfg).c_str());
    ::rmdir(session_spool_directory(newCfg).c_str());
    ::rmdir(directory);
}

TEST(ContinuousSpool, StopAcknowledgementChecksOnlyItsOwnSessionDirectory)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-stop-ack-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig stopped = make_cfg();
    stopped.spoolDirectory = directory;
    stopped.sessionSID = "stopped-session";
    ContinuousSamplerConfig active = stopped;
    active.sessionSID = "active-session";
    ASSERT_TRUE(ensure_directory(session_spool_directory(stopped)));
    ASSERT_TRUE(ensure_directory(session_spool_directory(active)));
    ASSERT_TRUE(persist_batch(stopped, "cpb-stopped", "{}"));
    ASSERT_TRUE(finalize_batch(stopped, "cpb-stopped"));
    ASSERT_TRUE(persist_batch(active, "cpb-active", "{}"));

    EXPECT_TRUE(ContinuousSessionHasPendingSpool(stopped));
    EXPECT_TRUE(ContinuousSessionHasPendingSpool(active));
    ::unlink(spool_path(stopped, "cpb-stopped").c_str());
    EXPECT_FALSE(ContinuousSessionHasPendingSpool(stopped));
    EXPECT_TRUE(ContinuousSessionHasPendingSpool(active));

    ::unlink(journal_path(active, "cpb-active").c_str());
    ::rmdir(session_spool_directory(stopped).c_str());
    ::rmdir(session_spool_directory(active).c_str());
    ::rmdir(directory);
}

TEST(ContinuousSpool, IgnoresAssignmentCacheOutsideSessionBatchDirectories)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-assignment-cache-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.spoolDirectory = directory;
    ASSERT_TRUE(ensure_directory(session_spool_directory(cfg)));
    ASSERT_TRUE(atomic_write_file(std::string(directory) + "/assignments.json", "{\"code\":0}"));
    ASSERT_TRUE(atomic_write_file(session_spool_directory(cfg) + "/cpb-real.json", "{\"batch_id\":\"cpb-real\"}"));

    auto files = list_spool_files(cfg);
    ASSERT_EQ(files.size(), 1u);
    EXPECT_NE(files[0].find("cpb-real.json"), std::string::npos);
    EXPECT_EQ(spool_usage_bytes(cfg), std::string("{\"batch_id\":\"cpb-real\"}").size());

    ::unlink(files[0].c_str());
    ::unlink((std::string(directory) + "/assignments.json").c_str());
    ::rmdir(session_spool_directory(cfg).c_str());
    ::rmdir(directory);
}

TEST(ContinuousSpool, AppliesQuotaBackpressureAndCapsRetry)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-quota-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.spoolDirectory = directory;
    cfg.spoolMaxBytes = 8;
    cfg.spoolMinFreeBytes = 1;
    cfg.retryMaxSec = 5;
    ASSERT_TRUE(ensure_directory(session_spool_directory(cfg)));
    ASSERT_TRUE(persist_batch(cfg, "cpb-quota", "0123456789"));
    EXPECT_FALSE(spool_has_collection_capacity(cfg));

    SpoolRetryState retry;
    for (int i = 0; i < 10; ++i)
        schedule_spool_retry(cfg, &retry);
    EXPECT_EQ(retry.delaySec, 5);

    ::unlink(journal_path(cfg, "cpb-quota").c_str());
    ::rmdir(session_spool_directory(cfg).c_str());
    ::rmdir(directory);
}

TEST(ContinuousProcessScope, UsesExplicitPIDTargetWithoutWholeHostFlag)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.scope = "process";
    cfg.selectorExe = "/opt/api";
    cfg.targetProcesses = {{42, 1000, "api", "/opt/api"}, {77, 2000, "api", "/opt/api"}};
    EXPECT_EQ(target_pid_csv(cfg), "42,77");
    EXPECT_EQ(bpftrace_target_predicate(cfg), "/pid == 42 || pid == 77/");
    const auto args = perf_record_args(cfg, "perf", "cpu-clock", "/tmp/test.data");
    EXPECT_NE(std::find(args.begin(), args.end(), "-p"), args.end());
    EXPECT_EQ(std::find(args.begin(), args.end(), "-a"), args.end());
    EXPECT_TRUE(process_targeted(cfg, 42, "/opt/api"));
    EXPECT_TRUE(process_targeted(cfg, 42, 1000, "/opt/api"));
    EXPECT_FALSE(process_targeted(cfg, 42, 9999, "/opt/api"));
    EXPECT_FALSE(process_targeted(cfg, 99, "/opt/other"));
}

TEST(ContinuousProcessScope, SerializesPIDReuseIdentity)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 11000;
    AggregatedSample sample = make_sample("api", 42, 2);
    sample.processStartMs = 987654321;
    window.samples.push_back(sample);
    std::string body = build_batch_json(cfg, "cpb-process-start", {window});
    EXPECT_NE(body.find("\"process_start_ms\":987654321"), std::string::npos);
}

TEST(SharedContinuousEngine, BuildsOneUnionTargetSetAtHighestFrequency)
{
    ContinuousSamplerConfig first = make_cfg();
    first.sessionSID = "cps-a";
    first.scope = "process";
    first.selectorExe = "/opt/api";
    first.sampleRateHz = 19;
    first.targetProcesses = {{42, 1000, "api", "/opt/api"}};
    ContinuousSamplerConfig second = first;
    second.sessionSID = "cps-b";
    second.selectorExe = "/opt/worker";
    second.sampleRateHz = 49;
    second.targetProcesses = {{77, 2000, "worker", "/opt/worker"}};

    ContinuousSamplerConfig physical = shared_physical_config({first, second});
    EXPECT_EQ(physical.scope, "process");
    EXPECT_EQ(physical.sampleRateHz, 49);
    EXPECT_EQ(physical.aggregationWindowSec, 5);
    EXPECT_EQ(physical.targetProcesses.size(), 2u);
    EXPECT_EQ(physical.signals, "cpu");
    const auto args = perf_record_args(physical, "perf", "cpu-clock", "/tmp/shared.data");
    EXPECT_EQ(std::find(args.begin(), args.end(), "-a"), args.end());
    auto pidFlag = std::find(args.begin(), args.end(), "-p");
    ASSERT_NE(pidFlag, args.end());
    ASSERT_NE(++pidFlag, args.end());
    EXPECT_EQ(*pidFlag, "42,77");
}

TEST(SharedContinuousEngine, FansOutByPIDStartIdentityWithoutHistogramLeakage)
{
    ContinuousSamplerConfig session = make_cfg();
    session.scope = "process";
    session.selectorExe = "/opt/api";
    session.targetProcesses = {{42, 1000, "api", "/opt/api"}};
    WindowPayload source;
    source.startMs = 1000;
    source.endMs = 6000;
    auto selected = make_sample("api", 42, 3);
    selected.exe = "/opt/api";
    selected.processStartMs = 1000;
    auto reused = selected;
    reused.processStartMs = 9999;
    auto other = make_sample("worker", 77, 5);
    other.exe = "/opt/worker";
    other.processStartMs = 2000;
    source.samples = {selected, reused, other};
    HistogramPayload histogram;
    histogram.signalType = "io_latency";
    histogram.backend = "bpftrace";
    histogram.eventCount = 8;
    source.histograms.push_back(histogram);

    WindowPayload filtered = filter_shared_window(source, session, false);
    ASSERT_EQ(filtered.samples.size(), 1u);
    EXPECT_EQ(filtered.samples[0].pid, 42);
    EXPECT_EQ(filtered.samples[0].processStartMs, 1000);
    ASSERT_EQ(filtered.histograms.size(), 3u);
    for (const auto &item : filtered.histograms)
    {
        EXPECT_TRUE(item.unavailable);
        EXPECT_EQ(item.eventCount, 0u);
    }
}

TEST(SharedContinuousEngine, DoesNotHideRealGapsWhenMergingBaseSlices)
{
    WindowPayload first;
    first.startMs = 1000;
    first.endMs = 6000;
    first.samples.push_back(make_sample("api", 42, 1));
    WindowPayload contiguous = first;
    contiguous.startMs = 6050;
    contiguous.endMs = 11050;
    WindowPayload afterGap = first;
    afterGap.startMs = 15000;
    afterGap.endMs = 20000;

    auto windows = merge_shared_slices_preserving_gaps({first, contiguous, afterGap});
    ASSERT_EQ(windows.size(), 2u);
    EXPECT_EQ(windows[0].startMs, 1000);
    EXPECT_EQ(windows[0].endMs, 11050);
    EXPECT_EQ(windows[1].startMs, 15000);
    EXPECT_EQ(windows[1].endMs, 20000);
}

TEST(SharedContinuousEngine, RetainsCoreHistogramsUntilPerfWindowArrives)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.scope = "process";
    std::vector<CoreHistogramSample> pending;
    uint64_t pendingLost = 0;
    std::vector<WindowPayload> noWindows;
    queue_core_histograms(&noWindows, cfg, &pending, &pendingLost,
                          {{2, 42, 3, 5}}, 2);
    ASSERT_EQ(pending.size(), 1u);
    EXPECT_EQ(pendingLost, 2u);

    std::vector<WindowPayload> windows(1);
    windows.front().startMs = 1000;
    windows.front().endMs = 3000;
    queue_core_histograms(&windows, cfg, &pending, &pendingLost,
                          {{2, 42, 3, 7}}, 0);
    EXPECT_TRUE(pending.empty());
    EXPECT_EQ(pendingLost, 0u);
    ASSERT_EQ(windows.front().histograms.size(), 1u);
    EXPECT_EQ(windows.front().histograms.front().signalType, "io_syscall_latency");
    EXPECT_EQ(windows.front().histograms.front().eventCount, 12u);
    EXPECT_EQ(windows.front().backendReason, "CO-RE lost events=2");
}

TEST(RollingPerfRecorder, UsesIsolatedGenerationDirectories)
{
    std::string first;
    std::string second;
    ASSERT_TRUE(create_rolling_perf_directory(&first));
    ASSERT_TRUE(create_rolling_perf_directory(&second));
    EXPECT_NE(first, second);
    EXPECT_EQ(first.rfind("/tmp/mini-drop-native-cp-rolling-", 0), 0u);
    EXPECT_EQ(second.rfind("/tmp/mini-drop-native-cp-rolling-", 0), 0u);
    EXPECT_EQ(::rmdir(first.c_str()), 0);
    EXPECT_EQ(::rmdir(second.c_str()), 0);
}

TEST(RollingPerfRecorder, NeverTreatsBaseOpenFileAsDrainable)
{
    std::string directory;
    ASSERT_TRUE(create_rolling_perf_directory(&directory));
    const std::string older = directory + "/perf.data.20260821010101";
    const std::string newer = directory + "/perf.data.20260821010103";
    const std::string current = directory + "/perf.data";
    {
        std::ofstream out(older);
        out << "older";
    }
    {
        std::ofstream out(newer);
        out << "newer";
    }
    {
        std::ofstream out(current);
        out << "current-open";
    }
    const auto normal = rolling_perf_files(directory, false);
    ASSERT_EQ(normal.size(), 2u);
    EXPECT_EQ(normal.front(), older);
    EXPECT_EQ(normal.back(), newer);
    const auto final = rolling_perf_files(directory, true);
    ASSERT_EQ(final.size(), 3u);
    EXPECT_EQ(final.front(), current);
    EXPECT_EQ(::unlink(older.c_str()), 0);
    EXPECT_EQ(::unlink(newer.c_str()), 0);
    EXPECT_EQ(::unlink(current.c_str()), 0);
    EXPECT_EQ(::rmdir(directory.c_str()), 0);
}

TEST(SharedContinuousEngine, PausesBeforeStartingRecorderWhenDiskReserveIsUnavailable)
{
    char directoryTemplate[] = "/tmp/mini-drop-shared-backpressure-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.sessionSID = "backpressure-session";
    cfg.spoolDirectory = directory;
    cfg.spoolMinFreeBytes = ~uint64_t{0};

    SharedDualTrackContinuousSampler sampler;
    std::string error;
    ASSERT_TRUE(sampler.Start({cfg}, &error)) << error;
    for (int attempt = 0; !sampler.Ready() && attempt < 100; ++attempt)
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    EXPECT_TRUE(sampler.Ready());
    EXPECT_FALSE(sampler.Strict());
    EXPECT_NE(sampler.DegradationReason().find("spool backpressure"), std::string::npos);
    sampler.Stop();

    EXPECT_EQ(::rmdir(session_spool_directory(cfg).c_str()), 0);
    EXPECT_EQ(::rmdir(directory), 0);
}

TEST(SharedContinuousEngine, RefusesUnapprovedDegradedFallback)
{
    ScopedEnvOverride missingCoreObject("DROP_NATIVE_CP_CORE_OBJECT", "/tmp/mini-drop-missing-core-object");
    char directoryTemplate[] = "/tmp/mini-drop-shared-no-fallback-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.sessionSID = "strict-no-fallback";
    cfg.spoolDirectory = directory;
    cfg.spoolMinFreeBytes = 0;
    cfg.allowDegraded = false;

    SharedDualTrackContinuousSampler sampler;
    std::string error;
    ASSERT_TRUE(sampler.Start({cfg}, &error)) << error;
    for (int attempt = 0; !sampler.Ready() && attempt < 100; ++attempt)
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    EXPECT_TRUE(sampler.Ready());
    EXPECT_TRUE(sampler.Running());
    EXPECT_TRUE(sampler.Failed());
    EXPECT_FALSE(sampler.Strict());
    EXPECT_NE(sampler.DegradationReason().find("degraded fallback is not allowed"), std::string::npos);
    sampler.Stop();

    EXPECT_EQ(::rmdir(session_spool_directory(cfg).c_str()), 0);
    EXPECT_EQ(::rmdir(directory), 0);
}
