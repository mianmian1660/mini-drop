#include "agent/ContinuousSessionManager.h"

#include "agent/AgentUtils.h"
#include "common/ContinuousSegmentProcessor.h"
#include "common/Utils.h"

#include <nlohmann/json.hpp>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <dirent.h>
#include <fcntl.h>
#include <fstream>
#include <iostream>
#include <set>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace drop_agent
{
namespace
{
using json = nlohmann::json;

uint64_t env_uint64(const char *name, uint64_t fallback)
{
    std::string raw = EnvString(name);
    if (raw.empty())
        return fallback;
    char *end = nullptr;
    unsigned long long value = std::strtoull(raw.c_str(), &end, 10);
    return end && *end == '\0' ? static_cast<uint64_t>(value) : fallback;
}

// apiserver's model.ContinuousSession.Labels is `[]byte` (raw jsonb bytes from
// Postgres). Go's encoding/json always base64-encodes a []byte field when it
// has no custom MarshalJSON, so the reconcile response's "labels" is a base64
// string wrapping a JSON object, not a nested JSON object directly — must
// decode before parsing. No base64 decoder existed in the C++ agent yet.
std::string base64_decode(const std::string &encoded)
{
    static const std::string kAlphabet =
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::vector<int> table(256, -1);
    for (size_t i = 0; i < kAlphabet.size(); ++i)
        table[static_cast<unsigned char>(kAlphabet[i])] = static_cast<int>(i);

    std::string out;
    int bits = 0;
    int bitCount = 0;
    for (unsigned char c : encoded)
    {
        if (c == '=' || c == '\n' || c == '\r')
            continue;
        int value = table[c];
        if (value < 0)
            continue;
        bits = (bits << 6) | value;
        bitCount += 6;
        if (bitCount >= 8)
        {
            bitCount -= 8;
            out.push_back(static_cast<char>((bits >> bitCount) & 0xFF));
        }
    }
    return out;
}

bool numeric_name(const char *name)
{
    if (!name || !*name)
        return false;
    for (const char *p = name; *p; ++p)
        if (*p < '0' || *p > '9')
            return false;
    return true;
}

std::string read_line(const std::string &path)
{
    std::ifstream in(path);
    std::string line;
    std::getline(in, line);
    return line;
}

std::string read_link(const std::string &path)
{
    char buffer[4096];
    ssize_t size = ::readlink(path.c_str(), buffer, sizeof(buffer) - 1);
    if (size <= 0)
        return "";
    buffer[size] = '\0';
    std::string value(buffer);
    const std::string deletedSuffix = " (deleted)";
    if (value.size() > deletedSuffix.size() &&
        value.compare(value.size() - deletedSuffix.size(), deletedSuffix.size(), deletedSuffix) == 0)
        value.resize(value.size() - deletedSuffix.size());
    return value;
}

int64_t boot_time_ms()
{
    static int64_t cached = 0;
    if (cached > 0)
        return cached;
    std::ifstream in("/proc/stat");
    std::string key;
    int64_t seconds = 0;
    while (in >> key)
    {
        if (key == "btime")
        {
            in >> seconds;
            break;
        }
        std::string rest;
        std::getline(in, rest);
    }
    cached = seconds * 1000;
    return cached;
}

int64_t process_start_ms(int pid)
{
    std::string stat = read_line("/proc/" + std::to_string(pid) + "/stat");
    size_t close = stat.rfind(')');
    if (close == std::string::npos || close + 2 >= stat.size())
        return 0;
    std::istringstream fields(stat.substr(close + 2));
    std::string value;
    uint64_t ticks = 0;
    // starttime is field 22; the substring begins at field 3.
    for (int field = 3; field <= 22; ++field)
    {
        if (!(fields >> value))
            return 0;
        if (field == 22)
            ticks = std::strtoull(value.c_str(), nullptr, 10);
    }
    long hz = ::sysconf(_SC_CLK_TCK);
    if (hz <= 0 || boot_time_ms() <= 0)
        return 0;
    return boot_time_ms() + static_cast<int64_t>(ticks * 1000 / static_cast<uint64_t>(hz));
}

int process_tgid(int pid)
{
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

uint64_t process_rss_bytes(int pid)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/status");
    std::string key;
    while (in >> key)
    {
        if (key == "VmRSS:")
        {
            uint64_t kb = 0;
            in >> kb;
            return kb * 1024;
        }
        std::string rest;
        std::getline(in, rest);
    }
    return 0;
}

bool same_targets(const std::vector<drop::ContinuousTargetProcess> &left,
                  const std::vector<drop::ContinuousTargetProcess> &right)
{
    if (left.size() != right.size())
        return false;
    for (size_t index = 0; index < left.size(); ++index)
        if (left[index].pid != right[index].pid || left[index].processStartMs != right[index].processStartMs)
            return false;
    return true;
}

bool atomic_write(const std::string &path, const std::string &body)
{
    std::string temporary = path + ".tmp." + std::to_string(::getpid());
    int fd = ::open(temporary.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0600);
    if (fd < 0)
        return false;
    size_t offset = 0;
    while (offset < body.size())
    {
        ssize_t count = ::write(fd, body.data() + offset, body.size() - offset);
        if (count < 0 && errno == EINTR)
            continue;
        if (count <= 0)
        {
            ::close(fd);
            ::unlink(temporary.c_str());
            return false;
        }
        offset += static_cast<size_t>(count);
    }
    bool ok = ::fsync(fd) == 0 && ::close(fd) == 0 && ::rename(temporary.c_str(), path.c_str()) == 0;
    if (!ok)
        ::unlink(temporary.c_str());
    return ok;
}

} // namespace

// 阶段六：读取 /proc/<pid>/cgroup 的 cgroup 路径。
//   - cgroup v2:  "0::/system.slice/docker-abc123.scope"
//   - cgroup v1:  "0:name=systemd:/system.slice/foo" 或 "2:cpu:/docker/abc123"
// 返回去掉控制器前缀后的路径（以 / 开头）；无法读取时返回空字符串。
std::string process_cgroup_path(int pid)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/cgroup");
    std::string line;
    while (std::getline(in, line))
    {
        if (line.empty())
            continue;
        // 取最后一个 ':' 之后的部分（v2 是 "0::/path"，v1 是 "N:controller:/path"）。
        size_t colon = line.rfind(':');
        if (colon == std::string::npos)
            continue;
        std::string path = line.substr(colon + 1);
        if (!path.empty() && path[0] == '/')
            return path;
    }
    return "";
}

