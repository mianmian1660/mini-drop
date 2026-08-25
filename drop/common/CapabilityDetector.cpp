// ============================================================
// common/CapabilityDetector.cpp — Linux native capability probe
// ============================================================

#include "common/CapabilityDetector.h"
#include "common/Utils.h"

#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <set>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <sys/resource.h>
#include <unistd.h>

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
    std::string coreObject = std::getenv("DROP_NATIVE_CP_CORE_OBJECT") && *std::getenv("DROP_NATIVE_CP_CORE_OBJECT")
                                 ? std::getenv("DROP_NATIVE_CP_CORE_OBJECT") : "/app/native_cp.bpf.o";
    bool coreObjectReady = path_exists(coreObject);
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

    // 阶段四：逐语言 enrichment 能力探测。路径优先 /usr/local/bin（Agent
    // 镜像内置位置），其次 PATH。
    report.goReSym = path_exists("/usr/local/bin/GoReSym") || command_exists("GoReSym");
    report.asprof = path_exists("/usr/local/bin/asprof") || command_exists("asprof");
    report.pySpy = path_exists("/usr/local/bin/py-spy") || command_exists("py-spy");
    {
        // runtime map 需要 Agent 可见的 /tmp 且可写（容器内绑定宿主 /tmp）。
        std::string probe = "/tmp/.mini-drop-runtime-map-probe-" +
                            std::to_string(static_cast<long>(::getpid()));
        FILE *f = fopen(probe.c_str(), "w");
        if (f)
        {
            fclose(f);
            ::remove(probe.c_str());
            report.runtimeMapSupport = true;
        }
    }

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
    if (report.btf && report.ebpfFS && report.memlockUnlimited && coreObjectReady)
        report.capabilities.push_back("native_cp_sampler_core_ready");
    // 阶段四：语言级 enrichment 能力位。
    if (report.goReSym)
        report.capabilities.push_back("lang_go_goresym");
    if (report.asprof)
        report.capabilities.push_back("lang_java_asprof");
    if (report.pySpy)
        report.capabilities.push_back("lang_python_pyspy");
    if (report.runtimeMapSupport)
        report.capabilities.push_back("lang_runtime_perf_map");
    if (report.perfCommand)
        report.capabilities.push_back("perf_dwarf_call_graph");
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
