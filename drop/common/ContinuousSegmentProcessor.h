// ============================================================
// common/ContinuousSegmentProcessor.h — 统一 strict/degraded 采集流水线
// ============================================================
// 阶段 2：把 perf CPU 原始段之后的处理链路统一到一个 ContinuousSegmentProcessor。
//   - strict 继续用滚动 perf + CO-RE；degraded 继续用窗口 perf + bpftrace。
//   - 两种模式都把不可变 perf.data 段交给同一个 processor，共享符号准备、解析、
//     runtime 分类、Python fallback 与诊断生成。
//   - 本文件承载共享的 payload 类型与处理辅助函数（inline drop::），让
//     ContinuousSampler.cpp（strict/degraded 引擎）与测试使用同一套解析实现，
//     避免"复制另一套解析实现"。
//   - 新增的 PerfSegment / SegmentProcessResult / PhysicalDiagnostics /
//     ContinuousSegmentProcessor 均为 Agent 内部 C++ 类型，不构成对外 API。
// ============================================================

#pragma once

#include "common/ContinuousSampler.h"
#include "common/BuildId.h"
#include "common/GoSymbolizer.h"
#include "common/PythonRuntimeProfiler.h"
#include "common/MemrayProfileIngest.h"
#include "common/RuntimeSymbolMap.h"
#include "common/SymbolCollector.h"
#include "common/KernelSymbols.h"
#include "common/Utils.h"

#include <algorithm>
#include <cctype>
#include <cerrno>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <fstream>
#include <map>
#include <memory>
#include <mutex>
#include <set>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <unistd.h>
#include <unordered_map>
#include <vector>