// 阶段六：从 cgroup 路径提取 container ID。支持常见运行时路径模式：
//   - /docker/<64hex>、/kubepods/.../docker/<64hex>
//   - /kubepods/.../cri-o-<64hex>、/kubepods/.../containerd/<64hex>
//   - /system.slice/docker-<64hex>.scope、containerd-<64hex>.scope、
//     crio-<64hex>.scope
// 无法识别时返回空字符串（调用方上报 unsupported/waiting 原因）。
std::string extract_container_id(const std::string &cgroupPath)
{
    if (cgroupPath.empty())
        return "";
    const std::string path = cgroupPath;
    // 1. 形如 /docker/<id> 或 /kubepods/.../docker/<id> 的段。
    size_t pos = 0;
    while ((pos = path.find('/', pos)) != std::string::npos)
    {
        size_t segmentStart = pos + 1;
        size_t segmentEnd = path.find('/', segmentStart);
        std::string segment = path.substr(segmentStart, segmentEnd == std::string::npos ? std::string::npos : segmentEnd - segmentStart);
        if (segment == "docker" || segment == "containerd" || segment == "cri-containerd")
        {
            if (segmentEnd != std::string::npos)
            {
                size_t idStart = segmentEnd + 1;
                size_t idEnd = path.find('/', idStart);
                std::string id = path.substr(idStart, idEnd == std::string::npos ? std::string::npos : idEnd - idStart);
                if (id.size() >= 12 && id.size() <= 64 &&
                    id.find_first_not_of("0123456789abcdefABCDEF") == std::string::npos)
                    return id;
            }
        }
        pos = segmentEnd == std::string::npos ? path.size() : segmentEnd;
    }
    // 2. 形如 /system.slice/docker-<id>.scope（systemd 驱动）。
    for (const char *prefix : {"docker-", "containerd-", "crio-", "libpod-"})
    {
        std::string marker = std::string("/") + prefix;
        size_t found = path.find(marker);
        if (found == std::string::npos)
            continue;
        size_t idStart = found + marker.size();
        size_t idEnd = path.find('.', idStart);
        std::string id = path.substr(idStart, idEnd == std::string::npos ? std::string::npos : idEnd - idStart);
        if (id.size() >= 12 && id.size() <= 64 &&
            id.find_first_not_of("0123456789abcdefABCDEF") == std::string::npos)
            return id;
    }
    // 3. 形如 /kubepods/.../cri-o-<id>（CRI-O 无斜杠分隔）。
    {
        std::string marker = "/cri-o-";
        size_t found = path.find(marker);
        if (found != std::string::npos)
        {
            size_t idStart = found + marker.size();
            size_t idEnd = path.find('/', idStart);
            std::string id = path.substr(idStart, idEnd == std::string::npos ? std::string::npos : idEnd - idStart);
            if (id.size() >= 12 && id.size() <= 64 &&
                id.find_first_not_of("0123456789abcdefABCDEF") == std::string::npos)
                return id;
        }
    }
    return "";
}

std::vector<drop::ContinuousTargetProcess> MatchContinuousProcessesByExe(
    const std::vector<drop::ContinuousTargetProcess> &processes,
    const std::string &selectorExe)
{
    std::vector<drop::ContinuousTargetProcess> matches;
    for (const auto &process : processes)
        if (!selectorExe.empty() && process.exe == selectorExe)
            matches.push_back(process);
    return matches;
}

