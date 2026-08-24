// ============================================================
// common/LanguageStatus.cpp — 阶段 4 统一语言诊断契约实现
// ============================================================

#include "common/LanguageStatus.h"

#include <algorithm>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <functional>
#include <set>

namespace drop
{

namespace
{

bool text_starts(const std::string &s, const char *prefix)
{
    return s.rfind(prefix, 0) == 0;
}

// py-spy: "<fn> (file.py:12)"；CPython -X perf: "py::fn:/p/m.py+0x1a"
bool text_is_python_frame(const std::string &frame)
{
    if (frame.empty() || unresolved_frame(frame))
        return false;
    if (text_starts(frame, "py::"))
        return true;
    const size_t open = frame.rfind(" (");
    if (open != std::string::npos && !frame.empty() && frame.back() == ')')
    {
        const std::string location = frame.substr(open + 2, frame.size() - open - 3);
        if (location.find(".py:") != std::string::npos ||
            (location.size() >= 3 && location.compare(location.size() - 3, 3, ".py") == 0))
            return true;
    }
    return false;
}

// V8 --perf-basic-prof: "LazyCompile:*fn /p/f.js:1:23"、"fn ~f.js"
bool text_is_js_frame(const std::string &frame)
{
    if (frame.empty())
        return false;
    if (text_starts(frame, "LazyCompile:") || text_starts(frame, "Script:") ||
        text_starts(frame, "Builtin:"))
        return true;
    auto endsWith = [&frame](const char *suffix) {
        const size_t len = std::strlen(suffix);
        return frame.size() >= len && frame.compare(frame.size() - len, len, suffix) == 0;
    };
    return frame.find(".js:") != std::string::npos || frame.find(".js ") != std::string::npos ||
           endsWith(".js") || frame.find(".mjs:") != std::string::npos || endsWith(".mjs");
}

// JVM 方法名：com.foo.Bar.baz / java/lang/Thread.run / lambda$main$0
bool text_is_java_frame(const std::string &frame)
{
    if (frame.empty() || unresolved_frame(frame))
        return false;
    if (text_is_python_frame(frame) || text_is_js_frame(frame))
        return false;
    if (text_starts(frame, "__") || text_starts(frame, "_ZN"))
        return false; // libc/libstdc++ 内部符号不是 Java 方法
    for (char c : frame)
    {
        if (c == '.' || c == '/' || c == '$')
            return true;
    }
    return false;
}

// Go 符号："pkg/path.Func"、"runtime.schedule"、"main.worker"。
// C++ 符号使用 "::"，与 Go 点分形式区分。
bool text_is_go_frame(const std::string &frame)
{
    if (frame.empty() || unresolved_frame(frame))
        return false;
    if (text_starts(frame, "__") || text_starts(frame, "_ZN"))
        return false;
    const size_t dot = frame.find('.');
    if (dot == std::string::npos || dot == 0 || dot + 1 >= frame.size())
        return false;
    return frame.find("::") == std::string::npos;
}

struct FrameCounts
{
    uint64_t total = 0;      // 非内核帧权重
    uint64_t unresolved = 0; // 未解析帧权重（非内核）
    uint64_t semantic = 0;   // 本语言语义帧权重
};

FrameCounts accumulate_sample_frames(
    const AggregatedSample &sample,
    const std::function<bool(const ContinuousStackFrame &, const std::string &)> &isSemantic,
    const std::vector<ContinuousStackFrame> *frames)
{
    FrameCounts counts;
    size_t index = 0;
    for (const auto &frameText : sample.stack)
    {
        const ContinuousStackFrame structured =
            frames != nullptr && index < frames->size() ? (*frames)[index] : ContinuousStackFrame{};
        ++index;
        const uint64_t weight = clamp_count(sample.count);
        if (is_kernel_frame(structured) || is_kernel_frame_text(frameText))
            continue; // 先排除内核帧再计算语言语义覆盖率
        counts.total = add_count(counts.total, weight);
        if (unresolved_frame(frameText))
        {
            counts.unresolved = add_count(counts.unresolved, weight);
            continue;
        }
        if (isSemantic(structured, frameText))
            counts.semantic = add_count(counts.semantic, weight);
    }
    return counts;
}

double percent(uint64_t part, uint64_t total)
{
    if (total == 0)
        return 0.0;
    return std::round(static_cast<double>(part) * 10000.0 / static_cast<double>(total)) / 100.0;
}

std::string format_percent(double value)
{
    char buf[32];
    std::snprintf(buf, sizeof(buf), "%.2f", value);
    return buf;
}

const RuntimeMapInfo *runtime_info_for(const std::string &runtime, const RuntimeSymbolReport &report)
{
    if (runtime == "java")
        return &report.java;
    if (runtime == "node")
        return &report.node;
    if (runtime == "python")
        return &report.python;
    return nullptr;
}

void append_reason(std::vector<std::string> *reasons, const std::string &reason)
{
    if (reason.empty())
        return;
    if (std::find(reasons->begin(), reasons->end(), reason) != reasons->end())
        return;
    reasons->push_back(reason);
}

// collector_status 聚合（契约口径，见头文件）。
std::string aggregate_collector_status(bool detected,
                                       bool anyReadyProcess,
                                       bool anyMissingProcess,
                                       bool anyFailedProcess,
                                       bool anyPending,
                                       bool semanticThresholdMet,
                                       bool unresolvedWithinLimit)
{
    if (!detected)
        return "not_applicable";
    if (anyFailedProcess && !anyReadyProcess)
        return "failed";
    if (anyPending && !anyReadyProcess)
        return "pending";
    if (anyReadyProcess && !anyMissingProcess && !anyFailedProcess && semanticThresholdMet &&
        unresolvedWithinLimit)
        return "ready";
    if (anyReadyProcess)
        return "partial";
    return "missing";
}

std::string symbol_status_from_weights(uint64_t total, uint64_t unresolved)
{
    if (total == 0)
        return "unknown";
    if (unresolved == 0)
        return "complete";
    if (percent(unresolved, total) <= 5.0 + 1e-9)
        return "complete";
    if (unresolved >= total)
        return "missing";
    return "partial";
}

} // namespace

bool is_kernel_frame(const ContinuousStackFrame &frame)
{
    if (!frame.mappingFile.empty())
        return frame.mappingFile.front() == '[';
    return false;
}

bool is_kernel_frame_text(const std::string &frameText)
{
    return text_starts(drop::trim(frameText), "[kernel");
}

bool is_python_semantic_frame(const ContinuousStackFrame &frame)
{
    if (!frame.function.empty() && text_starts(frame.function, "py::"))
        return true;
    return is_python_semantic_frame_text(frame.raw);
}

bool is_python_semantic_frame_text(const std::string &frameText)
{
    return text_is_python_frame(frameText);
}

bool is_js_semantic_frame_text(const std::string &frameText)
{
    return text_is_js_frame(frameText);
}

bool is_java_semantic_frame_text(const std::string &frameText)
{
    return text_is_java_frame(frameText);
}

bool is_go_semantic_frame_text(const std::string &frameText)
{
    return text_is_go_frame(frameText);
}

SampleFrameWeights sample_frame_weights(const AggregatedSample &sample)
{
    SampleFrameWeights weights;
    size_t index = 0;
    for (const auto &frameText : sample.stack)
    {
        const ContinuousStackFrame structured =
            index < sample.frames.size() ? sample.frames[index] : ContinuousStackFrame{};
        ++index;
        const uint64_t weight = clamp_count(sample.count);
        weights.total = add_count(weights.total, weight);
        if (is_kernel_frame(structured) || is_kernel_frame_text(frameText))
        {
            weights.kernel = add_count(weights.kernel, weight);
            continue;
        }
        if (unresolved_frame(frameText))
            weights.unresolved = add_count(weights.unresolved, weight);
    }
    return weights;
}

LanguageStatusReport build_language_status(const std::vector<AggregatedSample> &samples,
                                           const PhysicalDiagnostics &diagnostics,
                                           const std::vector<ContinuousTargetProcess> *targets,
                                           const std::string &unwindMode)
{
    LanguageStatusReport report;

    auto identityMatched = [&](int pid, int64_t startMs, const std::string &exe) {
        if (targets == nullptr)
            return true;
        ProcessIdentity identity{pid, startMs, exe, ""};
        return identity_targeted(*targets, identity);
    };
    auto liveIdentityMatched = [&](int pid) {
        if (targets == nullptr)
            return true;
        int64_t startMs = 0;
        drop::python_process_start_ms(pid, &startMs);
        return identityMatched(pid, startMs, read_exe(pid));
    };

    // ---- 按语言聚合帧统计与样本权重 ----
    struct LangStats
    {
        FrameCounts counts;
        uint64_t sampleWeight = 0;
    };
    std::map<std::string, LangStats> stats;
    const std::vector<std::string> kLanguages = {"go", "java", "node", "python", "native", "kernel"};
    for (const auto &lang : kLanguages)
        stats[lang]; // 稳定顺序

    auto semanticPredicate = [](const std::string &lang)
        -> std::function<bool(const ContinuousStackFrame &, const std::string &)> {
        if (lang == "python")
        {
            return [](const ContinuousStackFrame &structured, const std::string &text) {
                return is_python_semantic_frame(structured) || is_python_semantic_frame_text(text);
            };
        }
        if (lang == "node")
        {
            return [](const ContinuousStackFrame &, const std::string &text) {
                return is_js_semantic_frame_text(text);
            };
        }
        if (lang == "java")
        {
            return [](const ContinuousStackFrame &, const std::string &text) {
                return is_java_semantic_frame_text(text);
            };
        }
        if (lang == "go")
        {
            return [](const ContinuousStackFrame &, const std::string &text) {
                return is_go_semantic_frame_text(text);
            };
        }
        // native/kernel：排除内核后的已解析帧即语义帧。
        return [](const ContinuousStackFrame &, const std::string &) { return true; };
    };

    for (const auto &sample : samples)
    {
        std::string runtime = sample.runtime.empty() ? "unknown" : sample.runtime;
        if (stats.find(runtime) == stats.end())
            runtime = "native"; // 历史/未知 runtime 归并 native 统计
        LangStats &entry = stats[runtime];
        entry.sampleWeight = add_count(entry.sampleWeight, sample.count);
        const std::vector<ContinuousStackFrame> *frames =
            sample.frames.size() == sample.stack.size() ? &sample.frames : nullptr;
        FrameCounts counts = accumulate_sample_frames(sample, semanticPredicate(runtime), frames);
        entry.counts.total = add_count(entry.counts.total, counts.total);
        entry.counts.unresolved = add_count(entry.counts.unresolved, counts.unresolved);
        entry.counts.semantic = add_count(entry.counts.semantic, counts.semantic);
    }

    auto makeEntry = [&](const std::string &lang) -> LanguageStatusEntry & {
        LanguageStatusEntry empty;
        return report.languages.emplace(lang, empty).first->second;
    };

    // ---- Native 行 ----
    {
        LanguageStatusEntry &entry = makeEntry("native");
        entry.runtime = "native";
        const LangStats &lang = stats["native"];
        const bool detected = lang.sampleWeight > 0;
        entry.runtimeDetection = detected ? "detected" : "not_detected";
        entry.collectorModes.push_back(unwindMode == "dwarf" ? "perf-dwarf" : "perf-fp");
        entry.sampleCount = lang.sampleWeight;
        entry.semanticFramePercent = percent(lang.counts.semantic, lang.counts.total);
        entry.unresolvedFramePercent = percent(lang.counts.unresolved, lang.counts.total);
        entry.symbolStatus = symbol_status_from_weights(lang.counts.total, lang.counts.unresolved);
        entry.collectorStatus = aggregate_collector_status(
            detected, detected, false, false, false,
            entry.semanticFramePercent >= 70.0, entry.unresolvedFramePercent <= 20.0);
        if (detected && entry.collectorStatus == "missing" && entry.unresolvedFramePercent > 20.0)
            append_reason(&entry.reasons,
                          unwindMode == "dwarf"
                              ? "high unresolved ratio under DWARF unwinding"
                              : "target binary may lack frame pointers; keep -fno-omit-frame-pointer or switch DROP_NATIVE_CP_CALL_GRAPH=dwarf");
    }

    // ---- Go 行 ----
    {
        LanguageStatusEntry &entry = makeEntry("go");
        entry.runtime = "go";
        const LangStats &lang = stats["go"];
        const drop::GoSymbolReport &go = diagnostics.goReport;
        const bool detected = lang.sampleWeight > 0 || !go.ready.empty() || !go.pending.empty() ||
                              !go.failed.empty();
        entry.runtimeDetection = detected ? "detected" : "not_detected";
        if (!go.ready.empty())
            entry.collectorModes.push_back("goresym");
        entry.sampleCount = lang.sampleWeight;
        entry.semanticFramePercent = percent(lang.counts.semantic, lang.counts.total);
        entry.unresolvedFramePercent = percent(lang.counts.unresolved, lang.counts.total);
        entry.symbolStatus = symbol_status_from_weights(lang.counts.total, lang.counts.unresolved);

        bool anyReady = !go.ready.empty();
        bool anyPending = !go.pending.empty();
        bool anyFailed = false;
        for (const auto &item : go.failed)
        {
            // 开关关闭产生的伪失败不算确定性失败 → 归入 missing 而非 failed。
            if (item.reason == "DROP_CONTINUOUS_GORESYM disabled")
                continue;
            anyFailed = true;
            append_reason(&entry.reasons,
                          item.reason.empty() ? "GoReSym extraction failed" : item.reason);
        }
        if (!go.disabled.empty())
            append_reason(&entry.reasons, go.disabled);
        if (anyPending)
            append_reason(&entry.reasons, "GoReSym background extraction in progress");
        entry.collectorStatus = aggregate_collector_status(
            detected, anyReady, false, anyFailed, anyPending,
            entry.semanticFramePercent >= 70.0, entry.unresolvedFramePercent <= 20.0);
        if (detected && lang.counts.total > 0 && lang.counts.semantic == 0)
            append_reason(&entry.reasons, "no Go function names resolved from samples");
    }

    // ---- Java / Node / Python 行（runtime map + sidecar 驱动）----
    struct MapLangDef
    {
        const char *name;
        const char *mode;
    };
    for (const auto &def : {MapLangDef{"java", "perf-map"},
                            MapLangDef{"node", "perf-map"},
                            MapLangDef{"python", "perf-map"}})
    {
        LanguageStatusEntry &entry = makeEntry(def.name);
        entry.runtime = def.name;
        const LangStats &lang = stats[def.name];
        const RuntimeMapInfo *info = runtime_info_for(def.name, diagnostics.runtimeReport);
        const bool detected = (info != nullptr && info->detected) || lang.sampleWeight > 0 ||
                              (def.name == std::string("python") &&
                               !diagnostics.pythonFallback.empty());
        entry.runtimeDetection = detected ? "detected" : "not_detected";

        bool anyReadyProc = false, anyMissingProc = false, anyFailedProc = false;
        if (info != nullptr)
        {
            std::set<int> attachFailed(info->failedAttachPids.begin(), info->failedAttachPids.end());
            for (int pid : info->readyPids)
            {
                if (!liveIdentityMatched(pid))
                    continue;
                LanguageProcessStatus proc;
                proc.pid = pid;
                proc.mode = def.mode;
                proc.status = "ready";
                entry.processes.push_back(std::move(proc));
                anyReadyProc = true;
            }
            for (int pid : info->missingPids)
            {
                if (!liveIdentityMatched(pid))
                    continue;
                LanguageProcessStatus proc;
                proc.pid = pid;
                proc.mode = def.mode;
                // 阶段四：确定性 attach 失败（权限/退出/超时）标 failed，
                // 其余缺能力进程标 missing。
                if (attachFailed.count(pid) > 0)
                {
                    proc.status = "failed";
                    proc.reason = info->reason;
                    anyFailedProc = true;
                }
                else
                {
                    proc.status = "missing";
                    proc.reason = info->reason;
                    anyMissingProc = true;
                }
                entry.processes.push_back(std::move(proc));
            }
        }
        entry.sampleCount = lang.sampleWeight;
        entry.semanticFramePercent = percent(lang.counts.semantic, lang.counts.total);
        entry.unresolvedFramePercent = percent(lang.counts.unresolved, lang.counts.total);
        entry.symbolStatus = symbol_status_from_weights(lang.counts.total, lang.counts.unresolved);

        if (info != nullptr && !info->reason.empty())
            append_reason(&entry.reasons, info->reason);

        // Python 追加 py-spy sidecar 实例与模式。
        if (def.name == std::string("python"))
        {
            for (const auto &result : diagnostics.pythonFallback)
            {
                if (result.pid <= 0)
                    continue;
                if (targets != nullptr &&
                    !identityMatched(result.pid, result.startMs, result.exe))
                    continue;
                LanguageProcessStatus proc;
                proc.pid = result.pid;
                proc.processStartMs = result.startMs;
                proc.comm = result.comm;
                proc.exe = result.exe;
                proc.mode = "py-spy-native";
                proc.status = result.ready ? "ready" : "failed";
                proc.reason = result.ready ? "" : result.reason;
                entry.processes.push_back(std::move(proc));
                if (result.ready)
                {
                    anyReadyProc = true;
                    if (std::find(entry.collectorModes.begin(), entry.collectorModes.end(),
                                  "py-spy-native") == entry.collectorModes.end())
                        entry.collectorModes.push_back("py-spy-native");
                }
                else
                {
                    anyFailedProc = true;
                    append_reason(&entry.reasons, result.reason);
                }
            }
            if (diagnostics.pythonFallbackLimitedCount > 0)
                append_reason(&entry.reasons,
                              "py-spy limited to hottest instances (" +
                                  std::to_string(diagnostics.pythonFallbackLimitedCount) +
                                  " skipped)");
        }

        // perf-map 模式只有在确实拿到 map 时才登记，缺 flag 的 missing 进程
        // 不能把模式伪装成可用。
        if (anyReadyProc &&
            std::find(entry.collectorModes.begin(), entry.collectorModes.end(), def.mode) ==
                entry.collectorModes.end())
            entry.collectorModes.insert(entry.collectorModes.begin(), def.mode);

        entry.collectorStatus = aggregate_collector_status(
            detected, anyReadyProc, anyMissingProc, anyFailedProc, false,
            entry.semanticFramePercent >= 70.0, entry.unresolvedFramePercent <= 20.0);
        if (detected && entry.collectorStatus == "missing")
        {
            if (def.name == std::string("node"))
                append_reason(&entry.reasons, "restart node with --perf-basic-prof to emit JIT map");
            else if (def.name == std::string("python"))
                append_reason(&entry.reasons,
                              "run python 3.12+ with -X perf or rely on py-spy fallback");
        }
    }

    // ---- kernel 行 ----
    {
        LanguageStatusEntry &entry = makeEntry("kernel");
        entry.runtime = "kernel";
        const LangStats &lang = stats["kernel"];
        const bool detected = lang.sampleWeight > 0;
        entry.runtimeDetection = detected ? "detected" : "not_detected";
        entry.collectorModes.push_back("kallsyms");
        entry.sampleCount = lang.sampleWeight;
        entry.semanticFramePercent = percent(lang.counts.semantic, lang.counts.total);
        entry.unresolvedFramePercent = percent(lang.counts.unresolved, lang.counts.total);
        entry.symbolStatus = symbol_status_from_weights(lang.counts.total, lang.counts.unresolved);
        entry.collectorStatus = detected ? "ready" : "not_applicable";
    }

    return report;
}

std::string language_status_to_json(const LanguageStatusReport &report)
{
    std::string body = "{\"diagnostics_version\":" + std::to_string(report.diagnosticsVersion) + ",";
    body += "\"language_status\":{";
    bool firstLang = true;
    for (const auto &[name, entry] : report.languages)
    {
        if (!firstLang)
            body += ",";
        firstLang = false;
        body += "\"" + json_escape(name) + "\":{";
        body += "\"runtime_detection\":\"" + json_escape(entry.runtimeDetection) + "\",";
        body += "\"collector_modes\":[";
        for (size_t i = 0; i < entry.collectorModes.size(); ++i)
        {
            if (i)
                body += ",";
            body += "\"" + json_escape(entry.collectorModes[i]) + "\"";
        }
        body += "],";
        body += "\"collector_status\":\"" + json_escape(entry.collectorStatus) + "\",";
        body += "\"symbol_status\":\"" + json_escape(entry.symbolStatus) + "\",";
        body += "\"semantic_frame_percent\":" + format_percent(entry.semanticFramePercent) + ",";
        body += "\"unresolved_frame_percent\":" + format_percent(entry.unresolvedFramePercent) + ",";
        body += "\"sample_count\":" + std::to_string(entry.sampleCount) + ",";
        body += "\"reasons\":[";
        for (size_t i = 0; i < entry.reasons.size(); ++i)
        {
            if (i)
                body += ",";
            body += "\"" + json_escape(entry.reasons[i]) + "\"";
        }
        body += "],\"processes\":[";
        for (size_t i = 0; i < entry.processes.size(); ++i)
        {
            const auto &proc = entry.processes[i];
            if (i)
                body += ",";
            body += "{\"pid\":" + std::to_string(proc.pid);
            if (proc.processStartMs > 0)
                body += ",\"process_start_ms\":" + std::to_string(proc.processStartMs);
            if (!proc.comm.empty())
                body += ",\"comm\":\"" + json_escape(proc.comm) + "\"";
            if (!proc.exe.empty())
                body += ",\"exe\":\"" + json_escape(proc.exe) + "\"";
            if (!proc.mode.empty())
                body += ",\"mode\":\"" + json_escape(proc.mode) + "\"";
            body += ",\"status\":\"" + json_escape(proc.status) + "\"";
            if (!proc.reason.empty())
                body += ",\"reason\":\"" + json_escape(proc.reason) + "\"";
            body += "}";
        }
        body += "]}";
    }
    body += "}}";
    return body;
}

std::string language_status_fragment_for_symbol_refs(
    const std::vector<AggregatedSample> &samples,
    const drop::RuntimeSymbolReport &runtimeReport,
    const drop::GoSymbolReport &goReport,
    const std::vector<drop::PythonFallbackResult> &pythonFallback,
    size_t pythonLimitedCount,
    const std::vector<ContinuousTargetProcess> *targets,
    const std::string &unwindMode)
{
    PhysicalDiagnostics diagnostics;
    diagnostics.runtimeReport = runtimeReport;
    diagnostics.goReport = goReport;
    diagnostics.pythonFallback = pythonFallback;
    diagnostics.pythonFallbackLimitedCount = pythonLimitedCount;
    diagnostics.unwindMode = unwindMode.empty() ? "fp" : unwindMode;
    LanguageStatusReport report =
        build_language_status(samples, diagnostics, targets, diagnostics.unwindMode);
    // 去掉外层大括号：片段以 "diagnostics_version":...,"language_status":{...}
    // 的成员形式拼进 symbol_refs 根对象。
    const std::string full = language_status_to_json(report);
    if (full.size() >= 2 && full.front() == '{' && full.back() == '}')
        return full.substr(1, full.size() - 2);
    return full;
}

} // namespace drop