namespace drop
{

// 共享计数上限：数据库列 / 序列化用 63 位有符号上限。
inline constexpr uint64_t kMaxDBCount = (1ULL << 63) - 1;

inline uint64_t clamp_count(uint64_t value)
{
    return value > kMaxDBCount ? kMaxDBCount : value;
}

inline uint64_t add_count(uint64_t total, uint64_t value)
{
    value = clamp_count(value);
    if (total >= kMaxDBCount || value >= kMaxDBCount)
        return kMaxDBCount;
    if (total > kMaxDBCount - value)
        return kMaxDBCount;
    return total + value;
}

inline std::string json_escape(const std::string &s)
{
    std::string out;
    out.reserve(s.size());
    for (char c : s)
    {
        switch (c)
        {
        case '"':
            out += "\\\"";
            break;
        case '\\':
            out += "\\\\";
            break;
        case '\n':
            out += "\\n";
            break;
        case '\r':
            break;
        default:
            out += c;
        }
    }
    return out;
}

inline bool env_enabled_default(const char *name, bool fallback)
{
    const char *value = std::getenv(name);
    if (!value || !*value)
        return fallback;
    std::string text(value);
    std::transform(text.begin(), text.end(), text.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return text == "1" || text == "true" || text == "yes" || text == "on";
}

inline int env_positive_int(const char *name, int fallback)
{
    const char *value = std::getenv(name);
    if (!value || !*value)
        return fallback;
    int parsed = std::atoi(value);
    return parsed > 0 ? parsed : fallback;
}

// ============================================================
// 共享 payload 类型（strict/degraded/序列化/fan-out 共用）
// ============================================================

struct AggregatedSample
{
    std::vector<std::string> stack;
    std::vector<drop::ContinuousStackFrame> frames; // 阶段五：结构化栈（与 stack 并行，可能为空）
    std::string comm;
    int pid = 0;
    int64_t processStartMs = 0;
    std::string exe;
    std::string stackScope;
    std::string backend;
    std::string runtime;
    uint64_t count = 0;
};

// `perf script -F ...time...` prints the sample clock in seconds. Keep the
// bounds alongside the parsed stacks so rolling files can retain the actual
// capture interval instead of the (much later) parser wall-clock interval.
struct PerfScriptParseResult
{
    std::vector<AggregatedSample> samples;
    double startTimestampSec = 0.0;
    double endTimestampSec = 0.0;
    bool hasTimestamp = false;
};

struct ProfilePayload
{
    std::string signalType = "cpu_profile";
    std::string backend;
    std::string stackScope;
    std::string profileID;
    std::string unit = "samples";
    std::string readyPath;
    std::vector<AggregatedSample> samples;
};

struct MetricPayload
{
    std::string metric;
    std::string unit;
    std::string runtime;
    std::string comm;
    int pid = 0;
    int64_t processStartMs = 0;
    std::string exe;
    int64_t timestampMs = 0;
    uint64_t value = 0;
};

struct HistogramBucket
{
    std::string range;
    double low = 0;
    double high = 0;
    uint64_t count = 0;
};

struct HistogramPayload
{
    std::string signalType;
    std::string backend;
    std::string unit = "us";
    uint64_t eventCount = 0;
    std::vector<HistogramBucket> buckets;
    double min = 0;
    double max = 0;
    double p50 = 0;
    double p95 = 0;
    double p99 = 0;
    bool unavailable = false;
    std::string reason;
    // 阶段三：完整进程身份（strict CO-RE 按 TGID 归属；degraded bpftrace
    // 无法安全归属时 pid=0 且 unavailable）。process Session fan-out 必须
    // pid + process_start_ms + exe 三项精确匹配。
    int pid = 0;
    int64_t processStartMs = 0;
    std::string exe;
    std::string comm;
};

// 结构化数据库快照（SQL digest 聚合 / 锁等待链），与标量 MetricPayload 并存
// 于同一个 WindowPayload。kind 区分两种语义，未用到的字段留空/留 0 即可。
struct DBSnapshotSample
{
    std::string kind;          // "digest" | "lock_wait"
    std::string instanceLabel;
    int64_t timestampMs = 0;
    std::string schemaName;
    std::string digestText;
    uint64_t callCount = 0;
    uint64_t totalLatencyUs = 0;
    uint64_t rowsExaminedTotal = 0;
    int64_t waitingPid = 0;
    std::string waitingQuery;
    int64_t blockingPid = 0;
    std::string blockingQuery;
    uint64_t waitSeconds = 0;
    std::string lockedTable;
};

// 阶段二：逻辑信号名集合（cpu_profile/io_latency/io_syscall_latency/
// sched_latency/python_rss/db_snapshot）。供 fan-out 判断 Session 是否请求了
// 某信号。空集合 = 全部请求。
inline bool logical_signal_requested(const std::vector<std::string> &requested, const std::string &logical)
{
    if (requested.empty())
        return true;
    for (const auto &signal : requested)
        if (signal == logical)
            return true;
    return false;
}

// ============================================================
// 阶段三：统一进程身份与 Session 投影器
// ============================================================

// 统一 ProcessIdentity：所有样本、profile、metric、histogram、runtime、Go、
// py-spy、Memray 诊断统一引用该身份类型。process Session 必须
// pid + process_start_ms + exe 三项精确相等；身份缺失（complete()==false）
// 时不得猜测归属，对应数据丢弃并记录 identity_unavailable。
struct ProcessIdentity
{
    int pid = 0;
    int64_t processStartMs = 0;
    std::string exe;
    std::string comm;

    bool complete() const { return pid > 0 && processStartMs > 0 && !exe.empty(); }
    std::string key() const
    {
        return std::to_string(pid) + "|" + std::to_string(processStartMs) + "|" + exe;
    }
};

// 每 signal 的采集状态（v4 窗口序列化 + 状态窗口登记）。
enum class SignalCollectionStatus
{
    Collected,     // 有真实数据
    TargetIdle,    // 目标空闲/无事件（零计数但窗口存在）
    NoEvents,      // 无事件（非目标空闲，如直方图无样本）
    Unavailable,   // backend 不可用/无法安全归属
    Failed,        // 采集失败
    Unknown,       // 旧数据/未登记
};

inline const char *signal_status_name(SignalCollectionStatus status)
{
    switch (status)
    {
    case SignalCollectionStatus::Collected: return "collected";
    case SignalCollectionStatus::TargetIdle: return "target_idle";
    case SignalCollectionStatus::NoEvents: return "no_events";
    case SignalCollectionStatus::Unavailable: return "unavailable";
    case SignalCollectionStatus::Failed: return "failed";
    default: return "unknown";
    }
}

struct SignalStatus
{
    SignalCollectionStatus status = SignalCollectionStatus::Unknown;
    std::string reason;
    uint64_t lostEvents = 0;
};

// SessionContract：Session 投影所需的全部合同信息（SID、scope、targets、
// signals、请求采样率、聚合周期、降级策略）。由 ContinuousSessionManager
// 从 assignment 构造，SessionFanoutProjector 只依赖它做纯逻辑投影。
struct SessionContract
{
    std::string sid;
    std::string scope = "host"; // "host" | "process"
    std::vector<ContinuousTargetProcess> targets;
    std::vector<std::string> signals; // 请求信号（空 = 默认四类核心）
    int requestedSampleRateHz = 19;
    int aggregationWindowSec = 10;
    bool allowDegraded = false;
};

// ============================================================
// 物理层符号化诊断（结构化，供 Session 分流后重新计算，不提前固化为 JSON）
// ============================================================

// 阶段四：build-id 预热报告条目（每个 DSO 的 ready/missing/failed、解析
// 路径与失败原因），随 symbol_refs.build_id_report 上报。
struct BuildIdWarmEntry
{
    std::string buildId;
    std::string dsoPath;
    std::string status;       // ready | missing | failed
    std::string resolvedPath;
    std::string reason;
    bool cached = false;      // 命中本地缓存，无需重新解析
};

inline std::string build_id_report_to_json(const std::vector<BuildIdWarmEntry> &entries)
{
    if (entries.empty())
        return "";
    std::string body = "{\"dsos\":[";
    for (size_t i = 0; i < entries.size(); ++i)
    {
        const auto &e = entries[i];
        if (i)
            body += ",";
        body += "{\"dso\":\"" + json_escape(e.dsoPath) + "\",\"build_id\":\"" +
                json_escape(e.buildId) + "\",\"status\":\"" + json_escape(e.status) + "\"";
        if (!e.resolvedPath.empty())
            body += ",\"resolved_path\":\"" + json_escape(e.resolvedPath) + "\"";
        if (!e.reason.empty())
            body += ",\"reason\":\"" + json_escape(e.reason) + "\"";
        body += "}";
    }
    body += "]}";
    return body;
}

struct PhysicalDiagnostics
{
    // 基于本段实际样本计算的 frame 权重与 symbol_status
    uint64_t totalFrameWeight = 0;
    uint64_t unresolvedFrameWeight = 0;
    std::string symbolStatus; // "complete" | "partial" | "missing" | "not_applicable"

    // 本段引用的 build-id（warm_build_id_cache + discover_sampled_go_build_ids）
    std::vector<std::string> buildIds;
    std::vector<drop::BuildIdEntry> buildIdEntries;
    std::string kallsymsSha256;

    // runtime 诊断（JIT/perf map）。完整报告保留供 Session 分流后重建 symbol_refs。
    drop::RuntimeSymbolReport runtimeReport;
    drop::GoSymbolReport goReport;
    std::vector<drop::PythonFallbackResult> pythonFallback;
    size_t pythonFallbackLimitedCount = 0;
    std::vector<drop::MemrayProfileResult> memrayResults;

    // enrichment 总开关（DROP_CONTINUOUS_RUNTIME_ENRICHMENT=0 时置位）
    bool runtimeEnrichmentDisabled = false;
    std::string enrichmentDisabledReason;

    // 阶段四：物理采集栈回溯模式（fp|dwarf，DROP_NATIVE_CP_CALL_GRAPH）。
    // 进入 symbol_refs.language_status 的 native 行模式与诊断建议。
    std::string unwindMode = "fp";

    // 物理层是否执行过 enrich 步骤（关闭时 combined symbol_refs 缺 runtime 数据）。
    bool enrichmentApplied = true;

    // 阶段四：build-id 预热报告（结构化，Session 分流按 DSO 过滤）。
    std::vector<BuildIdWarmEntry> buildIdWarmReport;
};

// 不可变 perf.data 段。strict 滚动轮转（perf_rolling）与 degraded 单窗录制
// （perf）都把它交给统一的 ContinuousSegmentProcessor。
struct PerfSegment
{
    std::string path;
    std::string sourceBackend;    // "perf_rolling" | "perf"
    std::string collectorGeneration;
    std::string targetFingerprint;
    int64_t wallStartMs = 0;      // 录制启动锚点（wall clock）
    int64_t monotonicStartMs = 0; // 录制启动锚点（monotonic）
};

// 物理层 enrichment 能力集：哪些子步骤本次可用。阶段四起按语言独立开关
//（DROP_CONTINUOUS_GORESYM / _JAVA_PERFMAP / _NODE_PERFMAP / _PYTHON_PERF /
//  _PYSPY_FALLBACK），不再用统一的 goSymbols=true 代表全部 enrichment。
struct RuntimeCapabilitySet
{
    bool pythonFallback = true;
    bool pythonRss = true;
    bool memray = true;
    bool goSymbols = true; // 兼容旧开关语义：GoReSym 总门
    // 阶段四：逐语言能力（默认全部开启，可单独关闭单语言）。
    bool goReSym = true;
    bool javaPerfMap = true;
    bool nodePerfMap = true;
    bool pythonPerf = true;
    std::string unwindMode = "fp"; // "fp" | "dwarf"
};

struct WindowPayload;

struct SegmentProcessResult
{
    std::vector<WindowPayload> windows;
    PhysicalDiagnostics diagnostics;
    // true only when a ready py-spy capture actually replaced matching perf
    // samples in this segment.  The rolling sampler uses this to avoid dropping
    // a capture on an older, non-overlapping segment.
    bool pythonFallbackApplied = false;
    bool success = false;
    std::string failureReason;
};

struct WindowPayload
{
    int64_t startMs = 0;
    int64_t endMs = 0;
    std::vector<AggregatedSample> samples;
    std::vector<ProfilePayload> profiles;
    std::vector<HistogramPayload> histograms;
    std::vector<MetricPayload> metrics;
    std::vector<DBSnapshotSample> dbSnapshots;
    size_t rssTruncated = 0;
    // Backend metadata (5-8: strict fallback strategy)
    std::string backendStatus;                   // "ok" | "degraded" | "failed"
    std::string backendReason;                   // human-readable reason
    std::vector<std::string> attemptedBackends;  // ["core","bpftrace","perf"]
    std::string selectedBackend;                 // "bpftrace" | "perf" | ""
    // runtime map 符号化诊断（Java/Node/Python JIT perf map），序列化后写入
    // 批次/窗口 JSON 的 symbol_refs，服务端据此推断 symbol_status。
    std::string symbolRefsJson;
    // 结构化物理诊断：阶段二 fan-out 后按 Session 重新计算 symbol_refs 时使用，
    // 不参与 JSON 序列化。
    std::shared_ptr<const PhysicalDiagnostics> physicalDiagnostics;
    // 阶段一：逻辑窗口稳定 ID（内容摘要不参与 ID）与窗口内容摘要（冲突检测）。
    std::string windowID;
    std::string contentSHA256;
    // 阶段三：物理采集器 generation（fan-out 降采样稳定键与 v4 序列化用）。
    std::string collectorGeneration;
    // 阶段三：物理采样率与 Session 生效采样率（v4 序列化）。低频 Session 在
    // fan-out 时确定性降采样，effective 反映降采样后的实际频率。
    int physicalSampleRateHz = 0;
    int effectiveSampleRateHz = 0;
    // 阶段三：每 signal 采集状态（collected/target_idle/no_events/unavailable/
    // failed + reason + lost events）。零计数窗口也保留状态，使 coverage 能
    // 区分 idle/no-events、backend unavailable 和真实 gap。
    std::map<std::string, SignalStatus> signalStatuses;
    // 阶段三：身份不完整被丢弃的样本数（process Session 无法归属时记录）。
    uint64_t identityUnavailableCount = 0;
};

// ============================================================
// 共享时间/路径/进程辅助（inline，供采样器与 processor 复用）
// ============================================================

inline int64_t now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

inline int64_t monotonic_ms()
{
    struct timespec ts = {};
    if (::clock_gettime(CLOCK_MONOTONIC, &ts) != 0)
        return 0;
    return static_cast<int64_t>(ts.tv_sec) * 1000 + ts.tv_nsec / 1000000;
}

inline std::string rfc3339_from_ms(int64_t ms)
{
    std::time_t sec = static_cast<std::time_t>(ms / 1000);
    int milliseconds = static_cast<int>(ms % 1000);
    if (milliseconds < 0)
    {
        milliseconds += 1000;
        --sec;
    }
    std::tm tm{};
    gmtime_r(&sec, &tm);
    char buf[40];
    std::snprintf(buf, sizeof(buf), "%04d-%02d-%02dT%02d:%02d:%02d.%03dZ",
                  tm.tm_year + 1900, tm.tm_mon + 1, tm.tm_mday,
                  tm.tm_hour, tm.tm_min, tm.tm_sec, milliseconds);
    return std::string(buf);
}

inline bool file_exists_local(const std::string &path)
{
    struct stat st;
    return stat(path.c_str(), &st) == 0;
}

inline std::string perf_bin()
{
    const char *env = std::getenv("DROP_PERF_BIN");
    if (env && *env)
        return env;
    if (file_exists_local("/usr/local/bin/perf-real"))
        return "/usr/local/bin/perf-real";
    return "perf";
}

inline std::string read_exe(int pid)
{
    if (pid <= 0)
        return "";
    std::string path = "/proc/" + std::to_string(pid) + "/exe";
    char buf[4096];
    ssize_t n = readlink(path.c_str(), buf, sizeof(buf) - 1);
    if (n <= 0)
        return "";
    buf[n] = '\0';
    std::string exe(buf);
    const std::string deletedSuffix = " (deleted)";
    if (exe.size() > deletedSuffix.size() &&
        exe.compare(exe.size() - deletedSuffix.size(), deletedSuffix.size(), deletedSuffix) == 0)
        exe.resize(exe.size() - deletedSuffix.size());
    return exe;
}

inline int process_tgid(int pid)
{
    if (pid <= 0)
        return pid;
    std::ifstream in("/proc/" + std::to_string(pid) + "/status");
    std::string key;
    while (in >> key)
    {
        if (key == "Tgid:")
        {
            int tgid = 0;
            in >> tgid;
            return tgid > 0 ? tgid : pid;
        }
        std::string rest;
        std::getline(in, rest);
    }
    return pid;
}

// 阶段三：严格目标匹配。process Session 必须 pid + process_start_ms + exe
// 三项精确相等；样本身份缺失（start time 或 exe 为空）时不得放宽匹配——
// 放宽会让 PID 复用后的新进程数据错误进入旧实例 Session。host Session 恒
// 返回 true（整机数据）。
inline bool process_targeted(const ContinuousSamplerConfig &cfg,
                             int pid,
                             int64_t processStartMs,
                             const std::string &exe)
{
    if (cfg.scope != "process")
        return true;
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid && target.processStartMs > 0 && !target.exe.empty() &&
            processStartMs == target.processStartMs && exe == target.exe)
            return true;
    return false;
}

inline bool process_targeted(const ContinuousSamplerConfig &cfg, int pid, const std::string &exe)
{
    return process_targeted(cfg, pid, 0, exe);
}

// 按 ProcessIdentity 严格匹配（供 SessionFanoutProjector 使用）。
inline bool identity_targeted(const std::vector<ContinuousTargetProcess> &targets,
                              const ProcessIdentity &identity)
{
    if (!identity.complete())
        return false;
    for (const auto &target : targets)
        if (target.pid == identity.pid && target.processStartMs > 0 && !target.exe.empty() &&
            identity.processStartMs == target.processStartMs && identity.exe == target.exe)
            return true;
    return false;
}

inline int64_t configured_process_start_ms(const ContinuousSamplerConfig &cfg, int pid)
{
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid)
            return target.processStartMs;
    int64_t startMs = 0;
    drop::python_process_start_ms(pid, &startMs);
    return startMs;
}