// 阶段六：按 selector 模式匹配进程（见头文件注释）。
std::vector<drop::ContinuousTargetProcess> MatchContinuousProcessesBySelector(
    const std::vector<drop::ContinuousTargetProcess> &processes,
    const ContinuousAssignment &assignment)
{
    std::vector<drop::ContinuousTargetProcess> matches;
    if (assignment.scope != "process")
        return matches;
    for (const auto &process : processes)
    {
        if (assignment.selectorMode == "pid_instance")
        {
            // 精确三元组匹配：PID 复用后新进程的 start time 不同，不会误匹配。
            if (process.pid == assignment.selectorPid &&
                process.processStartMs == assignment.selectorProcessStartMs &&
                !assignment.selectorExe.empty() && process.exe == assignment.selectorExe)
                matches.push_back(process);
        }
        else if (assignment.selectorMode == "cgroup")
        {
            if (!assignment.selectorCgroup.empty() && process.cgroup == assignment.selectorCgroup)
                matches.push_back(process);
        }
        else if (assignment.selectorMode == "container_id")
        {
            if (!assignment.selectorContainerId.empty() &&
                process.containerId == assignment.selectorContainerId)
                matches.push_back(process);
        }
        else // exe_all_instances（含历史 all_instances 归一化）
        {
            if (!assignment.selectorExe.empty() && process.exe == assignment.selectorExe)
                matches.push_back(process);
        }
    }
    return matches;
}

ContinuousSessionManager::ContinuousSessionManager(const AgentConfig &config,
                                                   std::string apiBaseURL,
                                                   std::string authUID,
                                                   std::atomic<bool> &agentRunning)
    : config_(config), apiBaseURL_(std::move(apiBaseURL)), authUID_(std::move(authUID)), agentRunning_(agentRunning)
{
    spoolDirectory_ = EnvString("DROP_NATIVE_CP_SPOOL_DIR", "/var/lib/mini-drop/continuous-spool");
    cachePath_ = spoolDirectory_ + "/assignments.json";
    spoolMaxBytes_ = env_uint64("DROP_NATIVE_CP_SPOOL_MAX_BYTES", spoolMaxBytes_);
    spoolMinFreeBytes_ = env_uint64("DROP_NATIVE_CP_SPOOL_MIN_FREE_BYTES", spoolMinFreeBytes_);
    retryMaxSec_ = static_cast<int>(env_uint64("DROP_NATIVE_CP_RETRY_MAX_SEC", 300));
}

ContinuousSessionManager::~ContinuousSessionManager()
{
    Stop();
}

void ContinuousSessionManager::Start()
{
    if (!EnvEnabled("DROP_NATIVE_CP_ENABLED") || running_.exchange(true))
        return;
    EnsureDirRecursive(spoolDirectory_);
    std::vector<ContinuousAssignment> cached;
    uint64_t cachedRevision = 0;
    if (LoadAssignmentCache(&cached, &cachedRevision))
    {
        revision_ = cachedRevision;
        ApplyAssignments(cached, ScanProcesses());
    }
    thread_ = std::thread(&ContinuousSessionManager::Loop, this);
}

void ContinuousSessionManager::Stop()
{
    running_ = false;
    if (thread_.joinable())
        thread_.join();
    if (sharedSampler_)
    {
        sharedSampler_->Stop();
        sharedSampler_.reset();
    }
    for (auto &entry : runtimes_)
        StopRuntime(entry.second);
    runtimes_.clear();
}

void ContinuousSessionManager::Loop()
{
    auto nextReconcile = std::chrono::steady_clock::time_point{};
    while (running_ && agentRunning_)
    {
        auto processes = ScanProcesses();
        RefreshTargets(processes);
        AdvanceStoppingSessions();
        auto now = std::chrono::steady_clock::now();
        if (now >= nextReconcile)
        {
            Reconcile(processes);
            nextReconcile = now + std::chrono::seconds(5);
        }
        for (int step = 0; step < 10 && running_ && agentRunning_; ++step)
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }
}

std::vector<drop::ContinuousTargetProcess> ContinuousSessionManager::ScanProcesses() const
{
    std::vector<drop::ContinuousTargetProcess> out;
    std::set<int> seenTgids;
    DIR *directory = ::opendir("/proc");
    if (!directory)
        return out;
    while (dirent *entry = ::readdir(directory))
    {
        if (!numeric_name(entry->d_name))
            continue;
        int pid = std::atoi(entry->d_name);
        int tgid = process_tgid(pid);
        // /proc contains one entry for every thread. A process selector follows
        // the executable instance, so register only the thread-group leader.
        // Passing transient TIDs to perf -p makes startup fail when a worker
        // thread exits between the scan and recorder attach.
        if (pid <= 0 || tgid != pid || !seenTgids.insert(tgid).second)
            continue;
        std::string base = "/proc/" + std::to_string(pid);
        std::string exe = read_link(base + "/exe");
        int64_t started = process_start_ms(pid);
        if (pid <= 0 || exe.empty() || started <= 0)
            continue;
        drop::ContinuousTargetProcess process;
        process.pid = pid;
        process.processStartMs = started;
        process.comm = read_line(base + "/comm");
        process.exe = exe;
        // 阶段六：cgroup 路径与 container ID（供 cgroup/container_id selector）。
        process.cgroup = process_cgroup_path(pid);
        process.containerId = extract_container_id(process.cgroup);
        out.push_back(std::move(process));
    }
    ::closedir(directory);
    std::sort(out.begin(), out.end(), [](const auto &left, const auto &right) { return left.pid < right.pid; });
    return out;
}

