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

// ---- sample_count 语义：v3 起 batch 层写 0，分信号计数在 signal_counts ----
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
    // v3：batch 层 sample_count=0（废弃），histogram 事件走 signal_counts。
    EXPECT_NE(json.find("\"sample_count\":0"), std::string::npos);
    EXPECT_NE(json.find("\"signal_counts\":{\"cpu_profile\":4,\"io_latency\":7}"), std::string::npos);
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

TEST(ContinuousSpool, RecognizesOnlyExplicitNonRetryableClientErrorsAsPermanent)
{
    EXPECT_TRUE(response_is_permanent_rejection(
        400, "{\"error\":{\"retryable\":false}}"));
    EXPECT_FALSE(response_is_permanent_rejection(
        400, "{\"error\":{\"retryable\":true}}"));
    EXPECT_FALSE(response_is_permanent_rejection(
        503, "{\"error\":{\"retryable\":false}}"));
}

TEST(ContinuousSpool, BatchIDIsStableAndUniqueAcrossSessions)
{
    ContinuousSamplerConfig first = make_cfg();
    ContinuousSamplerConfig second = first;
    second.sessionSID = "another-session";

    const std::string firstID = make_batch_id(first);
    EXPECT_EQ(firstID, make_batch_id(first));
    EXPECT_NE(firstID, make_batch_id(second));
    second = first;
    second.scope = "process";
    second.selectorExe = "/opt/api";
    second.targetProcesses = {{42, 1000, "api", "/opt/api"}};
    EXPECT_NE(firstID, make_batch_id(second));
    EXPECT_LT(firstID.size(), 64u);
}

TEST(ContinuousSpool, QuarantinesPermanentlyRejectedBatchWithoutDeletingPayload)
{
    char directoryTemplate[] = "/tmp/mini-drop-spool-rejected-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.spoolDirectory = directory;
    ASSERT_TRUE(ensure_directory(session_spool_directory(cfg)));
    const std::string batchID = "cpb-rejected";
    const std::string path = spool_path(cfg, batchID);
    ASSERT_TRUE(atomic_write_file(path, "{\"batch_id\":\"cpb-rejected\"}"));

    ASSERT_TRUE(quarantine_rejected_spooled_batch(path, batchID));
    EXPECT_FALSE(file_exists_local(path));
    EXPECT_TRUE(file_exists_local(path + ".rejected"));
    EXPECT_TRUE(list_session_spool_files(cfg, ".json").empty());

    ::unlink((path + ".rejected").c_str());
    ::rmdir(session_spool_directory(cfg).c_str());
    ::rmdir(directory);
}

// ============================================================
// 阶段一：信号控制面 / 稳定 ID / 分信号计数 / cutover
// ============================================================

// ============================================================
// 阶段一：信号控制面 / 稳定 ID / 分信号计数 / cutover
// ============================================================

TEST(ContinuousSampler, PhysicalSignalsFromRequested)
{
    // 空集合 → 四类全开。
    EXPECT_EQ(physical_signals_from_requested({}), "cpu,io,io_syscall,sched");
    // 单信号子集。
    EXPECT_EQ(physical_signals_from_requested({"cpu_profile"}), "cpu");
    // 并集（去重、保持逻辑顺序映射）。
    EXPECT_EQ(physical_signals_from_requested({"io_latency", "cpu_profile", "sched_latency"}), "io,cpu,sched");
    // 公开 API 同语义。
    EXPECT_EQ(drop::PhysicalSignalsFromRequested({"cpu_profile", "io_latency"}), "cpu,io");
}

TEST(ContinuousSampler, WindowIDIsStableAndContentIndependent)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.collectorGeneration = "gen-test-1";
    cfg.targetFingerprint = "fp-1";
    WindowPayload window;
    window.startMs = 1'000;
    window.endMs = 11'000;
    window.samples.push_back(make_sample("a", 1, 3));
    const std::string id1 = make_window_id(cfg, window);
    // 相同窗口 → 相同 ID（重传稳定）。
    EXPECT_EQ(id1, make_window_id(cfg, window));
    // 内容不同但起止时间/身份相同 → 同一 ID（内容摘要不参与 ID，冲突时
    // 仍是同一合法 ID，绝不生成第二个 ID）。
    WindowPayload changed = window;
    changed.samples[0].count = 999;
    EXPECT_EQ(id1, make_window_id(cfg, changed));
    // 起止时间变化 → 新 ID。
    WindowPayload otherTime = window;
    otherTime.endMs = 12'000;
    EXPECT_NE(id1, make_window_id(cfg, otherTime));
    // generation 变化 → 新 ID（采集器切换即新 generation）。
    ContinuousSamplerConfig nextGen = cfg;
    nextGen.collectorGeneration = "gen-test-2";
    EXPECT_NE(id1, make_window_id(nextGen, window));
}