inline std::string configured_process_exe(const ContinuousSamplerConfig &cfg, int pid)
{
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid)
            return target.exe;
    return {};
}

// ============================================================
// 共享 perf script 解析（唯一实现，strict/degraded/测试共用）
// ============================================================

inline std::string parse_frame_name(const std::string &raw, int pid)
{
    std::string line = drop::trim(raw);
    if (line.empty())
        return "";
    std::istringstream iss(line);
    std::string first;
    iss >> first;
    std::string rest;
    std::getline(iss, rest);
    std::string name = rest.empty() ? first : drop::trim(rest);
    size_t paren = name.find(" (");
    std::string dso;
    if (paren != std::string::npos)
    {
        size_t close = name.rfind(')');
        if (close != std::string::npos && close > paren + 2)
            dso = name.substr(paren + 2, close - paren - 2);
        name = name.substr(0, paren);
    }
    if (name.empty())
        name = first;
    name = drop::trim(name);

    // 子项1.2：解析失败时展示 "0x<addr> [<模块名>]" 而不是裸 [unknown]。
    if (name == "[unknown]" && !dso.empty())
    {
        uint64_t address = std::strtoull(first.c_str(), nullptr, 16);
        std::string goName;
        if (address > 0 && drop::resolve_go_symbol(pid, dso, address, &goName))
            return goName;
        size_t slash = dso.rfind('/');
        std::string base = slash == std::string::npos ? dso : dso.substr(slash + 1);
        if (base.size() >= 2 && base.front() == '[' && base.back() == ']')
            base = base.substr(1, base.size() - 2);
        if (base.empty() || base == "unknown")
            return "0x" + first;
        return "0x" + first + " [" + base + "]";
    }
    return name;
}