std::string ContinuousSessionManager::BuildReconcileBody(const std::vector<drop::ContinuousTargetProcess> &processes) const
{
    json body = {
        {"target_ip", config_.ipAddr}, {"hostname", config_.hostname}, {"agent_id", config_.uid},
        {"strict_capable", drop::CoreContinuousSamplerAvailable()}, {"capabilities", config_.capabilities}, {"revision", revision_},
    };
    body["processes"] = json::array();
    for (const auto &process : processes)
        body["processes"].push_back({{"pid", process.pid}, {"process_start_ms", process.processStartMs},
                                      {"comm", process.comm}, {"exe", process.exe},
                                      {"rss_bytes", process_rss_bytes(process.pid)},
                                      {"cgroup_path", process.cgroup}, {"container_id", process.containerId}});
    body["sessions"] = json::array();
    for (const auto &entry : runtimes_)
    {
        const Runtime &runtime = entry.second;
        json active = json::array();
        for (const auto &target : runtime.targets)
            active.push_back({{"pid", target.pid}, {"process_start_ms", target.processStartMs},
                              {"comm", target.comm}, {"exe", target.exe}, {"rss_bytes", process_rss_bytes(target.pid)}});
        std::string observedState = runtime.observedState;
        std::string degradationReason = runtime.degradationReason;
        // 阶段五：服务器存储压力时上报 waiting/server_storage_pressure
        // （不覆盖用户主动 stop，desired_state 仍由服务端裁决）。
        if (drop::ContinuousServerPressureHalted() &&
            (observedState == "running" || observedState == "pending" || observedState == "degraded"))
        {
            observedState = "waiting";
            degradationReason = "server_storage_pressure";
        }
        body["sessions"].push_back({{"sid", entry.first}, {"observed_state", observedState},
                                     {"active_processes", active}, {"continuity_mode", runtime.effectiveContinuityMode},
                                     {"degradation_reason", degradationReason},
                                     {"last_error", runtime.lastError}});
    }
    for (const auto &entry : stoppedReports_)
        body["sessions"].push_back({{"sid", entry.first}, {"observed_state", "stopped"},
                                     {"active_processes", json::array()}, {"continuity_mode", entry.second.continuityMode},
                                     {"degradation_reason", entry.second.degradationReason}, {"last_error", ""}});
    for (const auto &entry : stoppingRuntimes_)
        body["sessions"].push_back({{"sid", entry.first}, {"observed_state", "stopping"},
                                     {"active_processes", json::array()}, {"continuity_mode", entry.second.continuityMode},
                                     {"degradation_reason", entry.second.degradationReason},
                                     {"last_error", entry.second.lastError}});
    return body.dump();
}

bool ContinuousSessionManager::Reconcile(const std::vector<drop::ContinuousTargetProcess> &processes)
{
    std::string requestPath = spoolDirectory_ + "/reconcile-request.json";
    if (!atomic_write(requestPath, BuildReconcileBody(processes)))
        return false;
    std::string response;
    int rc = drop::exec_capture({"curl", "-sS", "-m", "10", "-X", "POST",
                                 "-H", "Content-Type: application/json",
                                 "-H", "Drop-User-Uid: " + authUID_,
                                 "-d", "@" + requestPath,
                                 apiBaseURL_ + "/api/v1/internal/continuous/reconcile"},
                                &response, 4 * 1024 * 1024);
    ::unlink(requestPath.c_str());
    if (rc != 0)
    {
        std::cerr << "[native-cp] reconcile failed rc=" << rc << std::endl;
        return false;
    }
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = revision_;
    if (!ParseAssignments(response, &assignments, &revision))
    {
        std::cerr << "[native-cp] invalid reconcile response" << std::endl;
        return false;
    }
    revision_ = revision;
    SaveAssignmentCache(response);
    ApplyAssignments(assignments, processes);
    stoppedReports_.clear();
    return true;
}

