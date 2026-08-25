#pragma once

#include "common/PythonRuntimeProfiler.h"

#include <string>
#include <vector>

namespace drop
{

struct MemrayProfileResult
{
    std::string profileID;
    std::string readyPath;
    int pid = 0;
    // 阶段三：Memray sample 必须携带完整进程身份（pid + process_start_ms +
    // exe），process Session fan-out 才能按实例精确归属，防止 PID 复用串流。
    int64_t processStartMs = 0;
    std::string comm;
    std::string exe;
    bool ready = false;
    std::string reason;
    std::vector<PythonStackSample> samples;
};

struct MemrayProfileIdentity
{
    std::string profileID;
    int namespacePid = 0;
    uint64_t startTicks = 0;
};

struct MemrayProcessIdentity
{
    int hostPid = 0;
    int namespacePid = 0;
    uint64_t startTicks = 0;
    std::string rootIdentity;
    std::string comm;
    std::string exe;
};

std::vector<MemrayProfileResult> collect_memray_profiles(size_t maxProfiles = 32);
bool parse_memray_profile_identity(const std::string &path, MemrayProfileIdentity *identity);
int resolve_memray_profile_process(const MemrayProfileIdentity &profile,
                                   const std::string &rootIdentity,
                                   const std::vector<MemrayProcessIdentity> &processes);
bool acknowledge_memray_profile(const std::string &readyPath);
void release_memray_profile(const std::string &readyPath);

} // namespace drop