// proc_maps_containing 在 /proc/pid/maps 中查找包含 address 且路径为 dsoPath
// 的映射，返回映射 vaddr 基址与文件偏移（用于 file-relative offset）。
inline bool proc_maps_containing(int pid,
                                 const std::string &dsoPath,
                                 uint64_t address,
                                 uint64_t *base,
                                 uint64_t *fileOffset)
{
    if (pid <= 0 || dsoPath.empty() || address == 0)
        return false;
    std::string mapsPath = "/proc/" + std::to_string(pid) + "/maps";
    std::ifstream maps(mapsPath);
    if (!maps.is_open())
        return false;
    std::string line;
    while (std::getline(maps, line))
    {
        std::istringstream iss(line);
        std::string range;
        iss >> range;
        size_t dash = range.find('-');
        if (dash == std::string::npos)
            continue;
        uint64_t lo = std::strtoull(range.substr(0, dash).c_str(), nullptr, 16);
        uint64_t hi = std::strtoull(range.substr(dash + 1).c_str(), nullptr, 16);
        std::string perms;
        iss >> perms;
        std::string offsetHex;
        iss >> offsetHex;
        uint64_t offset = std::strtoull(offsetHex.c_str(), nullptr, 16);
        std::string dev, inode;
        iss >> dev >> inode;
        std::string path;
        std::getline(iss, path);
        path = drop::trim(path);
        if (path.empty())
            continue;
        bool match = (path == dsoPath);
        if (!match)
        {
            size_t slash = path.rfind('/');
            std::string baseName = slash == std::string::npos ? path : path.substr(slash + 1);
            size_t dslash = dsoPath.rfind('/');
            std::string dsoBase = dslash == std::string::npos ? dsoPath : dsoPath.substr(dslash + 1);
            match = (baseName == dsoBase);
        }
        if (!match)
            continue;
        if (address >= lo && address < hi)
        {
            if (base)
                *base = lo;
            if (fileOffset)
                *fileOffset = offset;
            return true;
        }
    }
    return false;
}

inline drop::ContinuousStackFrame parse_perf_frame(const std::string &raw, int pid)
{
    drop::ContinuousStackFrame frame;
    frame.raw = drop::trim(raw);
    if (frame.raw.empty())
        return frame;
    std::istringstream iss(frame.raw);
    std::string first;
    iss >> first;
    std::string rest;
    std::getline(iss, rest);
    if (!first.empty())
    {
        bool allHex = true;
        for (char ch : first)
        {
            if (!std::isxdigit(static_cast<unsigned char>(ch)))
            {
                allHex = false;
                break;
            }
        }
        if (allHex && first.size() >= 3)
            frame.address = std::strtoull(first.c_str(), nullptr, 16);
    }
    std::string name = drop::trim(rest);
    size_t paren = name.find(" (");
    if (paren != std::string::npos)
    {
        size_t close = name.rfind(')');
        if (close != std::string::npos && close > paren + 2)
            frame.mappingFile = name.substr(paren + 2, close - paren - 2);
        name = drop::trim(name.substr(0, paren));
    }
    if (!name.empty() && name != "[unknown]")
    {
        frame.function = name;
        frame.resolved = true;
    }
    if (frame.address != 0 && !frame.mappingFile.empty())
    {
        uint64_t base = 0, fileOffset = 0;
        if (proc_maps_containing(pid, frame.mappingFile, frame.address, &base, &fileOffset))
        {
            if (frame.address >= base)
                frame.normalizedOffset = frame.address - base + fileOffset;
        }
        std::string buildId;
        if (drop::elf_gnu_build_id(frame.mappingFile, &buildId))
            frame.buildId = buildId;
    }
    return frame;
}

// frames_to_json 序列化结构化栈（阶段五）。
inline std::string frames_to_json(const std::vector<drop::ContinuousStackFrame> &frames)
{
    if (frames.empty())
        return "";
    std::string out = "\"frames\":[";
    for (size_t i = 0; i < frames.size(); ++i)
    {
        const auto &frame = frames[i];
        if (i)
            out += ",";
        out += "{";
        out += "\"function\":\"" + json_escape(frame.function) + "\",";
        out += "\"raw\":\"" + json_escape(frame.raw) + "\",";
        out += "\"file\":\"" + json_escape(frame.file) + "\",";
        out += "\"line\":" + std::to_string(frame.line) + ",";
        out += "\"address\":" + std::to_string(frame.address) + ",";
        out += "\"mapping_file\":\"" + json_escape(frame.mappingFile) + "\",";
        out += "\"build_id\":\"" + json_escape(frame.buildId) + "\",";
        out += "\"normalized_offset\":" + std::to_string(frame.normalizedOffset) + ",";
        out += std::string("\"resolved\":") + (frame.resolved ? "true" : "false");
        out += "}";
    }
    out += "]";
    return out;
}

inline bool parse_sample_header(const std::string &line,
                                std::string *comm,
                                int *pid,
                                double *timestampSec = nullptr)
{
    std::string trimmed = drop::trim(line);
    if (trimmed.empty() || trimmed[0] == '#' || trimmed[0] == '\t')
        return false;
    if (trimmed.find(':') == std::string::npos)
        return false;
    std::istringstream iss(trimmed);
    std::string commToken, pidToken;
    if (!(iss >> commToken >> pidToken))
        return false;
    size_t slash = pidToken.find('/');
    if (slash != std::string::npos)
        pidToken = pidToken.substr(0, slash);
    int parsedPid = std::atoi(pidToken.c_str());
    if (parsedPid <= 0)
        return false;
    *comm = commToken;
    *pid = parsedPid;
    if (timestampSec)
    {
        *timestampSec = 0.0;
        std::string token;
        while (iss >> token)
        {
            const size_t colon = token.find(':');
            if (colon == std::string::npos || colon == 0)
                continue;
            const std::string number = token.substr(0, colon);
            char *end = nullptr;
            const double parsed = std::strtod(number.c_str(), &end);
            if (!end || *end != '\0' || !std::isfinite(parsed) || parsed < 0.0)
                continue;
            if (number.find('.') == std::string::npos && number.find('e') == std::string::npos &&
                number.find('E') == std::string::npos)
                continue;
            *timestampSec = parsed;
            break;
        }
    }
    return true;
}

