// ============================================================
// tests/test_language_status.cpp — 阶段 4 统一语言诊断契约单元测试
// ============================================================

#include "common/LanguageStatus.h"

#include <nlohmann/json.hpp>

#include <gtest/gtest.h>

namespace drop
{

namespace
{

AggregatedSample makeSample(const std::string &runtime,
                            int pid,
                            uint64_t count,
                            const std::vector<std::string> &stack,
                            const std::vector<std::string> &mappingFiles = {})
{
    AggregatedSample sample;
    sample.runtime = runtime;
    sample.pid = pid;
    sample.processStartMs = 1234;
    sample.exe = "/usr/bin/app";
    sample.comm = "app";
    sample.count = count;
    sample.stack = stack;
    // 结构化帧与 stack 平行（模拟 parse_perf_script_result 的产物）。
    for (size_t i = 0; i < stack.size(); ++i)
    {
        ContinuousStackFrame frame;
        frame.raw = stack[i];
        frame.mappingFile = i < mappingFiles.size() ? mappingFiles[i] : "";
        if (!frame.raw.empty() && !unresolved_frame(frame.raw))
        {
            frame.function = frame.raw;
            frame.resolved = true;
        }
        sample.frames.push_back(frame);
    }
    return sample;
}

} // namespace

TEST(LanguageStatusFrameClassification, PythonFrames)
{
    EXPECT_TRUE(is_python_semantic_frame_text("py::worker:/app/main.py+0x1a"));
    EXPECT_TRUE(is_python_semantic_frame_text("burn (app.py:12)"));
    EXPECT_TRUE(is_python_semantic_frame_text("helper (/opt/app/pkg/mod.py)"));
    EXPECT_FALSE(is_python_semantic_frame_text("0x7f3a2b [libpython3.12.so]"));
    EXPECT_FALSE(is_python_semantic_frame_text("__pthread_start ([kernel.kallsyms])"));
}

TEST(LanguageStatusFrameClassification, JsFrames)
{
    EXPECT_TRUE(is_js_semantic_frame_text("LazyCompile:*nodeBurnWorker /app/server.js:10:20"));
    EXPECT_TRUE(is_js_semantic_frame_text("handler ~util.js"));
    EXPECT_FALSE(is_js_semantic_frame_text("0x5f2a [libnode.so]"));
    EXPECT_FALSE(is_js_semantic_frame_text("v8::internal::Compiler::Compile [libnode.so]"));
}

TEST(LanguageStatusFrameClassification, JavaAndGoFrames)
{
    EXPECT_TRUE(is_java_semantic_frame_text("com.example.Burner.work"));
    EXPECT_TRUE(is_java_semantic_frame_text("java/lang/Thread.run"));
    EXPECT_FALSE(is_java_semantic_frame_text("0x7f00 [libjvm.so]"));
    EXPECT_FALSE(is_java_semantic_frame_text("[kernel.kallsyms]"));

    EXPECT_TRUE(is_go_semantic_frame_text("main.goBurnWorker"));
    EXPECT_TRUE(is_go_semantic_frame_text("runtime.schedule"));
    EXPECT_FALSE(is_go_semantic_frame_text("std::vector::_M_realloc"));
    EXPECT_FALSE(is_go_semantic_frame_text("0x400f00 [dockerd]"));
}

TEST(LanguageStatusReport, ReadyRequiresSemanticThreshold)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.runtimeReport.java.detected = true;
    diagnostics.runtimeReport.java.ready = true;
    diagnostics.runtimeReport.java.readyPids = {42};