TEST(ContinuousSampler, WindowContentDigestReflectsContent)
{
    WindowPayload window;
    window.startMs = 1'000;
    window.endMs = 11'000;
    window.samples.push_back(make_sample("a", 1, 3));
    const std::string d1 = window_content_digest(window);
    // 相同内容 → 相同摘要。
    EXPECT_EQ(d1, window_content_digest(window));
    // 计数变化 → 摘要变化。
    WindowPayload changed = window;
    changed.samples[0].count = 5;
    EXPECT_NE(d1, window_content_digest(changed));
    // 窗口时间变化也反映进摘要。
    WindowPayload moved = window;
    moved.startMs = 2'000;
    EXPECT_NE(d1, window_content_digest(moved));
    // 会影响查询/诊断语义的非计数字段也必须进入摘要，不能让不同 payload
    // 被误判成精确重传。
    WindowPayload changedBackend = window;
    changedBackend.samples[0].backend = "py-spy";
    EXPECT_NE(d1, window_content_digest(changedBackend));
    WindowPayload changedExe = window;
    changedExe.samples[0].exe = "/opt/other";
    EXPECT_NE(d1, window_content_digest(changedExe));
}

TEST(ContinuousSampler, SHA256ImplementationMatchesKnownVector)
{
    EXPECT_EQ(sha256_hex("abc"),
              "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
}

TEST(ContinuousSampler, WindowSignalCountsRecompute)
{
    WindowPayload window;
    window.startMs = 1'000;
    window.endMs = 11'000;
    window.samples.push_back(make_sample("a", 1, 3));
    HistogramPayload hist;
    hist.signalType = "io_latency";
    hist.eventCount = 7;
    window.histograms.push_back(hist);
    auto counts = window_signal_counts(window);
    EXPECT_EQ(counts["cpu_profile"], 3u);
    EXPECT_EQ(counts["io_latency"], 7u);
    // batch 汇总跨窗口求和。
    WindowPayload w2;
    w2.startMs = 12'000;
    w2.endMs = 22'000;
    w2.samples.push_back(make_sample("b", 2, 4));
    auto batchCounts = batch_signal_counts({window, w2});
    EXPECT_EQ(batchCounts["cpu_profile"], 7u);
}

TEST(ContinuousSampler, BuildBatchJsonV3EmitsCorrectnessFields)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.collectorGeneration = "gen-test-1";
    cfg.batchSequence = 3;
    cfg.targetFingerprint = "fp-1";
    WindowPayload window;
    window.startMs = 1'000;
    window.endMs = 11'000;
    window.samples.push_back(make_sample("a", 1, 3));
    window.windowID = make_window_id(cfg, window);
    window.contentSHA256 = window_content_digest(window);

    std::string json = build_batch_json(cfg, "cpb-v3", {window});
    // 阶段三：schema_version=4（v4 在 v3 基础上增加信号状态/采样率/身份字段）；
    // batch 层 sample_count 废弃写 0。
    EXPECT_NE(json.find("\"schema_version\":4"), std::string::npos);
    EXPECT_NE(json.find("\"sample_count\":0"), std::string::npos);
    // 新协议字段。
    EXPECT_NE(json.find("\"collector_generation\":\"gen-test-1\""), std::string::npos);
    EXPECT_NE(json.find("\"batch_sequence\":3"), std::string::npos);
    EXPECT_NE(json.find("\"content_sha256\":\""), std::string::npos);
    EXPECT_NE(json.find("\"signal_counts\":{\"cpu_profile\":3}"), std::string::npos);
    // 窗口级字段。
    EXPECT_NE(json.find("\"window_id\":\"cpw-"), std::string::npos);
    EXPECT_NE(json.find("\"target_fingerprint\":\"fp-1\""), std::string::npos);
    EXPECT_NE(json.find("\"signal_counts\":{\"cpu_profile\":3}"), std::string::npos);
    // 窗口行 sample_count 仍是该窗口自身计数（cpu=3）。
    EXPECT_NE(json.find("\"sample_count\":3"), std::string::npos);
}