bool ContinuousSessionManager::ParseAssignments(const std::string &response,
                                                std::vector<ContinuousAssignment> *assignments,
                                                uint64_t *revision) const
{
    try
    {
        json root = json::parse(response);
        if (root.value("code", -1) != 0 || !root.contains("data"))
            return false;
        const json &data = root.at("data");
        *revision = data.value("revision", static_cast<uint64_t>(0));
        // 阶段五：服务器存储压力 → 全局暂停新窗口产生。
        if (data.contains("server_pressure"))
        {
            const json &pressure = data.at("server_pressure");
            drop::SetContinuousServerPressure(pressure.value("halted", false));
        }
        assignments->clear();
        for (const auto &item : data.value("assignments", json::array()))
        {
            ContinuousAssignment assignment;
            assignment.sid = item.value("sid", "");
            assignment.scope = item.value("scope", "host");
            assignment.selectorExe = item.value("selector_exe", "");
            // 阶段六：selector 模式与结构化参数。selector_params 是服务端
            // jsonb 的原始字节（Go []byte 无自定义 MarshalJSON 时 base64 编码），
            // 与 labels 相同处理：先 base64 解码再解析 JSON。
            assignment.selectorMode = item.value("selector_mode", "exe_all_instances");
            if (assignment.selectorMode == "all_instances")
                assignment.selectorMode = "exe_all_instances";
            if (item.contains("selector_params") && !item.at("selector_params").is_null())
            {
                const std::string paramsB64 = item.value("selector_params", std::string());
                if (!paramsB64.empty())
                {
                    try
                    {
                        json params = json::parse(base64_decode(paramsB64));
                        assignment.selectorPid = params.value("pid", 0);
                        assignment.selectorProcessStartMs = params.value("process_start_ms", static_cast<int64_t>(0));
                        if (params.contains("exe") && params.at("exe").is_string())
                            assignment.selectorExe = params.value("exe", assignment.selectorExe);
                        assignment.selectorCgroup = params.value("cgroup", "");
                        assignment.selectorContainerId = params.value("container_id", "");
                    }
                    catch (const std::exception &error)
                    {
                        std::cerr << "[native-cp] reconcile selector_params parse failed sid=" << assignment.sid
                                  << ": " << error.what() << std::endl;
                    }
                }
            }
            assignment.desiredState = item.value("desired_state", "running");
            assignment.continuityMode = item.value("continuity_mode", "degraded");
            assignment.allowDegraded = item.value("allow_degraded", assignment.continuityMode == "degraded");
            assignment.revision = item.value("revision", static_cast<uint64_t>(0));
            assignment.sampleRateHz = item.value("sample_rate_hz", 19);
            assignment.aggregationWindowSec = item.value("aggregation_window_sec", 10);
            assignment.uploadBatchSec = item.value("upload_batch_sec", 60);
            assignment.retentionHours = item.value("retention_hours", 24);
            // 阶段一：signals 字符串数组（Reconcile assignment DTO 显式下发）。
            // 空集合保持为空（BuildSamplerConfig 回退四类默认）。
            assignment.requestedSignals.clear();
            for (const auto &signal : item.value("signals", json::array()))
                if (signal.is_string() && !signal.get<std::string>().empty())
                    assignment.requestedSignals.push_back(signal.get<std::string>());
            if (assignment.requestedSignals.empty())
                assignment.requestedSignals = {"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"};
            // labels 是 base64 包一层 JSON（见 base64_decode 上方注释），解码失败
            // 或不含 db_targets 时静默跳过——数据库巡检是可选能力，不能因为
            // labels 格式问题拖垮整条 CPU/IO/sched 采集链路。labels 可能为
            // null（无 labels 的 Session），此时跳过解析。
            if (item.contains("labels") && !item.at("labels").is_null())
            {
                const std::string labelsB64 = item.value("labels", std::string());
                if (!labelsB64.empty())
                {
                    try
                    {
                        json labels = json::parse(base64_decode(labelsB64));
                        for (const auto &dbItem : labels.value("db_targets", json::array()))
                        {
                            drop::DBTargetConfig target;
                            target.engine = dbItem.value("engine", "");
                            target.instanceLabel = dbItem.value("instance_label", "");
                            target.host = dbItem.value("host", "");
                            target.port = dbItem.value("port", 0);
                            target.user = dbItem.value("user", "");
                            target.passwordRef = dbItem.value("password_ref", "");
                            target.pollIntervalSec = dbItem.value("poll_interval_sec", assignment.aggregationWindowSec);
                            target.queryTimeoutMs = dbItem.value("query_timeout_ms", 500);
                            if (!target.engine.empty() && !target.host.empty())
                                assignment.dbTargets.push_back(std::move(target));
                        }
                    }
                    catch (const std::exception &error)
                    {
                        std::cerr << "[native-cp] reconcile labels parse failed sid=" << assignment.sid
                                  << ": " << error.what() << std::endl;
                    }
                }
            }
            if (!assignment.sid.empty())
                assignments->push_back(std::move(assignment));
        }
        return true;
    }
    catch (const std::exception &error)
    {
        std::cerr << "[native-cp] reconcile JSON parse failed: " << error.what() << std::endl;
        return false;
    }
}

// 阶段六：按 selector 模式生成 waiting 原因（进程退出/无法读取元数据时）。
std::string selector_waiting_reason(const ContinuousAssignment &assignment)
{
    if (assignment.selectorMode == "pid_instance")
        return "target pid " + std::to_string(assignment.selectorPid) +
               " is not currently present; collection stays waiting and will NOT follow a reused PID or a new process at the same path";
    if (assignment.selectorMode == "cgroup")
        return "no process currently matches cgroup " + assignment.selectorCgroup +
               "; collection will resume when a process joins the cgroup";
    if (assignment.selectorMode == "container_id")
        return "no process currently matches container_id " + assignment.selectorContainerId +
               "; collection will resume when a process in the container is visible";
    return "target exe is not currently present; collection will resume when any instance returns";
}