    // 全部帧都是已解析 JVM 方法 → ready
    std::vector<AggregatedSample> samples = {
        makeSample("java", 42, 10, {"com.example.Burner.work", "java.lang.Thread.run"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *java = report.find("java");
    ASSERT_NE(java, nullptr);
    EXPECT_EQ(java->runtimeDetection, "detected");
    EXPECT_EQ(java->collectorStatus, "ready");
    EXPECT_EQ(java->symbolStatus, "complete");
    EXPECT_EQ(java->semanticFramePercent, 100.0);
    EXPECT_EQ(java->semanticSamplePercent, 100.0);
    EXPECT_EQ(java->unresolvedFramePercent, 0.0);
    EXPECT_EQ(java->sampleCount, 10u);
}

TEST(LanguageStatusReport, ReadyUsesSemanticSampleCoverageNotStackDepth)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.runtimeReport.java.detected = true;
    diagnostics.runtimeReport.java.ready = true;
    diagnostics.runtimeReport.java.readyPids = {42};

    // 每个样本都命中业务方法，但栈里还有已解析的 JVM 辅助帧。帧占比只有
    // 25%，语言级样本覆盖是 100%，采集能力应判 ready。
    std::vector<AggregatedSample> samples = {
        makeSample("java", 42, 10,
                   {"com.example.Burner.work", "CompilerThread", "Safepoint", "VMThread"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *java = report.find("java");
    ASSERT_NE(java, nullptr);
    EXPECT_EQ(java->semanticFramePercent, 25.0);
    EXPECT_EQ(java->semanticSamplePercent, 100.0);
    EXPECT_EQ(java->collectorStatus, "ready");
}

TEST(LanguageStatusReport, NativeQualityUsesTargetModuleUnresolvedRatio)
{
    PhysicalDiagnostics diagnostics;
    std::vector<AggregatedSample> samples = {
        makeSample("native", 3, 10, {"hot_a", "0x7f00 [libc.so.6]"},
                   {"/usr/bin/app", "/lib/x86_64-linux-gnu/libc.so.6"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *native = report.find("native");
    ASSERT_NE(native, nullptr);
    EXPECT_EQ(native->unresolvedFramePercent, 50.0);
    EXPECT_EQ(native->targetModuleFrameWeight, 10u);
    EXPECT_EQ(native->targetModuleUnresolvedPercent, 0.0);
    EXPECT_EQ(native->collectorStatus, "ready");
}

TEST(LanguageStatusReport, MissingWhenNoMapEvenWithSamples)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.runtimeReport.java.detected = true;
    diagnostics.runtimeReport.java.missingPids = {7};
    diagnostics.runtimeReport.java.reason = "no JIT perf map";

    std::vector<AggregatedSample> samples = {
        makeSample("java", 7, 5, {"com.example.Burner.work"}),
        makeSample("java", 7, 15, {"0xff01 [unknown]", "0xff02 [unknown]"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *java = report.find("java");
    ASSERT_NE(java, nullptr);
    // 有样本但没有可用采集方式 → missing，不能误报 ready。
    EXPECT_EQ(java->collectorStatus, "missing");
    ASSERT_FALSE(java->reasons.empty());
    EXPECT_EQ(java->reasons[0], "no JIT perf map");
    // 未解析率 = 30 / (5 + 30)
    EXPECT_NEAR(java->unresolvedFramePercent, 85.71, 0.05);
    EXPECT_EQ(java->symbolStatus, "partial");
    ASSERT_EQ(java->processes.size(), 1u);
    EXPECT_EQ(java->processes[0].status, "missing");
}

TEST(LanguageStatusReport, KernelFramesExcludedFromLanguageCoverage)
{
    PhysicalDiagnostics diagnostics;
    // python 样本混入内核帧：内核帧（结构化 mappingFile=[kernel.kallsyms]）
    // 不进入分母。
    AggregatedSample sample = makeSample("python", 9, 4,
                                         {"py::main:/srv/api.py+0x0",
                                          "__do_syscall",
                                          "0xdead [unknown]",
                                          "py::handle:/srv/api.py+0x2"},
                                         {"", "[kernel.kallsyms]", "", ""});
    LanguageStatusReport report = build_language_status({sample}, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *python = report.find("python");
    ASSERT_NE(python, nullptr);
    // 非内核帧权重 = 3（2 语义 + 1 未解析），语义覆盖 = 2/3
    EXPECT_NEAR(python->semanticFramePercent, 66.67, 0.05);
    EXPECT_NEAR(python->unresolvedFramePercent, 33.33, 0.05);
    EXPECT_LT(python->semanticFramePercent, 70.0); // 低于 ready 门槛
}

TEST(LanguageStatusReport, NoFakePythonRowWhenNotDetected)
{
    PhysicalDiagnostics diagnostics; // 空：没有 python 检测、无样本
    std::vector<AggregatedSample> samples = {
        makeSample("native", 3, 2, {"malloc", "main"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *python = report.find("python");
    ASSERT_NE(python, nullptr);
    EXPECT_EQ(python->runtimeDetection, "not_detected");
    EXPECT_EQ(python->collectorStatus, "not_applicable");
    EXPECT_EQ(python->sampleCount, 0u);
    // native 行正常存在
    const LanguageStatusEntry *native = report.find("native");
    ASSERT_NE(native, nullptr);
    EXPECT_EQ(native->collectorStatus, "ready");
    EXPECT_EQ(native->sampleCount, 2u);
}

TEST(LanguageStatusReport, PySpyFailureMarksFailedProcess)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.pythonFallbackLimitedCount = 1;
    drop::PythonFallbackResult failed;
    failed.pid = 11;
    failed.startMs = 999;
    failed.exe = "/usr/bin/python3";
    failed.ready = false;
    failed.reason = "py-spy attach permission denied";
    diagnostics.pythonFallback.push_back(failed);

    std::vector<AggregatedSample> samples = {
        makeSample("python", 11, 6, {"0x1 [unknown]"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *python = report.find("python");
    ASSERT_NE(python, nullptr);
    EXPECT_EQ(python->collectorStatus, "failed");
    ASSERT_EQ(python->processes.size(), 1u);
    EXPECT_EQ(python->processes[0].status, "failed");
    EXPECT_EQ(python->processes[0].mode, "py-spy");
}

TEST(LanguageStatusReport, ReadyPySpySupersedesMissingPerfMapForSameProcess)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.runtimeReport.python.detected = true;
    diagnostics.runtimeReport.python.missingPids = {11};
    diagnostics.runtimeReport.python.reason = "python perf map missing; start with -X perf";
    PythonFallbackResult ready;
    ready.pid = 11;
    ready.startMs = 1234;
    ready.exe = "/usr/bin/app";
    ready.comm = "python";
    ready.ready = true;
    ready.nativeStacks = false;
    diagnostics.pythonFallback.push_back(ready);

    std::vector<AggregatedSample> samples = {
        makeSample("python", 11, 10, {"hot_a (/app/main.py:7)"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *python = report.find("python");
    ASSERT_NE(python, nullptr);
    EXPECT_EQ(python->collectorStatus, "ready");
    ASSERT_EQ(python->processes.size(), 1u);
    EXPECT_EQ(python->processes[0].mode, "py-spy");
    EXPECT_EQ(python->processes[0].status, "ready");
}

TEST(LanguageStatusReport, GoPendingReportsPendingState)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.goReport.pending.push_back({"abc123", "/usr/bin/dockerd", "background extraction"});
    // 质量不足（未解析帧为主）时 GoReSym pending 才作为 collector 状态呈现；
    // 质量达标（原生符号足够）时应直接 ready。
    std::vector<AggregatedSample> lowQuality = {
        makeSample("go", 20, 3, {"0xa1 [unknown]"}), makeSample("go", 20, 1, {"0xa2 [unknown]"})};
    LanguageStatusReport report =
        build_language_status(lowQuality, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *go = report.find("go");
    ASSERT_NE(go, nullptr);
    EXPECT_EQ(go->collectorStatus, "pending");
    EXPECT_NE(std::find(go->reasons.begin(), go->reasons.end(),
                        "GoReSym background extraction in progress"),
              go->reasons.end());

    std::vector<AggregatedSample> highQuality = {
        makeSample("go", 20, 3, {"main.handler"})};
    LanguageStatusReport readyReport =
        build_language_status(highQuality, diagnostics, nullptr, "fp");
    EXPECT_EQ(readyReport.find("go")->collectorStatus, "ready");
}

TEST(LanguageStatusReport, GoreSymDisabledIsMissingNotFailed)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.goReport.failed.push_back({"abc123", "/usr/bin/app", "DROP_CONTINUOUS_GORESYM disabled"});
    diagnostics.goReport.disabled = "DROP_CONTINUOUS_GORESYM disabled";
    std::vector<AggregatedSample> samples = {
        makeSample("go", 21, 3, {"0xa1 [unknown]"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "fp");
    const LanguageStatusEntry *go = report.find("go");
    ASSERT_NE(go, nullptr);
    EXPECT_EQ(go->collectorStatus, "missing");
}

TEST(LanguageStatusJson, SerializesContractFields)
{
    LanguageStatusReport report;
    LanguageStatusEntry entry;
    entry.runtime = "python";
    entry.runtimeDetection = "detected";
    entry.collectorModes = {"perf-map", "py-spy-native"};
    entry.collectorStatus = "ready";
    entry.symbolStatus = "complete";
    entry.semanticFramePercent = 92.46;
    entry.semanticSamplePercent = 98.25;
    entry.unresolvedFramePercent = 4.07;
    entry.targetModuleUnresolvedPercent = 1.25;
    entry.frameWeight = 2000;
    entry.semanticFrameWeight = 1849;
    entry.unresolvedFrameWeight = 81;
    entry.semanticSampleWeight = 1212;
    entry.targetModuleFrameWeight = 800;
    entry.targetModuleUnresolvedFrameWeight = 10;
    entry.sampleCount = 1234;
    LanguageProcessStatus proc;
    proc.pid = 5;
    proc.processStartMs = 777;
    proc.mode = "perf-map";
    proc.status = "ready";
    entry.processes.push_back(proc);
    report.languages["python"] = entry;

    std::string json = language_status_to_json(report);
    EXPECT_NE(json.find("\"diagnostics_version\":2"), std::string::npos);
    EXPECT_NE(json.find("\"language_status\":{\"python\":{"), std::string::npos);
    EXPECT_NE(json.find("\"runtime_detection\":\"detected\""), std::string::npos);
    EXPECT_NE(json.find("\"collector_modes\":[\"perf-map\",\"py-spy-native\"]"), std::string::npos);
    EXPECT_NE(json.find("\"collector_status\":\"ready\""), std::string::npos);
    EXPECT_NE(json.find("\"symbol_status\":\"complete\""), std::string::npos);
    EXPECT_NE(json.find("\"semantic_frame_percent\":92.46"), std::string::npos);
    EXPECT_NE(json.find("\"semantic_sample_percent\":98.25"), std::string::npos);
    EXPECT_NE(json.find("\"unresolved_frame_percent\":4.07"), std::string::npos);
    EXPECT_NE(json.find("\"target_module_unresolved_percent\":1.25"), std::string::npos);
    EXPECT_NE(json.find("\"frame_weight\":2000"), std::string::npos);
    EXPECT_NE(json.find("\"semantic_sample_weight\":1212"), std::string::npos);
    EXPECT_NE(json.find("\"sample_count\":1234"), std::string::npos);
    EXPECT_NE(json.find("\"process_start_ms\":777"), std::string::npos);
}

TEST(LanguageStatusFragment, InjectedIntoSymbolRefs)
{
    drop::RuntimeSymbolReport runtimeReport;
    drop::GoSymbolReport goReport;
    std::vector<drop::PythonFallbackResult> noPython;
    std::vector<AggregatedSample> samples = {
        makeSample("native", 3, 2, {"malloc", "main"})};

    const std::string refs = combined_symbol_refs_json(
        runtimeReport, goReport, noPython, 0, {}, samples, {}, "", nullptr, "fp");
    EXPECT_NE(refs.find("\"diagnostics_version\":2"), std::string::npos);
    EXPECT_NE(refs.find("\"language_status\":{"), std::string::npos);
    // 旧字段保留一个兼容周期
    EXPECT_NE(refs.find("\"runtime_maps\""), std::string::npos);
    EXPECT_NE(refs.find("\"native_go\""), std::string::npos);
    EXPECT_NE(refs.find("\"python_fallback\""), std::string::npos);

    // 回归：注入 v2 片段后整体必须是合法 JSON（曾因少一个闭合大括号
    // 导致服务端 400 拒收整批数据）。
    ASSERT_NO_THROW({
        auto parsed = nlohmann::json::parse(refs);
        EXPECT_EQ(parsed["python_memory"]["ready"].is_array(), true);
        EXPECT_EQ(parsed["language_status"]["native"]["collector_status"], "ready");
        EXPECT_EQ(parsed["diagnostics_version"], 2);
    });
}

TEST(LanguageStatusPySpyMerge, StaleCaptureDoesNotReplacePerfSamples)
{
    AggregatedSample perfPython;
    perfPython.comm = "python";
    perfPython.pid = 100;
    perfPython.processStartMs = 111;
    perfPython.exe = "/usr/bin/python3";
    perfPython.backend = "perf_rolling";
    perfPython.runtime = "python";
    perfPython.stack = {"py::worker:w.py+0x1"};
    perfPython.count = 5;
    std::vector<AggregatedSample> samples = {perfPython};

    PythonFallbackResult ready;
    ready.pid = 100;
    ready.startMs = 111;
    ready.comm = "python";
    ready.exe = "/usr/bin/python3";
    ready.ready = true;
    ready.captureStartMs = 100000; // 与窗口 [200000,210000] 完全不重叠
    ready.captureEndMs = 105000;
    ready.samples.push_back(PythonStackSample{{"py::main:m.py+0x9"}, 9});

    bool replaced = false;
    // 过期结果（capture 区间早于窗口）不得替换当前窗口的 perf 样本。
    merge_python_sidecar_samples(&samples, {ready}, &replaced, 200000, 210000);
    EXPECT_FALSE(replaced);
    ASSERT_EQ(samples.size(), 1u);
    EXPECT_EQ(samples.front().backend, "perf_rolling");

    // 时间重叠 + 身份一致 → 正常替换。
    PythonFallbackResult fresh = ready;
    fresh.captureStartMs = 195000;
    fresh.captureEndMs = 205000;
    merge_python_sidecar_samples(&samples, {fresh}, &replaced, 200000, 210000);
    EXPECT_TRUE(replaced);
    ASSERT_EQ(samples.size(), 1u);
    EXPECT_EQ(samples.front().backend, "py-spy");

    // 身份不一致（PID 复用后的新进程）→ 不删除新进程样本。
    std::vector<AggregatedSample> reused = {perfPython};
    reused.front().processStartMs = 999; // 新进程
    PythonFallbackResult staleIdentity = ready;
    staleIdentity.captureStartMs = 195000;
    staleIdentity.captureEndMs = 205000;
    bool replacedReused = false;
    merge_python_sidecar_samples(&reused, {staleIdentity}, &replacedReused, 200000, 210000);
    EXPECT_FALSE(replacedReused);
    for (const auto &sample : reused)
        EXPECT_NE(sample.backend, "py-spy");
}

TEST(LanguageStatusUnwindMode, NativeRowReflectsDwarfMode)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.unwindMode = "dwarf";
    std::vector<AggregatedSample> samples = {
        makeSample("native", 3, 2, {"worker", "main"})};
    LanguageStatusReport report = build_language_status(samples, diagnostics, nullptr, "dwarf");
    const LanguageStatusEntry *native = report.find("native");
    ASSERT_NE(native, nullptr);
    ASSERT_EQ(native->collectorModes.size(), 1u);
    EXPECT_EQ(native->collectorModes.front(), "perf-dwarf");

    LanguageStatusReport fpReport = build_language_status(samples, diagnostics, nullptr, "fp");
    EXPECT_EQ(fpReport.find("native")->collectorModes.front(), "perf-fp");
}

} // namespace drop