inline void add_sample(std::map<std::string, AggregatedSample> *out,
                       const std::string &comm,
                       int pid,
                       const std::vector<std::string> &rawStack,
                       const std::vector<drop::ContinuousStackFrame> &rawFrames,
                       const std::string &stackScope = "",
                       const std::string &backend = "")
{
    if (rawStack.empty())
        return;
    pid = process_tgid(pid);
    std::vector<std::string> stack = rawStack;
    std::reverse(stack.begin(), stack.end());
    std::vector<drop::ContinuousStackFrame> frames = rawFrames;
    std::reverse(frames.begin(), frames.end());
    std::string exe = read_exe(pid);
    std::string key = comm + "|" + std::to_string(pid) + "|" + exe;
    for (const auto &frame : stack)
        key += "|" + frame;
    AggregatedSample &sample = (*out)[key];
    if (sample.count == 0)
    {
        sample.comm = comm;
        sample.pid = pid;
        sample.exe = exe;
        sample.stack = stack;
        sample.frames = frames;
        sample.stackScope = stackScope;
        sample.backend = backend;
    }
    sample.count++;
}

inline PerfScriptParseResult parse_perf_script_result(const std::string &script)
{
    PerfScriptParseResult result;
    std::map<std::string, AggregatedSample> byKey;
    std::istringstream iss(script);
    std::string line;
    std::string currentComm;
    int currentPid = 0;
    std::vector<std::string> currentStack;
    std::vector<drop::ContinuousStackFrame> currentFrames;
    auto flush = [&]() {
        add_sample(&byKey, currentComm, currentPid, currentStack, currentFrames);
        currentComm.clear();
        currentPid = 0;
        currentStack.clear();
        currentFrames.clear();
    };
    while (std::getline(iss, line))
    {
        if (drop::trim(line).empty())
        {
            flush();
            continue;
        }
        std::string comm;
        int pid = 0;
        double timestampSec = 0.0;
        if (parse_sample_header(line, &comm, &pid, &timestampSec))
        {
            flush();
            currentComm = comm;
            currentPid = pid;
            if (timestampSec > 0.0)
            {
                if (!result.hasTimestamp)
                {
                    result.startTimestampSec = timestampSec;
                    result.endTimestampSec = timestampSec;
                    result.hasTimestamp = true;
                }
                else
                {
                    result.startTimestampSec = std::min(result.startTimestampSec, timestampSec);
                    result.endTimestampSec = std::max(result.endTimestampSec, timestampSec);
                }
            }
            continue;
        }
        if (!currentComm.empty() && (line[0] == ' ' || line[0] == '\t'))
        {
            std::string frame = parse_frame_name(line, currentPid);
            if (!frame.empty())
                currentStack.push_back(frame);
            drop::ContinuousStackFrame structured = parse_perf_frame(line, currentPid);
            if (!structured.raw.empty())
                currentFrames.push_back(structured);
        }
    }
    flush();
    result.samples.reserve(byKey.size());
    for (auto &kv : byKey)
        result.samples.push_back(kv.second);
    return result;
}

inline std::vector<AggregatedSample> parse_perf_script(const std::string &script)
{
    return parse_perf_script_result(script).samples;
}

inline int64_t perf_timestamp_to_unix_ms(double timestampSec,
                                         int64_t wallAnchorMs,
                                         int64_t monotonicAnchorMs)
{
    if (!std::isfinite(timestampSec) || timestampSec <= 0.0)
        return 0;
    if (timestampSec >= 1000000000.0)
        return static_cast<int64_t>(std::llround(timestampSec * 1000.0));
    if (wallAnchorMs <= 0 || monotonicAnchorMs <= 0)
        return 0;
    return wallAnchorMs + static_cast<int64_t>(std::llround(timestampSec * 1000.0)) - monotonicAnchorMs;
}

// ============================================================
// 任务1：perf script 之前本地 build-id 缓存预热（三态尝试缓存）
// ============================================================
enum class BuildIdAttemptState
{
    TransientFail,
    PermanentFail,
};

struct BuildIdAttempt
{
    BuildIdAttemptState state;
    int64_t retryAfterMs;
    int64_t lastTouchedMs;
};

inline std::mutex g_buildIdAttemptMutex;
inline std::map<std::string, BuildIdAttempt> g_buildIdAttempts;
inline constexpr int64_t kBuildIdTransientRetryMs = 5 * 60 * 1000;
inline constexpr size_t kBuildIdAttemptMaxEntries = 4096;

inline void evict_oldest_build_id_attempt_locked()
{
    if (g_buildIdAttempts.size() <= kBuildIdAttemptMaxEntries)
        return;
    int64_t oldest = INT64_MAX;
    std::string oldestKey;
    for (const auto &kv : g_buildIdAttempts)
    {
        if (kv.second.lastTouchedMs < oldest)
        {
            oldest = kv.second.lastTouchedMs;
            oldestKey = kv.first;
        }
    }
    if (!oldestKey.empty())
        g_buildIdAttempts.erase(oldestKey);
}

inline bool should_skip_build_id_attempt(const std::string &buildId, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    auto it = g_buildIdAttempts.find(buildId);
    if (it == g_buildIdAttempts.end())
        return false;
    if (it->second.state == BuildIdAttemptState::PermanentFail)
        return true;
    if (nowMs < it->second.retryAfterMs)
        return true;
    g_buildIdAttempts.erase(it);
    return false;
}

inline void record_build_id_transient_fail(const std::string &buildId, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::TransientFail, nowMs + kBuildIdTransientRetryMs, nowMs};
    evict_oldest_build_id_attempt_locked();
}

inline void record_build_id_permanent_fail(const std::string &buildId)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::PermanentFail, 0, now_ms()};
    evict_oldest_build_id_attempt_locked();
}

inline void clear_build_id_attempt(const std::string &buildId)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts.erase(buildId);
}

inline bool looks_like_elf(const std::string &path)
{
    std::ifstream f(path, std::ios::binary);
    if (!f.is_open())
        return false;
    char magic[4] = {0};
    f.read(magic, 4);
    return f.gcount() == 4 && magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F';
}

