// ============================================================
// common/ContinuousSegmentProcessor.cpp — 统一 perf.data 段处理流水线实现
// ============================================================
// 阶段 2：strict（滚动 perf + CO-RE）与 degraded（窗口 perf + bpftrace）都把
// 不可变 perf.data 段交给这里，共享符号准备、解析、runtime 分类与诊断生成。
// ============================================================

#include "common/ContinuousSegmentProcessor.h"
#include "common/LanguageStatus.h"

#include <iostream>
#include <set>
#include <sstream>

namespace drop
{

// 把物理级 py-spy sidecar 结果合并进 CPU samples（声明见头文件）：
//   - py-spy 就绪：删除该 PID 在 perf 段中的 Python 样本（避免双计数），
//     追加带真实计数的 py-spy sidecar 样本（backend=py-spy）。
//   - py-spy 失败（ready=false，含超时/PID 复用被上游 capture_one 判定）：
//     保留原 perf 样本，fallback 诊断标记失败。
// 阶段四：结果只能替换"capture 区间与窗口时间重叠 + 身份一致"的 perf 样本；
// 过期 capture（与窗口完全不重叠）不得应用；窗口内不存在同身份 perf 样本时
// 也不得凭空追加 py-spy 样本（防 PID 复用后把旧进程数据算进新进程窗口）。
void merge_python_sidecar_samples(std::vector<AggregatedSample> *samples,
                                  const std::vector<drop::PythonFallbackResult> &pythonResults,
                                  bool *anyReplaced,
                                  int64_t windowStartMs,
                                  int64_t windowEndMs)
{
    if (!samples || pythonResults.empty())
        return;
    auto overlapsWindow = [&](const drop::PythonFallbackResult &result) {
        if (result.captureStartMs <= 0 || result.captureEndMs <= 0)
            return true; // 历史数据无区间：保持旧行为
        if (windowStartMs <= 0 || windowEndMs <= 0)
            return true;
        return result.captureStartMs <= windowEndMs && result.captureEndMs >= windowStartMs;
    };
    // 只有"时间重叠且身份能在本窗 perf 样本中找到"的结果才会替换；
    // 其余（过期 / 身份不一致 / 窗口无该进程样本）一律忽略，不删除也不追加。
    std::set<const drop::PythonFallbackResult *> replacing;
    for (const auto &result : pythonResults)
    {
        if (!result.ready || result.pid <= 0 || result.startMs <= 0 || result.exe.empty())
            continue;
        if (!overlapsWindow(result))
            continue; // 过期结果不得替换当前窗口样本
        const bool present = std::any_of(samples->begin(), samples->end(), [&](const auto &sample) {
            return sample.pid == result.pid && sample.processStartMs > 0 &&
                   sample.processStartMs == result.startMs && !sample.exe.empty() &&
                   sample.exe == result.exe;
        });
        if (present)
            replacing.insert(&result);
    }
    if (replacing.empty())
        return;
    std::vector<AggregatedSample> appended;
    for (const auto *result : replacing)
    {
        for (const auto &raw : result->samples)
        {
            AggregatedSample sample;
            sample.stack = raw.stack;
            sample.comm = result->comm.empty() ? "python" : result->comm;
            sample.pid = result->pid;
            sample.processStartMs = result->startMs;
            sample.exe = result->exe;
            sample.backend = "py-spy";
            sample.runtime = "python";
            sample.count = clamp_count(raw.count);
            appended.push_back(std::move(sample));
        }
    }
    // 删除被替换的 perf Python 样本：按 pid + start time + exe 匹配——PID 复用
    // 时（同 PID 不同 start time）不得删除新进程的 perf 样本。
    samples->erase(std::remove_if(samples->begin(), samples->end(), [&](const auto &sample) {
                       return std::any_of(replacing.begin(), replacing.end(), [&](const auto *candidate) {
                           return candidate->pid == sample.pid &&
                                  candidate->startMs > 0 && sample.processStartMs > 0 &&
                                  candidate->startMs == sample.processStartMs &&
                                  !candidate->exe.empty() && !sample.exe.empty() &&
                                  candidate->exe == sample.exe;
                       });
                   }),
                   samples->end());
    samples->insert(samples->end(), appended.begin(), appended.end());
    if (anyReplaced)
        *anyReplaced = true;
}

SegmentProcessResult ContinuousSegmentProcessor::Process(
    const PerfSegment &segment,
    const ContinuousSamplerConfig &physicalConfig,
    const RuntimeCapabilitySet &capabilities,
    const std::vector<drop::PythonFallbackResult> &pythonResults,
    size_t pythonLimitedCount,
    const std::vector<drop::MemrayProfileResult> &memrayResults)
{
    SegmentProcessResult result;
    const std::string perf = perf_bin();
    const std::string &dataPath = segment.path;

    // 1. 校验并读取不可变 perf segment
    if (dataPath.empty() || !file_exists_local(dataPath))
    {
        result.failureReason = "perf segment missing: " + dataPath;
        return result;
    }

    // enrichment 总开关：只控制 enrichment 子步骤，不恢复旧 strict 解析分支。
    const bool runtimeEnrichment = env_enabled_default("DROP_CONTINUOUS_RUNTIME_ENRICHMENT", true) &&
                                   capabilities.goSymbols;

    // 2. warm_build_id_cache（perf script 自带缓存回退命中所需，始终执行）。
    // 阶段四：输出逐 DSO 预热诊断。
    std::vector<BuildIdWarmEntry> buildIdWarmReport;
    std::vector<drop::BuildIdEntry> buildIds =
        warm_build_id_cache(perf, dataPath, &buildIdWarmReport);
    const std::string buildIdReportJson = build_id_report_to_json(buildIdWarmReport);

    // 3-5. runtime maps / Go symbols（enrichment 关闭时跳过，仍走统一解析器）
    drop::RuntimeSymbolReport runtimeReport;
    drop::GoSymbolReport goReport;
    if (runtimeEnrichment)
    {
        drop::RuntimeSymbolReport fullReport = drop::collect_runtime_maps(perf, dataPath);
        // 阶段四：逐语言开关过滤（关闭的语言不产出 ready 状态与 map 复制）。
        if (!capabilities.javaPerfMap)
        {
            fullReport.java.missingPids.insert(fullReport.java.missingPids.end(),
                                               fullReport.java.readyPids.begin(),
                                               fullReport.java.readyPids.end());
            fullReport.java.readyPids.clear();
            fullReport.java.ready = false;
            if (fullReport.java.detected && fullReport.java.reason.empty())
                fullReport.java.reason = "DROP_CONTINUOUS_JAVA_PERFMAP disabled";
        }
        if (!capabilities.nodePerfMap)
        {
            fullReport.node.missingPids.insert(fullReport.node.missingPids.end(),
                                               fullReport.node.readyPids.begin(),
                                               fullReport.node.readyPids.end());
            fullReport.node.readyPids.clear();
            fullReport.node.ready = false;
            if (fullReport.node.detected && fullReport.node.reason.empty())
                fullReport.node.reason = "DROP_CONTINUOUS_NODE_PERFMAP disabled";
        }
        if (!capabilities.pythonPerf)
        {
            fullReport.python.missingPids.insert(fullReport.python.missingPids.end(),
                                                 fullReport.python.readyPids.begin(),
                                                 fullReport.python.readyPids.end());
            fullReport.python.readyPids.clear();
            fullReport.python.ready = false;
            if (fullReport.python.detected && fullReport.python.reason.empty())
                fullReport.python.reason = "DROP_CONTINUOUS_PYTHON_PERF disabled; py-spy fallback applies";
        }
        runtimeReport = std::move(fullReport);
        std::set<std::string> knownDsoPaths;
        for (const auto &entry : buildIds)
            knownDsoPaths.insert(entry.dsoPath);
        for (auto &entry : drop::discover_sampled_go_build_ids(runtimeReport.sampledPids))
            if (knownDsoPaths.insert(entry.dsoPath).second)
                buildIds.push_back(std::move(entry));
        goReport = drop::prepare_go_symbols(buildIds);
    }

    // 6. 唯一一次 perf script
    std::string scriptOutput;
    int rc = drop::exec_capture({perf, "script", "-F", "comm,pid,tid,time,event,ip,sym,dso", "-i", dataPath},
                                &scriptOutput, 32 * 1024 * 1024);
    if (rc != 0)
    {
        result.failureReason = "perf script failed rc=" + std::to_string(rc);
        std::cout << "[native-cp] " << segment.sourceBackend
                  << " perf script failed rc=" << rc << std::endl;
        return result; // 不删除段，可重试
    }

    // 7-12：解析/规范化/合并/诊断/symbol_refs（纯逻辑，与测试共用同一套实现）
    SegmentProcessResult pure = ProcessScript(scriptOutput, segment, physicalConfig, capabilities,
                                              runtimeReport, goReport, buildIds,
                                              pythonResults, pythonLimitedCount, memrayResults,
                                              buildIdWarmReport);
    if (!pure.success)
        return pure;
    pure.diagnostics.enrichmentApplied = runtimeEnrichment;
    pure.diagnostics.runtimeEnrichmentDisabled = !runtimeEnrichment;
    if (!runtimeEnrichment)
    {
        pure.diagnostics.enrichmentDisabledReason =
            env_enabled_default("DROP_CONTINUOUS_RUNTIME_ENRICHMENT", true)
                ? "go symbols capability disabled"
                : "DROP_CONTINUOUS_RUNTIME_ENRICHMENT=0";
        for (auto &window : pure.windows)
        {
            if (!window.symbolRefsJson.empty() && window.symbolRefsJson.back() == '}')
            {
                window.symbolRefsJson.pop_back();
                window.symbolRefsJson +=
                    ",\"runtime_enrichment\":{\"enabled\":false,\"reason\":\"" +
                    json_escape(pure.diagnostics.enrichmentDisabledReason) + "\"}}";
            }
        }
    }
    // ProcessScript 创建的是当时 diagnostics 的快照；补完开关状态后必须刷新，
    // 否则 process Session fan-out 读到的是缺少最终状态的旧副本。
    for (auto &window : pure.windows)
        window.physicalDiagnostics =
            std::make_shared<const PhysicalDiagnostics>(pure.diagnostics);
    // 12. 异步上报 build-id（enrichment 开启时；不阻塞当次解析）
    if (runtimeEnrichment)
        drop::upload_build_ids_async(buildIds, physicalConfig.apiBaseURL);
    return pure;
}

SegmentProcessResult ProcessScript(
    const std::string &scriptOutput,
    const PerfSegment &segment,
    const ContinuousSamplerConfig &physicalConfig,
    const RuntimeCapabilitySet &capabilities,
    const drop::RuntimeSymbolReport &runtimeReport,
    const drop::GoSymbolReport &goReport,
    const std::vector<drop::BuildIdEntry> &buildIds,
    const std::vector<drop::PythonFallbackResult> &pythonResults,
    size_t pythonLimitedCount,
    const std::vector<drop::MemrayProfileResult> &memrayResults,
    const std::vector<BuildIdWarmEntry> &buildIdWarmReport)
{
    (void)capabilities;
    SegmentProcessResult result;
    const std::string &dataPath = segment.path;
    if (dataPath.empty())
    {
        result.failureReason = "perf segment missing path";
        return result;
    }

    // 7-8. 解析并规范化 samples / structured frames，补齐 PID/start/exe/runtime/backend
    PerfScriptParseResult parsed = parse_perf_script_result(scriptOutput);
    WindowPayload window;
    window.collectorGeneration = segment.collectorGeneration;
    window.physicalSampleRateHz = physicalConfig.sampleRateHz;
    window.effectiveSampleRateHz = physicalConfig.sampleRateHz;
    window.attemptedBackends = {segment.sourceBackend};
    window.selectedBackend = segment.sourceBackend;
    window.startMs = now_ms();
    window.endMs = window.startMs;
    if (parsed.hasTimestamp)
    {
        const int64_t parsedStart = perf_timestamp_to_unix_ms(parsed.startTimestampSec,
                                                              segment.wallStartMs, segment.monotonicStartMs);
        const int64_t parsedEnd = perf_timestamp_to_unix_ms(parsed.endTimestampSec,
                                                            segment.wallStartMs, segment.monotonicStartMs);
        if (parsedStart > 0 && parsedEnd >= parsedStart)
        {
            window.startMs = parsedStart;
            window.endMs = std::max(parsedStart + 1, parsedEnd);
        }
    }
    if (window.endMs <= window.startMs)
        window.endMs = std::max(window.startMs + 1, now_ms());
    window.samples = std::move(parsed.samples);

    std::map<int, bool> goBuildInfoCache;
    for (auto &sample : window.samples)
    {
        sample.processStartMs = configured_process_start_ms(physicalConfig, sample.pid);
        if (sample.exe.empty())
            sample.exe = configured_process_exe(physicalConfig, sample.pid);
        sample.backend = segment.sourceBackend;
        sample.runtime = sample_runtime(sample, goReport, &goBuildInfoCache);
        // 9. 清理 Python 绝对源码路径
        if (sample.runtime == "python")
            for (auto &frame : sample.stack)
                frame = sanitize_python_perf_frame(frame);
    }
    if (physicalConfig.scope == "process")
    {
        window.samples.erase(std::remove_if(window.samples.begin(), window.samples.end(), [&](const auto &sample) {
                                 return !process_targeted(physicalConfig, sample.pid,
                                                          sample.processStartMs, sample.exe);
                             }),
                             window.samples.end());
    }

    // py-spy sidecar 合并（成功替换 perf Python 样本，失败保留 perf fallback）。
    // 阶段四：capture 区间与窗口时间重叠才替换，过期结果不应用。
    bool pythonReplaced = false;
    merge_python_sidecar_samples(&window.samples, pythonResults, &pythonReplaced,
                                 window.startMs, window.endMs);

    // 10. unresolved/frame 诊断
    uint64_t totalFrames = 0;
    uint64_t unresolvedFrames = 0;
    for (const auto &sample : window.samples)
    {
        for (const auto &frame : sample.stack)
        {
            totalFrames = add_count(totalFrames, sample.count);
            if (unresolved_frame(frame))
                unresolvedFrames = add_count(unresolvedFrames, sample.count);
        }
    }
    result.diagnostics.totalFrameWeight = totalFrames;
    result.diagnostics.unresolvedFrameWeight = unresolvedFrames;
    result.diagnostics.symbolStatus = totalFrames == 0 ? "not_applicable"
        : (unresolvedFrames == 0 ? "complete" : (unresolvedFrames >= totalFrames ? "missing" : "partial"));
    for (const auto &entry : buildIds)
        result.diagnostics.buildIds.push_back(entry.buildId);
    result.diagnostics.buildIdEntries = buildIds;
    result.diagnostics.runtimeReport = runtimeReport;
    result.diagnostics.goReport = goReport;
    // 阶段三：py-spy/Memray 诊断必须进入结构化物理诊断，process Session
    // fan-out 才能按身份过滤后重建 symbol_refs（否则 process 查询丢失
    // py-spy/Memray 诊断）。
    result.diagnostics.pythonFallback = pythonResults;
    result.diagnostics.pythonFallbackLimitedCount = pythonLimitedCount;
    result.diagnostics.memrayResults = memrayResults;
    result.diagnostics.unwindMode = capabilities.unwindMode.empty() ? "fp" : capabilities.unwindMode;
    result.diagnostics.buildIdWarmReport = buildIdWarmReport;
    result.diagnostics.kallsymsSha256 = ensure_kallsyms_snapshot(physicalConfig);

    // 11. 结构化物理 symbol_refs（含 py-spy/Memray sidecar 诊断与 v2 language_status）
    window.symbolRefsJson = combined_symbol_refs_json(
        runtimeReport, goReport, pythonResults, pythonLimitedCount, memrayResults,
        window.samples, buildIds, result.diagnostics.kallsymsSha256,
        nullptr, result.diagnostics.unwindMode, build_id_report_to_json(buildIdWarmReport));
    window.physicalDiagnostics = std::make_shared<const PhysicalDiagnostics>(result.diagnostics);
    result.windows.push_back(std::move(window));

    result.success = true;
    return result;
}

// 从 targets 列表补齐进程身份（供纯逻辑 fan-out 使用，不依赖 /proc）。
inline int64_t configured_process_start_ms_for_targets(const std::vector<ContinuousTargetProcess> &targets, int pid)
{
    for (const auto &target : targets)
        if (target.pid == pid)
            return target.processStartMs;
    return 0;
}

inline std::string configured_process_exe_for_targets(const std::vector<ContinuousTargetProcess> &targets, int pid)
{
    for (const auto &target : targets)
        if (target.pid == pid)
            return target.exe;
    return {};
}

// 阶段二：Session 分流后重建 symbol_refs。host Session 复用完整物理诊断；
// process Session 按本 Session 目标身份（pid+process_start_ms+exe）过滤，
// 不泄漏其他 selector 的 PID/路径/诊断。
static drop::RuntimeSymbolReport filter_runtime_report_to_session(const drop::RuntimeSymbolReport &report,
                                                                  const std::vector<ContinuousTargetProcess> &targets)
{
    drop::RuntimeSymbolReport out;
    auto keep = [&](const drop::RuntimeMapInfo &info) {
        drop::RuntimeMapInfo filtered;
        filtered.detected = false;
        filtered.ready = false;
        filtered.requiredFlag = info.requiredFlag;
        for (int pid : info.readyPids)
        {
            ProcessIdentity identity{pid, configured_process_start_ms_for_targets(targets, pid),
                                     configured_process_exe_for_targets(targets, pid), ""};
            if (identity_targeted(targets, identity))
                filtered.readyPids.push_back(pid);
        }
        for (int pid : info.missingPids)
        {
            ProcessIdentity identity{pid, configured_process_start_ms_for_targets(targets, pid),
                                     configured_process_exe_for_targets(targets, pid), ""};
            if (identity_targeted(targets, identity))
                filtered.missingPids.push_back(pid);
        }
        filtered.detected = !filtered.readyPids.empty() || !filtered.missingPids.empty();
        filtered.ready = filtered.detected && !filtered.readyPids.empty() && filtered.missingPids.empty();
        // 阶段三：reason 必须绑定具体进程，禁止复制包含其他 PID/路径的全局
        // reason；只在本 Session 确实检测到该 runtime 时保留。
        if (filtered.detected)
            filtered.reason = info.reason;
        return filtered;
    };
    out.java = keep(report.java);
    out.node = keep(report.node);
    out.python = keep(report.python);
    out.skippedRefresh = 0; // 物理预算统计无法安全归属到单个 selector
    for (const auto &entry : report.sampledPids)
    {
        ProcessIdentity identity{entry.first, configured_process_start_ms_for_targets(targets, entry.first),
                                 configured_process_exe_for_targets(targets, entry.first), ""};
        if (identity_targeted(targets, identity))
            out.sampledPids.emplace(entry);
    }
    out.status = drop::runtime_aggregate_status(out);
    return out;
}

// 核心实现：按 targets 列表严格身份过滤重建 symbol_refs（纯函数，供
// rebuild_filtered_symbol_refs 与 SessionFanoutProjector 共用）。
static std::string rebuild_filtered_symbol_refs_for_targets(
    const std::string &physicalJson,
    const PhysicalDiagnostics &diagnostics,
    const std::vector<AggregatedSample> &filteredSamples,
    const std::vector<ContinuousTargetProcess> &targets)
{
    // process Session：不能看到其他 selector 的 PID、路径或诊断。
    uint64_t totalFrames = 0;
    uint64_t unresolvedFrames = 0;
    std::set<std::string> sessionBuildIds;
    std::set<std::string> sessionMappings;
    for (const auto &sample : filteredSamples)
    {
        for (const auto &frame : sample.stack)
        {
            totalFrames = add_count(totalFrames, sample.count);
            if (unresolved_frame(frame))
                unresolvedFrames = add_count(unresolvedFrames, sample.count);
        }
        for (const auto &frame : sample.frames)
        {
            if (!frame.buildId.empty())
                sessionBuildIds.insert(frame.buildId);
            if (!frame.mappingFile.empty())
                sessionMappings.insert(frame.mappingFile);
        }
        if (!sample.exe.empty())
            sessionMappings.insert(sample.exe);
    }
    for (const auto &entry : diagnostics.buildIdEntries)
        if (sessionMappings.count(entry.dsoPath) > 0)
            sessionBuildIds.insert(entry.buildId);
    std::string status = totalFrames == 0 ? "not_applicable"
        : (unresolvedFrames == 0 ? "complete" : (unresolvedFrames >= totalFrames ? "missing" : "partial"));

    std::string body = "{";
    body += "\"symbol_status\":\"" + status + "\",";
    body += "\"frame_stats\":{\"total_frame_weight\":" + std::to_string(totalFrames) +
            ",\"unresolved_frame_weight\":" + std::to_string(unresolvedFrames) + "},";
    body += "\"build_ids\":[";
    bool firstBuild = true;
    for (const auto &buildId : sessionBuildIds)
    {
        if (!firstBuild)
            body += ",";
        firstBuild = false;
        body += "\"" + json_escape(buildId) + "\"";
    }
    body += "],";
    if (!diagnostics.kallsymsSha256.empty())
        body += "\"kallsyms_sha256\":\"" + json_escape(diagnostics.kallsymsSha256) + "\",";
    // 阶段三：Go 诊断按 DSO 归属——只保留本 Session 引用的 DSO 条目。
    drop::RuntimeSymbolReport filteredRuntimeReport =
        filter_runtime_report_to_session(diagnostics.runtimeReport, targets);
    body += "\"runtime_maps\":" + drop::runtime_maps_to_json(filteredRuntimeReport) + ",";
    drop::GoSymbolReport filteredGo;
    auto filterGo = [&](const std::vector<drop::GoSymbolItem> &source,
                        std::vector<drop::GoSymbolItem> *target) {
        for (const auto &item : source)
            if (sessionMappings.count(item.dsoPath) > 0)
                target->push_back(item);
    };
    filterGo(diagnostics.goReport.ready, &filteredGo.ready);
    filterGo(diagnostics.goReport.pending, &filteredGo.pending);
    filterGo(diagnostics.goReport.failed, &filteredGo.failed);
    body += "\"native_go\":" + drop::go_symbol_report_json(filteredGo) + ",";

    // 阶段三：py-spy 诊断按完整身份过滤（pid+start+exe）。
    std::vector<drop::PythonFallbackResult> filteredPython;
    for (const auto &item : diagnostics.pythonFallback)
    {
        ProcessIdentity identity{item.pid, item.startMs, item.exe, item.comm};
        if (identity_targeted(targets, identity))
            filteredPython.push_back(item);
    }
    body += "\"python_fallback\":" +
            python_fallback_json(filteredPython, filteredPython.empty() ? 0 : diagnostics.pythonFallbackLimitedCount) + ",";

    // 阶段三：Memray 诊断按完整身份过滤。
    std::vector<drop::MemrayProfileResult> filteredMemray;
    for (const auto &item : diagnostics.memrayResults)
    {
        ProcessIdentity identity{item.pid, item.processStartMs, item.exe, item.comm};
        if (identity_targeted(targets, identity))
            filteredMemray.push_back(item);
    }
    body += "\"python_memory\":{\"ready\":[";
    bool firstMemrayReady = true;
    for (const auto &item : filteredMemray)
    {
        if (!item.ready)
            continue;
        if (!firstMemrayReady)
            body += ",";
        firstMemrayReady = false;
        body += "{\"pid\":" + std::to_string(item.pid) + ",\"process_start_ms\":" +
                std::to_string(item.processStartMs) + ",\"exe\":\"" + json_escape(item.exe) +
                "\",\"profile_id\":\"" + json_escape(item.profileID) + "\"}";
    }
    body += "],\"failed\":[";
    bool firstMemrayFailed = true;
    for (const auto &item : filteredMemray)
    {
        if (item.ready)
            continue;
        if (!firstMemrayFailed)
            body += ",";
        firstMemrayFailed = false;
        body += "{\"pid\":" + std::to_string(item.pid) + ",\"process_start_ms\":" +
                std::to_string(item.processStartMs) + ",\"exe\":\"" + json_escape(item.exe) +
                "\",\"reason\":\"" + json_escape(item.reason) + "\"}";
    }
    body += "]}";
    // 阶段四：build-id 预热报告按本 Session 引用的 DSO/build-id 过滤。
    std::vector<BuildIdWarmEntry> sessionWarmEntries;
    for (const auto &entry : diagnostics.buildIdWarmReport)
        if (sessionMappings.count(entry.dsoPath) > 0 || sessionBuildIds.count(entry.buildId) > 0)
            sessionWarmEntries.push_back(entry);
    const std::string sessionBuildIdReport = build_id_report_to_json(sessionWarmEntries);
    if (!sessionBuildIdReport.empty())
        body += ",\"build_id_report\":" + sessionBuildIdReport;
    // 阶段四：v2 language_status 按本 Session 身份过滤后重建。
    body += "," + language_status_fragment_for_symbol_refs(
                       filteredSamples, filteredRuntimeReport, filteredGo, filteredPython,
                       filteredPython.empty() ? 0 : diagnostics.pythonFallbackLimitedCount,
                       &targets, diagnostics.unwindMode);
    if (diagnostics.runtimeEnrichmentDisabled)
        body += ",\"runtime_enrichment\":{\"enabled\":false,\"reason\":\"" +
                json_escape(diagnostics.enrichmentDisabledReason) + "\"}";
    body += "}";
    return body;
}

std::string rebuild_filtered_symbol_refs(const std::string &physicalJson,
                                         const PhysicalDiagnostics &diagnostics,
                                         const std::vector<AggregatedSample> &filteredSamples,
                                         const ContinuousSamplerConfig &session)
{
    if (session.scope != "process")
        return physicalJson;
    return rebuild_filtered_symbol_refs_for_targets(physicalJson, diagnostics, filteredSamples,
                                                    session.targetProcesses);
}

std::string build_session_symbol_refs(const PhysicalDiagnostics &diagnostics,
                                      const std::vector<AggregatedSample> &samples,
                                      const ContinuousSamplerConfig &session)
{
    PhysicalDiagnostics filtered = diagnostics;
    if (!logical_signal_requested(session.requestedSignals, "cpu_profile"))
    {
        filtered.runtimeReport = {};
        filtered.goReport = {};
        filtered.pythonFallback.clear();
        filtered.pythonFallbackLimitedCount = 0;
        filtered.buildIds.clear();
        filtered.buildIdEntries.clear();
    }
    if (!logical_signal_requested(session.requestedSignals, "python_memory"))
        filtered.memrayResults.clear();
    if (session.scope == "process")
        return rebuild_filtered_symbol_refs_for_targets("", filtered, samples,
                                                        session.targetProcesses);
    return combined_symbol_refs_json(
        filtered.runtimeReport, filtered.goReport, filtered.pythonFallback,
        filtered.pythonFallbackLimitedCount, filtered.memrayResults, samples,
        filtered.buildIdEntries, filtered.kallsymsSha256,
        nullptr, filtered.unwindMode, build_id_report_to_json(filtered.buildIdWarmReport));
}

// ============================================================
// 阶段三：SessionFanoutProjector 实现
// ============================================================

namespace
{

// 聚合 stack 的稳定排序键（降采样余数相同时保证重试结果与 content hash 稳定）。
std::string stack_identity_key(const AggregatedSample &sample)
{
    std::string key = sample.stackScope + "|" + std::to_string(sample.pid) + "|" +
                      sample.exe + "|" + sample.comm;
    for (const auto &frame : sample.stack)
        key += "|" + frame;
    return key;
}

// 构造无法安全归属的直方图（degraded 多进程 fallback）。
HistogramPayload unavailable_shared_histogram_payload(const std::string &signalType)
{
    HistogramPayload histogram;
    histogram.signalType = signalType;
    histogram.backend = "shared-bpftrace-fallback";
    histogram.unavailable = true;
    histogram.reason = "shared rolling fallback cannot attribute this histogram across multiple process selectors";
    return histogram;
}

// 物理窗口是否包含某信号的直方图条目（供 process Session 区分
// "被身份过滤剔除"与"物理层就没有"）。
bool physical_has_histogram(const WindowPayload &physical, const std::string &signalType)
{
    for (const auto &hist : physical.histograms)
        if (hist.signalType == signalType)
            return true;
    return false;
}

} // namespace

std::vector<AggregatedSample> SessionFanoutProjector::DownsampleDeterministic(
    const std::vector<AggregatedSample> &samples,
    uint64_t requestedHz,
    uint64_t physicalHz,
    const std::string &stabilityKey)
{
    if (samples.empty() || requestedHz == 0 || physicalHz == 0 || requestedHz >= physicalHz)
        return samples;
    uint64_t filteredTotal = 0;
    for (const auto &sample : samples)
        filteredTotal = add_count(filteredTotal, sample.count);
    if (filteredTotal == 0)
        return samples;
    // 目标总数 = round(filtered_total × requested_hz / physical_hz)。
    const long double ratio = static_cast<long double>(requestedHz) / static_cast<long double>(physicalHz);
    uint64_t targetTotal = static_cast<uint64_t>(std::llround(static_cast<long double>(filteredTotal) * ratio));
    if (targetTotal >= filteredTotal)
        return samples; // 不放大样本
    if (targetTotal == 0)
        return {}; // 极低流量窗口允许零样本（调用方上报 target_idle）

    // 最大余数法：先按 floor 分配，再把剩余配额按余数从大到小分配；相同
    // 余数用稳定排序键（collector_generation + 窗口身份 + session_sid +
    // stack 身份）排序，保证重试结果与 content hash 稳定。
    struct Entry
    {
        size_t index;
        uint64_t base;
        uint64_t remainder;
        std::string sortKey;
    };
    std::vector<Entry> entries;
    entries.reserve(samples.size());
    uint64_t assigned = 0;
    for (size_t i = 0; i < samples.size(); ++i)
    {
        const __int128 product = static_cast<__int128>(samples[i].count) * targetTotal;
        const uint64_t base = static_cast<uint64_t>(product / filteredTotal);
        const uint64_t remainder = static_cast<uint64_t>(product % filteredTotal);
        assigned = add_count(assigned, base);
        entries.push_back({i, base, remainder, stabilityKey + "|" + stack_identity_key(samples[i])});
    }
    uint64_t remaining = targetTotal - assigned;
    std::sort(entries.begin(), entries.end(), [](const Entry &left, const Entry &right) {
        if (left.remainder != right.remainder)
            return left.remainder > right.remainder;
        return left.sortKey < right.sortKey;
    });
    for (size_t i = 0; i < entries.size() && remaining > 0; ++i, --remaining)
        ++entries[i].base;

    std::vector<AggregatedSample> out;
    out.reserve(entries.size());
    for (const auto &entry : entries)
    {
        if (entry.base == 0)
            continue;
        AggregatedSample sample = samples[entry.index];
        sample.count = entry.base;
        out.push_back(std::move(sample));
    }
    return out;
}

WindowPayload SessionFanoutProjector::Project(const WindowPayload &physical,
                                              const SessionContract &contract,
                                              int physicalSampleRateHz,
                                              bool histogramAttributionSafe) const
{
    WindowPayload out = physical;
    // 逻辑窗口 ID / 内容摘要在 fan-out 后由调用方按 Session 内容重算。
    out.windowID.clear();
    out.contentSHA256.clear();
    out.physicalSampleRateHz = physicalSampleRateHz;
    out.effectiveSampleRateHz = physicalSampleRateHz;
    out.identityUnavailableCount = 0;
    out.signalStatuses.clear();

    const bool cpuRequested = logical_signal_requested(contract.signals, "cpu_profile");
    const bool ioRequested = logical_signal_requested(contract.signals, "io_latency");
    const bool ioSyscallRequested = logical_signal_requested(contract.signals, "io_syscall_latency");
    const bool schedRequested = logical_signal_requested(contract.signals, "sched_latency");
    const bool rssRequested = logical_signal_requested(contract.signals, "python_rss");
    const bool memrayRequested = logical_signal_requested(contract.signals, "python_memory");
    const bool dbRequested = logical_signal_requested(contract.signals, "db_snapshot");
    const bool isProcess = contract.scope == "process";

    // 1. samples：CPU 信号过滤 + process 身份精确过滤（身份缺失/PID 复用 →
    // 丢弃并记录 identity_unavailable，不得猜测归属）。
    if (!cpuRequested)
    {
        out.samples.clear();
    }
    else if (isProcess)
    {
        std::vector<AggregatedSample> kept;
        kept.reserve(out.samples.size());
        for (const auto &sample : out.samples)
        {
            ProcessIdentity identity{sample.pid, sample.processStartMs, sample.exe, sample.comm};
            if (identity_targeted(contract.targets, identity))
                kept.push_back(sample);
            else if (!identity.complete() || std::any_of(contract.targets.begin(), contract.targets.end(),
                                                         [&](const auto &target) { return target.pid == sample.pid; }))
                out.identityUnavailableCount = add_count(out.identityUnavailableCount, sample.count);
        }
        out.samples = std::move(kept);
    }

    // 2. profiles：按各自 signal_type 过滤（cpu_profile 与 python_memory 独立
    // 判定，不能因 CPU 未启用而整体清空 Memray）。
    std::vector<ProfilePayload> keptProfiles;
    for (auto &profile : out.profiles)
    {
        const std::string signalType = profile.signalType.empty() ? "cpu_profile" : profile.signalType;
        if (signalType == "cpu_profile" && !cpuRequested)
            continue;
        if (signalType == "python_memory" && !memrayRequested)
            continue;
        if (isProcess)
        {
            std::vector<AggregatedSample> kept;
            for (const auto &sample : profile.samples)
            {
                ProcessIdentity identity{sample.pid, sample.processStartMs, sample.exe, sample.comm};
                if (identity_targeted(contract.targets, identity))
                    kept.push_back(sample);
            }
            profile.samples = std::move(kept);
            if (profile.samples.empty())
                continue;
        }
        keptProfiles.push_back(std::move(profile));
    }
    out.profiles = std::move(keptProfiles);

    // 3. metrics（python_rss）：按信号 + 身份过滤。
    if (!rssRequested)
    {
        out.metrics.clear();
    }
    else if (isProcess)
    {
        std::vector<MetricPayload> kept;
        for (const auto &metric : out.metrics)
        {
            ProcessIdentity identity{metric.pid, metric.processStartMs, metric.exe, metric.comm};
            if (identity_targeted(contract.targets, identity))
                kept.push_back(metric);
        }
        out.metrics = std::move(kept);
    }

    // 4. dbSnapshots：按信号过滤（每 Session 独立 DB sampler，物理窗口不含
    // 其他 Session 的 db 数据，这里只做信号级剔除）。
    if (!dbRequested)
        out.dbSnapshots.clear();

    // 5. histograms：按信号 + 身份过滤。pid=0 的整机直方图（degraded
    // bpftrace 无法按实例归属）不得进入 process Session——无法验证 start
    // time 的 histogram 不得进入 process Session，只登记 unavailable。
    std::vector<HistogramPayload> keptHistograms;
    for (const auto &hist : out.histograms)
    {
        const std::string &signal = hist.signalType;
        if (signal == "io_latency" && !ioRequested)
            continue;
        if (signal == "io_syscall_latency" && !ioSyscallRequested)
            continue;
        if (signal == "sched_latency" && !schedRequested)
            continue;
        if (isProcess)
        {
            if (hist.pid > 0)
            {
                ProcessIdentity identity{hist.pid, hist.processStartMs, hist.exe, hist.comm};
                if (!identity_targeted(contract.targets, identity))
                    continue;
            }
            else
            {
                // 整机直方图无法安全归属到单个 selector，不得复制。
                continue;
            }
        }
        keptHistograms.push_back(hist);
    }
    out.histograms = std::move(keptHistograms);

    // 6. 无法安全归属的直方图（多进程 + 滚动 bpftrace fallback）：只登记
    // unavailable 状态窗口，不得复制整机直方图。
    if (!histogramAttributionSafe)
    {
        out.histograms.clear();
        if (ioRequested)
            out.histograms.push_back(unavailable_shared_histogram_payload("io_latency"));
        if (ioSyscallRequested)
            out.histograms.push_back(unavailable_shared_histogram_payload("io_syscall_latency"));
        if (schedRequested)
            out.histograms.push_back(unavailable_shared_histogram_payload("sched_latency"));
    }

    // 7. 确定性降采样：低频 Session 按 requested/physical 比例降采样。
    if (cpuRequested && physicalSampleRateHz > 0 && contract.requestedSampleRateHz > 0 &&
        contract.requestedSampleRateHz < physicalSampleRateHz)
    {
        const std::string stabilityKey = physical.collectorGeneration + "|" +
                                         std::to_string(physical.startMs) + "|" +
                                         std::to_string(physical.endMs) + "|" + contract.sid;
        out.samples = DownsampleDeterministic(out.samples,
                                              static_cast<uint64_t>(contract.requestedSampleRateHz),
                                              static_cast<uint64_t>(physicalSampleRateHz),
                                              stabilityKey);
        for (auto &profile : out.profiles)
        {
            const std::string signalType = profile.signalType.empty() ? "cpu_profile" : profile.signalType;
            if (signalType != "cpu_profile")
                continue;
            profile.samples = DownsampleDeterministic(
                profile.samples, static_cast<uint64_t>(contract.requestedSampleRateHz),
                static_cast<uint64_t>(physicalSampleRateHz),
                stabilityKey + "|profile|" + profile.profileID + "|" + profile.backend);
        }
        out.effectiveSampleRateHz = contract.requestedSampleRateHz;
    }

    // 8. 重建 symbol_refs（process Session 按过滤后样本重算；host 复用物理）。
    if (isProcess)
    {
        if (physical.physicalDiagnostics)
        {
            ContinuousSamplerConfig config;
            config.scope = contract.scope;
            config.targetProcesses = contract.targets;
            config.requestedSignals = contract.signals;
            out.symbolRefsJson = build_session_symbol_refs(*physical.physicalDiagnostics,
                                                           out.samples, config);
        }
        else if (!out.symbolRefsJson.empty())
            out.symbolRefsJson.clear();
    }

    // 9. 每 signal 状态登记：零计数窗口也保留状态，使 coverage 能区分
    // idle/no-events、backend unavailable 和真实 gap。
    auto registerStatus = [&](const std::string &signal, SignalCollectionStatus status,
                              const std::string &reason = "", uint64_t lost = 0) {
        SignalStatus st;
        st.status = status;
        st.reason = reason;
        st.lostEvents = lost;
        out.signalStatuses[signal] = st;
    };
    if (cpuRequested)
    {
        const bool hasCPUProfile = std::any_of(out.profiles.begin(), out.profiles.end(), [](const auto &profile) {
            return (profile.signalType.empty() || profile.signalType == "cpu_profile") &&
                   !profile.samples.empty();
        });
        const bool physicalHasCPUProfile = std::any_of(physical.profiles.begin(), physical.profiles.end(), [](const auto &profile) {
            return (profile.signalType.empty() || profile.signalType == "cpu_profile") &&
                   !profile.samples.empty();
        });
        if (!out.samples.empty() || hasCPUProfile)
            registerStatus("cpu_profile", SignalCollectionStatus::Collected);
        else if (!physical.samples.empty() || physicalHasCPUProfile)
            registerStatus("cpu_profile", SignalCollectionStatus::TargetIdle,
                           isProcess ? "target idle or identity unavailable" : "target idle");
        else if (physical.backendStatus == "failed")
            registerStatus("cpu_profile", SignalCollectionStatus::Failed, physical.backendReason);
        else
            registerStatus("cpu_profile", SignalCollectionStatus::NoEvents);
    }
    auto registerHistogramStatus = [&](const std::string &signal, bool requested) {
        if (!requested)
            return;
        bool sawAvailable = false;
        bool sawUnavailable = false;
        std::string reason;
        for (const auto &hist : out.histograms)
        {
            if (hist.signalType != signal)
                continue;
            if (hist.unavailable)
            {
                sawUnavailable = true;
                if (reason.empty())
                    reason = hist.reason;
            }
            else
            {
                sawAvailable = true;
            }
        }
        if (sawAvailable)
            registerStatus(signal, SignalCollectionStatus::Collected);
        else if (sawUnavailable)
            registerStatus(signal, SignalCollectionStatus::Unavailable, reason);
        else if (isProcess && physical_has_histogram(physical, signal))
            // 物理层有该信号直方图但被身份过滤剔除（pid=0 整机直方图无法
            // 归属）→ unavailable，不得伪装成 no_events。
            registerStatus(signal, SignalCollectionStatus::Unavailable,
                           "histogram cannot be attributed to this process instance");
        else
            registerStatus(signal, SignalCollectionStatus::NoEvents);
    };
    registerHistogramStatus("io_latency", ioRequested);
    registerHistogramStatus("io_syscall_latency", ioSyscallRequested);
    registerHistogramStatus("sched_latency", schedRequested);
    if (rssRequested)
        registerStatus("python_rss", out.metrics.empty() ? SignalCollectionStatus::NoEvents
                                                         : SignalCollectionStatus::Collected);
    if (memrayRequested)
    {
        bool sawMemray = false;
        for (const auto &profile : out.profiles)
            if (profile.signalType == "python_memory" && !profile.samples.empty())
            {
                sawMemray = true;
                break;
            }
        registerStatus("python_memory", sawMemray ? SignalCollectionStatus::Collected
                                                  : SignalCollectionStatus::NoEvents);
    }
    if (dbRequested)
        registerStatus("db_snapshot", out.dbSnapshots.empty() ? SignalCollectionStatus::NoEvents
                                                              : SignalCollectionStatus::Collected);
    return out;
}

} // namespace drop