void ContinuousSessionManager::ApplyAssignments(const std::vector<ContinuousAssignment> &assignments,
                                                const std::vector<drop::ContinuousTargetProcess> &processes)
{
    std::map<std::string, bool> authoritative;
    for (const auto &assignment : assignments)
    {
        authoritative[assignment.sid] = true;
        if (assignment.desiredState == "stopped")
        {
            if (stoppedReports_.count(assignment.sid) > 0 || stoppingRuntimes_.count(assignment.sid) > 0)
                continue;
            auto existing = runtimes_.find(assignment.sid);
            if (existing != runtimes_.end())
            {
                StoppingRuntime stopping;
                stopping.samplerConfig = BuildSamplerConfig(existing->second);
                stopping.continuityMode = existing->second.effectiveContinuityMode;
                stopping.degradationReason = existing->second.degradationReason;
                stoppingRuntimes_[assignment.sid] = std::move(stopping);
                StopRuntime(existing->second);
                runtimes_.erase(existing);
            }
            else
            {
                Runtime stopped;
                stopped.assignment = assignment;
                StoppingRuntime stopping;
                stopping.samplerConfig = BuildSamplerConfig(stopped);
                stopping.continuityMode = assignment.continuityMode;
                stoppingRuntimes_[assignment.sid] = std::move(stopping);
            }
            continue;
        }
        Runtime &runtime = runtimes_[assignment.sid];
        runtime.assignment = assignment;
        if (runtime.observedState == "pending")
            runtime.effectiveContinuityMode = assignment.continuityMode;
        ReconcileDBSampler(runtime);
    }
    for (auto it = runtimes_.begin(); it != runtimes_.end();)
    {
        if (!authoritative[it->first])
        {
            StopRuntime(it->second);
            it = runtimes_.erase(it);
        }
        else
            ++it;
    }
    for (auto it = stoppingRuntimes_.begin(); it != stoppingRuntimes_.end();)
    {
        if (!authoritative[it->first])
            it = stoppingRuntimes_.erase(it);
        else
            ++it;
    }
    RefreshTargets(processes);
}

void ContinuousSessionManager::RefreshTargets(const std::vector<drop::ContinuousTargetProcess> &processes)
{
    for (auto &entry : runtimes_)
    {
        Runtime &runtime = entry.second;
        std::vector<drop::ContinuousTargetProcess> targets;
        if (runtime.assignment.scope == "process")
            targets = MatchContinuousProcessesBySelector(processes, runtime.assignment);
        bool changed = !same_targets(runtime.targets, targets);
        if (runtime.assignment.scope == "process" && targets.empty())
        {
            runtime.targets.clear();
            runtime.observedState = "waiting";
            runtime.effectiveContinuityMode = runtime.assignment.continuityMode;
            // 阶段六：按 selector 模式给出可诊断的 waiting 原因。
            runtime.degradationReason = selector_waiting_reason(runtime.assignment);
            runtime.lastError.clear();
            continue;
        }
        if (changed)
            runtime.targets = std::move(targets);
    }
    RebuildSharedEngine();
}

drop::ContinuousSamplerConfig ContinuousSessionManager::BuildSamplerConfig(const Runtime &runtime) const
{
    drop::ContinuousSamplerConfig samplerConfig;
    samplerConfig.sampleRateHz = runtime.assignment.sampleRateHz;
    samplerConfig.aggregationWindowSec = runtime.assignment.aggregationWindowSec;
    samplerConfig.uploadBatchSec = runtime.assignment.uploadBatchSec;
    samplerConfig.retentionHours = runtime.assignment.retentionHours;
    samplerConfig.spoolDirectory = spoolDirectory_;
    samplerConfig.spoolMaxBytes = spoolMaxBytes_;
    samplerConfig.spoolMinFreeBytes = spoolMinFreeBytes_;
    samplerConfig.retryMaxSec = retryMaxSec_;
    samplerConfig.sessionSID = runtime.assignment.sid;
    samplerConfig.targetIP = config_.ipAddr;
    samplerConfig.hostname = config_.hostname;
    samplerConfig.apiBaseURL = apiBaseURL_;
    samplerConfig.authUID = authUID_;
    samplerConfig.scope = runtime.assignment.scope;
    samplerConfig.selectorExe = runtime.assignment.selectorExe;
    // 阶段六：selector 模式与结构化参数透传（fan-out 身份过滤与诊断）。
    samplerConfig.selectorMode = runtime.assignment.selectorMode;
    samplerConfig.selectorPid = runtime.assignment.selectorPid;
    samplerConfig.selectorProcessStartMs = runtime.assignment.selectorProcessStartMs;
    samplerConfig.selectorCgroup = runtime.assignment.selectorCgroup;
    samplerConfig.selectorContainerId = runtime.assignment.selectorContainerId;
    // 阶段一：信号控制面。requestedSignals 来自 assignment；为空时回退四类
    // 默认。signals（物理采集集）由 requestedSignals 换算；共享采集器还会在
    // shared_physical_config 里对所有活动 Session 取并集。
    samplerConfig.requestedSignals = runtime.assignment.requestedSignals;
    if (samplerConfig.requestedSignals.empty())
        samplerConfig.requestedSignals = {"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"};
    samplerConfig.signals = drop::PhysicalSignalsFromRequested(samplerConfig.requestedSignals);
    samplerConfig.allowDegraded = runtime.assignment.allowDegraded || runtime.assignment.continuityMode == "degraded";
    samplerConfig.targetProcesses = runtime.targets;
    samplerConfig.dbTargets = runtime.assignment.dbTargets;
    return samplerConfig;
}