// 任务1核心：perf script 之前，把能拿到的用户态二进制预热进本地 build-id
// 缓存，让 perf script 自带的缓存回退机制命中，当场生效不依赖网络往返。
// 阶段四：report 非空时输出逐 DSO 诊断（ready/missing/failed + 原因）；
// 容器 DSO 用 resolve_dso_deep（宿主路径 → /proc/<pid>/root → deleted
// mapping），并在缓存前校验 ELF magic 与 build-id 匹配。
inline std::vector<drop::BuildIdEntry> warm_build_id_cache(const std::string &perf, const std::string &dataPath,
                                                           std::vector<BuildIdWarmEntry> *report = nullptr)
{
    if (report)
        report->clear();
    std::string listOutput;
    int rc = drop::exec_capture({perf, "buildid-list", "-i", dataPath}, &listOutput, 65536);
    if (rc != 0)
        return {};

    std::vector<drop::BuildIdEntry> entries = drop::parse_buildid_list(listOutput);
    if (entries.empty())
        return entries;

    int64_t nowMs = now_ms();
    std::vector<drop::BuildIdEntry> pending;
    for (auto &e : entries)
    {
        if (drop::build_id_cached_locally(e.buildId))
        {
            if (report)
                report->push_back({e.buildId, e.dsoPath, "ready",
                                   drop::build_id_local_cache_path(e.buildId), "", true});
            continue;
        }
        if (should_skip_build_id_attempt(e.buildId, nowMs))
        {
            if (report)
                report->push_back({e.buildId, e.dsoPath, "missing", "",
                                   "previous resolution failed; backing off", false});
            continue;
        }
        pending.push_back(e);
    }
    if (pending.empty())
        return entries;

    std::unordered_map<std::string, int> dsoIndex = drop::build_dso_path_index();

    for (auto &e : pending)
    {
        auto it = dsoIndex.find(e.dsoPath);
        if (it == dsoIndex.end())
        {
            record_build_id_transient_fail(e.buildId, nowMs);
            if (report)
                report->push_back({e.buildId, e.dsoPath, "missing", "",
                                   "no live process maps this DSO", false});
            continue;
        }
        // 阶段四：宿主路径 → /proc/<pid>/root → deleted mapping 深度解析。
        std::string resolved = drop::resolve_dso_deep(it->second, e.dsoPath);
        if (resolved.empty())
        {
            record_build_id_transient_fail(e.buildId, nowMs);
            if (report)
                report->push_back({e.buildId, e.dsoPath, "failed", "",
                                   "DSO unreadable in host and container views", false});
            continue;
        }
        if (!looks_like_elf(resolved))
        {
            record_build_id_permanent_fail(e.buildId);
            if (report)
                report->push_back({e.buildId, e.dsoPath, "failed", resolved,
                                   "not a valid ELF file", false});
            continue;
        }
        // 缓存前校验 build-id：不匹配的文件不能进缓存（防 PID 复用后旧文件
        // 冒充新 build-id 的符号源）。
        std::string actualBuildId;
        bool buildIdMatches = true;
        if (!e.buildId.empty() && drop::elf_gnu_build_id(resolved, &actualBuildId))
        {
            if (!actualBuildId.empty() && actualBuildId != e.buildId)
            {
                buildIdMatches = false;
                record_build_id_permanent_fail(e.buildId);
                if (report)
                    report->push_back({e.buildId, e.dsoPath, "failed", resolved,
                                       "build-id mismatch: ELF has " + actualBuildId, false});
            }
        }
        if (!buildIdMatches)
            continue;
        if (drop::cache_build_id_locally(e.buildId, resolved))
        {
            clear_build_id_attempt(e.buildId);
            if (report)
                report->push_back({e.buildId, e.dsoPath, "ready",
                                   drop::build_id_local_cache_path(e.buildId), "", false});
        }
        else
        {
            record_build_id_transient_fail(e.buildId, nowMs);
            if (report)
                report->push_back({e.buildId, e.dsoPath, "failed", resolved,
                                   "cannot write local build-id cache", false});
        }
    }
    return entries;
}

// ============================================================
// 共享 runtime 分类 / Python 路径清理 / symbol_refs 诊断
// ============================================================

inline bool unresolved_frame(const std::string &raw)
{
    std::string frame = drop::trim(raw);
    if (frame.empty() || frame == "unknown" || frame == "[unknown]")
        return true;
    if (frame.size() > 2 && frame.front() == '[' && frame.back() == ']')
        return true;
    if (frame.rfind("0x", 0) != 0)
        return false;
    size_t i = 2;
    while (i < frame.size() && std::isxdigit(static_cast<unsigned char>(frame[i])))
        ++i;
    return i > 2 && (i == frame.size() || std::isspace(static_cast<unsigned char>(frame[i])));
}

inline std::string path_basename(const std::string &path)
{
    size_t slash = path.rfind('/');
    return slash == std::string::npos ? path : path.substr(slash + 1);
}

inline std::string sanitize_python_perf_frame(const std::string &frame)
{
    // CPython -X perf names are typically
    // "py::function:/absolute/path/module.py+0x...". Keep the function,
    // short file name and offset/line while preventing paths from entering
    // uploaded batches.
    if (frame.rfind("py::", 0) != 0)
        return frame;
    size_t pathStart = frame.find(':', 4);
    if (pathStart == std::string::npos)
        return frame;
    ++pathStart;
    size_t slash = frame.find_last_of("/\\");
    if (slash == std::string::npos || slash < pathStart)
        return frame;
    return frame.substr(0, pathStart) + frame.substr(slash + 1);
}

// 阶段四：Go runtime 识别顺序（禁止依赖 exe 名称）：
//   1. Go build-info 标记；
//   2. .gopclntab 段（stripped / 重命名 ELF 仍可识别，含 dockerd）；
//   3. GoReSym 结果（goReport 中已登记的 DSO）；
//   4. 最后才允许用 runtime.* 等栈帧作结果级辅助判断。
inline bool stack_frames_hint_go(const std::vector<std::string> &stack)
{
    for (const auto &frame : stack)
        if (frame.rfind("runtime.", 0) == 0)
            return true;
    return false;
}

inline std::string sample_runtime_with_go_hint(const AggregatedSample &sample,
                                               const drop::GoSymbolReport &goReport,
                                               bool hasGoBuildInfo,
                                               bool hasGoPclntab = false)
{
    std::string base = path_basename(sample.exe);
    if (base.rfind("python", 0) == 0)
        return "python";
    if (base.rfind("java", 0) == 0)
        return "java";
    if (base.rfind("node", 0) == 0)
        return "node";
    auto isGo = [&](const std::vector<drop::GoSymbolItem> &items) {
        return std::any_of(items.begin(), items.end(), [&](const auto &item) {
            return item.dsoPath == sample.exe;
        });
    };
    if (hasGoBuildInfo || hasGoPclntab || isGo(goReport.ready) || isGo(goReport.pending) ||
        isGo(goReport.failed))
        return "go";
    // 结果级辅助判断：栈里出现 runtime.* 帧（仅在更强证据缺失时兜底）。
    if (!sample.exe.empty() && stack_frames_hint_go(sample.stack))
        return "go";
    if (sample.exe.empty())
        return sample.pid <= 2 || sample.comm.rfind("kworker", 0) == 0 ? "kernel" : "unknown";
    return "native";
}

inline std::string sample_runtime(const AggregatedSample &sample,
                                  const drop::GoSymbolReport &goReport,
                                  std::map<int, bool> *goBuildInfoCache)
{
    bool hasGoBuildInfo = false;
    bool hasGoPclntab = false;
    if (sample.pid > 0 && !sample.exe.empty())
    {
        static thread_local std::map<int, std::pair<bool, bool>> tlsDetectionCache;
        auto cached = tlsDetectionCache.find(sample.pid);
        if (cached == tlsDetectionCache.end())
        {
            std::string procExe = "/proc/" + std::to_string(sample.pid) + "/exe";
            hasGoBuildInfo = drop::go_binary_has_build_info(procExe);
            // build-info 已确认时无需再扫段表。
            hasGoPclntab = hasGoBuildInfo ? true : drop::go_binary_has_pclntab(procExe);
            cached = tlsDetectionCache.emplace(sample.pid, std::make_pair(hasGoBuildInfo, hasGoPclntab)).first;
        }
        else
        {
            hasGoBuildInfo = cached->second.first;
            hasGoPclntab = cached->second.second;
        }
    }
    return sample_runtime_with_go_hint(sample, goReport, hasGoBuildInfo, hasGoPclntab);
}

