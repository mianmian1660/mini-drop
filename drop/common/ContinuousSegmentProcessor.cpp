// ============================================================
// common/ContinuousSegmentProcessor.cpp — 统一 perf.data 段处理流水线实现
// ============================================================
// 阶段 2：strict（滚动 perf + CO-RE）与 degraded（窗口 perf + bpftrace）都把
// 不可变 perf.data 段交给这里，共享符号准备、解析、runtime 分类与诊断生成。
// ============================================================

#include "common/ContinuousSegmentProcessor.h"

#include <iostream>
#include <set>
#include <sstream>

namespace drop
{

static bool python_result_overlaps_window(const drop::PythonFallbackResult &result,
                                          const WindowPayload &window)
{
    // 旧测试/调用方可能没有时间字段；仅为兼容这种同步、同窗输入回退为 true。
    if (result.captureStartMs <= 0 || result.captureEndMs <= result.captureStartMs)
        return true;
    return result.captureStartMs < window.endMs && result.captureEndMs > window.startMs;
}

// 把物理级 py-spy sidecar 结果合并进 CPU samples（声明见头文件）：
//   - py-spy 就绪：删除该 PID 在 perf 段中的 Python 样本（避免双计数），
//     追加带真实计数的 py-spy sidecar 样本（backend=py-spy）。
//   - py-spy 失败（ready=false，含超时/PID 复用被上游 capture_one 判定）：
//     保留原 perf 样本，fallback 诊断标记失败。
// 注：PID 复用/已退出的判定由上游 capture_one（采集前后各校验一次）与
// degraded collector 完成，这里只做纯合并，保证可单测且不重复读 /proc。
void merge_python_sidecar_samples(std::vector<AggregatedSample> *samples,
                                  const std::vector<drop::PythonFallbackResult> &pythonResults,
                                  bool *anyReplaced)
{
    if (!samples || pythonResults.empty())
        return;
    std::set<int> replacedPythonPids;
    std::vector<AggregatedSample> appended;
    for (const auto &result : pythonResults)
    {
        if (!result.ready)
            continue;
        replacedPythonPids.insert(result.pid);
        for (const auto &raw : result.samples)
        {
            AggregatedSample sample;
            sample.stack = raw.stack;
            sample.comm = result.comm.empty() ? "python" : result.comm;
            sample.pid = result.pid;
            sample.processStartMs = result.startMs;
            sample.exe = result.exe;
            sample.backend = "py-spy";
            sample.runtime = "python";
            sample.count = clamp_count(raw.count);
            appended.push_back(std::move(sample));
        }
    }
    if (replacedPythonPids.empty())
        return;
    samples->erase(std::remove_if(samples->begin(), samples->end(), [&](const auto &sample) {
                       auto result = std::find_if(pythonResults.begin(), pythonResults.end(), [&](const auto &candidate) {
                           return candidate.ready && candidate.pid == sample.pid &&
                                  (candidate.startMs <= 0 || sample.processStartMs <= 0 ||
                                   candidate.startMs == sample.processStartMs);
                       });
                       return result != pythonResults.end();
                   }),
                   samples->end());
    samples->insert(samples->end(), appended.begin(), appended.end());
    if (anyReplaced)
        *anyReplaced = true;
}

void apply_python_sidecars_to_windows(std::vector<WindowPayload> *windows,
                                      const std::vector<drop::PythonFallbackResult> &pythonResults,
                                      size_t pythonLimitedCount)
{
    if (!windows || (pythonResults.empty() && pythonLimitedCount == 0))
        return;
    const size_t perfWindowCount = windows->size();
    std::vector<WindowPayload> sidecars;
    bool limitedAttachedToSidecar = false;
    for (const auto &result : pythonResults)
    {
        if (!result.ready || result.samples.empty())
            continue;
        bool overlapped = false;
        for (auto &window : *windows)
        {
            if (!python_result_overlaps_window(result, window))
                continue;
            overlapped = true;
            window.samples.erase(std::remove_if(window.samples.begin(), window.samples.end(), [&](const auto &sample) {
                                     return sample.pid == result.pid &&
                                            (result.startMs <= 0 || sample.processStartMs <= 0 ||
                                             result.startMs == sample.processStartMs);
                                 }),
                                 window.samples.end());
        }
        if (!overlapped)
            continue;

        WindowPayload sidecar;
        sidecar.startMs = result.captureStartMs;
        sidecar.endMs = std::max(result.captureStartMs + 1, result.captureEndMs);
        sidecar.attemptedBackends = {"py-spy"};
        sidecar.selectedBackend = "py-spy";
        sidecar.backendStatus = "ok";
        for (const auto &raw : result.samples)
        {
            AggregatedSample sample;
            sample.stack = raw.stack;
            sample.comm = result.comm.empty() ? "python" : result.comm;
            sample.pid = result.pid;
            sample.processStartMs = result.startMs;
            sample.exe = result.exe;
            sample.backend = "py-spy";
            sample.runtime = "python";
            sample.count = clamp_count(raw.count);
            sidecar.samples.push_back(std::move(sample));
        }
        PhysicalDiagnostics diagnostics;
        diagnostics.pythonFallback = {result};
        const size_t sidecarLimited = limitedAttachedToSidecar ? 0 : pythonLimitedCount;
        diagnostics.pythonFallbackLimitedCount = sidecarLimited;
        for (const auto &sample : sidecar.samples)
            for (const auto &frame : sample.stack)
            {
                diagnostics.totalFrameWeight = add_count(diagnostics.totalFrameWeight, sample.count);
                if (unresolved_frame(frame))
                    diagnostics.unresolvedFrameWeight = add_count(diagnostics.unresolvedFrameWeight, sample.count);
            }
        diagnostics.symbolStatus = diagnostics.totalFrameWeight == 0 ? "not_applicable"
            : (diagnostics.unresolvedFrameWeight == 0 ? "complete"
               : (diagnostics.unresolvedFrameWeight >= diagnostics.totalFrameWeight ? "missing" : "partial"));
        sidecar.symbolRefsJson = combined_symbol_refs_json(
            {}, {}, {result}, sidecarLimited, {}, sidecar.samples, {}, "");
        sidecar.physicalDiagnostics = std::make_shared<const PhysicalDiagnostics>(diagnostics);
        sidecars.push_back(std::move(sidecar));
        limitedAttachedToSidecar = true;
    }
    // ready/failed/limited 都是诊断事实。把与原 perf window 重叠的结果写回其
    // 结构化诊断；这样失败时保留 perf 样本，但 host/process 查询仍能解释
    // 为什么没有切到 py-spy。
    bool limitedAttachedToPerf = limitedAttachedToSidecar;
    for (size_t index = 0; index < perfWindowCount; ++index)
    {
        auto &window = (*windows)[index];
        std::vector<drop::PythonFallbackResult> relevant;
        for (const auto &result : pythonResults)
            // 成功结果由独立 sidecar window 表达；perf window 只承载失败诊断。
            if (!result.ready && python_result_overlaps_window(result, window))
                relevant.push_back(result);
        const size_t windowLimited = limitedAttachedToPerf ? 0 : pythonLimitedCount;
        if (relevant.empty() && windowLimited == 0)
            continue;
        PhysicalDiagnostics diagnostics = window.physicalDiagnostics
                                              ? *window.physicalDiagnostics
                                              : PhysicalDiagnostics{};
        diagnostics.pythonFallback = relevant;
        diagnostics.pythonFallbackLimitedCount = windowLimited;
        window.symbolRefsJson = combined_symbol_refs_json(
            diagnostics.runtimeReport, diagnostics.goReport, relevant,
            windowLimited, diagnostics.memrayResults, window.samples,
            diagnostics.buildIdEntries, diagnostics.kallsymsSha256);
        window.physicalDiagnostics =
            std::make_shared<const PhysicalDiagnostics>(std::move(diagnostics));
        limitedAttachedToPerf = true;
    }
    windows->insert(windows->end(), std::make_move_iterator(sidecars.begin()),
                    std::make_move_iterator(sidecars.end()));
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
    const bool runtimeEnrichment = env_enabled_default("DROP_CONTINUOUS_RUNTIME_ENRICHMENT", true);

    // 2. warm_build_id_cache（perf script 自带缓存回退命中所需，始终执行）
    std::vector<drop::BuildIdEntry> buildIds = warm_build_id_cache(perf, dataPath);

    // 3-5. runtime maps / Go symbols（enrichment 关闭时跳过，仍走统一解析器）
    drop::RuntimeSymbolReport runtimeReport;
    drop::GoSymbolReport goReport;
    if (runtimeEnrichment)
    {
        runtimeReport = drop::collect_runtime_maps(perf, dataPath);
        std::set<std::string> knownDsoPaths;
        for (const auto &entry : buildIds)
            knownDsoPaths.insert(entry.dsoPath);
        if (capabilities.goSymbols)
        {
            for (auto &entry : drop::discover_sampled_go_build_ids(runtimeReport.sampledPids))
                if (knownDsoPaths.insert(entry.dsoPath).second)
                    buildIds.push_back(std::move(entry));
            goReport = drop::prepare_go_symbols(buildIds);
        }
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
                                              pythonResults, pythonLimitedCount, memrayResults);
    if (!pure.success)
        return pure;
    pure.diagnostics.enrichmentApplied = runtimeEnrichment;
    pure.diagnostics.runtimeEnrichmentDisabled = !runtimeEnrichment;
    if (!runtimeEnrichment)
    {
        pure.diagnostics.enrichmentDisabledReason = "DROP_CONTINUOUS_RUNTIME_ENRICHMENT=0";
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
    const std::vector<drop::MemrayProfileResult> &memrayResults)
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

    // degraded 同步捕获与 perf record 覆盖同一窗口；只合并真实重叠结果。
    bool pythonReplaced = false;
    std::vector<drop::PythonFallbackResult> overlappingPython;
    for (const auto &item : pythonResults)
        if (python_result_overlaps_window(item, window))
            overlappingPython.push_back(item);
    merge_python_sidecar_samples(&window.samples, overlappingPython, &pythonReplaced);

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
    result.diagnostics.pythonFallback = overlappingPython;
    result.diagnostics.pythonFallbackLimitedCount = pythonLimitedCount;
    result.diagnostics.memrayResults = memrayResults;
    result.diagnostics.kallsymsSha256 = ensure_kallsyms_snapshot(physicalConfig);

    // 11. 结构化物理 symbol_refs（含 py-spy/Memray sidecar 诊断）
    window.symbolRefsJson = combined_symbol_refs_json(
        runtimeReport, goReport, overlappingPython, pythonLimitedCount, memrayResults,
        window.samples, buildIds, result.diagnostics.kallsymsSha256);
    window.physicalDiagnostics = std::make_shared<const PhysicalDiagnostics>(result.diagnostics);
    result.windows.push_back(std::move(window));

    result.success = true;
    return result;
}

// 阶段二：Session 分流后重建 symbol_refs。host Session 复用完整物理诊断；
// process Session 按本 Session 目标 PID 过滤，不泄漏其他 selector 的 PID/路径。
static drop::RuntimeSymbolReport filter_runtime_report_to_session(const drop::RuntimeSymbolReport &report,
                                                                  const ContinuousSamplerConfig &session)
{
    drop::RuntimeSymbolReport out;
    if (session.scope != "process")
        return report;
    auto keep = [&](const drop::RuntimeMapInfo &info) {
        drop::RuntimeMapInfo filtered;
        filtered.detected = false;
        filtered.ready = false;
        filtered.requiredFlag = info.requiredFlag;
        for (int pid : info.readyPids)
            if (process_targeted(session, pid, ""))
                filtered.readyPids.push_back(pid);
        for (int pid : info.missingPids)
            if (process_targeted(session, pid, ""))
                filtered.missingPids.push_back(pid);
        filtered.detected = !filtered.readyPids.empty() || !filtered.missingPids.empty();
        filtered.ready = filtered.detected && !filtered.readyPids.empty() && filtered.missingPids.empty();
        // 当前 reason 不含路径/PID，但只在本 Session 确实检测到该 runtime 时保留。
        if (filtered.detected)
            filtered.reason = info.reason;
        return filtered;
    };
    out.java = keep(report.java);
    out.node = keep(report.node);
    out.python = keep(report.python);
    out.skippedRefresh = 0; // 物理预算统计无法安全归属到单个 selector
    for (const auto &entry : report.sampledPids)
        if (process_targeted(session, entry.first, ""))
            out.sampledPids.emplace(entry);
    out.status = drop::runtime_aggregate_status(out);
    return out;
}

std::string rebuild_filtered_symbol_refs(const std::string &physicalJson,
                                         const PhysicalDiagnostics &diagnostics,
                                         const std::vector<AggregatedSample> &filteredSamples,
                                         const ContinuousSamplerConfig &session)
{
    if (session.scope != "process")
        return physicalJson;

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
    body += "\"runtime_maps\":" + drop::runtime_maps_to_json(
                                      filter_runtime_report_to_session(diagnostics.runtimeReport, session)) + ",";
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

    std::vector<drop::PythonFallbackResult> filteredPython;
    for (const auto &item : diagnostics.pythonFallback)
        if (process_targeted(session, item.pid, item.startMs, item.exe))
            filteredPython.push_back(item);
    body += "\"python_fallback\":" +
            python_fallback_json(filteredPython, filteredPython.empty() ? 0 : diagnostics.pythonFallbackLimitedCount) + ",";

    std::vector<drop::MemrayProfileResult> filteredMemray;
    for (const auto &item : diagnostics.memrayResults)
        if (process_targeted(session, item.pid, 0, item.exe))
            filteredMemray.push_back(item);
    body += "\"python_memory\":{\"ready\":[";
    bool firstMemrayReady = true;
    for (const auto &item : filteredMemray)
    {
        if (!item.ready)
            continue;
        if (!firstMemrayReady)
            body += ",";
        firstMemrayReady = false;
        body += "{\"pid\":" + std::to_string(item.pid) + ",\"profile_id\":\"" +
                json_escape(item.profileID) + "\"}";
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
        body += "{\"pid\":" + std::to_string(item.pid) + ",\"reason\":\"" +
                json_escape(item.reason) + "\"}";
    }
    body += "]}";
    if (diagnostics.runtimeEnrichmentDisabled)
        body += ",\"runtime_enrichment\":{\"enabled\":false,\"reason\":\"" +
                json_escape(diagnostics.enrichmentDisabledReason) + "\"}";
    body += "}";
    return body;
}

} // namespace drop
