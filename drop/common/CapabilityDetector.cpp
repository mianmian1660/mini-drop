// ============================================================
// common/CapabilityDetector.cpp — Linux native capability probe
// ============================================================

#include "common/CapabilityDetector.h"
#include "common/Utils.h"

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <fstream>
#include <set>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <sys/resource.h>

namespace drop
{

static bool path_exists(const std::string &path)
{
    struct stat st;
    return stat(path.c_str(), &st) == 0;
}

static bool command_exists(const std::string &name)
{
    const char *pathEnv = std::getenv("PATH");
    if (!pathEnv)
        return false;
    std::stringstream ss(pathEnv);
    std::string dir;
    while (std::getline(ss, dir, ':'))
    {
        if (dir.empty())
            dir = ".";
        if (path_exists(dir + "/" + name))
            return true;
    }
    return false;
}

static int read_int_file(const std::string &path, int fallback)
{
    std::ifstream in(path);
    if (!in.is_open())
        return fallback;
    int value = fallback;
    in >> value;
    return value;
}

static bool file_contains(const std::string &path, const std::string &needle)
{
    std::ifstream in(path);
    if (!in.is_open())
        return false;
    std::string line;
    while (std::getline(in, line))
    {
        if (line.find(needle) != std::string::npos)
            return true;
    }
    return false;
}

CapabilityReport detect_capabilities()
{
    CapabilityReport report;
    report.perfEventParanoid = read_int_file("/proc/sys/kernel/perf_event_paranoid", 999);
    report.perfEventReadable = report.perfEventParanoid != 999;
    report.perfCommand = command_exists("perf") || command_exists("perf-real");
    report.bpftraceCommand = command_exists("bpftrace");
    report.btf = path_exists("/sys/kernel/btf/vmlinux");
    report.ebpfFS = path_exists("/sys/fs/bpf");
    report.traceFS = path_exists("/sys/kernel/tracing") || path_exists("/sys/kernel/debug/tracing");
    report.blockTracepoint =
        file_contains("/sys/kernel/tracing/available_events", "block:block_rq_issue") ||
        file_contains("/sys/kernel/debug/tracing/available_events", "block:block_rq_issue");
    report.schedTracepoint =
        file_contains("/sys/kernel/tracing/available_events", "sched:sched_switch") ||
        file_contains("/sys/kernel/debug/tracing/available_events", "sched:sched_switch");
    struct rlimit memlock {};
    if (getrlimit(RLIMIT_MEMLOCK, &memlock) == 0)
        report.memlockUnlimited = memlock.rlim_cur == RLIM_INFINITY || memlock.rlim_max == RLIM_INFINITY;

    if (report.perfEventReadable)
        report.capabilities.push_back("native_cp_perf_event");
    if (report.perfCommand)
        report.capabilities.push_back("native_cp_perf");
    if (report.bpftraceCommand)
        report.capabilities.push_back("native_cp_bpftrace");
    if (report.btf)
        report.capabilities.push_back("native_cp_btf");
    if (report.ebpfFS)
        report.capabilities.push_back("native_cp_ebpf_fs");
    if (report.traceFS)
        report.capabilities.push_back("native_cp_tracefs");
    if (report.blockTracepoint)
        report.capabilities.push_back("native_cp_tracepoint_block");
    if (report.schedTracepoint)
        report.capabilities.push_back("native_cp_tracepoint_sched");
    if (report.memlockUnlimited)
        report.capabilities.push_back("native_cp_memlock_unlimited");
    if (report.perfEventReadable && report.perfCommand)
        report.capabilities.push_back("native_cp_sampler_perf_event");
    if (report.bpftraceCommand && report.traceFS && report.ebpfFS)
        report.capabilities.push_back("native_cp_sampler_bpftrace_ready");
    if (report.btf && report.ebpfFS && report.memlockUnlimited)
        report.capabilities.push_back("native_cp_sampler_core_ready");
    return report;
}

std::vector<std::string> merge_capabilities(const std::vector<std::string> &base,
                                            const std::vector<std::string> &detected)
{
    std::set<std::string> seen;
    std::vector<std::string> out;
    for (const auto &cap : base)
    {
        if (!cap.empty() && seen.insert(cap).second)
            out.push_back(cap);
    }
    for (const auto &cap : detected)
    {
        if (!cap.empty() && seen.insert(cap).second)
            out.push_back(cap);
    }
    return out;
}

} // namespace drop