inline std::string python_fallback_json(const std::vector<drop::PythonFallbackResult> &results, size_t limitedCount)
{
    std::string body = "{\"ready\":[";
    bool firstReady = true;
    for (const auto &result : results)
    {
        if (!result.ready)
            continue;
        if (!firstReady)
            body += ",";
        firstReady = false;
        // 阶段三：py-spy 诊断携带完整进程身份（pid+process_start_ms+exe）。
        body += "{\"pid\":" + std::to_string(result.pid) +
                ",\"process_start_ms\":" + std::to_string(result.startMs) +
                ",\"exe\":\"" + json_escape(result.exe) +
                "\",\"mode\":\"" + std::string(result.nativeStacks ? "py-spy-native" : "py-spy") +
                "\",\"samples\":" + std::to_string(result.samples.size()) +
                ",\"capture_start_ms\":" + std::to_string(result.captureStartMs) +
                ",\"capture_end_ms\":" + std::to_string(result.captureEndMs);
        if (!result.warning.empty())
            body += ",\"warning\":\"" + json_escape(result.warning) + "\"";
        body += "}";
    }
    body += "],\"failed\":[";
    bool firstFailed = true;
    for (const auto &result : results)
    {
        if (result.ready)
            continue;
        if (!firstFailed)
            body += ",";
        firstFailed = false;
        body += "{\"pid\":" + std::to_string(result.pid) +
                ",\"process_start_ms\":" + std::to_string(result.startMs) +
                ",\"exe\":\"" + json_escape(result.exe) +
                "\",\"reason\":\"" + json_escape(result.reason) + "\"}";
    }
    body += "],\"limited_count\":" + std::to_string(limitedCount) + "}";
    return body;
}

// 阶段四：language_status v2 片段（实现位于 LanguageStatus.cpp，避免头文件
// 循环依赖）。返回 "{\"diagnostics_version\":2,\"language_status\":{...}}"。
// targets 非空时按 Session 身份过滤进程实例；unwindMode 进入 native 行。
std::string language_status_fragment_for_symbol_refs(
    const std::vector<AggregatedSample> &samples,
    const drop::RuntimeSymbolReport &runtimeReport,
    const drop::GoSymbolReport &goReport,
    const std::vector<drop::PythonFallbackResult> &pythonFallback,
    size_t pythonLimitedCount,
    const std::vector<ContinuousTargetProcess> *targets,
    const std::string &unwindMode);

inline std::string combined_symbol_refs_json(const drop::RuntimeSymbolReport &runtimeReport,
                                             const drop::GoSymbolReport &goReport,
                                             const std::vector<drop::PythonFallbackResult> &pythonFallback,
                                             size_t pythonLimitedCount,
                                             const std::vector<drop::MemrayProfileResult> &memrayResults,
                                             const std::vector<AggregatedSample> &samples,
                                             const std::vector<drop::BuildIdEntry> &buildIds,
                                             const std::string &kallsymsSha256,
                                             const std::vector<ContinuousTargetProcess> *sessionTargets = nullptr,
                                             const std::string &unwindMode = "fp",
                                             const std::string &buildIdReport = "")
{
    uint64_t totalFrames = 0;
    uint64_t unresolvedFrames = 0;
    for (const auto &sample : samples)
    {
        for (const auto &frame : sample.stack)
        {
            totalFrames = add_count(totalFrames, sample.count);
            if (unresolved_frame(frame))
                unresolvedFrames = add_count(unresolvedFrames, sample.count);
        }
    }
    std::string status = "not_applicable";
    if (totalFrames > 0)
        status = unresolvedFrames == 0 ? "complete" : (unresolvedFrames >= totalFrames ? "missing" : "partial");
    std::string body = "{";
    body += "\"symbol_status\":\"" + status + "\",";
    body += "\"frame_stats\":{\"total_frame_weight\":" + std::to_string(totalFrames) +
            ",\"unresolved_frame_weight\":" + std::to_string(unresolvedFrames) + "},";
    body += "\"build_ids\":[";
    for (size_t i = 0; i < buildIds.size(); ++i)
    {
        if (i)
            body += ",";
        body += "\"" + json_escape(buildIds[i].buildId) + "\"";
    }
    body += "],";
    if (!kallsymsSha256.empty())
        body += "\"kallsyms_sha256\":\"" + json_escape(kallsymsSha256) + "\",";
    body += "\"runtime_maps\":" + drop::runtime_maps_to_json(runtimeReport) + ",";
    body += "\"native_go\":" + drop::go_symbol_report_json(goReport) + ",";
    body += "\"python_fallback\":" + python_fallback_json(pythonFallback, pythonLimitedCount) + ",";
    body += "\"python_memory\":{\"ready\":[";
    bool firstMemrayReady = true;
    for (const auto &result : memrayResults)
    {
        if (!result.ready) continue;
        if (!firstMemrayReady) body += ",";
        firstMemrayReady = false;
        // 阶段三：Memray 诊断携带完整进程身份（pid+process_start_ms+exe），
        // 服务端按实例键去重，PID 复用显示为两个实例。
        body += "{\"pid\":" + std::to_string(result.pid) +
                ",\"process_start_ms\":" + std::to_string(result.processStartMs) +
                ",\"exe\":\"" + json_escape(result.exe) +
                "\",\"profile_id\":\"" + json_escape(result.profileID) + "\"}";
    }
    body += "],\"failed\":[";
    bool firstMemrayFailed = true;
    for (const auto &result : memrayResults)
    {
        if (result.ready) continue;
        if (!firstMemrayFailed) body += ",";
        firstMemrayFailed = false;
        body += "{\"pid\":" + std::to_string(result.pid) +
                ",\"process_start_ms\":" + std::to_string(result.processStartMs) +
                ",\"exe\":\"" + json_escape(result.exe) +
                "\",\"reason\":\"" + json_escape(result.reason) + "\"}";
    }
    // "]}" 关闭 failed 数组 + python_memory 对象（v2 片段之后还有根对象闭合）。
    body += "]}";
    // 阶段四：v2 语言诊断契约。旧字段（runtime_maps/native_go/python_fallback）
    // 原样保留一个兼容周期。
    body += "," + language_status_fragment_for_symbol_refs(
                       samples, runtimeReport, goReport, pythonFallback, pythonLimitedCount,
                       sessionTargets, unwindMode);
    if (!buildIdReport.empty())
        body += ",\"build_id_report\":" + buildIdReport;
    body += "}";
    return body;
}

// 内核符号快照：低频（每 10 分钟）快照并去重上传一次 /proc/kallsyms，把
// sha256 写进后续窗口的 symbol_refs，供"重新检查符号"诊断 kallsyms 是否入库。
inline std::mutex g_kallsymsSnapshotMutex;
inline std::string g_lastKallsymsSha256;
inline int64_t g_lastKallsymsSnapshotMs = 0;
inline constexpr int64_t kKallsymsSnapshotIntervalMs = 10 * 60 * 1000;

