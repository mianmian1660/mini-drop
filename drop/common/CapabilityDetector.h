// ============================================================
// common/CapabilityDetector.h — Native CP capability detection
// ============================================================

#pragma once

#include <string>
#include <vector>

namespace drop
{

struct CapabilityReport
{
    bool perfEventReadable = false;
    bool perfCommand = false;
    bool bpftraceCommand = false;
    bool btf = false;
    bool ebpfFS = false;
    bool traceFS = false;
    bool blockTracepoint = false;
    bool schedTracepoint = false;
    bool memlockUnlimited = false;
    int perfEventParanoid = 999;
    // 阶段四：逐语言 enrichment 能力（不能再用统一 goSymbols=true 代表全部）。
    bool goReSym = false;        // GoReSym 二进制可用
    bool asprof = false;         // async-profiler（Java perf-map attach）
    bool pySpy = false;          // py-spy（Python fallback）
    bool runtimeMapSupport = false; // /tmp 可写，JIT perf map 原子搬运可用
    std::vector<std::string> capabilities;
};

CapabilityReport detect_capabilities();
std::vector<std::string> merge_capabilities(const std::vector<std::string> &base,
                                            const std::vector<std::string> &detected);

} // namespace drop