// 数据库巡检不接入 SharedDualTrackContinuousSampler 的整机唯一采集器模型
// （那是为了绕开 perf_event/eBPF 单一物理挂载点限制），每个 Session 独立持有
// 自己的 DBSnapshotSampler。当前实现是启动一次不再热更新配置——db_targets
// 变化需要重新创建 Session 才会生效，这个限制记在设计文档，不是本次要解决的点。
// 阶段三：只有显式请求 db_snapshot 信号且配置有效 db_targets 时才启动
// （"不选就不采、不存"）。
void ContinuousSessionManager::ReconcileDBSampler(Runtime &runtime)
{
    const bool dbRequested = drop::logical_signal_requested(runtime.assignment.requestedSignals, "db_snapshot");
    if (runtime.assignment.dbTargets.empty() || !dbRequested)
    {
        if (runtime.dbSampler)
        {
            runtime.dbSampler->Stop();
            runtime.dbSampler.reset();
        }
        return;
    }
    if (runtime.dbSampler && runtime.dbSampler->Running())
        return;
    runtime.dbSampler = std::make_unique<drop::DBSnapshotSampler>();
    drop::ContinuousSamplerConfig samplerConfig = BuildSamplerConfig(runtime);
    std::string error;
    if (!runtime.dbSampler->Start(samplerConfig, &error))
    {
        std::cerr << "[native-cp] db snapshot sampler failed to start sid=" << runtime.assignment.sid
                  << ": " << error << std::endl;
        runtime.dbSampler.reset();
    }
}

void ContinuousSessionManager::UpdateRuntimeEngineStatus(
    const std::vector<drop::ContinuousSamplerConfig> &configs)
{
    if (!sharedSampler_)
        return;
    if (sharedSampler_->Failed())
    {
        const std::string engineError = sharedSampler_->DegradationReason();
        for (auto &entry : runtimes_)
        {
            Runtime &runtime = entry.second;
            if (runtime.assignment.scope == "process" && runtime.targets.empty())
                continue;
            runtime.observedState = "error";
            runtime.effectiveContinuityMode = runtime.assignment.continuityMode;
            runtime.degradationReason.clear();
            runtime.lastError = engineError;
        }
        return;
    }
    const bool sharedProcessFallback = configs.size() > 1 && configs.front().scope == "process";
    std::string engineDegradation = sharedSampler_->DegradationReason();
    if (!sharedSampler_->Strict() && sharedProcessFallback &&
        engineDegradation.find("spool backpressure") == std::string::npos)
        engineDegradation = "CPU is collected once for the union TGID set and isolated by PID/start time; "
                            "rolling bpftrace histograms are unavailable because this fallback cannot safely attribute them across selectors";
    for (auto &entry : runtimes_)
    {
        Runtime &runtime = entry.second;
        if (runtime.assignment.scope == "process" && runtime.targets.empty())
            continue;
        runtime.observedState = engineDegradation.empty() && sharedSampler_->Strict() ? "running" : "degraded";
        runtime.effectiveContinuityMode = sharedSampler_->Strict() ? "strict" : "degraded";
        runtime.lastError.clear();
        runtime.degradationReason = engineDegradation;
    }
}

