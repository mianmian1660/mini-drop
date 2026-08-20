// ============================================================
// common/RuntimeSymbolMap.cpp — Java/Node/Python JIT perf map 自动接入
// ============================================================

#include "common/RuntimeSymbolMap.h"
#include "common/Utils.h"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <dirent.h>
#include <fstream>
#include <iostream>
#include <map>
#include <mutex>
#include <sstream>
#include <set>
#include <sys/stat.h>
#include <unistd.h>

namespace drop
{

namespace
{

// 每轮最多枚举的 /proc 目录项（防御，避免超大 /proc 拖慢窗口）
static constexpr size_t kMaxScanProcEntries = 4096;
// Java 刷新：每轮最多处理的 JVM 数
static constexpr int kMaxJvmRefreshPerRound = 8;
// Java 刷新：每轮总时间预算（ms）
static constexpr int64_t kJavaRefreshBudgetMs = 5000;
// Java 刷新：每 PID 冷却（ms）
static constexpr int64_t kJavaRefreshCooldownMs = 60 * 1000;

static std::map<int, int64_t> g_javaLastRefreshMap;
static std::mutex g_javaRefreshMutex;

std::string read_exe_symlink(int pid)
{
    std::string path = "/proc/" + std::to_string(pid) + "/exe";
    char buf[4096];
    ssize_t n = readlink(path.c_str(), buf, sizeof(buf) - 1);
    if (n <= 0)
        return "";
    buf[n] = '\0';
    return std::string(buf);
}

std::string basename_of(const std::string &path)
{
    size_t slash = path.rfind('/');
    return slash == std::string::npos ? path : path.substr(slash + 1);
}

bool starts_with(const std::string &s, const char *prefix)
{
    return s.rfind(prefix, 0) == 0;
}

// 识别 exe basename 属于哪种 runtime
enum class RuntimeKind
{
    None,
    Java,
    Node,
    Python,
};

RuntimeKind kind_from_exe(const std::string &exe)
{
    std::string base = basename_of(exe);
    if (base == "java" || starts_with(base, "java"))
        return RuntimeKind::Java;
    if (base == "node" || starts_with(base, "node"))
        return RuntimeKind::Node;
    if (starts_with(base, "python"))
        return RuntimeKind::Python;
    return RuntimeKind::None;
}

// 枚举 /proc 下所有 java/node/python 进程（按 exe basename），并求与采样 PID 的交集。
// minSamples：样本数低于该值的进程不视为"被关注的 runtime 进程"（避免把只被
// 采样到 1~2 次的基础设施进程如 analysis_daemon 也标记为 missing）。
void enumerate_runtime_pids(const std::map<int, int> &sampledPids,
                            int minSamples,
                            std::vector<int> *javaPids,
                            std::vector<int> *nodePids,
                            std::vector<int> *pythonPids)
{
    DIR *dir = opendir("/proc");
    if (!dir)
        return;
    struct dirent *ent;
    size_t scanned = 0;
    while ((ent = readdir(dir)) != nullptr && scanned++ < kMaxScanProcEntries)
    {
        if (ent->d_name[0] < '0' || ent->d_name[0] > '9')
            continue;
        int pid = std::atoi(ent->d_name);
        if (pid <= 0)
            continue;
        auto it = sampledPids.find(pid);
        if (it == sampledPids.end() || it->second < minSamples)
            continue;
        RuntimeKind kind = kind_from_exe(read_exe_symlink(pid));
        if (kind == RuntimeKind::Java)
            javaPids->push_back(pid);
        else if (kind == RuntimeKind::Node)
            nodePids->push_back(pid);
        else if (kind == RuntimeKind::Python)
            pythonPids->push_back(pid);
    }
    closedir(dir);
}

// 从 perf.data 提取被采样到的 PID -> 样本数（perf script -F comm,pid）
std::map<int, int> sampled_pids_from_perf_data(const std::string &perfBin, const std::string &dataPath)
{
    std::map<int, int> pidCounts;
    if (perfBin.empty() || dataPath.empty())
        return pidCounts;
    std::string out;
    // 注意：不能加 -q！perf script 的 -q 会连同样本行一起抑制，导致输出为空
    //（实测 perf 5.15）。格式为 "       comm pid"，comm 左对齐填充。
    int rc = exec_capture({perfBin, "script", "-i", dataPath, "-F", "comm,pid"}, &out, 4 * 1024 * 1024);
    if (rc != 0)
        return pidCounts;
    std::istringstream iss(out);
    std::string line;
    while (std::getline(iss, line))
    {
        std::istringstream ls(line);
        std::string comm, pidToken;
        if (!(ls >> comm >> pidToken))
            continue;
        size_t slash = pidToken.find('/');
        if (slash != std::string::npos)
            pidToken = pidToken.substr(0, slash);
        int pid = std::atoi(pidToken.c_str());
        if (pid > 0)
            pidCounts[pid]++;
    }
    return pidCounts;
}

} // namespace

int64_t runtime_now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

std::string runtime_perf_map_host_path(int pid)
{
    return "/tmp/perf-" + std::to_string(pid) + ".map";
}

int runtime_pid_namespace_pid(int hostPid)
{
    if (hostPid <= 0)
        return hostPid;
    std::ifstream statusFile("/proc/" + std::to_string(hostPid) + "/status");
    std::string line;
    while (std::getline(statusFile, line))
    {
        if (line.rfind("NSpid:", 0) != 0)
            continue;
        std::istringstream values(line.substr(std::strlen("NSpid:")));
        int pid = 0;
        int namespacePid = hostPid;
        while (values >> pid)
            namespacePid = pid;
        return namespacePid;
    }
    return hostPid;
}

std::string runtime_perf_map_pid_root_path(int hostPid, int runtimePid)
{
    return "/proc/" + std::to_string(hostPid) + "/root/tmp/perf-" + std::to_string(runtimePid) + ".map";
}

bool runtime_process_start_ms(int pid, int64_t *out)
{
    if (pid <= 0 || !out)
        return false;
    std::ifstream statFile("/proc/" + std::to_string(pid) + "/stat");
    std::string content;
    if (!std::getline(statFile, content) || content.empty())
        return false;
    size_t close = content.rfind(')');
    if (close == std::string::npos)
        return false;
    std::istringstream ss(content.substr(close + 1));
    std::string token;
    std::vector<std::string> fields;
    while (ss >> token)
        fields.push_back(token);
    // fields[0] 对应 stat 第 3 字段(state)，starttime 是第 22 字段 → fields[19]
    if (fields.size() < 20)
        return false;
    unsigned long long startTicks = std::strtoull(fields[19].c_str(), nullptr, 10);

    double uptime = 0;
    std::ifstream upFile("/proc/uptime");
    upFile >> uptime;
    if (uptime <= 0)
        return false;
    long ticksPerSec = sysconf(_SC_CLK_TCK);
    if (ticksPerSec <= 0)
        return false;

    double startSecSinceBoot = static_cast<double>(startTicks) / static_cast<double>(ticksPerSec);
    int64_t nowMs = runtime_now_ms();
    int64_t bootEpochMs = nowMs - static_cast<int64_t>(uptime * 1000.0);
    *out = bootEpochMs + static_cast<int64_t>(startSecSinceBoot * 1000.0);
    return true;
}

bool runtime_map_ready(const std::string &path, int64_t processStartMs)
{
    struct stat st;
    if (::stat(path.c_str(), &st) != 0)
        return false;
    if (st.st_size <= 0)
        return false;
    if (processStartMs > 0)
    {
        int64_t mtimeMs = static_cast<int64_t>(st.st_mtime) * 1000;
        // 允许 3s 容差（mtime 秒级精度 + 换算误差）；map 不能比进程启动早太多，
        // 否则是 PID 复用的旧文件。
        if (mtimeMs < processStartMs - 3000)
            return false;
    }
    return true;
}

bool runtime_same_file(const std::string &a, const std::string &b)
{
    struct stat sa, sb;
    if (::stat(a.c_str(), &sa) != 0 || ::stat(b.c_str(), &sb) != 0)
        return false;
    return sa.st_dev == sb.st_dev && sa.st_ino == sb.st_ino;
}

bool runtime_copy_map_atomic(const std::string &src, const std::string &dst)
{
    if (src == dst)
        return true;
    if (runtime_same_file(src, dst))
        return true;
    std::string tmp = dst + ".tmp" + std::to_string(::getpid());
    {
        std::ifstream in(src, std::ios::binary);
        if (!in.is_open())
            return false;
        std::ofstream out(tmp, std::ios::binary);
        if (!out.is_open())
            return false;
        out << in.rdbuf();
        out.flush();
        if (!out.good())
        {
            ::remove(tmp.c_str());
            return false;
        }
    }
    if (::rename(tmp.c_str(), dst.c_str()) != 0)
    {
        ::remove(tmp.c_str());
        return false;
    }
    return true;
}

bool runtime_java_refresh_allowed(int pid, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_javaRefreshMutex);
    auto it = g_javaLastRefreshMap.find(pid);
    if (it == g_javaLastRefreshMap.end())
        return true;
    return nowMs - it->second >= kJavaRefreshCooldownMs;
}

