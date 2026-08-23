// ============================================================
// agent/RunnerUtils.cpp — 实现
// ============================================================
// 逻辑逐字对齐旧 agent/main.cpp 里的 RemoteKeyFor/ContentTypeFor/
// get_error_message/get_error_code（现在是自由函数，用 profilerName
// 字符串匹配，不再依赖文件路径后缀猜测——每个 Runner 子类现在能直接
// 传自己确切的 profilerName，不需要靠路径反推）。
// ============================================================

#include "agent/RunnerUtils.h"

#include <cctype>
#include <cstdlib>
#include <sys/stat.h>

using namespace std;

namespace drop_agent
{

    bool LayoutV2Enabled()
    {
        const char *raw = getenv("DROP_AGENT_LAYOUT_V2");
        if (raw == nullptr || *raw == '\0')
            return true; // 阶段 4 Release C 默认开启
        string s(raw);
        return s == "1" || s == "true" || s == "yes" || s == "on" ||
               s == "TRUE" || s == "YES" || s == "ON";
    }

    bool ValidRemoteBasename(const string &base)
    {
        if (base.empty() || base == "." || base == "..")
            return false;
        for (unsigned char c : base)
        {
            if (!(isalnum(c) || c == '-' || c == '_' || c == '.'))
                return false;
        }
        return true;
    }

    string RawObjectKey(const string &tid, uint64_t attemptId, const string &basename)
    {
        if (LayoutV2Enabled() && attemptId > 0 && ValidRemoteBasename(basename))
        {
            return "tasks/" + tid + "/attempts/" + to_string(attemptId) +
                   "/raw/" + basename;
        }
        return tid + "/" + basename;
    }

    string ManifestObjectKey(const string &tid, uint64_t attemptId)
    {
        if (LayoutV2Enabled() && attemptId > 0)
        {
            return "tasks/" + tid + "/attempts/" + to_string(attemptId) +
                   "/manifest.json";
        }
        return tid + "/manifest.json";
    }

    string ResolveOutputPath(const string &basePath)
    {
        auto exists = [](const string &p)
        {
            struct stat st
            {
            };
            return stat(p.c_str(), &st) == 0;
        };
        if (exists(basePath))
            return basePath;
        if (exists(basePath + ".collapsed"))
            return basePath + ".collapsed";
        if (exists(basePath + ".pb.gz"))
            return basePath + ".pb.gz";
        if (exists(basePath + ".bpf"))
            return basePath + ".bpf";
        if (exists(basePath + ".bpf.raw"))
            return basePath + ".bpf.raw";
        return basePath;
    }

    string RemoteKeyFor(const string &profilerName)
    {
        if (profilerName.find("eBPF") != string::npos)
            return "raw.bpf";
        if (profilerName.find("async-profiler") != string::npos)
            return "profile.collapsed";
        if (profilerName.find("pprof") != string::npos)
            return "profile.pb.gz";
        return "perf.data";
    }

    string ContentTypeFor(const string &profilerName)
    {
        if (profilerName.find("eBPF") != string::npos || profilerName.find("async-profiler") != string::npos)
            return "text/plain; charset=utf-8";
        if (profilerName.find("pprof") != string::npos)
            return "application/gzip";
        return "application/octet-stream";
    }

    string GetErrorMessage(int resultCode, const string &profilerName, const hotmethod::TaskDesc &task)
    {
        switch (resultCode)
        {
        case 0:
            return "";
        case -7:
            return "任务已被取消（Server 下发取消指令或用户主动取消）";
        case -4:
            if (profilerName == "pprof")
                return "无法连接 Go pprof：请确认 pprof_url 可从 Agent 访问并已启用 /debug/pprof/profile";
            return "目标 PID " + to_string(task.sampleargv().pid()) + " 不存在";
        case -3:
            return profilerName + " 采集超时（" + to_string(task.timeoutsec()) + "秒）";
        case -6:
            if (profilerName == "pprof")
                return "pprof 返回的不是有效 gzip profile；请确认 /debug/pprof/profile 已启用且未被代理改写";
            return profilerName + " 未生成有效采集文件";
        case -1:
        case -2:
        case -5:
            if (profilerName == "eBPF")
            {
                string event = task.sampleargv().event();
                if (event.empty())
                    event = "cpu";
                if (resultCode == -5)
                    return "eBPF 采集失败：BPFTRACE_UNAVAILABLE，请确认 Agent 容器内已安装 bpftrace，resultCode=" + to_string(resultCode);
                if (event == "cpu")
                    return "eBPF CPU 采集失败：NO_EBPF_SAMPLES，请提高采样频率/加长 duration，或确认 kstack/ustack 权限可用，resultCode=" + to_string(resultCode);
                return "eBPF 采集失败：NO_EBPF_SAMPLES，请确认 bpftrace/tracefs 权限可用，并在采集窗口内制造 " + event + " 负载，resultCode=" + to_string(resultCode);
            }
            return profilerName + " 进程异常, resultCode=" + to_string(resultCode);
        default:
            return profilerName + " 采集失败, exitCode=" + to_string(resultCode);
        }
    }

    string GetErrorCode(int resultCode, const string &profilerName)
    {
        if (resultCode == 0)
            return "";
        if (resultCode == -7)
            return "TASK_CANCELED";
        if (resultCode == -3)
            return "TASK_TIMEOUT";
        if (resultCode == -4)
            return "TARGET_NOT_FOUND";
        if (resultCode == -6)
            return "ARTIFACT_MISSING";
        if (profilerName.find("eBPF") != string::npos)
        {
            if (resultCode == -2)
                return "NO_EBPF_SAMPLES";
            if (resultCode == -5)
                return "BPFTRACE_UNAVAILABLE";
            return "EBPF_UNAVAILABLE";
        }
        if (profilerName.find("pprof") != string::npos)
            return "PPROF_UNAVAILABLE";
        return "TASK_EXECUTION_FAILED";
    }

    bool FileExistsNonEmpty(const string &path)
    {
        struct stat st
        {
        };
        return stat(path.c_str(), &st) == 0 && st.st_size > 0;
    }

} // namespace drop_agent
