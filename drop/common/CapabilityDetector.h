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
    std::vector<std::string> capabilities;
};

CapabilityReport detect_capabilities();
std::vector<std::string> merge_capabilities(const std::vector<std::string> &base,
                                            const std::vector<std::string> &detected);

} // namespace drop
