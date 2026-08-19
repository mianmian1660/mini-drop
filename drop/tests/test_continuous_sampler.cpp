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

#include <string>

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