void runtime_java_refresh_record(int pid, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_javaRefreshMutex);
    g_javaLastRefreshMap[pid] = nowMs;
    // 防 map 无限增长：超过一定数量清理最旧的
    if (g_javaLastRefreshMap.size() > 4096)
    {
        int64_t oldest = INT64_MAX;
        int oldestPid = 0;
        for (const auto &kv : g_javaLastRefreshMap)
        {
            if (kv.second < oldest)
            {
                oldest = kv.second;
                oldestPid = kv.first;
            }
        }
        if (oldestPid)
            g_javaLastRefreshMap.erase(oldestPid);
    }
}

std::string runtime_aggregate_status(const RuntimeSymbolReport &report)
{
    bool anyDetected = report.java.detected || report.node.detected || report.python.detected;
    if (!anyDetected)
        return "complete";

    auto runtimeReady = [](const RuntimeMapInfo &info) {
        return info.detected && info.ready && info.missingPids.empty();
    };
    bool anyReady = runtimeReady(report.java) || runtimeReady(report.node) || runtimeReady(report.python);
    bool allReady = (!report.java.detected || runtimeReady(report.java)) &&
                    (!report.node.detected || runtimeReady(report.node)) &&
                    (!report.python.detected || runtimeReady(report.python));

    if (allReady)
        return "complete";
    if (anyReady)
        return "partial";
    return "missing";
}