TEST(ContinuousSampler, FilterSharedWindowByRequestedSignals)
{
    ContinuousSamplerConfig session = make_cfg();
    session.requestedSignals = {"cpu_profile"}; // CPU-only Session
    WindowPayload source;
    source.startMs = 1'000;
    source.endMs = 11'000;
    source.samples.push_back(make_sample("a", 1, 3));
    HistogramPayload io;
    io.signalType = "io_latency";
    io.eventCount = 5;
    source.histograms.push_back(io);

    // CPU-only：直方图被剔除，CPU 样本保留。
    WindowPayload out = filter_shared_window(source, session, true);
    EXPECT_EQ(out.samples.size(), 1u);
    EXPECT_TRUE(out.histograms.empty());

    // 四信号 Session：直方图保留。
    ContinuousSamplerConfig all = make_cfg();
    all.requestedSignals = {"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"};
    WindowPayload out2 = filter_shared_window(source, all, true);
    EXPECT_EQ(out2.histograms.size(), 1u);

    // 未请求 histogramAttributionSafe=false：请求了 io 的 Session 得到
    // unavailable 标记，未请求的 sched 不出现。
    ContinuousSamplerConfig ioOnly = make_cfg();
    ioOnly.requestedSignals = {"io_latency"};
    WindowPayload out3 = filter_shared_window(source, ioOnly, false);
    ASSERT_EQ(out3.histograms.size(), 1u);
    EXPECT_TRUE(out3.histograms[0].unavailable);
    EXPECT_EQ(out3.histograms[0].signalType, "io_latency");
    EXPECT_TRUE(out3.samples.empty()); // cpu 未请求 → 剔除
}

TEST(ContinuousSampler, BatchIDIncludesGenerationAndSequence)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.collectorGeneration = "gen-test-1";
    cfg.batchSequence = 1;
    const std::string id1 = make_batch_id(cfg);
    // 同一 generation + sequence → 稳定。
    EXPECT_EQ(id1, make_batch_id(cfg));
    // sequence 递增 → 新 ID。
    cfg.batchSequence = 2;
    EXPECT_NE(id1, make_batch_id(cfg));
    // generation 变化 → 新 ID（采集器切换，不允许旧 ID 复用）。
    cfg.batchSequence = 1;
    cfg.collectorGeneration = "gen-test-2";
    EXPECT_NE(id1, make_batch_id(cfg));
}