inline std::string ensure_kallsyms_snapshot(const ContinuousSamplerConfig &cfg)
{
    std::lock_guard<std::mutex> lock(g_kallsymsSnapshotMutex);
    int64_t now = now_ms();
    if (now - g_lastKallsymsSnapshotMs < kKallsymsSnapshotIntervalMs)
        return g_lastKallsymsSha256;
    g_lastKallsymsSnapshotMs = now;
    std::string path = "/tmp/mini_drop_cp_kallsyms_" + cfg.sessionSID + ".txt";
    if (!drop::snapshot_kallsyms(path))
        return g_lastKallsymsSha256;
    g_lastKallsymsSha256 = drop::ensure_kernel_symbol_uploaded(cfg.apiBaseURL, cfg.sessionSID, path);
    ::remove(path.c_str());
    return g_lastKallsymsSha256;
}

// ============================================================
// ContinuousSegmentProcessor：统一 perf.data 段处理流水线
// ============================================================

class ContinuousSegmentProcessor
{
public:
    /// 处理一个不可变 perf segment，按固定顺序执行统一流水线：
    ///   1 校验段 → 2 warm_build_id_cache → 3 collect_runtime_maps →
    ///   4 discover_sampled_go_build_ids → 5 prepare_go_symbols →
    ///   6 唯一一次 perf script → 7 解析/规范化 samples+structured frames →
    ///   8 补齐 PID/start/exe/runtime/backend → 9 Python 路径清理 →
    ///   10 unresolved/frame 诊断 → 11 结构化物理 symbol_refs →
    ///   12 异步上报 build-id + kallsyms 引用（节流）。
    /// pythonResults/memrayResults 为物理级 sidecar 结果（py-spy 覆盖真实
    /// capture 区间；Memray 用自身采集时间），由调用方在进入 fan-out 前提供。
    /// 外部失败分级：
    ///   - perf segment 无效或 perf script 失败：整个段处理失败（success=false，
    ///     调用方不应删除该段，可重试）。
    ///   - runtime map / GoReSym / Java perf-map / 符号上传失败：保留 CPU
    ///     samples，在 diagnostics 中标记 partial/missing，不伪造成功。
    ///   - enrichment 总开关关闭：仍走统一解析器，只跳过 runtime enrichment。
    SegmentProcessResult Process(
        const PerfSegment &segment,
        const ContinuousSamplerConfig &physicalConfig,
        const RuntimeCapabilitySet &capabilities,
        const std::vector<drop::PythonFallbackResult> &pythonResults = {},
        size_t pythonLimitedCount = 0,
        const std::vector<drop::MemrayProfileResult> &memrayResults = {});
};

/// 纯函数：给定 perf script 文本与已就绪的 runtime/go/build-id 报告，完成
/// 解析、规范化、PID/start/exe/runtime/backend 补齐、Python 路径清理、
/// py-spy 合并（防双计数）、诊断与 symbol_refs 生成。供单元测试直接以 perf
/// fixture 驱动（不经真实 perf 命令），也供 Process 复用同一套实现。
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
    const std::vector<BuildIdWarmEntry> &buildIdWarmReport = {});

/// 把物理级 py-spy sidecar 结果合并进 CPU samples（纯函数，供测试与处理器
/// 共用）：py-spy 就绪 → 删除该 PID 的 perf Python 样本并追加 py-spy 样本
/// （不双计数）；失败/PID 复用 → 保留原 perf 样本。阶段四：结果只能替换
/// capture 区间与窗口时间重叠且身份一致的 perf 样本；capture 区间缺失时按
/// 兼容口径处理（视为重叠）。
void merge_python_sidecar_samples(std::vector<AggregatedSample> *samples,
                                  const std::vector<drop::PythonFallbackResult> &pythonResults,
                                  bool *anyReplaced = nullptr,
                                  int64_t windowStartMs = 0,
                                  int64_t windowEndMs = 0);

/// 阶段二：Session 分流后重算 symbol_refs。physicalJson 为物理 symbol_refs
/// （host Session 直接复用）；process Session 用 filteredSamples 重新计算
/// frame 统计与 build-id，并只保留本 Session 的 runtime PID，杜绝跨 selector
/// 泄漏。kallsymsSha256 等整机级信息允许复用。
std::string rebuild_filtered_symbol_refs(const std::string &physicalJson,
                                         const PhysicalDiagnostics &diagnostics,
                                         const std::vector<AggregatedSample> &filteredSamples,
                                         const ContinuousSamplerConfig &session);

/// 用结构化诊断和最终窗口样本生成 Session 专属 symbol_refs。host Session
/// 也重新序列化，避免多 slice 聚合后沿用最后一个 slice 的 JSON。
std::string build_session_symbol_refs(const PhysicalDiagnostics &diagnostics,
                                      const std::vector<AggregatedSample> &samples,
                                      const ContinuousSamplerConfig &session);

// ============================================================
// 阶段三：SessionFanoutProjector — 物理窗口 → Session 逻辑窗口的纯逻辑投影
// ============================================================
// 共享采集循环不再分别手写过滤规则；所有 Session 分流统一走这里：
//   1. 按 SessionContract.signals 严格过滤 samples/profiles/metrics/
//      histograms/dbSnapshots（profiles 按各自 signal_type 过滤，不能因 CPU
//      未启用而整体清空）。
//   2. process scope：按 ProcessIdentity（pid+process_start_ms+exe）精确过滤；
//      身份缺失/进程退出/PID 复用 → 丢弃并记录 identity_unavailable。
//   3. 确定性按比例降采样：requested_hz < physical_hz 时用最大余数法在聚合
//      stack 间分配样本数，相同余数用稳定排序键保证重试结果与 content hash
//      稳定；不放大样本，极低流量窗口允许零样本并上报 target_idle。
//   4. 重建 process Session 的 symbol_refs（复用 rebuild_filtered_symbol_refs）。
//   5. 直方图按 signal + 身份过滤；无法安全归属的 degraded 路径标记
//      unavailable，不得复制整机直方图。
//   6. 每 signal 登记状态（collected/target_idle/no_events/unavailable/
//      failed），零计数窗口也保留，使 coverage 能区分 idle 与真实 gap。
class SessionFanoutProjector
{
public:
    /// 纯函数投影。physical 为物理窗口（含物理诊断与物理采样率）；
    /// contract 为 Session 合同；physicalSampleRateHz 为物理采集频率；
    /// histogramAttributionSafe=false 表示当前 backend 无法把直方图安全归属
    /// 到单个 selector（多进程 + 滚动 bpftrace fallback），此时对请求了该
    /// 信号的 Session 只登记 unavailable 状态窗口。
    /// 返回投影后的 Session 窗口；即使零计数也返回（状态窗口），调用方据此
    /// 决定是否持久化。
    WindowPayload Project(const WindowPayload &physical,
                          const SessionContract &contract,
                          int physicalSampleRateHz,
                          bool histogramAttributionSafe) const;

    /// 确定性降采样（纯函数，供测试直接驱动）：把 filteredSamples 按
    /// requested/physical 比例降采样。返回降采样后的样本；不放大样本。
    static std::vector<AggregatedSample> DownsampleDeterministic(
        const std::vector<AggregatedSample> &samples,
        uint64_t requestedHz,
        uint64_t physicalHz,
        const std::string &stabilityKey);
};

} // namespace drop