void ContinuousSessionManager::RebuildSharedEngine()
{
    std::vector<drop::ContinuousSamplerConfig> configs;
    std::ostringstream fingerprint;
    for (const auto &entry : runtimes_)
    {
        const Runtime &runtime = entry.second;
        if (runtime.assignment.scope == "process" && runtime.targets.empty())
            continue;
        configs.push_back(BuildSamplerConfig(runtime));
        fingerprint << entry.first << '|' << runtime.assignment.revision << '|'
                    << runtime.assignment.scope << '|' << runtime.assignment.continuityMode << '|'
                    << runtime.assignment.allowDegraded << '|' << runtime.assignment.sampleRateHz << '|'
                    << runtime.assignment.aggregationWindowSec << '|' << runtime.assignment.uploadBatchSec << '|';
        // 阶段六：selector 模式与参数计入 fingerprint（变化时触发受控切换）。
        fingerprint << runtime.assignment.selectorMode << '|' << runtime.assignment.selectorPid << '|'
                    << runtime.assignment.selectorProcessStartMs << '|' << runtime.assignment.selectorCgroup << '|'
                    << runtime.assignment.selectorContainerId << '|';
        // 阶段一：请求信号集与目标集（target fingerprint 的来源）也计入共享
        // 引擎 fingerprint，变化时触发受控切换。
        for (const auto &signal : runtime.assignment.requestedSignals)
            fingerprint << signal << ',';
        fingerprint << ';';
        for (const auto &target : runtime.targets)
            fingerprint << target.pid << ':' << target.processStartMs << ',';
    }
    const std::string nextFingerprint = fingerprint.str();
    if (configs.empty())
    {
        if (sharedSampler_)
        {
            sharedSampler_->Stop();
            sharedSampler_.reset();
        }
        sharedFingerprint_.clear();
        return;
    }
    if (sharedSampler_ && sharedSampler_->Running() && sharedFingerprint_ == nextFingerprint)
    {
        UpdateRuntimeEngineStatus(configs);
        return;
    }

    // Blue/green handoff: attach the replacement collector while the current
    // one is still sampling. Only after the new backend reports ready do we
    // stop the old recorder, preventing a target/PID change from creating an
    // avoidable startup gap. Both generations use the same session spool.
    const bool hasPrevious = sharedSampler_ && sharedSampler_->Running();
    auto replacement = std::make_unique<drop::SharedDualTrackContinuousSampler>();
    // standby 必须在线程启动前设置，否则 Start() 后到 BeginHandoff() 之间
    // replacement 可能抢先持久化窗口，破坏 cutover 的单一所有权。
    if (hasPrevious)
        replacement->BeginHandoff();
    std::string error;
    if (!replacement->Start(configs, &error))
    {
        for (auto &entry : runtimes_)
        {
            Runtime &runtime = entry.second;
            if (runtime.assignment.scope == "process" && runtime.targets.empty())
                continue;
            runtime.observedState = "error";
            runtime.lastError = error;
        }
        return;
    }
    bool ready = replacement->Ready();
    const uint64_t readyTimeoutMs = env_uint64("DROP_NATIVE_CP_HANDOFF_READY_TIMEOUT_MS", 8000);
    const auto readyDeadline = std::chrono::steady_clock::now() +
                               std::chrono::milliseconds(readyTimeoutMs);
    while (!ready && std::chrono::steady_clock::now() < readyDeadline)
    {
        std::this_thread::sleep_for(std::chrono::milliseconds(50));
        ready = replacement->Ready();
    }
    if (!ready)
    {
        replacement->Stop();
        for (auto &entry : runtimes_)
        {
            Runtime &runtime = entry.second;
            if (runtime.assignment.scope == "process" && runtime.targets.empty())
                continue;
            runtime.observedState = "error";
            runtime.lastError = "replacement continuous collector did not become ready";
        }
        return;
    }
    // 阶段一：唯一 cutover watermark——切点前后由不同 generation 独占，绝不
    // 产生重叠窗口。旧 generation 只提交完整结束于切点前的数据
    //（keepBefore=true），跨切点聚合窗丢弃；新
    // generation 在切点重置并只提交切点后数据（Own(watermark)）。若无法完成
    // barrier，旧实例在 Own 之前已停止，允许形成短缺口但绝无重叠。
    if (hasPrevious)
    {
        const int64_t watermarkMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                                        std::chrono::system_clock::now().time_since_epoch())
                                        .count();
        sharedSampler_->SetCutoverWatermark(watermarkMs, /*keepBefore=*/true);
        replacement->Own(watermarkMs);
        auto previous = std::move(sharedSampler_);
        sharedSampler_ = std::move(replacement);
        previous->Stop();
    }
    else
    {
        // 首次启动（无前代）：owning，无 cutover（全量持久化）。
        replacement->Own(0);
        sharedSampler_ = std::move(replacement);
    }
    sharedFingerprint_ = nextFingerprint;
    UpdateRuntimeEngineStatus(configs);
}

void ContinuousSessionManager::AdvanceStoppingSessions()
{
    bool attemptedUpload = false;
    for (auto it = stoppingRuntimes_.begin(); it != stoppingRuntimes_.end();)
    {
        StoppingRuntime &stopping = it->second;
        if (drop::ContinuousSessionHasPendingSpool(stopping.samplerConfig))
        {
            // The active shared engine only drains the Sessions in its current
            // assignment set. A stopped Session therefore owns its retry here.
            if (!attemptedUpload)
            {
                attemptedUpload = true;
                if (!drop::DrainOneContinuousSessionBatch(stopping.samplerConfig))
                {
                    stopping.lastError = "final batch upload pending; retrying from Session spool";
                    ++it;
                    continue;
                }
            }
            if (drop::ContinuousSessionHasPendingSpool(stopping.samplerConfig))
            {
                stopping.lastError = "final batch upload pending; retrying from Session spool";
                ++it;
                continue;
            }
        }
        stoppedReports_[it->first] = {stopping.continuityMode, stopping.degradationReason};
        it = stoppingRuntimes_.erase(it);
    }
}

void ContinuousSessionManager::StopRuntime(Runtime &runtime)
{
    if (runtime.dbSampler)
    {
        runtime.dbSampler->Stop();
        runtime.dbSampler.reset();
    }
    runtime.observedState = "stopped";
}

void ContinuousSessionManager::SaveAssignmentCache(const std::string &response) const
{
    atomic_write(cachePath_, response);
}

bool ContinuousSessionManager::LoadAssignmentCache(std::vector<ContinuousAssignment> *assignments, uint64_t *revision) const
{
    std::ifstream in(cachePath_);
    if (!in.is_open())
        return false;
    std::ostringstream body;
    body << in.rdbuf();
    return ParseAssignments(body.str(), assignments, revision);
}

} // namespace drop_agent