TEST(ContinuousSampler, SortsSharedSlicesAndPreservesValidMergedBounds)
{
    WindowPayload later;
    later.startMs = 20'000;
    later.endMs = 30'000;
    later.samples.push_back(make_sample("later", 2, 1));
    WindowPayload earlier;
    earlier.startMs = 10'000;
    earlier.endMs = 20'050;
    earlier.samples.push_back(make_sample("earlier", 1, 1));

    const auto merged = merge_shared_slices_preserving_gaps({later, earlier});
    ASSERT_EQ(merged.size(), 1u);
    EXPECT_EQ(merged.front().startMs, 10'000);
    EXPECT_EQ(merged.front().endMs, 30'000);
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
    EXPECT_TRUE(process_targeted(cfg, 42, 1000, "/opt/api"));
    EXPECT_FALSE(process_targeted(cfg, 42, 9999, "/opt/api"));
    // 阶段三：无 start time 的调用（身份缺失）不得放宽匹配——PID 复用防护。
    EXPECT_FALSE(process_targeted(cfg, 42, "/opt/api"));
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
    // 阶段一：物理采集信号 = 所有 Session 请求信号的并集。两个 Session 均未
    // 显式请求（回退四类默认）→ 物理全开。
    EXPECT_EQ(physical.signals, "cpu,io,io_syscall,sched");
    // 单 Session 只请求 cpu → 物理只采 cpu。
    ContinuousSamplerConfig cpuOnly = first;
    cpuOnly.requestedSignals = {"cpu_profile"};
    EXPECT_EQ(shared_physical_config({cpuOnly}).signals, "cpu");
    const auto args = perf_record_args(physical, "perf", "cpu-clock", "/tmp/shared.data");
    EXPECT_EQ(std::find(args.begin(), args.end(), "-a"), args.end());
    auto pidFlag = std::find(args.begin(), args.end(), "-p");
    ASSERT_NE(pidFlag, args.end());
    ASSERT_NE(++pidFlag, args.end());
    EXPECT_EQ(*pidFlag, "42,77");

    // 物理 requestedSignals 也必须保存并集；degraded 路径直接读取该字段，
    // 不能受第一个 Session 顺序影响。
    ContinuousSamplerConfig rssOnly = first;
    rssOnly.requestedSignals = {"python_rss"};
    ContinuousSamplerConfig cpuSecond = second;
    cpuSecond.requestedSignals = {"cpu_profile"};
    const auto mixedSignals = shared_physical_config({rssOnly, cpuSecond});
    EXPECT_TRUE(logical_signal_requested(mixedSignals.requestedSignals, "python_rss"));
    EXPECT_TRUE(logical_signal_requested(mixedSignals.requestedSignals, "cpu_profile"));
    EXPECT_EQ(mixedSignals.signals, "cpu");
}

TEST(SharedContinuousEngine, MergeKeepsCollectedStatusAndSeparatesPidReuse)
{
    WindowPayload first;
    first.startMs = 1000;
    first.endMs = 6000;
    first.signalStatuses["cpu_profile"].status = SignalCollectionStatus::Collected;
    HistogramPayload oldProcess;
    oldProcess.signalType = "io_latency";
    oldProcess.pid = 42;
    oldProcess.processStartMs = 1000;
    oldProcess.exe = "/opt/old";
    oldProcess.eventCount = 3;
    oldProcess.buckets.push_back({"[1, 2)", 1, 2, 3});
    first.histograms.push_back(oldProcess);

    WindowPayload second = first;
    second.startMs = 6000;
    second.endMs = 11000;
    second.signalStatuses["cpu_profile"].status = SignalCollectionStatus::NoEvents;
    second.histograms.front().processStartMs = 2000;
    second.histograms.front().exe = "/opt/new";
    second.histograms.front().eventCount = 5;
    second.histograms.front().buckets.front().count = 5;

    const auto merged = merge_shared_slices({first, second});
    EXPECT_EQ(merged.signalStatuses.at("cpu_profile").status, SignalCollectionStatus::Collected);
    ASSERT_EQ(merged.histograms.size(), 2u);
    EXPECT_NE(merged.histograms[0].processStartMs, merged.histograms[1].processStartMs);
}

TEST(SharedContinuousEngine, MergeCarriesDiagnosticsFromEverySlice)
{
    WindowPayload first;
    first.startMs = 1000;
    first.endMs = 6000;
    auto firstDiagnostics = std::make_shared<PhysicalDiagnostics>();
    PythonFallbackResult firstPython;
    firstPython.pid = 10;
    firstPython.startMs = 1000;
    firstPython.exe = "/usr/bin/python3";
    firstDiagnostics->pythonFallback.push_back(firstPython);
    first.physicalDiagnostics = firstDiagnostics;

    WindowPayload second;
    second.startMs = 6000;
    second.endMs = 11000;
    auto secondDiagnostics = std::make_shared<PhysicalDiagnostics>();
    PythonFallbackResult secondPython = firstPython;
    secondPython.pid = 20;
    secondPython.startMs = 2000;
    secondDiagnostics->pythonFallback.push_back(secondPython);
    second.physicalDiagnostics = secondDiagnostics;

    const auto merged = merge_shared_slices({first, second});
    ASSERT_NE(merged.physicalDiagnostics, nullptr);
    EXPECT_EQ(merged.physicalDiagnostics->pythonFallback.size(), 2u);
}

TEST(SharedContinuousEngine, WaitsForEverySessionBeforeAcknowledgingSharedProfile)
{
    {
        std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
        g_sharedProfileDeliveries.clear();
    }
    WindowPayload projected;
    ProfilePayload profile;
    profile.signalType = "python_memory";
    profile.readyPath = "/tmp/mini-drop-nonexistent-shared-profile.ready";
    projected.profiles.push_back(profile);

    register_shared_profile_deliveries(projected, "sid-a");
    register_shared_profile_deliveries(projected, "sid-b");
    acknowledge_shared_profile_deliveries({projected}, "sid-a");
    {
        std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
        ASSERT_EQ(g_sharedProfileDeliveries.size(), 1U);
        EXPECT_EQ(g_sharedProfileDeliveries.begin()->second,
                  (std::set<std::string>{"sid-b"}));
    }
    acknowledge_shared_profile_deliveries({projected}, "sid-b");
    {
        std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
        EXPECT_TRUE(g_sharedProfileDeliveries.empty());
    }
}

TEST(SharedContinuousEngine, KeepsAggregateInMemoryWhenDurableSpoolFails)
{
    SharedSessionAccumulator session;
    session.config.sessionSID = "sid-spool-failure";
    session.config.spoolDirectory = "/proc/mini-drop-unwritable-spool";
    session.config.collectorGeneration = "gen-spool-failure";
    session.config.targetFingerprint = "fp-spool-failure";
    WindowPayload window;
    window.startMs = 1000;
    window.endMs = 2000;
    window.signalStatuses["cpu_profile"].status = SignalCollectionStatus::NoEvents;
    session.slices.push_back(window);

    EXPECT_FALSE(persist_shared_aggregate(&session));
    ASSERT_EQ(session.slices.size(), 1U);
    EXPECT_TRUE(session.batch.empty());
    EXPECT_TRUE(session.batchID.empty());
}

TEST(SharedContinuousEngine, HostCoreHistogramDoesNotDuplicatePidZeroAggregate)
{
    ContinuousSamplerConfig cfg = make_cfg();
    cfg.scope = "host";
    WindowPayload window;
    append_core_histograms(&window, cfg, {{1, 42, 1, 3}, {1, 77, 1, 5}}, 0);
    uint64_t ioEvents = 0;
    for (const auto &hist : window.histograms)
    {
        EXPECT_NE(hist.pid, 0);
        if (hist.signalType == "io_latency")
            ioEvents += hist.eventCount;
    }
    EXPECT_EQ(ioEvents, 8u);
}

TEST(ContinuousSampler, V4DigestIncludesIdentityStatusAndRates)
{
    WindowPayload base;
    base.startMs = 1000;
    base.endMs = 6000;
    HistogramPayload hist;
    hist.signalType = "io_latency";
    hist.pid = 42;
    hist.processStartMs = 1000;
    hist.exe = "/opt/app";
    hist.eventCount = 1;
    hist.buckets.push_back({"[1, 2)", 1, 2, 1});
    base.histograms.push_back(hist);
    base.physicalSampleRateHz = 49;
    base.effectiveSampleRateHz = 19;
    base.signalStatuses["io_latency"].status = SignalCollectionStatus::Collected;

    WindowPayload changed = base;
    changed.histograms.front().processStartMs = 2000;
    EXPECT_NE(window_content_digest(base), window_content_digest(changed));
    changed = base;
    changed.signalStatuses["io_latency"].status = SignalCollectionStatus::Unavailable;
    EXPECT_NE(window_content_digest(base), window_content_digest(changed));
    changed = base;
    changed.effectiveSampleRateHz = 29;
    EXPECT_NE(window_content_digest(base), window_content_digest(changed));
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

// ============================================================
// digest 增量边界单测（3a6230f 专项）：首轮基线 / 窗口增量 / 零增量 /
// 计数器回退重建基线 / 跨 Session 隔离 / 字段解析 / JSON 序列化
// ============================================================

// ---- 首轮只建立基线 ----
TEST(ContinuousSampler, DigestDeltaFirstSeenBaselineOnly)
{
    DigestCounterState first{100, 5000000000ULL, 50};
    DigestDeltaResult delta = compute_digest_delta(nullptr, first);
    EXPECT_EQ(delta.kind, DigestDeltaKind::FirstSeen);
    EXPECT_EQ(delta.deltaCalls, 0u);
    EXPECT_EQ(delta.deltaLatencyUs, 0u);
    EXPECT_EQ(delta.deltaRows, 0u);
}

// ---- 后续仅上报窗口增量，latency 从 ps 换算成 us ----
TEST(ContinuousSampler, DigestDeltaReportsWindowIncrement)
{
    DigestCounterState prev{100, 5000000000ULL, 50};
    DigestCounterState cur{120, 5200000000ULL, 60};
    DigestDeltaResult delta = compute_digest_delta(&prev, cur);
    EXPECT_EQ(delta.kind, DigestDeltaKind::Increment);
    EXPECT_EQ(delta.deltaCalls, 20u);
    // (5.2e12 - 5.0e12) ps = 200000000 ps = 200 us（ps -> us 除以 1e6）
    EXPECT_EQ(delta.deltaLatencyUs, 200ULL);
    EXPECT_EQ(delta.deltaRows, 10u);
}

// ---- 零增量不输出（deltaCalls == 0）----
TEST(ContinuousSampler, DigestDeltaZeroCallsNotEmitted)
{
    DigestCounterState prev{100, 5000000000ULL, 50};
    DigestCounterState cur{100, 5100000000ULL, 55};
    DigestDeltaResult delta = compute_digest_delta(&prev, cur);
    EXPECT_EQ(delta.kind, DigestDeltaKind::Increment);
    EXPECT_EQ(delta.deltaCalls, 0u);
    // 调用方应据此跳过上报
}

// ---- countStar 回退：重建基线，本轮不上报 ----
TEST(ContinuousSampler, DigestDeltaCounterResetRebuildsBaseline)
{
    DigestCounterState prev{100, 5000000000ULL, 50};
    DigestCounterState cur{3, 5000000000ULL, 50};
    DigestDeltaResult delta = compute_digest_delta(&prev, cur);
    EXPECT_EQ(delta.kind, DigestDeltaKind::Reset);
}

// ---- sumTimerWaitPs 回退：重建基线，避免无符号下溢 ----
TEST(ContinuousSampler, DigestDeltaLatencyResetRebuildsBaseline)
{
    DigestCounterState prev{100, 5000000000ULL, 50};
    DigestCounterState cur{120, 4000000000ULL, 60};
    DigestDeltaResult delta = compute_digest_delta(&prev, cur);
    EXPECT_EQ(delta.kind, DigestDeltaKind::Reset);
    // 旧实现只检查 countStar，这里 sumTimerWaitPs 回退会触发无符号下溢
}

// ---- sumRowsExamined 回退：重建基线，避免无符号下溢 ----
TEST(ContinuousSampler, DigestDeltaRowsResetRebuildsBaseline)
{
    DigestCounterState prev{100, 5000000000ULL, 50};
    DigestCounterState cur{120, 5200000000ULL, 10};
    DigestDeltaResult delta = compute_digest_delta(&prev, cur);
    EXPECT_EQ(delta.kind, DigestDeltaKind::Reset);
}

// ---- 跨 Session / 跨实例状态隔离 ----
TEST(ContinuousSampler, DigestStateKeyIsolatesSessionAndInstance)
{
    const std::string k1 = digest_state_key("session-a", "mysql-a", "digest-1");
    const std::string k2 = digest_state_key("session-b", "mysql-a", "digest-1");
    const std::string k3 = digest_state_key("session-a", "mysql-b", "digest-1");
    const std::string k4 = digest_state_key("session-a", "mysql-a", "digest-2");
    EXPECT_NE(k1, k2); // 不同 Session 采同一实例
    EXPECT_NE(k1, k3); // 同一 Session 采不同实例
    EXPECT_NE(k1, k4); // 同一 Session 同一实例不同 digest
    EXPECT_EQ(k1, digest_state_key("session-a", "mysql-a", "digest-1"));
}

// ---- digest 行解析：正常 6 列 ----
TEST(ContinuousSampler, DigestRowParsesValidLine)
{
    ParsedDigestRow row;
    EXPECT_TRUE(parse_digest_row("mydb\tdigest123\tSELECT * FROM t WHERE id = ?\t42\t5000000000\t99", &row));
    EXPECT_EQ(row.schemaName, "mydb");
    EXPECT_EQ(row.digest, "digest123");
    EXPECT_EQ(row.digestText, "SELECT * FROM t WHERE id = ?");
    EXPECT_EQ(row.countStar, 42u);
    EXPECT_EQ(row.sumTimerWaitPs, 5000000000ULL);
    EXPECT_EQ(row.sumRowsExamined, 99u);
}

// ---- digest 行解析：列数不足 / 计数列非数字 -> false ----
TEST(ContinuousSampler, DigestRowRejectsMalformedLines)
{
    ParsedDigestRow row;
    EXPECT_FALSE(parse_digest_row("mydb\tdigest123\ttext\t42\t5000", &row));           // 只有 5 列
    EXPECT_FALSE(parse_digest_row("mydb\tdigest123\ttext\tnotnum\t5000\t1", &row));    // count 非数字
    EXPECT_FALSE(parse_digest_row("mydb\tdigest123\ttext\t42\tnotnum\t1", &row));      // latency 非数字
    EXPECT_FALSE(parse_digest_row("mydb\tdigest123\ttext\t42\t5000\tnotnum", &row));   // rows 非数字
    EXPECT_FALSE(parse_digest_row("", &row));
}

// ---- digest JSON 序列化：字段、转义、时间戳、signal_types ----
TEST(ContinuousSampler, BatchJsonEmitsDigestSnapshot)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1700000000000;
    window.endMs = 1700000001000;
    window.backendStatus = "ok";
    DBSnapshotSample snap;
    snap.kind = "digest";
    snap.instanceLabel = "mysql-a";
    snap.timestampMs = 1700000000500;
    snap.schemaName = "mydb";
    snap.digestText = "SELECT * FROM t WHERE id = \"1\" AND x = '\\'";
    snap.callCount = 7;
    snap.totalLatencyUs = 123456;
    snap.rowsExaminedTotal = 88;
    window.dbSnapshots.push_back(snap);

    const std::string json = build_batch_json(cfg, "cpb-db-digest", {window});
    EXPECT_NE(json.find("\"db_snapshots\":[{"), std::string::npos);
    EXPECT_NE(json.find("\"kind\":\"digest\""), std::string::npos);
    EXPECT_NE(json.find("\"instance_label\":\"mysql-a\""), std::string::npos);
    EXPECT_NE(json.find("\"schema_name\":\"mydb\""), std::string::npos);
    // digest_text 中的引号/反斜杠被转义
    EXPECT_NE(json.find("\"digest_text\":\"SELECT * FROM t WHERE id = \\\"1\\\" AND x = '\\\\'\""), std::string::npos);
    EXPECT_NE(json.find("\"call_count\":7"), std::string::npos);
    EXPECT_NE(json.find("\"total_latency_us\":123456"), std::string::npos);
    EXPECT_NE(json.find("\"rows_examined_total\":88"), std::string::npos);
    // 时间戳 RFC3339
    EXPECT_NE(json.find("\"timestamp\":\"2023-11-14T22:13:20.500Z\""), std::string::npos);
    // signal_types / backends 包含 db_snapshot
    EXPECT_NE(json.find("\"db_snapshot\""), std::string::npos);
    EXPECT_NE(json.find("\"db_system_views\""), std::string::npos);
}

// ---- lock_wait JSON 序列化：字段齐全 ----
TEST(ContinuousSampler, BatchJsonEmitsLockWaitSnapshot)
{
    ContinuousSamplerConfig cfg = make_cfg();
    WindowPayload window;
    window.startMs = 1700000000000;
    window.endMs = 1700000001000;
    window.backendStatus = "ok";
    DBSnapshotSample snap;
    snap.kind = "lock_wait";
    snap.instanceLabel = "mysql-a";
    snap.timestampMs = 1700000000500;
    snap.waitingPid = 1001;
    snap.waitingQuery = "UPDATE t SET x=1";
    snap.blockingPid = 1002;
    snap.blockingQuery = "SELECT * FROM t FOR UPDATE";
    snap.waitSeconds = 12;
    snap.lockedTable = "db.t";
    window.dbSnapshots.push_back(snap);

    const std::string json = build_batch_json(cfg, "cpb-db-lock", {window});
    EXPECT_NE(json.find("\"kind\":\"lock_wait\""), std::string::npos);
    EXPECT_NE(json.find("\"waiting_pid\":1001"), std::string::npos);
    EXPECT_NE(json.find("\"waiting_query\":\"UPDATE t SET x=1\""), std::string::npos);
    EXPECT_NE(json.find("\"blocking_pid\":1002"), std::string::npos);
    EXPECT_NE(json.find("\"blocking_query\":\"SELECT * FROM t FOR UPDATE\""), std::string::npos);
    EXPECT_NE(json.find("\"wait_seconds\":12"), std::string::npos);
    EXPECT_NE(json.find("\"locked_table\":\"db.t\""), std::string::npos);
}

// ---- 密码文件缺失 / 无密码引用：读密码降级不崩溃 ----
TEST(ContinuousSampler, ReadDbPasswordMissingFileDegrades)
{
    DBTargetConfig target;
    target.instanceLabel = "mysql-a";
    target.passwordRef = "/tmp/definitely-missing-password-file-xyz";
    std::string password;
    EXPECT_FALSE(read_db_target_password(target, &password));

    DBTargetConfig empty;
    EXPECT_TRUE(read_db_target_password(empty, &password));
    EXPECT_TRUE(password.empty());
}