namespace
{

std::string json_escape_runtime(const std::string &s)
{
    std::string out;
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

std::string info_to_json(const RuntimeMapInfo &info)
{
    std::string body = "{";
    body += "\"detected\":" + std::string(info.detected ? "true" : "false") + ",";
    body += "\"ready\":" + std::string(info.ready ? "true" : "false") + ",";
    if (!info.requiredFlag.empty())
        body += "\"required_flag\":\"" + json_escape_runtime(info.requiredFlag) + "\",";
    body += "\"missing\":[";
    for (size_t i = 0; i < info.missingPids.size(); ++i)
    {
        if (i)
            body += ",";
        body += std::to_string(info.missingPids[i]);
    }
    body += "],";
    body += "\"ready_pids\":[";
    for (size_t i = 0; i < info.readyPids.size(); ++i)
    {
        if (i)
            body += ",";
        body += std::to_string(info.readyPids[i]);
    }
    body += "],";
    body += "\"reason\":\"" + json_escape_runtime(info.reason) + "\"";
    body += "}";
    return body;
}

} // namespace

std::string runtime_report_to_json(const RuntimeSymbolReport &report)
{
    std::string body = "{";
    body += "\"symbol_status\":\"" + report.status + "\",";
    body += "\"runtime_maps\":" + runtime_maps_to_json(report);
    body += "}";
    return body;
}

std::string runtime_maps_to_json(const RuntimeSymbolReport &report)
{
    std::string body = "{";
    body += "\"java\":" + info_to_json(report.java) + ",";
    body += "\"node\":" + info_to_json(report.node) + ",";
    body += "\"python\":" + info_to_json(report.python);
    body += "}";
    return body;
}

RuntimeSymbolReport collect_runtime_maps(const std::string &perfBin, const std::string &dataPath)
{
    RuntimeSymbolReport report;
    int64_t nowMs = runtime_now_ms();

    // 1. 提取被采样 PID -> 样本数
    std::map<int, int> sampled = sampled_pids_from_perf_data(perfBin, dataPath);
    report.sampledPids = sampled;
    if (sampled.empty())
    {
        // perf 无样本：本窗没有可符号化的数据，不把符号问题归因给 runtime map。
        report.status = "complete";
        return report;
    }

    // 样本数达到该阈值的进程才视为"被关注的 runtime 进程"，避免把只被采样到
    // 1~2 次的基础设施进程（如 analysis_daemon python）也标记为 missing。
    const int kMinRuntimeSamples = 3;

    // 2. 枚举 java/node/python 采样进程（按 exe basename）
    std::vector<int> javaPids, nodePids, pythonPids;
    enumerate_runtime_pids(sampled, kMinRuntimeSamples, &javaPids, &nodePids, &pythonPids);

    report.java.detected = !javaPids.empty();
    report.node.detected = !nodePids.empty();
    report.python.detected = !pythonPids.empty();

    auto ensureReady = [&](int pid, RuntimeMapInfo *info) {
        std::string rootPath = runtime_perf_map_pid_root_path(pid, runtime_pid_namespace_pid(pid));
        std::string hostPath = runtime_perf_map_host_path(pid);
        int64_t procStart = 0;
        bool haveStart = runtime_process_start_ms(pid, &procStart);
        const std::string &src = runtime_map_ready(rootPath, haveStart ? procStart : 0)
                                     ? rootPath
                                     : (runtime_map_ready(hostPath, haveStart ? procStart : 0) ? hostPath : "");
        if (src.empty())
            return false;
        if (runtime_same_file(src, hostPath) || runtime_copy_map_atomic(src, hostPath))
        {
            info->ready = true;
            info->readyPids.push_back(pid);
            return true;
        }
        return false;
    };

    // 3. Java：周期性刷新 perf map（asprof），冷却 + 预算控制。
    int jvmProcessed = 0;
    int64_t refreshBudgetMs = kJavaRefreshBudgetMs;
    for (int pid : javaPids)
    {
        if (jvmProcessed >= kMaxJvmRefreshPerRound)
        {
            report.skippedRefresh++;
            report.java.missingPids.push_back(pid);
            continue;
        }
        bool hadReadyMap = ensureReady(pid, &report.java);
        // 即使已有 map，JVM 仍可能在后续窗口编译新方法，因此冷却到期后刷新。
        if (refreshBudgetMs > 0 && runtime_java_refresh_allowed(pid, nowMs))
        {
            int64_t t0 = runtime_now_ms();
            std::string out;
            // asprof 位于 agent 镜像 /usr/local/bin/asprof（Dockerfile 内置）
            int rc = exec_capture({"timeout", "2", "asprof", "jcmd", std::to_string(pid), "Compiler.perfmap"}, &out, 8192);
            runtime_java_refresh_record(pid, nowMs);
            refreshBudgetMs -= (runtime_now_ms() - t0);
            if (rc == 0 && ensureReady(pid, &report.java))
            {
                jvmProcessed++;
                continue;
            }
            if (rc != 0)
            {
                if (report.java.reason.empty())
                    report.java.reason = "asprof jcmd refresh failed rc=" + std::to_string(rc);
            }
        }
        else
        {
            report.skippedRefresh++;
        }
        if (!hadReadyMap)
        {
            report.java.missingPids.push_back(pid);
            if (report.java.reason.empty())
                report.java.reason = "no JIT perf map";
        }
        jvmProcessed++;
    }

    // 4. Node / Python：不注入，只标记 required_flag
    report.node.requiredFlag = "--perf-basic-prof";
    for (int pid : nodePids)
    {
        if (ensureReady(pid, &report.node))
            continue;
        report.node.missingPids.push_back(pid);
        if (report.node.reason.empty())
            report.node.reason = "missing --perf-basic-prof flag";
    }

    report.python.requiredFlag = "-X perf";
    for (int pid : pythonPids)
    {
        if (ensureReady(pid, &report.python))
            continue;
        report.python.missingPids.push_back(pid);
        if (report.python.reason.empty())
            report.python.reason = "missing -X perf flag";
    }

    auto finalizeRuntimeInfo = [](RuntimeMapInfo *info) {
        info->ready = info->detected && !info->readyPids.empty() && info->missingPids.empty();
    };
    finalizeRuntimeInfo(&report.java);
    finalizeRuntimeInfo(&report.node);
    finalizeRuntimeInfo(&report.python);

    // 5. 聚合 symbol_status
    report.status = runtime_aggregate_status(report);

    std::cout << "[native-cp] runtime_maps status=" << report.status
              << " java(detected=" << (report.java.detected ? "y" : "n")
              << ",ready=" << (report.java.ready ? "y" : "n")
              << ",missing=" << report.java.missingPids.size() << ")"
              << " node(detected=" << (report.node.detected ? "y" : "n")
              << ",ready=" << (report.node.ready ? "y" : "n")
              << ",missing=" << report.node.missingPids.size() << ")"
              << " python(detected=" << (report.python.detected ? "y" : "n")
              << ",ready=" << (report.python.ready ? "y" : "n")
              << ",missing=" << report.python.missingPids.size() << ")"
              << " skipped=" << report.skippedRefresh << std::endl;
    return report;
}

} // namespace drop
