// ============================================================
// common/ContinuousSampler.cpp — perf_event-first continuous sampler
// ============================================================

#include "common/ContinuousSampler.h"
#include "common/BuildId.h"
#include "common/GoSymbolizer.h"
#include "common/PythonRuntimeProfiler.h"
#include "common/MemrayProfileIngest.h"
#include "common/RuntimeSymbolMap.h"
#include "common/CoreEbpfCollector.h"
#include "common/SymbolCollector.h"
#include "common/KernelSymbols.h"
#include "common/Utils.h"

#include <algorithm>
#include <chrono>
#include <cerrno>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cctype>
#include <ctime>
#include <fstream>
#include <future>
#include <iostream>
#include <iterator>
#include <map>
#include <mutex>
#include <regex>
#include <set>
#include <sstream>
#include <dirent.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/wait.h>
#include <signal.h>
#include <unistd.h>
#include <unordered_map>

namespace drop
{

namespace
{

struct WindowPayload;

struct RollingPerfRecorder
{
    pid_t pid = -1;
    std::string directory;
    std::set<std::string> consumed;
    int64_t wallStartMs = 0;
    int64_t monotonicStartMs = 0;
    bool Start(const ContinuousSamplerConfig &, std::string *error);
    bool HasParseableOutput() const;
    std::vector<WindowPayload> Drain(const ContinuousSamplerConfig &, bool final);
    void Stop();
};

static bool create_rolling_perf_directory(std::string *directory)
{
    if (!directory)
        return false;
    std::string pattern = "/tmp/mini-drop-native-cp-rolling-" +
                          std::to_string(::getpid()) + "-XXXXXX";
    std::vector<char> writable(pattern.begin(), pattern.end());
    writable.push_back('\0');
    char *created = ::mkdtemp(writable.data());
    if (!created)
        return false;
    *directory = created;
    return true;
}

static std::vector<std::string> rolling_perf_files(const std::string &directory, bool final)
{
    std::vector<std::string> files;
    DIR *dir = ::opendir(directory.c_str());
    if (dir)
    {
        while (dirent *entry = ::readdir(dir))
        {
            std::string name = entry->d_name;
            if (name == "perf.data" || name.rfind("perf.data.", 0) == 0)
                files.push_back(directory + "/" + name);
        }
        ::closedir(dir);
    }
    std::sort(files.begin(), files.end());
    // perf keeps the original `perf.data` path open and renames completed
    // switch-output segments to `perf.data.<timestamp>`. During live drains
    // every timestamped file is immutable; only the base path must wait for
    // the recorder to stop.
    if (!final)
        files.erase(std::remove(files.begin(), files.end(), directory + "/perf.data"), files.end());
    return files;
}

static double strict_histogram_low(uint32_t slot);
static void append_core_histograms(WindowPayload *, const ContinuousSamplerConfig &,
                                   const std::vector<CoreHistogramSample> &, uint64_t lost);
static constexpr uint64_t kMaxDBCount = (1ULL << 63) - 1;

struct SpoolRetryState
{
    int64_t nextAttemptMs = 0;
    int delaySec = 1;
};

static std::string safe_spool_component(const std::string &value)
{
    std::string out;
    for (char c : value)
        out += (std::isalnum(static_cast<unsigned char>(c)) || c == '-' || c == '_') ? c : '_';
    return out.empty() ? "unknown" : out;
}

static uint64_t stable_string_hash(const std::string &value)
{
    uint64_t hash = 14695981039346656037ULL;
    for (unsigned char c : value)
    {
        hash ^= c;
        hash *= 1099511628211ULL;
    }
    return hash;
}

static uint64_t agent_generation_id()
{
    // PID alone is reusable after a container restart. The Agent's /proc
    // start identity is stable for its lifetime and changes on every process
    // generation, which keeps a recovered partial batch distinct from a new
    // capture that happens to start at the same sample timestamp.
    static const uint64_t identity = [] {
        int64_t started = 0;
        if (drop::python_process_start_ms(::getpid(), &started) && started > 0)
            return static_cast<uint64_t>(started);
        struct timespec ts = {};
        ::clock_gettime(CLOCK_REALTIME, &ts);
        return static_cast<uint64_t>(static_cast<uint64_t>(ts.tv_sec) * 1000000000ULL +
                                     static_cast<uint64_t>(ts.tv_nsec));
    }();
    return identity;
}

static std::string make_batch_id(const ContinuousSamplerConfig &cfg)
{
    // 阶段一：batch ID 由 session、collector generation 与单调递增的
    // batch_sequence 生成；重试始终使用相同 ID 与摘要。collector_generation
    // 为空时回退 agent 进程身份（兼容尚未赋 generation 的调用路径）。
    std::ostringstream identity;
    identity << cfg.scope << '|' << cfg.selectorExe << '|';
    for (const auto &target : cfg.targetProcesses)
        identity << target.pid << ':' << target.processStartMs << ':' << target.exe << ';';
    const std::string generation = cfg.collectorGeneration.empty()
                                       ? ("gen-" + std::to_string(agent_generation_id()))
                                       : cfg.collectorGeneration;
    std::ostringstream id;
    id << "cpb-" << std::hex << stable_string_hash(cfg.sessionSID)
       << '-' << stable_string_hash(identity.str())
       << std::dec << '-' << cfg.batchSequence
       << '-' << std::hex << stable_string_hash(generation);
    return id.str();
}

// ============================================================
// 阶段一：SHA-256（自研紧凑实现，避免对 sha256sum 子进程的依赖）
// ============================================================

static const uint32_t kSha256K[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2};

static void sha256_transform(uint32_t state[8], const uint8_t block[64])
{
    uint32_t w[64];
    for (int i = 0; i < 16; ++i)
        w[i] = (static_cast<uint32_t>(block[i * 4]) << 24) | (static_cast<uint32_t>(block[i * 4 + 1]) << 16) |
               (static_cast<uint32_t>(block[i * 4 + 2]) << 8) | static_cast<uint32_t>(block[i * 4 + 3]);
    for (int i = 16; i < 64; ++i)
    {
        uint32_t s0 = ((w[i - 15] >> 7) | (w[i - 15] << 25)) ^ ((w[i - 15] >> 18) | (w[i - 15] << 14)) ^ (w[i - 15] >> 3);
        uint32_t s1 = ((w[i - 2] >> 17) | (w[i - 2] << 15)) ^ ((w[i - 2] >> 19) | (w[i - 2] << 13)) ^ (w[i - 2] >> 10);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3];
    uint32_t e = state[4], f = state[5], g = state[6], h = state[7];
    for (int i = 0; i < 64; ++i)
    {
        uint32_t S1 = ((e >> 6) | (e << 26)) ^ ((e >> 11) | (e << 21)) ^ ((e >> 25) | (e << 7));
        uint32_t ch = (e & f) ^ ((~e) & g);
        uint32_t temp1 = h + S1 + ch + kSha256K[i] + w[i];
        uint32_t S0 = ((a >> 2) | (a << 30)) ^ ((a >> 13) | (a << 19)) ^ ((a >> 22) | (a << 10));
        uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        uint32_t temp2 = S0 + maj;
        h = g; g = f; f = e; e = d + temp1;
        d = c; c = b; b = a; a = temp1 + temp2;
    }
    state[0] += a; state[1] += b; state[2] += c; state[3] += d;
    state[4] += e; state[5] += f; state[6] += g; state[7] += h;
}

static std::string sha256_hex(const std::string &input)
{
    uint32_t state[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
                         0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
    uint64_t bitLen = static_cast<uint64_t>(input.size()) * 8;
    std::string data = input;
    data.push_back(static_cast<char>(0x80));
    while ((data.size() % 64) != 56)
        data.push_back('\0');
    for (int i = 7; i >= 0; --i)
        data.push_back(static_cast<char>((bitLen >> (i * 8)) & 0xFF));
    for (size_t offset = 0; offset < data.size(); offset += 64)
        sha256_transform(state, reinterpret_cast<const uint8_t *>(data.data() + offset));
    static const char *hexDigits = "0123456789abcdef";
    std::string out;
    for (int i = 0; i < 8; ++i)
        for (int shift = 28; shift >= 0; shift -= 4)
            out.push_back(hexDigits[(state[i] >> shift) & 0xF]);
    return out;
}

// ============================================================
// 阶段一：信号映射（物理采集名 ↔ 服务端逻辑 signal_type）
// ============================================================

static const std::vector<std::pair<std::string, std::string>> &signal_name_map()
{
    static const std::vector<std::pair<std::string, std::string>> map = {
        {"cpu", "cpu_profile"}, {"io", "io_latency"}, {"io_syscall", "io_syscall_latency"}, {"sched", "sched_latency"}};
    return map;
}

// requestedSignals（逻辑名）→ 物理信号集合字符串（逗号分隔），供共享采集器
// 取并集。空集合回退四类全开。
static std::string physical_signals_from_requested(const std::vector<std::string> &requested)
{
    if (requested.empty())
        return "cpu,io,io_syscall,sched";
    std::vector<std::string> physical;
    for (const auto &logical : requested)
        for (const auto &entry : signal_name_map())
            if (entry.second == logical)
            {
                physical.push_back(entry.first);
                break;
            }
    if (physical.empty())
        return "cpu,io,io_syscall,sched";
    std::string out;
    for (size_t i = 0; i < physical.size(); ++i)
    {
        if (i)
            out += ",";
        out += physical[i];
    }
    return out;
}

// 判断逻辑信号名是否被 requestedSignals 请求（空集合 = 全部请求）。
static bool logical_signal_requested(const std::vector<std::string> &requested, const std::string &logical)
{
    if (requested.empty())
        return true;
    for (const auto &signal : requested)
        if (signal == logical)
            return true;
    return false;
}

// ============================================================
// 阶段一：collector generation / target fingerprint
// ============================================================

// 每个共享采集器实例拥有独立 collector generation：agent 进程身份 + 进程内
// 自增实例号。切换（replacement）即新实例 → 新 generation；Agent/容器重启时
// agent 进程身份变化 → 新 generation。
static std::string collector_generation_id()
{
    static std::atomic<uint64_t> instanceCounter{0};
    std::ostringstream id;
    id << "gen-" << std::hex << agent_generation_id() << '-' << std::dec << instanceCounter.fetch_add(1);
    return id.str();
}

// 目标进程集稳定指纹：scope + selector + 有序 pid:start:exe 列表的摘要。
// 目标集变化 → 指纹变化 → 触发共享引擎受控切换。
static std::string target_fingerprint_for(const ContinuousSamplerConfig &cfg)
{
    std::ostringstream identity;
    identity << cfg.scope << '|' << cfg.selectorExe << '|';
    for (const auto &target : cfg.targetProcesses)
        identity << target.pid << ':' << target.processStartMs << ':' << target.exe << ';';
    return sha256_hex(identity.str()).substr(0, 32);
}

static bool ensure_directory(const std::string &path)
{
    if (path.empty())
        return false;
    std::string current;
    if (path.front() == '/')
        current = "/";
    std::stringstream parts(path);
    std::string part;
    while (std::getline(parts, part, '/'))
    {
        if (part.empty())
            continue;
        if (current.size() > 1 && current.back() != '/')
            current += '/';
        current += part;
        if (::mkdir(current.c_str(), 0700) != 0 && errno != EEXIST)
            return false;
    }
    return ::chmod(path.c_str(), 0700) == 0;
}

static std::string session_spool_directory(const ContinuousSamplerConfig &cfg)
{
    return cfg.spoolDirectory + "/" + safe_spool_component(cfg.sessionSID);
}

static bool read_file(const std::string &path, std::string *body)
{
    std::ifstream in(path, std::ios::binary);
    if (!in.is_open())
        return false;
    std::ostringstream stream;
    stream << in.rdbuf();
    *body = stream.str();
    return in.good() || in.eof();
}

static bool atomic_write_file(const std::string &path, const std::string &body)
{
    std::string temporary = path + ".tmp." + std::to_string(::getpid());
    int fd = ::open(temporary.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0600);
    if (fd < 0)
        return false;
    size_t written = 0;
    while (written < body.size())
    {
        ssize_t count = ::write(fd, body.data() + written, body.size() - written);
        if (count < 0 && errno == EINTR)
            continue;
        if (count <= 0)
        {
            ::close(fd);
            ::unlink(temporary.c_str());
            return false;
        }
        written += static_cast<size_t>(count);
    }
    bool ok = ::fsync(fd) == 0 && ::close(fd) == 0 &&
              ::rename(temporary.c_str(), path.c_str()) == 0;
    if (!ok)
    {
        ::unlink(temporary.c_str());
        return false;
    }
    std::string directory = path.substr(0, path.find_last_of('/'));
    int dirfd = ::open(directory.c_str(), O_RDONLY | O_DIRECTORY);
    if (dirfd >= 0)
    {
        ::fsync(dirfd);
        ::close(dirfd);
    }
    return true;
}

static void append_spool_files(const std::string &directory,
                               const std::string &suffix,
                               std::vector<std::string> *files)
{
    DIR *dir = ::opendir(directory.c_str());
    if (!dir)
        return;
    while (dirent *entry = ::readdir(dir))
    {
        std::string name = entry->d_name;
        if (name == "." || name == "..")
            continue;
        std::string path = directory + "/" + name;
        struct stat st = {};
        if (::stat(path.c_str(), &st) != 0)
            continue;
        if (S_ISDIR(st.st_mode))
            append_spool_files(path, suffix, files);
        else if (name.rfind("cpb-", 0) == 0 && name.size() > suffix.size() &&
                 name.compare(name.size() - suffix.size(), suffix.size(), suffix) == 0)
            files->push_back(path);
    }
    ::closedir(dir);
}

static std::vector<std::string> list_spool_files(const ContinuousSamplerConfig &cfg)
{
    std::vector<std::string> files;
    append_spool_files(cfg.spoolDirectory, ".json", &files);
    std::sort(files.begin(), files.end(), [](const std::string &a, const std::string &b) {
        return a.substr(a.find_last_of('/') + 1) < b.substr(b.find_last_of('/') + 1);
    });
    return files;
}

static std::vector<std::string> list_session_spool_files(const ContinuousSamplerConfig &cfg,
                                                         const std::string &suffix)
{
    std::vector<std::string> files;
    append_spool_files(session_spool_directory(cfg), suffix, &files);
    std::sort(files.begin(), files.end());
    return files;
}

static uint64_t spool_usage_bytes(const ContinuousSamplerConfig &cfg)
{
    uint64_t total = 0;
    std::vector<std::string> files;
    append_spool_files(cfg.spoolDirectory, ".json", &files);
    append_spool_files(cfg.spoolDirectory, ".journal", &files);
    for (const auto &path : files)
    {
        struct stat st = {};
        if (::stat(path.c_str(), &st) == 0 && st.st_size > 0)
            total += static_cast<uint64_t>(st.st_size);
    }
    return total;
}

static uint64_t spool_free_bytes(const ContinuousSamplerConfig &cfg)
{
    struct statvfs fs = {};
    if (::statvfs(session_spool_directory(cfg).c_str(), &fs) != 0)
        return 0;
    return static_cast<uint64_t>(fs.f_bavail) * static_cast<uint64_t>(fs.f_frsize);
}

// 阶段五：服务器存储压力全局标志（ContinuousSessionManager 心跳解析设置）。
static std::atomic<bool> g_continuousServerPressureHalted{false};

static bool spool_has_collection_capacity(const ContinuousSamplerConfig &cfg)
{
    // 服务器存储压力：停止产生新窗口（Agent 进入 waiting/server_storage_pressure）。
    if (ContinuousServerPressureHalted())
        return false;
    return spool_usage_bytes(cfg) < cfg.spoolMaxBytes &&
           spool_free_bytes(cfg) >= cfg.spoolMinFreeBytes;
}

static std::string spool_path(const ContinuousSamplerConfig &cfg, const std::string &batchID)
{
    return session_spool_directory(cfg) + "/" + safe_spool_component(batchID) + ".json";
}

static std::string journal_path(const ContinuousSamplerConfig &cfg, const std::string &batchID)
{
    return session_spool_directory(cfg) + "/" + safe_spool_component(batchID) + ".journal";
}

static std::string batch_id_from_spool_path(const std::string &path)
{
    size_t slash = path.find_last_of('/');
    std::string name = slash == std::string::npos ? path : path.substr(slash + 1);
    if (name.size() > 5 && name.compare(name.size() - 5, 5, ".json") == 0)
        name.resize(name.size() - 5);
    return name;
}

static uint64_t clamp_count(uint64_t value)
{
    return value > kMaxDBCount ? kMaxDBCount : value;
}

static uint64_t add_count(uint64_t total, uint64_t value)
{
    value = clamp_count(value);
    if (total >= kMaxDBCount || value >= kMaxDBCount)
        return kMaxDBCount;
    if (total > kMaxDBCount - value)
        return kMaxDBCount;
    return total + value;
}

static bool parse_count_strict(const std::string &text, uint64_t *out)
{
    std::string s = drop::trim(text);
    if (s.empty())
        return false;
    for (char c : s)
    {
        if (!std::isdigit(static_cast<unsigned char>(c)))
            return false;
    }
    char *end = nullptr;
    unsigned long long value = std::strtoull(s.c_str(), &end, 10);
    if (!end || *end != '\0')
        return false;
    *out = clamp_count(static_cast<uint64_t>(value));
    return true;
}

static bool looks_like_bpftrace_error(const std::string &text)
{
    std::string s = drop::trim(text);
    return s.rfind("ERROR:", 0) == 0 ||
           s.rfind("stdin:", 0) == 0 ||
           s.find("failed to look up stack id") != std::string::npos ||
           s.find("Unknown error") != std::string::npos;
}

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
    int pid = 0;
};

// 阶段二：结构化数据库快照（SQL digest 聚合 / 锁等待链），与阶段一的标量
// MetricPayload 并存于同一个 WindowPayload。kind 区分两种语义，未用到的字段
// 留空/留 0 即可（和 HistogramPayload 的 unavailable/reason 一样，不为每种
// kind 单独开结构体，减少序列化分支）。
struct DBSnapshotSample
{
    std::string kind;          // "digest" | "lock_wait"
    std::string instanceLabel; // 对应 DBTargetConfig.instanceLabel
    int64_t timestampMs = 0;

    // kind == "digest"：跨轮次增量（本窗口内新发生的调用），不是累计值
    std::string schemaName;
    std::string digestText;     // 归一化 SQL（占位符形式），不落原始参数
    uint64_t callCount = 0;
    uint64_t totalLatencyUs = 0;
    uint64_t rowsExaminedTotal = 0;

    // kind == "lock_wait"
    int64_t waitingPid = 0;
    std::string waitingQuery;
    int64_t blockingPid = 0;
    std::string blockingQuery;
    uint64_t waitSeconds = 0;
    std::string lockedTable;
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
    // 阶段一：逻辑窗口稳定 ID（内容摘要不参与 ID，见 make_window_id）与
    // 窗口内容摘要（冲突检测）。
    std::string windowID;
    std::string contentSHA256;
};

// ============================================================
// 阶段一：分信号计数 / 窗口内容摘要 / 窗口 ID / 批次内容摘要
// ============================================================

// 某窗口的分信号计数（逻辑信号名 → 计数）。histogram 用 eventCount，CPU 样本
// 用 count 累加（含 profiles），metric/db 用条目数。与服务端
// continuousWindowSampleCount 口径一致。
static std::map<std::string, uint64_t> window_signal_counts(const WindowPayload &window)
{
    std::map<std::string, uint64_t> counts;
    auto add = [&counts](const std::string &signal, uint64_t value) {
        if (signal.empty())
            return;
        counts[signal] = add_count(counts[signal], value);
    };
    if (!window.samples.empty())
    {
        uint64_t total = 0;
        for (const auto &sample : window.samples)
            total = add_count(total, sample.count);
        add("cpu_profile", total);
    }
    for (const auto &profile : window.profiles)
    {
        uint64_t total = 0;
        for (const auto &sample : profile.samples)
            total = add_count(total, sample.count);
        add(profile.signalType.empty() ? "cpu_profile" : profile.signalType, total);
    }
    for (const auto &hist : window.histograms)
        add(hist.signalType, hist.eventCount);
    if (!window.metrics.empty())
        add("python_rss", static_cast<uint64_t>(window.metrics.size()));
    if (!window.dbSnapshots.empty())
        add("db_snapshot", static_cast<uint64_t>(window.dbSnapshots.size()));
    if (counts.empty() && !window.samples.empty())
        add("cpu_profile", 1);
    return counts;
}

// 窗口内容摘要：对窗口 payload 的规范化表示做 sha256。同一逻辑内容 → 相同
// 摘要；内容变化 → 摘要变化（服务端据此判冲突）。与字段顺序无关。
static std::string window_content_digest(const WindowPayload &window)
{
    std::ostringstream content;
    auto text = [&content](const std::string &value) {
        content << value.size() << ':' << value << '|';
    };
    content << "ws=" << window.startMs << "|we=" << window.endMs << ";";
    for (const auto &sample : window.samples)
    {
        content << "s:" << sample.pid << ':' << sample.processStartMs << ':' << sample.count << ':';
        text(sample.comm); text(sample.exe); text(sample.runtime); text(sample.stackScope); text(sample.backend);
        for (const auto &frame : sample.stack)
            text(frame);
        for (const auto &frame : sample.frames)
        {
            text(frame.function); text(frame.raw); text(frame.file); text(frame.mappingFile); text(frame.buildId);
            content << frame.line << ':' << frame.address << ':' << frame.normalizedOffset << ':' << frame.resolved << '|';
        }
        content << ";";
    }
    for (const auto &profile : window.profiles)
    {
        content << "p:"; text(profile.signalType); text(profile.backend); text(profile.stackScope);
        text(profile.profileID); text(profile.unit);
        for (const auto &sample : profile.samples)
        {
            content << sample.pid << ':' << sample.processStartMs << ':' << sample.count << ':';
            text(sample.comm); text(sample.exe); text(sample.runtime); text(sample.backend);
            for (const auto &frame : sample.stack)
                text(frame);
            for (const auto &frame : sample.frames)
            {
                text(frame.function); text(frame.raw); text(frame.file); text(frame.mappingFile); text(frame.buildId);
                content << frame.line << ':' << frame.address << ':' << frame.normalizedOffset << ':' << frame.resolved << '|';
            }
        }
        content << ";";
    }
    for (const auto &hist : window.histograms)
    {
        content << "h:"; text(hist.signalType); text(hist.backend); text(hist.unit); text(hist.reason);
        content << hist.pid << ':' << hist.eventCount << ':' << hist.unavailable << ':'
                << hist.min << ':' << hist.max << ':' << hist.p50 << ':' << hist.p95 << ':' << hist.p99 << ':';
        for (const auto &bucket : hist.buckets)
        {
            text(bucket.range);
            content << bucket.low << ':' << bucket.high << ':' << bucket.count << ',';
        }
        content << ";";
    }
    for (const auto &metric : window.metrics)
    {
        content << "m:" << metric.pid << ':' << metric.processStartMs << ':' << metric.timestampMs << ':' << metric.value << ':';
        text(metric.metric); text(metric.unit); text(metric.runtime); text(metric.comm); text(metric.exe);
        content << ';';
    }
    for (const auto &snap : window.dbSnapshots)
    {
        content << "d:" << snap.timestampMs << ':' << snap.callCount << ':' << snap.totalLatencyUs << ':'
                << snap.rowsExaminedTotal << ':' << snap.waitingPid << ':' << snap.blockingPid << ':' << snap.waitSeconds << ':';
        text(snap.kind); text(snap.instanceLabel); text(snap.schemaName); text(snap.digestText);
        text(snap.waitingQuery); text(snap.blockingQuery); text(snap.lockedTable);
        content << ';';
    }
    content << "meta:" << window.rssTruncated << ':';
    text(window.backendStatus); text(window.backendReason); text(window.selectedBackend);
    for (const auto &backend : window.attemptedBackends)
        text(backend);
    if (!window.symbolRefsJson.empty())
    {
        content << "sr:";
        text(window.symbolRefsJson);
    }
    return sha256_hex(content.str());
}

// 逻辑窗口稳定 ID：session + collector generation + 起止时间 + target
// fingerprint。内容摘要不参与 ID——内容冲突仍是同一合法 ID，绝不允许生成
// 第二个 ID（服务端靠该 ID 判冲突/幂等）。
static std::string make_window_id(const ContinuousSamplerConfig &cfg, const WindowPayload &window)
{
    std::ostringstream identity;
    identity << cfg.sessionSID << '|' << cfg.collectorGeneration << '|'
             << window.startMs << '|' << window.endMs << '|' << cfg.targetFingerprint;
    std::ostringstream id;
    id << "cpw-" << sha256_hex(identity.str()).substr(0, 32);
    return id.str();
}

// 批次内容摘要：session + generation + sequence + 起止时间 + 各窗口内容摘要
// 的汇总 sha256。同一内容重传 → 相同摘要；内容变化 → 摘要变化。
static std::string batch_content_digest(const ContinuousSamplerConfig &cfg,
                                        const std::vector<WindowPayload> &windows)
{
    std::ostringstream content;
    content << cfg.sessionSID << '|' << cfg.collectorGeneration << '|' << cfg.batchSequence << '|';
    for (const auto &window : windows)
        content << window.startMs << '-' << window.endMs << ':' << window_content_digest(window) << ';';
    return sha256_hex(content.str());
}

// 批次分信号计数（跨窗口求和）。
static std::map<std::string, uint64_t> batch_signal_counts(const std::vector<WindowPayload> &windows)
{
    std::map<std::string, uint64_t> counts;
    for (const auto &window : windows)
        for (const auto &entry : window_signal_counts(window))
            counts[entry.first] = add_count(counts[entry.first], entry.second);
    return counts;
}

static std::string signal_counts_json(const std::map<std::string, uint64_t> &counts)
{
    // 信号名来自固定白名单（cpu_profile/io_latency/io_syscall_latency/
    // sched_latency/python_rss/db_snapshot），不含 JSON 特殊字符，无需转义。
    std::string out = "{";
    size_t index = 0;
    for (const auto &entry : counts)
    {
        if (index++)
            out += ",";
        out += "\"" + entry.first + "\":" + std::to_string(entry.second);
    }
    out += "}";
    return out;
}

static int64_t now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

static bool env_enabled_default(const char *name, bool fallback)
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

// 阶段五：frames-only 模式（DROP_CONTINUOUS_FRAMES_ONLY=1）。
// shadow/prefer 阶段同时发送 stack+frames；进入 v2-only 且回滚窗口结束后
// 仅发送 frames（服务端按 frames 生成展示名称）。默认关闭保持兼容。
static bool frames_only_mode()
{
    static const bool enabled = env_enabled_default("DROP_CONTINUOUS_FRAMES_ONLY", false);
    return enabled;
}

static int env_positive_int(const char *name, int fallback)
{
    const char *value = std::getenv(name);
    if (!value || !*value)
        return fallback;
    int parsed = std::atoi(value);
    return parsed > 0 ? parsed : fallback;
}

static std::string rfc3339_from_ms(int64_t ms)
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

static std::string json_escape(const std::string &s)
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

static bool file_exists_local(const std::string &path)
{
    struct stat st;
    return stat(path.c_str(), &st) == 0;
}

static std::string perf_bin()
{
    const char *env = std::getenv("DROP_PERF_BIN");
    if (env && *env)
        return env;
    if (file_exists_local("/usr/local/bin/perf-real"))
        return "/usr/local/bin/perf-real";
    return "perf";
}

static std::string read_exe(int pid)
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

static int process_tgid(int pid)
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

static bool process_targeted(const ContinuousSamplerConfig &cfg,
                             int pid,
                             int64_t processStartMs,
                             const std::string &exe)
{
    if (cfg.scope != "process")
        return true;
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid &&
            (processStartMs <= 0 || target.processStartMs <= 0 || processStartMs == target.processStartMs) &&
            (cfg.selectorExe.empty() || exe.empty() || exe == cfg.selectorExe))
            return true;
    return false;
}

static bool process_targeted(const ContinuousSamplerConfig &cfg, int pid, const std::string &exe)
{
    return process_targeted(cfg, pid, 0, exe);
}

static int64_t configured_process_start_ms(const ContinuousSamplerConfig &cfg, int pid)
{
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid)
            return target.processStartMs;
    int64_t startMs = 0;
    drop::python_process_start_ms(pid, &startMs);
    return startMs;
}

static std::string configured_process_exe(const ContinuousSamplerConfig &cfg, int pid)
{
    for (const auto &target : cfg.targetProcesses)
        if (target.pid == pid)
            return target.exe;
    return {};
}

static std::string target_pid_csv(const ContinuousSamplerConfig &cfg)
{
    std::ostringstream out;
    bool first = true;
    for (const auto &target : cfg.targetProcesses)
    {
        if (target.pid <= 0)
            continue;
        if (!first)
            out << ',';
        first = false;
        out << target.pid;
    }
    return out.str();
}

static std::vector<std::string> perf_record_args(const ContinuousSamplerConfig &cfg,
                                                 const std::string &perf,
                                                 const std::string &perfEvent,
                                                 const std::string &dataPath)
{
    std::vector<std::string> args{perf, "record", "--no-buildid-cache", "-q"};
    if (cfg.scope == "process")
    {
        std::string pids = target_pid_csv(cfg);
        if (pids.empty())
            return {};
        args.insert(args.end(), {"-p", pids});
    }
    else
    {
        args.push_back("-a");
    }
    args.insert(args.end(), {"-e", perfEvent, "-F", std::to_string(cfg.sampleRateHz),
                             "-g", "-o", dataPath, "--", "sleep",
                             std::to_string(cfg.aggregationWindowSec)});
    return args;
}

static std::string bpftrace_target_predicate(const ContinuousSamplerConfig &cfg, const std::string &pidExpr = "pid")
{
    if (cfg.scope != "process")
        return "";
    std::string predicate;
    for (const auto &target : cfg.targetProcesses)
    {
        if (target.pid <= 0)
            continue;
        if (!predicate.empty())
            predicate += " || ";
        predicate += pidExpr + " == " + std::to_string(target.pid);
    }
    return predicate.empty() ? "/0/" : "/" + predicate + "/";
}

static std::string parse_frame_name(const std::string &raw, int pid)
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
        // perf script 解析失败时格式是 "[unknown] (<dso路径>)"——括号内的
        // DSO 路径先取出来，符号解析成功时用不上（行为和之前一致，丢弃）。
        size_t close = name.rfind(')');
        if (close != std::string::npos && close > paren + 2)
            dso = name.substr(paren + 2, close - paren - 2);
        name = name.substr(0, paren);
    }
    if (name.empty())
        name = first;
    name = drop::trim(name);

    // 子项1.2：解析失败时不再丢弃 DSO 信息，展示 "0x<addr> [<模块名>]"
    // 而不是裸 [unknown]——只展示确定知道的信息（地址、DSO 归属），不猜
    // 函数名，遵循 symbolization-design.md 的"宁可标记未解析"原则。
    if (name == "[unknown]" && !dso.empty())
    {
        uint64_t address = std::strtoull(first.c_str(), nullptr, 16);
        std::string goName;
        if (address > 0 && drop::resolve_go_symbol(pid, dso, address, &goName))
            return goName;
        size_t slash = dso.rfind('/');
        std::string base = slash == std::string::npos ? dso : dso.substr(slash + 1);
        // perf 退化输出时 DSO 位置可能本身就是方括号占位符，比如
        // "0x... [unknown] ([unknown])" 或 "[kernel.kallsyms]"——先剥掉外层
        // 方括号，避免拼出 "0x... [[unknown]]" 这类双层括号。
        if (base.size() >= 2 && base.front() == '[' && base.back() == ']')
            base = base.substr(1, base.size() - 2);
        if (base.empty() || base == "unknown")
            return "0x" + first;
        return "0x" + first + " [" + base + "]";
    }
    return name;
}

// ------------------------------------------------------------
// 阶段五：perf script 行 → 结构化栈帧
// ------------------------------------------------------------
// perf script 每帧行的典型格式：
//   "        7f1234abcdef symbol_name (/usr/lib/libc.so.6)"
//   "        7f1234 [unknown] (/usr/lib/libc.so.6)"
// 解析 IP/symbol/DSO，并从 /proc/pid/maps 的 mmap 信息计算 file-relative
// normalized_offset；build-id 从 ELF 读取（有缓存）。取不到的字段如实留空
// /0，不推测。
// ------------------------------------------------------------

// proc_maps_containing 在 /proc/pid/maps 中查找包含 address 且路径为 dsoPath
// 的映射，返回映射 vaddr 基址与文件偏移（用于 file-relative offset）。
static bool proc_maps_containing(int pid,
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
        // dev inode
        std::string dev, inode;
        iss >> dev >> inode;
        std::string path;
        std::getline(iss, path);
        path = drop::trim(path);
        if (path.empty())
            continue;
        // 匹配路径本身或 basename（perf 输出可能是相对名）
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

static drop::ContinuousStackFrame parse_perf_frame(const std::string &raw, int pid)
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
    // IP：perf script 输出形如 "7a93e5545ca8"（无 0x 前缀）或 "0x7a93..."，
    // 只按"全十六进制"判定，避免把符号名误当地址。
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
    // symbol 与 dso（perf 输出 "symbol (dso)"）
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
    // normalized_offset：mmap 信息（file-relative = address - vaddr + file_offset）
    if (frame.address != 0 && !frame.mappingFile.empty())
    {
        uint64_t base = 0, fileOffset = 0;
        if (proc_maps_containing(pid, frame.mappingFile, frame.address, &base, &fileOffset))
        {
            if (frame.address >= base)
                frame.normalizedOffset = frame.address - base + fileOffset;
        }
        // build-id（ELF 读取，缓存友好）
        std::string buildId;
        if (drop::elf_gnu_build_id(frame.mappingFile, &buildId))
            frame.buildId = buildId;
    }
    return frame;
}

// frames_to_json 序列化结构化栈（阶段五）。
static std::string frames_to_json(const std::vector<drop::ContinuousStackFrame> &frames)
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

static bool parse_sample_header(const std::string &line,
                                std::string *comm,
                                int *pid,
                                double *timestampSec = nullptr)
{
    std::string trimmed = drop::trim(line);
    if (trimmed.empty() || trimmed[0] == '#' || trimmed[0] == '\t')
        return false;
    // System-wide perf output includes a [CPU] column, while process-attached
    // output from `perf record -p` does not. Both formats still have a numeric
    // PID token followed by a timestamp containing ':'.
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
            // The timestamp field is the only header token whose numeric
            // value is immediately followed by ':'. This works for both
            // `pid/tid [cpu] time:` and process-attached `pid tid time:`
            // layouts emitted by perf versions in the supported images.
            const size_t colon = token.find(':');
            if (colon == std::string::npos || colon == 0)
                continue;
            const std::string number = token.substr(0, colon);
            char *end = nullptr;
            const double parsed = std::strtod(number.c_str(), &end);
            if (!end || *end != '\0' || !std::isfinite(parsed) || parsed < 0.0)
                continue;
            // Event names can contain digits, but the timestamp contains a
            // decimal point in perf's text format. Accept exponent notation
            // too, while avoiding event tokens such as "v2:".
            if (number.find('.') == std::string::npos && number.find('e') == std::string::npos &&
                number.find('E') == std::string::npos)
                continue;
            *timestampSec = parsed;
            break;
        }
    }
    return true;
}

static void add_sample(std::map<std::string, AggregatedSample> *out,
                       const std::string &comm,
                       int pid,
                       const std::vector<std::string> &rawStack,
                       const std::vector<drop::ContinuousStackFrame> &rawFrames,
                       const std::string &stackScope = "",
                       const std::string &backend = "")
{
    if (rawStack.empty())
        return;
    // `perf script` reports the sampled thread ID for multithreaded runtimes
    // such as Go and the JVM. Continuous process selectors are TGID-based, so
    // normalize every sample before executable and start-time attribution.
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

static PerfScriptParseResult parse_perf_script_result(const std::string &script)
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
            // 阶段五：结构化帧与旧字符串栈并行（frames-only 模式由
            // 服务端按 frames 生成展示名称，这里始终发送两份保持兼容）。
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

static std::vector<AggregatedSample> parse_perf_script(const std::string &script)
{
    return parse_perf_script_result(script).samples;
}

static int64_t monotonic_ms()
{
    struct timespec ts = {};
    if (::clock_gettime(CLOCK_MONOTONIC, &ts) != 0)
        return 0;
    return static_cast<int64_t>(ts.tv_sec) * 1000 + ts.tv_nsec / 1000000;
}

static int64_t perf_timestamp_to_unix_ms(double timestampSec,
                                         int64_t wallAnchorMs,
                                         int64_t monotonicAnchorMs)
{
    if (!std::isfinite(timestampSec) || timestampSec <= 0.0)
        return 0;
    // Some perf exporters print CLOCK_REALTIME seconds, while the native
    // perf tool normally prints CLOCK_MONOTONIC seconds. Support both forms.
    if (timestampSec >= 1000000000.0)
        return static_cast<int64_t>(std::llround(timestampSec * 1000.0));
    if (wallAnchorMs <= 0 || monotonicAnchorMs <= 0)
        return 0;
    return wallAnchorMs + static_cast<int64_t>(std::llround(timestampSec * 1000.0)) - monotonicAnchorMs;
}

// ------------------------------------------------------------
// 任务1 + 子项1.1：perf script 解析之前的本地 build-id 缓存预热。
// 详见 docs/continuous-symbolization-design.md 任务1。
// ------------------------------------------------------------

// 子项1.1：Agent 本地三态尝试缓存，避免每个窗口对已知读不到的 build-id
// 反复做无用功。PerfEventSampler::Loop 和 DualTrackContinuousSampler::Loop
// (经 std::async 调用 collect_window) 两条路径都可能触发预热，未确认二者
// 是否会在同一进程内并发运行，加锁保安全。
enum class BuildIdAttemptState
{
    TransientFail, // 这次没定位到，之后可能有别的进程映射同一二进制，值得重试
    PermanentFail, // 确认读到的内容不是合法 ELF，再等也不会变好
};

struct BuildIdAttempt
{
    BuildIdAttemptState state;
    int64_t retryAfterMs;  // 仅 TransientFail 有意义
    int64_t lastTouchedMs; // 淘汰排序用；PermanentFail 的 retryAfterMs 固定是 0，不能复用它做排序
};

static std::mutex g_buildIdAttemptMutex;
static std::map<std::string, BuildIdAttempt> g_buildIdAttempts;
static constexpr int64_t kBuildIdTransientRetryMs = 5 * 60 * 1000; // 5 分钟
// 长时间运行的 Agent 会遇到很多不同的 build-id，这张表只清成功、不淘汰
// 失败，原本没有上限。参考 RuntimeSymbolMap.cpp 的 g_javaLastRefreshMap
// 同款做法：超过上限时线性扫一遍淘汰最久没碰过的一条。
static constexpr size_t kBuildIdAttemptMaxEntries = 4096;

// 调用方必须持有 g_buildIdAttemptMutex。
static void evict_oldest_build_id_attempt_locked()
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

static bool should_skip_build_id_attempt(const std::string &buildId, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    auto it = g_buildIdAttempts.find(buildId);
    if (it == g_buildIdAttempts.end())
        return false;
    if (it->second.state == BuildIdAttemptState::PermanentFail)
        return true;
    if (nowMs < it->second.retryAfterMs)
        return true;
    g_buildIdAttempts.erase(it); // 过期了，允许重试
    return false;
}

static void record_build_id_transient_fail(const std::string &buildId, int64_t nowMs)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::TransientFail, nowMs + kBuildIdTransientRetryMs, nowMs};
    evict_oldest_build_id_attempt_locked();
}

static void record_build_id_permanent_fail(const std::string &buildId)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::PermanentFail, 0, now_ms()};
    evict_oldest_build_id_attempt_locked();
}

static void clear_build_id_attempt(const std::string &buildId)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts.erase(buildId);
}

static bool looks_like_elf(const std::string &path)
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
static std::vector<drop::BuildIdEntry> warm_build_id_cache(const std::string &perf, const std::string &dataPath)
{
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
            continue;
        if (should_skip_build_id_attempt(e.buildId, nowMs))
            continue;
        pending.push_back(e);
    }
    if (pending.empty())
        return entries;

    // 只在确实有需要解析的 build-id 时才建索引——O(进程数) 的一次性开销
    // 不针对每个 build-id 重复付出（详见设计文档任务1"技术方案依据"）。
    std::unordered_map<std::string, int> dsoIndex = drop::build_dso_path_index();

    for (auto &e : pending)
    {
        auto it = dsoIndex.find(e.dsoPath);
        if (it == dsoIndex.end())
        {
            record_build_id_transient_fail(e.buildId, nowMs);
            continue;
        }
        std::string resolved = drop::resolve_via_pid(e.dsoPath, it->second);
        if (resolved.empty())
        {
            record_build_id_transient_fail(e.buildId, nowMs);
            continue;
        }
        if (!looks_like_elf(resolved))
        {
            record_build_id_permanent_fail(e.buildId);
            continue;
        }
        if (drop::cache_build_id_locally(e.buildId, resolved))
            clear_build_id_attempt(e.buildId);
        else
            record_build_id_transient_fail(e.buildId, nowMs);
    }
    return entries;
}

static bool unresolved_frame(const std::string &raw)
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

static std::string path_basename(const std::string &path)
{
    size_t slash = path.rfind('/');
    return slash == std::string::npos ? path : path.substr(slash + 1);
}

static std::string sanitize_python_perf_frame(const std::string &frame)
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

static std::string sample_runtime_with_go_hint(const AggregatedSample &sample,
                                               const drop::GoSymbolReport &goReport,
                                               bool hasGoBuildInfo)
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
    if (hasGoBuildInfo || isGo(goReport.ready) || isGo(goReport.pending) || isGo(goReport.failed))
        return "go";
    if (sample.exe.empty())
        return sample.pid <= 2 || sample.comm.rfind("kworker", 0) == 0 ? "kernel" : "unknown";
    return "native";
}

static std::string sample_runtime(const AggregatedSample &sample,
                                  const drop::GoSymbolReport &goReport,
                                  std::map<int, bool> *goBuildInfoCache)
{
    bool hasGoBuildInfo = false;
    if (sample.pid > 0 && !sample.exe.empty())
    {
        auto cached = goBuildInfoCache->find(sample.pid);
        if (cached == goBuildInfoCache->end())
        {
            std::string procExe = "/proc/" + std::to_string(sample.pid) + "/exe";
            hasGoBuildInfo = drop::go_binary_has_build_info(procExe);
            (*goBuildInfoCache)[sample.pid] = hasGoBuildInfo;
        }
        else
        {
            hasGoBuildInfo = cached->second;
        }
    }
    return sample_runtime_with_go_hint(sample, goReport, hasGoBuildInfo);
}

static std::string python_fallback_json(const std::vector<drop::PythonFallbackResult> &results, size_t limitedCount)
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
        body += "{\"pid\":" + std::to_string(result.pid) +
                ",\"mode\":\"py-spy\",\"samples\":" + std::to_string(result.samples.size()) + "}";
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
                ",\"reason\":\"" + json_escape(result.reason) + "\"}";
    }
    body += "],\"limited_count\":" + std::to_string(limitedCount) + "}";
    return body;
}

static std::string combined_symbol_refs_json(const drop::RuntimeSymbolReport &runtimeReport,
                                             const drop::GoSymbolReport &goReport,
                                             const std::vector<drop::PythonFallbackResult> &pythonFallback,
                                             size_t pythonLimitedCount,
                                             const std::vector<drop::MemrayProfileResult> &memrayResults,
                                             const std::vector<AggregatedSample> &samples,
                                             const std::vector<drop::BuildIdEntry> &buildIds,
                                             const std::string &kallsymsSha256)
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
    // 本次窗口引用的全部用户态 build-id 清单（warm_build_id_cache +
    // discover_sampled_go_build_ids 合并结果）。后端 collectContinuousSymbolRefs
    // 递归扫 key 含 "build_id" 的字段时能捡到这里，使"重新检查符号"对非 Go
    // 原生二进制（如 PostgreSQL/libc）也能做真实存在性检查，而不是空集恒真。
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
        body += "{\"pid\":" + std::to_string(result.pid) + ",\"profile_id\":\"" + json_escape(result.profileID) + "\"}";
    }
    body += "],\"failed\":[";
    bool firstMemrayFailed = true;
    for (const auto &result : memrayResults)
    {
        if (result.ready) continue;
        if (!firstMemrayFailed) body += ",";
        firstMemrayFailed = false;
        body += "{\"pid\":" + std::to_string(result.pid) + ",\"reason\":\"" + json_escape(result.reason) + "\"}";
    }
    body += "]}";
    body += "}";
    return body;
}

// 内核符号快照：低频（每 10 分钟）快照并去重上传一次 /proc/kallsyms，把
// sha256 写进后续窗口的 symbol_refs，供"重新检查符号"诊断 kallsyms 是否入库。
// 持续采集的内核帧本就靠 perf script 当场读本机 /proc/kallsyms 解析，快照
// 上传只服务于符号库口径与事后审计，不阻塞当次解析。
static std::mutex g_kallsymsSnapshotMutex;
static std::string g_lastKallsymsSha256;
static int64_t g_lastKallsymsSnapshotMs = 0;
static constexpr int64_t kKallsymsSnapshotIntervalMs = 10 * 60 * 1000;

static std::string ensure_kallsyms_snapshot(const ContinuousSamplerConfig &cfg)
{
    std::lock_guard<std::mutex> lock(g_kallsymsSnapshotMutex);
    int64_t now = now_ms();
    if (now - g_lastKallsymsSnapshotMs < kKallsymsSnapshotIntervalMs)
        return g_lastKallsymsSha256;
    g_lastKallsymsSnapshotMs = now;
    std::string path = "/tmp/mini_drop_cp_kallsyms_" + cfg.sessionSID + ".txt";
    if (!drop::snapshot_kallsyms(path))
        return g_lastKallsymsSha256; // 快照失败（权限受限等），复用上次结果
    g_lastKallsymsSha256 = drop::ensure_kernel_symbol_uploaded(cfg.apiBaseURL, cfg.sessionSID, path);
    ::remove(path.c_str());
    return g_lastKallsymsSha256;
}

static WindowPayload collect_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    std::string dataPath = "/tmp/mini_drop_native_cp_" + std::to_string(window.startMs) + ".data";
    std::string perf = perf_bin();
    const bool pythonFallbackEnabled = env_enabled_default("DROP_NATIVE_CP_PYTHON_FALLBACK_ENABLED", true);
    const int pythonMaxProcesses = env_positive_int("DROP_NATIVE_CP_PYTHON_MAX_PROCESSES", 4);
    const int pythonRateHz = env_positive_int("DROP_NATIVE_CP_PYTHON_RATE_HZ", 19);
    drop::PythonFallbackCapture pythonCapture;
    if (pythonFallbackEnabled)
        pythonCapture = drop::start_python_fallback_capture(
            cfg.sessionSID, cfg.aggregationWindowSec, pythonRateHz, pythonMaxProcesses);
    // 云 VM 上硬件 cycles 计数器可能冻结（perf stat 读到 2^50 固定值），
    // perf 默认的 cycles 事件会采不到任何样本。默认改用软件事件 cpu-clock，
    // 可用 DROP_NATIVE_CP_PERF_EVENT 覆盖（Step 1 实测结论）。
    std::string perfEvent = "cpu-clock";
    if (const char *env = std::getenv("DROP_NATIVE_CP_PERF_EVENT"))
        if (*env)
            perfEvent = env;
    std::string recordOutput;
    int64_t tRecordStart = now_ms();
    std::vector<std::string> recordArgs = perf_record_args(cfg, perf, perfEvent, dataPath);
    if (recordArgs.empty())
    {
        window.endMs = now_ms();
        pythonCapture.Finish();
        return window;
    }
    int rc = drop::exec_capture(recordArgs, &recordOutput, 4096);
    window.endMs = now_ms();
    int64_t recordMs = window.endMs - tRecordStart;
    if (rc != 0)
    {
        std::cout << "[native-cp] perf record failed rc=" << rc << " output=" << recordOutput << std::endl;
        ::remove(dataPath.c_str());
        // std::future from std::async blocks during destruction. Consume the
        // concurrent fallback here so an error cannot create an implicit,
        // unreported wait at function exit.
        pythonCapture.Finish();
        return window;
    }
    int64_t tWarmStart = now_ms();
    std::vector<drop::BuildIdEntry> buildIds = warm_build_id_cache(perf, dataPath);
    int64_t warmMs = now_ms() - tWarmStart;
    // Java/Node/Python JIT perf map 定位/校验/搬运 + Java map 刷新，
    // 必须在 perf script 前完成，让同一份 perf.data 能解析出用户函数名。
    int64_t tRtStart = now_ms();
    drop::RuntimeSymbolReport runtimeReport = drop::collect_runtime_maps(perf, dataPath);
    int64_t rtMs = now_ms() - tRtStart;
    std::set<std::string> knownDsoPaths;
    for (const auto &entry : buildIds)
        knownDsoPaths.insert(entry.dsoPath);
    for (auto &entry : drop::discover_sampled_go_build_ids(runtimeReport.sampledPids))
        if (knownDsoPaths.insert(entry.dsoPath).second)
            buildIds.push_back(std::move(entry));
    int64_t tGoStart = now_ms();
    drop::GoSymbolReport goReport = drop::prepare_go_symbols(buildIds);
    int64_t goMs = now_ms() - tGoStart;
    int64_t tScriptStart = now_ms();
    std::string scriptOutput;
    rc = drop::exec_capture({perf, "script", "-F", "comm,pid,tid,time,event,ip,sym,dso", "-i", dataPath},
                            &scriptOutput, 32 * 1024 * 1024);
    ::remove(dataPath.c_str());
    int64_t scriptMs = now_ms() - tScriptStart;
    std::cout << "[native-cp] perf window record_ms=" << recordMs
              << " buildid_ms=" << warmMs
              << " runtime_map_ms=" << rtMs
              << " script_ms=" << scriptMs << std::endl;
    if (rc != 0)
    {
        std::cout << "[native-cp] perf script failed rc=" << rc << std::endl;
        pythonCapture.Finish();
        return window;
    }
    window.samples = parse_perf_script(scriptOutput);
    std::map<int, bool> goBuildInfoCache;
    for (auto &sample : window.samples)
    {
		sample.processStartMs = configured_process_start_ms(cfg, sample.pid);
		if (sample.exe.empty())
			sample.exe = configured_process_exe(cfg, sample.pid);
        sample.backend = "perf";
        sample.runtime = sample_runtime(sample, goReport, &goBuildInfoCache);
        if (sample.runtime == "python")
            for (auto &frame : sample.stack)
                frame = sanitize_python_perf_frame(frame);
    }
	if (cfg.scope == "process")
	{
		window.samples.erase(std::remove_if(window.samples.begin(), window.samples.end(), [&](const auto &sample) {
				return !process_targeted(cfg, sample.pid, sample.processStartMs, sample.exe);
			}), window.samples.end());
	}

    size_t pythonLimitedCount = pythonCapture.LimitedCount();
    std::vector<drop::PythonFallbackResult> pythonResults = pythonCapture.Finish();
    for (auto &result : pythonResults)
    {
        if (result.ready && !drop::python_process_is_same(result.pid, result.startMs))
        {
            result.ready = false;
            result.samples.clear();
            result.reason = "process exited or PID was reused before stack replacement";
        }
    }
    std::set<int> replacedPythonPids;
    for (const auto &result : pythonResults)
        if (result.ready)
            replacedPythonPids.insert(result.pid);
    if (!replacedPythonPids.empty())
    {
        window.samples.erase(std::remove_if(window.samples.begin(), window.samples.end(), [&](const auto &sample) {
                                 return replacedPythonPids.count(sample.pid) > 0;
                             }),
                             window.samples.end());
        for (const auto &result : pythonResults)
        {
            if (!result.ready)
                continue;
            for (const auto &raw : result.samples)
            {
                AggregatedSample sample;
                sample.stack = raw.stack;
                sample.comm = result.comm.empty() ? "python" : result.comm;
                sample.pid = result.pid;
				sample.processStartMs = result.startMs;
                sample.exe = result.exe;
                sample.backend = "py-spy";
                sample.runtime = "python";
                sample.count = clamp_count(raw.count);
                window.samples.push_back(std::move(sample));
            }
        }
    }

    std::map<int, AggregatedSample> metadata;
    for (const auto &sample : window.samples)
        metadata.emplace(sample.pid, sample);
    std::vector<drop::PythonCandidate> nextCandidates;
    if (pythonFallbackEnabled)
    {
        for (int pid : runtimeReport.python.missingPids)
        {
            int64_t startMs = 0;
            if (!drop::python_process_start_ms(pid, &startMs))
                continue;
            drop::PythonCandidate candidate;
            candidate.pid = pid;
            candidate.startMs = startMs;
            candidate.samples = runtimeReport.sampledPids[pid];
            auto it = metadata.find(pid);
            candidate.comm = it == metadata.end() ? "python" : it->second.comm;
            candidate.exe = it == metadata.end() ? read_exe(pid) : it->second.exe;
            nextCandidates.push_back(std::move(candidate));
        }
    }
    drop::schedule_python_fallback(cfg.sessionSID, nextCandidates);

    // User-driven process Sessions expose only CPU/IO/sched. Do not scan or
    // ingest unrelated Python memory data from the host into those Sessions.
    if (cfg.scope != "process" && env_enabled_default("DROP_NATIVE_CP_PYTHON_RSS_ENABLED", true))
    {
        size_t truncated = 0;
        auto rss = drop::collect_python_rss(
            static_cast<size_t>(env_positive_int("DROP_NATIVE_CP_PYTHON_RSS_MAX_PROCESSES", 128)), &truncated);
        window.rssTruncated = truncated;
        for (const auto &point : rss)
        {
            MetricPayload metric;
            metric.metric = "rss_bytes";
            metric.unit = "bytes";
            metric.runtime = "python";
            metric.comm = point.comm;
            metric.pid = point.pid;
            metric.processStartMs = point.startMs;
            metric.exe = point.exe;
            metric.timestampMs = point.timestampMs;
            metric.value = point.valueBytes;
            window.metrics.push_back(std::move(metric));
        }
    }
    std::vector<drop::MemrayProfileResult> memrayResults;
    if (cfg.scope != "process" && env_enabled_default("DROP_NATIVE_CP_MEMRAY_INGEST_ENABLED", true))
    {
        memrayResults = drop::collect_memray_profiles();
        for (const auto &result : memrayResults)
        {
            ProfilePayload profile;
            profile.signalType = "python_memory";
            profile.backend = "memray";
            profile.profileID = result.profileID;
            profile.unit = "bytes";
            if (result.ready)
                profile.readyPath = result.readyPath;
            for (const auto &raw : result.samples)
            {
                AggregatedSample sample;
                sample.stack = raw.stack;
                sample.comm = result.comm;
                sample.pid = result.pid;
                sample.exe = result.exe;
                sample.backend = "memray";
                sample.runtime = "python";
                sample.count = clamp_count(raw.count);
                profile.samples.push_back(std::move(sample));
            }
            window.profiles.push_back(std::move(profile));
        }
    }
	if (cfg.scope == "process")
	{
		window.metrics.erase(std::remove_if(window.metrics.begin(), window.metrics.end(), [&](const auto &metric) {
				return !process_targeted(cfg, metric.pid, metric.processStartMs, metric.exe);
			}), window.metrics.end());
			for (auto &profile : window.profiles)
				profile.samples.erase(std::remove_if(profile.samples.begin(), profile.samples.end(), [&](const auto &sample) {
					return !process_targeted(cfg, sample.pid, sample.processStartMs, sample.exe);
				}), profile.samples.end());
		window.profiles.erase(std::remove_if(window.profiles.begin(), window.profiles.end(), [](const auto &profile) {
			return profile.samples.empty();
		}), window.profiles.end());
	}
    // 低频内核符号快照上传，sha256 写入 symbol_refs 供诊断接口判断入库状态。
    std::string kallsymsSha256 = ensure_kallsyms_snapshot(cfg);
    window.symbolRefsJson = combined_symbol_refs_json(runtimeReport, goReport, pythonResults, pythonLimitedCount, memrayResults, window.samples, buildIds, kallsymsSha256);
    // 异步上报本次窗口引用的 build-id 到服务端符号库（不阻塞当次解析）。
    drop::upload_build_ids_async(buildIds, cfg.apiBaseURL);
    std::cout << "[native-cp] Go symbols ready=" << goReport.ready.size()
              << " pending=" << goReport.pending.size()
              << " failed=" << goReport.failed.size()
              << " prepare_ms=" << goMs << std::endl;
    return window;
}

static bool env_enabled_local(const char *name)
{
    const char *v = std::getenv(name);
    if (!v)
        return false;
    std::string s(v);
    std::transform(s.begin(), s.end(), s.begin(), [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return s == "1" || s == "true" || s == "yes" || s == "on";
}

static std::string env_string_local(const char *name, const std::string &fallback = "")
{
    const char *v = std::getenv(name);
    if (v && *v)
        return v;
    return fallback;
}

static bool signal_enabled(const std::string &signals, const std::string &name)
{
    std::string all = signals;
    std::transform(all.begin(), all.end(), all.begin(), [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    if (all.empty())
        all = "cpu,io,sched";
    std::stringstream ss(all);
    std::string item;
    while (std::getline(ss, item, ','))
    {
        item = drop::trim(item);
        if (item == name || item == "all")
            return true;
    }
    return false;
}

static bool tracepoint_exists(const std::string &name)
{
    std::ifstream tracing("/sys/kernel/tracing/available_events");
    std::string line;
    while (std::getline(tracing, line))
        if (line == name)
            return true;
    std::ifstream debug("/sys/kernel/debug/tracing/available_events");
    while (std::getline(debug, line))
        if (line == name)
            return true;
    return false;
}

static bool command_available(const std::string &cmd)
{
    std::string out;
    return drop::exec_capture({"sh", "-c", "command -v " + cmd}, &out, 1024) == 0 && !drop::trim(out).empty();
}

static std::vector<AggregatedSample> parse_bpftrace_stack_output(const std::string &text,
                                                                 const std::string &scope,
                                                                 const std::string &backend)
{
    std::map<std::string, AggregatedSample> byKey;
    std::istringstream iss(text);
    std::string line;
    auto addStack = [&](std::vector<std::string> frames, const std::string &countText) {
        uint64_t count = 0;
        if (!parse_count_strict(countText, &count))
            return;
        if (count == 0 || frames.empty())
            return;
        std::vector<std::string> clean;
        for (auto frame : frames)
        {
            frame = drop::trim(frame);
            if (frame.empty() || frame == "@[" || frame == "]" || looks_like_bpftrace_error(frame))
                continue;
            clean.push_back(frame);
        }
        if (clean.empty())
            return;
        std::reverse(clean.begin(), clean.end());
        std::string key;
        for (const auto &f : clean)
            key += f + ";";
        AggregatedSample &sample = byKey[key];
        if (sample.count == 0)
        {
            sample.comm = scope == "user" ? "ebpf-user" : "ebpf-kernel";
            sample.pid = 0;
            sample.exe = "";
            sample.stack = clean;
            sample.stackScope = scope;
            sample.backend = backend;
        }
        sample.count = add_count(sample.count, count);
    };
    while (std::getline(iss, line))
    {
        std::string trimmed = drop::trim(line);
        if (trimmed.empty() || trimmed.find("Attaching") == 0 || looks_like_bpftrace_error(trimmed))
            continue;
        if (trimmed.find("@[") == 0 && trimmed.find("]:") == std::string::npos)
        {
            std::vector<std::string> frames;
            std::string first = drop::trim(trimmed.substr(2));
            if (!first.empty())
                frames.push_back(first);
            while (std::getline(iss, line))
            {
                std::string inner = drop::trim(line);
                size_t end = inner.rfind("]:");
                if (end != std::string::npos)
                {
                    std::string frame = drop::trim(inner.substr(0, end));
                    if (!frame.empty())
                        frames.push_back(frame);
                    addStack(frames, inner.substr(end + 2));
                    break;
                }
                if (!inner.empty())
                    frames.push_back(inner);
            }
            continue;
        }
        size_t colon = trimmed.rfind(':');
        if (colon == std::string::npos)
            continue;
        std::string stackText = drop::trim(trimmed.substr(0, colon));
        std::string countText = drop::trim(trimmed.substr(colon + 1));
        if (stackText.find('[') != std::string::npos)
            stackText = stackText.substr(stackText.find('[') + 1);
        if (!stackText.empty() && stackText.back() == ']')
            stackText.pop_back();
        std::vector<std::string> frames;
        std::stringstream ss(stackText);
        std::string frame;
        while (std::getline(ss, frame, ';'))
        {
            frame = drop::trim(frame);
            if (!frame.empty() && frame != "@")
                frames.push_back(frame);
        }
        addStack(frames, countText);
    }
    std::vector<AggregatedSample> out;
    for (auto &kv : byKey)
        out.push_back(kv.second);
    return out;
}

static ProfilePayload collect_bpftrace_cpu_profile(const ContinuousSamplerConfig &cfg,
                                                   const std::string &scope)
{
    ProfilePayload profile;
    profile.signalType = "cpu_profile";
    profile.backend = "bpftrace";
    profile.stackScope = scope;
    std::string stackExpr = scope == "user" ? "ustack" : "kstack";
    std::string scriptPath = "/tmp/mini_drop_native_cp_cpu_" + scope + "_" + std::to_string(now_ms()) + ".bt";
    {
        std::ofstream out(scriptPath);
        out << "profile:hz:" << cfg.sampleRateHz << " " << bpftrace_target_predicate(cfg)
            << "\n{\n  @samples[" << stackExpr << "] = count();\n}\n";
    }
    std::string output;
    int rc = drop::exec_capture({"timeout", "-s", "INT", "-k", "2", std::to_string(std::max(1, cfg.aggregationWindowSec)), "bpftrace", scriptPath}, &output, 16 * 1024 * 1024);
    ::remove(scriptPath.c_str());
    profile.samples = parse_bpftrace_stack_output(output, scope, "bpftrace");
    if (rc != 0 && profile.samples.empty())
    {
        std::cout << "[native-cp] bpftrace cpu " << scope << " failed rc=" << rc << " output=" << output << std::endl;
        return profile;
    }
    return profile;
}

static double parse_hist_bound(const std::string &raw)
{
    std::string s = drop::trim(raw);
    if (s.empty() || s == "..." || s == "-inf" || s == "inf")
        return 0;
    double multiplier = 1;
    char suffix = s.empty() ? '\0' : static_cast<char>(std::toupper(static_cast<unsigned char>(s.back())));
    if (suffix == 'K' || suffix == 'M' || suffix == 'G' || suffix == 'T')
    {
        s.pop_back();
        if (suffix == 'K')
            multiplier = 1024.0;
        else if (suffix == 'M')
            multiplier = 1024.0 * 1024.0;
        else if (suffix == 'G')
            multiplier = 1024.0 * 1024.0 * 1024.0;
        else
            multiplier = 1024.0 * 1024.0 * 1024.0 * 1024.0;
    }
    return std::strtod(s.c_str(), nullptr) * multiplier;
}

static void summarize_histogram(HistogramPayload *hist)
{
    if (!hist || hist->buckets.empty())
        return;
    uint64_t total = 0;
    hist->min = hist->buckets.front().low;
    hist->max = hist->buckets.front().high;
    for (const auto &bucket : hist->buckets)
    {
        total = add_count(total, bucket.count);
        hist->min = std::min(hist->min, bucket.low);
        hist->max = std::max(hist->max, bucket.high);
    }
    auto valueAt = [&](double pct) {
        if (total == 0)
            return 0.0;
        uint64_t threshold = static_cast<uint64_t>(total * pct + 0.999999);
        if (threshold == 0)
            threshold = 1;
        uint64_t seen = 0;
        for (const auto &bucket : hist->buckets)
        {
            seen = add_count(seen, bucket.count);
            if (seen >= threshold)
                return (bucket.low + bucket.high) / 2.0;
        }
        const auto &last = hist->buckets.back();
        return (last.low + last.high) / 2.0;
    };
    hist->p50 = valueAt(0.50);
    hist->p95 = valueAt(0.95);
    hist->p99 = valueAt(0.99);
    if (hist->eventCount == 0)
        hist->eventCount = total;
}

static HistogramPayload parse_bpftrace_histogram(const std::string &text,
                                                 const std::string &signalType,
                                                 const std::string &backend)
{
    HistogramPayload hist;
    hist.signalType = signalType;
    hist.backend = backend;
    std::regex bucketRe(R"(^\s*[\[\(]\s*([^,\]\)]+)\s*(?:,\s*([^\]\)]+)\s*)?[\]\)]\s+([0-9]+))");
    std::istringstream iss(text);
    std::string line;
    std::map<std::string, HistogramBucket> merged;
    while (std::getline(iss, line))
    {
        std::smatch m;
        if (!std::regex_search(line, m, bucketRe))
            continue;
        std::string lowRaw = m[1].str();
        std::string highRaw = m.size() > 2 ? m[2].str() : "";
        uint64_t count = clamp_count(static_cast<uint64_t>(std::strtoull(m[3].str().c_str(), nullptr, 10)));
        double low = parse_hist_bound(lowRaw);
        double high = highRaw.empty() ? low : parse_hist_bound(highRaw);
        std::string range = highRaw.empty() ? ("[" + drop::trim(lowRaw) + "]") : ("[" + drop::trim(lowRaw) + ", " + drop::trim(highRaw) + ")");
        std::string key = range + "|" + std::to_string(low) + "|" + std::to_string(high);
        auto &bucket = merged[key];
        if (bucket.range.empty())
        {
            bucket.range = range;
            bucket.low = low;
            bucket.high = high;
        }
        bucket.count = add_count(bucket.count, count);
    }
    for (auto &kv : merged)
        hist.buckets.push_back(kv.second);
    std::sort(hist.buckets.begin(), hist.buckets.end(), [](const HistogramBucket &a, const HistogramBucket &b) {
        if (a.low == b.low)
            return a.high < b.high;
        return a.low < b.low;
    });
    summarize_histogram(&hist);
    return hist;
}

static HistogramPayload collect_bpftrace_latency_histogram(const ContinuousSamplerConfig &cfg,
                                                           const std::string &signalType)
{
    HistogramPayload hist;
    hist.signalType = signalType;
    hist.backend = "bpftrace";
    bool sched = signalType == "sched_latency";
	bool syscallIO = signalType == "io_syscall_latency";
    if (!command_available("bpftrace"))
    {
        hist.unavailable = true;
        hist.reason = "bpftrace unavailable";
        return hist;
    }
    if (!sched && !syscallIO && (!tracepoint_exists("block:block_rq_issue") || !tracepoint_exists("block:block_rq_complete")))
    {
        hist.unavailable = true;
        hist.reason = "block tracepoints unavailable";
        return hist;
    }
    if (sched && (!tracepoint_exists("sched:sched_wakeup") || !tracepoint_exists("sched:sched_switch")))
    {
        hist.unavailable = true;
        hist.reason = "sched tracepoints unavailable";
        return hist;
    }

    std::string scriptPath = "/tmp/mini_drop_native_cp_" + signalType + "_" + std::to_string(now_ms()) + ".bt";
    {
        std::ofstream out(scriptPath);
        if (syscallIO)
		{
			std::string predicate = bpftrace_target_predicate(cfg, "pid");
			out << "tracepoint:syscalls:sys_enter_read,tracepoint:syscalls:sys_enter_write,tracepoint:syscalls:sys_enter_pread64,tracepoint:syscalls:sys_enter_pwrite64 " << predicate << " { @sys_start[tid] = nsecs; }\n"
				<< "tracepoint:syscalls:sys_exit_read,tracepoint:syscalls:sys_exit_write,tracepoint:syscalls:sys_exit_pread64,tracepoint:syscalls:sys_exit_pwrite64 /@sys_start[tid]/ { $lat = (nsecs - @sys_start[tid]) / 1000; @lat = hist($lat); @events = count(); delete(@sys_start[tid]); }\n";
		}
        else if (sched)
        {
			std::string nextPredicate = bpftrace_target_predicate(cfg, "args->pid");
            out << "#define pid_t int\n"
				<< "tracepoint:sched:sched_wakeup " << nextPredicate << " { @wake[args->pid] = nsecs; }\n"
				<< "tracepoint:sched:sched_wakeup_new " << nextPredicate << " { @wake[args->pid] = nsecs; }\n"
                << "tracepoint:sched:sched_switch /@wake[args->next_pid]/ { $lat = (nsecs - @wake[args->next_pid]) / 1000; @lat = hist($lat); @events = count(); delete(@wake[args->next_pid]); }\n";
        }
        else
        {
			std::string issuePredicate = bpftrace_target_predicate(cfg, "pid");
            out << "#define dev_t unsigned int\n#define sector_t unsigned long\n"
				<< "tracepoint:block:block_rq_issue " << issuePredicate << " { @rq_start[args->dev, args->sector] = nsecs; }\n"
                << "tracepoint:block:block_rq_complete /@rq_start[args->dev, args->sector]/ { $lat = (nsecs - @rq_start[args->dev, args->sector]) / 1000; @lat = hist($lat); @events = count(); delete(@rq_start[args->dev, args->sector]); }\n";
        }
    }
    // io/sched histogram 用比 CPU 窗口略短的采集时长（默认 aggregationWindowSec-3），
    // 让它们总能先于 perf 路径完成；即使偶发被搁置（超预算），后台 bpftrace 也能
    // 更早自灭，减少与后续窗口的 tracepoint 竞争级联。可用 DROP_NATIVE_CP_HISTOGRAM_SEC 覆盖。
    int histSec = std::max(1, cfg.aggregationWindowSec - 3);
    if (const char *env = std::getenv("DROP_NATIVE_CP_HISTOGRAM_SEC"))
    {
        int v = std::atoi(env);
        if (v > 0)
            histSec = v;
    }
    std::string output;
    int64_t tExecStart = now_ms();
    int rc = drop::exec_capture({"timeout", "-s", "INT", "-k", "2", std::to_string(histSec), "bpftrace", scriptPath}, &output, 8 * 1024 * 1024);
    int64_t execMs = now_ms() - tExecStart;
    ::remove(scriptPath.c_str());
    std::cout << "[native-cp] " << signalType << " exec_ms=" << execMs
              << " out_bytes=" << output.size() << " rc=" << rc << std::endl;
    hist = parse_bpftrace_histogram(output, signalType, "bpftrace");
    if (rc != 0 && hist.buckets.empty())
    {
        hist.unavailable = true;
        hist.reason = "bpftrace failed rc=" + std::to_string(rc);
        std::cout << "[native-cp] " << signalType << " failed rc=" << rc << " output=" << output << std::endl;
        return hist;
    }
    if (hist.buckets.empty())
    {
        hist.unavailable = true;
        hist.reason = "no histogram samples";
    }
    return hist;
}

static std::string build_batch_json(const ContinuousSamplerConfig &cfg,
                                    const std::string &batchID,
                                    const std::vector<WindowPayload> &windows)
{
    std::string start = windows.empty() ? rfc3339_from_ms(now_ms()) : rfc3339_from_ms(windows.front().startMs);
    std::string end = windows.empty() ? start : rfc3339_from_ms(windows.back().endMs);

    std::set<std::string> signalTypes;
    std::map<std::string, std::string> backends;
    for (const auto &window : windows)
    {
        if (!window.samples.empty())
        {
            signalTypes.insert("cpu_profile");
            for (const auto &sample : window.samples)
            {
                if (!sample.backend.empty())
                {
                    backends["cpu_profile"] = sample.backend;
                    break;
                }
            }
        }
        for (const auto &profile : window.profiles)
        {
            signalTypes.insert(profile.signalType.empty() ? "cpu_profile" : profile.signalType);
            if (!profile.backend.empty())
                backends[profile.stackScope.empty() ? "cpu_profile" : ("cpu_" + profile.stackScope)] = profile.backend;
        }
        for (const auto &hist : window.histograms)
        {
            if (!hist.signalType.empty())
                signalTypes.insert(hist.signalType);
            if (!hist.signalType.empty() && !hist.backend.empty())
                backends[hist.signalType] = hist.backend;
        }
        if (!window.metrics.empty())
        {
            signalTypes.insert("python_rss");
            backends["python_rss"] = "procfs";
        }
        if (!window.dbSnapshots.empty())
        {
            signalTypes.insert("db_snapshot");
            backends["db_snapshot"] = "db_system_views";
        }
    }

    std::string body = "{";
    body += "\"session_sid\":\"" + json_escape(cfg.sessionSID) + "\",";
    body += "\"batch_id\":\"" + json_escape(batchID) + "\",";
    body += "\"target_ip\":\"" + json_escape(cfg.targetIP) + "\",";
    // 阶段一：协议 v3。batch 层 sample_count 废弃写 0；分信号计数
    // signal_counts、collector_generation、batch_sequence、content_sha256
    // 是服务端幂等/冲突校验与统计的新事实来源。
    body += "\"schema_version\":3,";
    body += "\"collector_generation\":\"" + json_escape(cfg.collectorGeneration) + "\",";
    body += "\"batch_sequence\":" + std::to_string(cfg.batchSequence) + ",";
    body += "\"content_sha256\":\"" + batch_content_digest(cfg, windows) + "\",";
    body += "\"signal_counts\":" + signal_counts_json(batch_signal_counts(windows)) + ",";
    body += "\"profile_format\":\"json\",";
    // Batch-level backend metadata (aggregated from windows)
    std::string batchBackendStatus = "ok";
    std::string batchBackendReason;
    std::string batchSelectedBackend;
    std::set<std::string> batchAttempted;
    bool anyFailed = false;
    for (const auto &window : windows)
    {
        if (window.backendStatus == "failed")
            anyFailed = true;
        for (const auto &b : window.attemptedBackends)
            batchAttempted.insert(b);
        if (!window.selectedBackend.empty() && batchSelectedBackend.empty())
            batchSelectedBackend = window.selectedBackend;
        if (!window.backendReason.empty() && batchBackendReason.empty())
            batchBackendReason = window.backendReason;
    }
    if (anyFailed && batchBackendStatus != "failed")
        batchBackendStatus = "degraded";
    if (windows.empty() || (batchAttempted.empty() && !anyFailed))
        batchBackendStatus = "ok";
    body += "\"backend_status\":\"" + json_escape(batchBackendStatus) + "\",";
    body += "\"backend_reason\":\"" + json_escape(batchBackendReason) + "\",";
    body += "\"attempted_backends\":[";
    {
        size_t ai = 0;
        for (const auto &b : batchAttempted)
        {
            if (ai++)
                body += ",";
            body += "\"" + json_escape(b) + "\"";
        }
    }
    body += "],";
    body += "\"selected_backend\":\"" + json_escape(batchSelectedBackend) + "\",";
    body += "\"signal_types\":[";
    size_t sigIndex = 0;
    for (const auto &signal : signalTypes)
    {
        if (sigIndex++)
            body += ",";
        body += "\"" + json_escape(signal) + "\"";
    }
    body += "],";
    body += "\"backends\":{";
    size_t beIndex = 0;
    for (const auto &kv : backends)
    {
        if (beIndex++)
            body += ",";
        body += "\"" + json_escape(kv.first) + "\":\"" + json_escape(kv.second) + "\"";
    }
    body += "},";
    body += "\"start_time\":\"" + start + "\",";
    body += "\"end_time\":\"" + end + "\",";
    body += "\"window_count\":" + std::to_string(windows.size()) + ",";
    // 阶段一：batch 层 sample_count 废弃并写 0（所有计数读 signal_counts）。
    body += "\"sample_count\":0,";
    // Batch-level symbol_refs：取最后一个非空窗口的 runtime map 报告（反映最新
    // 运行时状态；runtime map 是进程生命周期相关，用最新比用首个更有代表性）。
    std::string batchSymbolRefs;
    for (auto it = windows.rbegin(); it != windows.rend(); ++it)
    {
        if (!it->symbolRefsJson.empty())
        {
            batchSymbolRefs = it->symbolRefsJson;
            break;
        }
    }
    if (!batchSymbolRefs.empty())
        body += "\"symbol_refs\":" + batchSymbolRefs + ",";
    body += "\"windows\":[";
    for (size_t wi = 0; wi < windows.size(); ++wi)
    {
        const auto &window = windows[wi];
        if (wi)
            body += ",";
        uint64_t windowSamples = 0;
        for (const auto &sample : window.samples)
            windowSamples = add_count(windowSamples, sample.count);
        for (const auto &profile : window.profiles)
            for (const auto &sample : profile.samples)
                windowSamples = add_count(windowSamples, sample.count);
        // 阶段一：逻辑窗口稳定 ID 与内容摘要（确定性，重传/内容冲突时一致）。
        const std::string windowID = window.windowID.empty() ? make_window_id(cfg, window) : window.windowID;
        const std::string windowContent = window.contentSHA256.empty() ? window_content_digest(window) : window.contentSHA256;
        const std::string windowCountsJson = signal_counts_json(window_signal_counts(window));
        body += "{";
        body += "\"window_id\":\"" + json_escape(windowID) + "\",";
        body += "\"collector_generation\":\"" + json_escape(cfg.collectorGeneration) + "\",";
        body += "\"target_fingerprint\":\"" + json_escape(cfg.targetFingerprint) + "\",";
        body += "\"content_sha256\":\"" + json_escape(windowContent) + "\",";
        body += "\"signal_counts\":" + windowCountsJson + ",";
        body += "\"window_start\":\"" + rfc3339_from_ms(window.startMs) + "\",";
        body += "\"window_end\":\"" + rfc3339_from_ms(window.endMs) + "\",";
        body += "\"sample_count\":" + std::to_string(windowSamples) + ",";
        body += "\"profile_format\":\"json\",";
        body += "\"backend_status\":\"" + json_escape(window.backendStatus.empty() ? "ok" : window.backendStatus) + "\",";
        body += "\"backend_reason\":\"" + json_escape(window.backendReason) + "\",";
        body += "\"attempted_backends\":[";
        for (size_t ai = 0; ai < window.attemptedBackends.size(); ++ai)
        {
            if (ai)
                body += ",";
            body += "\"" + json_escape(window.attemptedBackends[ai]) + "\"";
        }
        body += "],";
        body += "\"selected_backend\":\"" + json_escape(window.selectedBackend) + "\",";
        if (!window.symbolRefsJson.empty())
            body += "\"symbol_refs\":" + window.symbolRefsJson + ",";
        body += "\"samples\":[";
        for (size_t si = 0; si < window.samples.size(); ++si)
        {
            const auto &sample = window.samples[si];
            if (si)
                body += ",";
            body += "{";
            body += "\"comm\":\"" + json_escape(sample.comm) + "\",";
            body += "\"pid\":" + std::to_string(sample.pid) + ",";
			body += "\"process_start_ms\":" + std::to_string(sample.processStartMs) + ",";
            body += "\"exe\":\"" + json_escape(sample.exe) + "\",";
            body += "\"runtime\":\"" + json_escape(sample.runtime) + "\",";
            body += "\"count\":" + std::to_string(sample.count) + ",";
            body += "\"stack_scope\":\"" + json_escape(sample.stackScope) + "\",";
            body += "\"backend\":\"" + json_escape(sample.backend) + "\",";
            {
                std::string framesJson = frames_to_json(sample.frames);
                if (!framesJson.empty())
                    body += framesJson + ",";
                if (!frames_only_mode() || framesJson.empty())
                {
            body += "\"stack\":[";
            for (size_t fi = 0; fi < sample.stack.size(); ++fi)
            {
                if (fi)
                    body += ",";
                body += "\"" + json_escape(sample.stack[fi]) + "\"";
            }
            body += "]}";
                }
            }
            body = body.substr(0, body.size() - 1); // 去掉结尾逗号
            body += "}";
        }
        body += "],";
        body += "\"profiles\":[";
        for (size_t pi = 0; pi < window.profiles.size(); ++pi)
        {
            const auto &profile = window.profiles[pi];
            if (pi)
                body += ",";
            body += "{";
            body += "\"signal_type\":\"" + json_escape(profile.signalType.empty() ? "cpu_profile" : profile.signalType) + "\",";
            body += "\"backend\":\"" + json_escape(profile.backend) + "\",";
            body += "\"stack_scope\":\"" + json_escape(profile.stackScope) + "\",";
            body += "\"profile_id\":\"" + json_escape(profile.profileID) + "\",";
            body += "\"unit\":\"" + json_escape(profile.unit) + "\",";
            body += "\"samples\":[";
            for (size_t si = 0; si < profile.samples.size(); ++si)
            {
                const auto &sample = profile.samples[si];
                if (si)
                    body += ",";
                body += "{";
                body += "\"comm\":\"" + json_escape(sample.comm) + "\",";
                body += "\"pid\":" + std::to_string(sample.pid) + ",";
				body += "\"process_start_ms\":" + std::to_string(sample.processStartMs) + ",";
                body += "\"exe\":\"" + json_escape(sample.exe) + "\",";
                body += "\"runtime\":\"" + json_escape(sample.runtime) + "\",";
                body += "\"count\":" + std::to_string(sample.count) + ",";
                body += "\"stack_scope\":\"" + json_escape(profile.stackScope) + "\",";
                body += "\"backend\":\"" + json_escape(profile.backend) + "\",";
                {
                    std::string framesJson = frames_to_json(sample.frames);
                    if (!framesJson.empty())
                        body += framesJson + ",";
                    if (!frames_only_mode() || framesJson.empty())
                    {
                body += "\"stack\":[";
                for (size_t fi = 0; fi < sample.stack.size(); ++fi)
                {
                    if (fi)
                        body += ",";
                    body += "\"" + json_escape(sample.stack[fi]) + "\"";
                }
                body += "]}";
                    }
                }
                body = body.substr(0, body.size() - 1);
                body += "}";
            }
            body += "]}";
        }
        body += "],";
        body += "\"rss_truncated\":" + std::to_string(window.rssTruncated) + ",";
        body += "\"metrics\":[";
        for (size_t mi = 0; mi < window.metrics.size(); ++mi)
        {
            const auto &metric = window.metrics[mi];
            if (mi)
                body += ",";
            body += "{";
            body += "\"metric\":\"" + json_escape(metric.metric) + "\",";
            body += "\"unit\":\"" + json_escape(metric.unit) + "\",";
            body += "\"runtime\":\"" + json_escape(metric.runtime) + "\",";
            body += "\"comm\":\"" + json_escape(metric.comm) + "\",";
            body += "\"pid\":" + std::to_string(metric.pid) + ",";
            body += "\"process_start_ms\":" + std::to_string(metric.processStartMs) + ",";
            body += "\"exe\":\"" + json_escape(metric.exe) + "\",";
            body += "\"timestamp\":\"" + rfc3339_from_ms(metric.timestampMs) + "\",";
            body += "\"value\":" + std::to_string(metric.value);
            body += "}";
        }
        body += "],";
        body += "\"histograms\":[";
        for (size_t hi = 0; hi < window.histograms.size(); ++hi)
        {
            const auto &hist = window.histograms[hi];
            if (hi)
                body += ",";
            body += "{";
            body += "\"signal_type\":\"" + json_escape(hist.signalType) + "\",";
            body += "\"backend\":\"" + json_escape(hist.backend) + "\",";
            body += "\"pid\":" + std::to_string(hist.pid) + ",";
            body += "\"unit\":\"" + json_escape(hist.unit) + "\",";
            body += "\"event_count\":" + std::to_string(hist.eventCount) + ",";
            body += "\"unavailable\":" + std::string(hist.unavailable ? "true" : "false") + ",";
            body += "\"reason\":\"" + json_escape(hist.reason) + "\",";
            body += "\"summary\":{";
            body += "\"min\":" + std::to_string(hist.min) + ",";
            body += "\"max\":" + std::to_string(hist.max) + ",";
            body += "\"p50\":" + std::to_string(hist.p50) + ",";
            body += "\"p95\":" + std::to_string(hist.p95) + ",";
            body += "\"p99\":" + std::to_string(hist.p99) + "},";
            body += "\"buckets\":[";
            for (size_t bi = 0; bi < hist.buckets.size(); ++bi)
            {
                const auto &bucket = hist.buckets[bi];
                if (bi)
                    body += ",";
                body += "{";
                body += "\"range\":\"" + json_escape(bucket.range) + "\",";
                body += "\"low\":" + std::to_string(bucket.low) + ",";
                body += "\"high\":" + std::to_string(bucket.high) + ",";
                body += "\"count\":" + std::to_string(bucket.count);
                body += "}";
            }
            body += "]}";
        }
        body += "],";
        body += "\"db_snapshots\":[";
        for (size_t di = 0; di < window.dbSnapshots.size(); ++di)
        {
            const auto &snap = window.dbSnapshots[di];
            if (di)
                body += ",";
            body += "{";
            body += "\"kind\":\"" + json_escape(snap.kind) + "\",";
            body += "\"instance_label\":\"" + json_escape(snap.instanceLabel) + "\",";
            body += "\"timestamp\":\"" + rfc3339_from_ms(snap.timestampMs) + "\",";
            body += "\"schema_name\":\"" + json_escape(snap.schemaName) + "\",";
            body += "\"digest_text\":\"" + json_escape(snap.digestText) + "\",";
            body += "\"call_count\":" + std::to_string(snap.callCount) + ",";
            body += "\"total_latency_us\":" + std::to_string(snap.totalLatencyUs) + ",";
            body += "\"rows_examined_total\":" + std::to_string(snap.rowsExaminedTotal) + ",";
            body += "\"waiting_pid\":" + std::to_string(snap.waitingPid) + ",";
            body += "\"waiting_query\":\"" + json_escape(snap.waitingQuery) + "\",";
            body += "\"blocking_pid\":" + std::to_string(snap.blockingPid) + ",";
            body += "\"blocking_query\":\"" + json_escape(snap.blockingQuery) + "\",";
            body += "\"wait_seconds\":" + std::to_string(snap.waitSeconds) + ",";
            body += "\"locked_table\":\"" + json_escape(snap.lockedTable) + "\"";
            body += "}";
        }
        body += "]}";
    }
    body += "]}";
    return body;
}

static bool response_acknowledges_batch(const std::string &response, const std::string &batchID)
{
    return response.find("\"accepted\":true") != std::string::npos &&
           response.find("\"batch_id\":\"" + batchID + "\"") != std::string::npos;
}

enum class SpoolPostResult
{
    Acknowledged,
    PermanentlyRejected,
    Failed,
};

static bool response_is_permanent_rejection(int httpStatus, const std::string &response)
{
    if (httpStatus < 400 || httpStatus >= 500)
        return false;
    return response.find("\"retryable\":false") != std::string::npos ||
           response.find("\"retryable\": false") != std::string::npos;
}

static SpoolPostResult post_spooled_batch(const ContinuousSamplerConfig &cfg,
                                          const std::string &path,
                                          const std::string &batchID)
{
    std::string response;
    int rc = drop::exec_capture({"curl", "-sS", "-m", "20", "-X", "POST",
                                 "-H", "Content-Type: application/json",
                                 "-H", "Drop-User-Uid: " + cfg.authUID,
                                 "-H", "X-Mini-Drop-Agent-Time-Ms: " + std::to_string(now_ms()),
                                 "-d", "@" + path,
                                 "-w", "\n%{http_code}",
                                 cfg.apiBaseURL + "/api/v1/internal/continuous/batches"},
                                &response, 8192);
    int httpStatus = 0;
    size_t statusOffset = response.rfind('\n');
    if (statusOffset != std::string::npos)
    {
        httpStatus = std::atoi(response.substr(statusOffset + 1).c_str());
        response.resize(statusOffset);
    }
    if (rc == 0 && httpStatus == 200 && response_acknowledges_batch(response, batchID))
    {
        std::cout << "[native-cp] batch ACK received batch=" << batchID
                  << " response=" << response << std::endl;
        return SpoolPostResult::Acknowledged;
    }
    if (rc == 0 && httpStatus == 409)
    {
        // 阶段一：内容冲突/同一 ID 已被占用。服务端对这类冲突一律返回
        // 不可重试；Agent 移入 .rejected 隔离区并上报错误，绝不通过换 ID
        // （旧版 cpb-retry-* rekey）绕过幂等约束。
        std::cout << "[native-cp] batch/window content conflict batch=" << batchID
                  << " response=" << response << std::endl;
        return SpoolPostResult::PermanentlyRejected;
    }
    if (rc == 0 && response_is_permanent_rejection(httpStatus, response))
    {
        std::cout << "[native-cp] batch permanently rejected batch=" << batchID
                  << " http_status=" << httpStatus
                  << " response=" << response << std::endl;
        return SpoolPostResult::PermanentlyRejected;
    }
    std::cout << "[native-cp] batch upload failed batch=" << batchID
              << " rc=" << rc << " http_status=" << httpStatus
              << " response=" << response << std::endl;
    return SpoolPostResult::Failed;
}

static bool quarantine_rejected_spooled_batch(const std::string &path,
                                               const std::string &batchID)
{
    std::string rejectedPath = path + ".rejected";
    if (file_exists_local(rejectedPath))
        rejectedPath += "." + std::to_string(now_ms());
    if (::rename(path.c_str(), rejectedPath.c_str()) != 0)
        return false;
    const size_t slash = rejectedPath.find_last_of('/');
    const std::string directory = slash == std::string::npos ? "." : rejectedPath.substr(0, slash);
    int dirfd = ::open(directory.c_str(), O_RDONLY | O_DIRECTORY);
    if (dirfd >= 0)
    {
        ::fsync(dirfd);
        ::close(dirfd);
    }
    std::cout << "[native-cp] quarantined permanently rejected batch=" << batchID
              << " path=" << rejectedPath << std::endl;
    return true;
}

static bool persist_batch(const ContinuousSamplerConfig &cfg,
                          const std::string &batchID,
                          const std::string &body)
{
    if (!ensure_directory(session_spool_directory(cfg)))
        return false;
    return atomic_write_file(journal_path(cfg, batchID), body);
}

static bool finalize_batch(const ContinuousSamplerConfig &cfg, const std::string &batchID)
{
    std::string from = journal_path(cfg, batchID);
    std::string to = spool_path(cfg, batchID);
    if (::rename(from.c_str(), to.c_str()) != 0)
        return false;
    int dirfd = ::open(session_spool_directory(cfg).c_str(), O_RDONLY | O_DIRECTORY);
    if (dirfd >= 0)
    {
        ::fsync(dirfd);
        ::close(dirfd);
    }
    return true;
}

static void recover_spool_journals(const ContinuousSamplerConfig &cfg)
{
    std::vector<std::string> journals;
    append_spool_files(cfg.spoolDirectory, ".journal", &journals);
    for (const auto &path : journals)
    {
        size_t slash = path.find_last_of('/');
        std::string directory = path.substr(0, slash);
        std::string name = path.substr(slash + 1);
        std::string batchID = name.substr(0, name.size() - 8);
        std::string destination = directory + "/" + safe_spool_component(batchID) + ".json";
        if (::rename(path.c_str(), destination.c_str()) != 0)
            std::cout << "[native-cp] failed to recover spool journal batch=" << batchID << std::endl;
    }
}

static void recover_session_spool_journals(const ContinuousSamplerConfig &cfg)
{
    for (const auto &path : list_session_spool_files(cfg, ".journal"))
    {
        size_t slash = path.find_last_of('/');
        std::string directory = path.substr(0, slash);
        std::string name = path.substr(slash + 1);
        std::string batchID = name.substr(0, name.size() - 8);
        std::string destination = directory + "/" + safe_spool_component(batchID) + ".json";
        if (::rename(path.c_str(), destination.c_str()) != 0)
            std::cout << "[native-cp] failed to recover stopped Session journal batch=" << batchID << std::endl;
    }
}

static void schedule_spool_retry(const ContinuousSamplerConfig &cfg, SpoolRetryState *retry)
{
    int jitterMs = retry->delaySec <= 1 ? 0 : std::rand() % (retry->delaySec * 250 + 1);
    retry->nextAttemptMs = now_ms() + retry->delaySec * 1000LL + jitterMs;
    retry->delaySec = std::min(std::max(1, cfg.retryMaxSec), retry->delaySec * 2);
}

static bool drain_one_spooled_batch(const ContinuousSamplerConfig &cfg,
                                    SpoolRetryState *retry,
                                    bool force)
{
    // A shared Agent spool contains one directory per Session. Draining the
    // global recursive list with one Session's config could retry a different
    // Session's batch forever after a conflict, starving every other Session.
    std::vector<std::string> files = list_session_spool_files(cfg, ".json");
    if (files.empty())
    {
        retry->delaySec = 1;
        retry->nextAttemptMs = 0;
        return true;
    }
    if (!force && now_ms() < retry->nextAttemptMs)
        return false;

    const std::string &path = files.front();
    std::string batchID = batch_id_from_spool_path(path);
    const SpoolPostResult result = post_spooled_batch(cfg, path, batchID);
    if (result == SpoolPostResult::Acknowledged)
    {
        if (::unlink(path.c_str()) != 0)
            std::cout << "[native-cp] ACK received but spool removal failed path=" << path
                      << " errno=" << errno << std::endl;
        retry->delaySec = 1;
        retry->nextAttemptMs = 0;
        return true;
    }
    if (result == SpoolPostResult::PermanentlyRejected &&
        quarantine_rejected_spooled_batch(path, batchID))
    {
        retry->delaySec = 1;
        retry->nextAttemptMs = 0;
        return true;
    }
    schedule_spool_retry(cfg, retry);
    return false;
}

static void interruptible_wait(const std::atomic<bool> &running, int milliseconds)
{
    int remaining = milliseconds;
    while (running.load() && remaining > 0)
    {
        int slice = std::min(remaining, 200);
        std::this_thread::sleep_for(std::chrono::milliseconds(slice));
        remaining -= slice;
    }
}

static void acknowledge_batch_profiles(const std::vector<WindowPayload> &windows)
{
    for (const auto &window : windows)
        for (const auto &profile : window.profiles)
            if (!profile.readyPath.empty() && !drop::acknowledge_memray_profile(profile.readyPath))
                std::cout << "[native-cp] failed to mark Memray profile done: " << profile.readyPath << std::endl;
}

static void release_batch_profiles(const std::vector<WindowPayload> &windows)
{
    for (const auto &window : windows)
        for (const auto &profile : window.profiles)
            if (!profile.readyPath.empty())
                drop::release_memray_profile(profile.readyPath);
}

static void release_window_profiles(const WindowPayload &window)
{
    for (const auto &profile : window.profiles)
        if (!profile.readyPath.empty())
            drop::release_memray_profile(profile.readyPath);
}

static std::string core_unavailable_reason()
{
    std::string btf = env_string_local("DROP_BTF_PATH", "/sys/kernel/btf/vmlinux");
    if (!file_exists_local(btf))
        return "CO-RE BTF unavailable";
    return "CO-RE CPU sampler object is not enabled in this build";
}

// 等待 io/sched future 的预算：超出则标记该信号本轮 unavailable，不阻塞 CPU 窗口
static constexpr int64_t kIoWaitBudgetMs = 3000;

// std::async 返回的 future 析构会阻塞直到任务完成；超出预算时不能直接丢弃，
// 否则析构照样把窗口拖长。把超预算的 future 移入该列表，后续窗口迭代惰性
// 回收（wait_for(0) 就绪才 get()+erase），其后台 bpftrace 会按自身 timeout
// 在 ~10s 内退出，不会无限增长。
static std::mutex g_abandonedFuturesMutex;
static std::vector<std::future<HistogramPayload>> g_abandonedFutures;

static void reap_abandoned_hist_futures()
{
    std::lock_guard<std::mutex> lock(g_abandonedFuturesMutex);
    for (auto it = g_abandonedFutures.begin(); it != g_abandonedFutures.end();)
    {
        if (it->wait_for(std::chrono::milliseconds(0)) == std::future_status::ready)
        {
            try
            {
                it->get(); // 丢弃结果
            }
            catch (...)
            {
                // 忽略后台任务异常
            }
            it = g_abandonedFutures.erase(it);
        }
        else
        {
            ++it;
        }
    }
}

// 等待 io/sched histogram future，带预算；超预算标记 unavailable 并移入
// abandoned 列表（不阻塞窗口）。
static HistogramPayload wait_histogram_budgeted(std::future<HistogramPayload> &f,
                                                const std::string &signalType)
{
    if (!f.valid())
    {
        HistogramPayload h;
        h.signalType = signalType;
        h.backend = "bpftrace";
        h.unavailable = true;
        h.reason = "not started";
        return h;
    }
    if (f.wait_for(std::chrono::milliseconds(kIoWaitBudgetMs)) == std::future_status::ready)
        return f.get();
    // 超预算：不阻塞 CPU 窗口，本轮标记 unavailable，后台任务继续（自身 timeout 会退出）
    {
        std::lock_guard<std::mutex> lock(g_abandonedFuturesMutex);
        g_abandonedFutures.push_back(std::move(f));
    }
    HistogramPayload h;
    h.signalType = signalType;
    h.backend = "bpftrace";
    h.unavailable = true;
    h.reason = "not ready within " + std::to_string(kIoWaitBudgetMs) + "ms budget";
    std::cout << "[native-cp] " << signalType << " exceeded budget, marked unavailable" << std::endl;
    return h;
}

static WindowPayload collect_dual_track_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    int64_t captureEndMs = window.startMs + static_cast<int64_t>(std::max(1, cfg.aggregationWindowSec)) * 1000;
    // 回收上一轮超预算被搁置的 io/sched future（它们已完成后台任务则清理）
    reap_abandoned_hist_futures();
    std::string signals = cfg.signals.empty() ? env_string_local("DROP_NATIVE_CP_SIGNALS", "cpu,io,io_syscall,sched") : cfg.signals;
    bool ebpfEnabled = env_enabled_local("DROP_NATIVE_CP_EBPF_ENABLED");
    std::vector<std::string> &attempted = window.attemptedBackends;

    // ============================================================
    // 同窗并发：perf CPU / io / sched 三个采集 future 在窗口开始时同时启动，
    // 保证 CPU、IO、sched 覆盖同一个真实 ~10s 区间（修复窗口被串行拉长到
    // ~32-34s 的问题）。默认 CPU 后端固定为 perf；bpftrace CPU 仅作为实验性
    // 路径保留（显式配置只含 bpftrace 时才使用）。
    // ============================================================
    bool cpuStarted = false;
    bool cpuUsePerf = false;
    std::future<WindowPayload> perfCpuFuture;
    std::future<ProfilePayload> bpftraceUserFuture;
    std::future<ProfilePayload> bpftraceKernelFuture;

    if (signal_enabled(signals, "cpu"))
    {
        std::string backends = env_string_local("DROP_NATIVE_CP_CPU_BACKENDS", "perf");
        bool coreAllowed = backends.find("core") != std::string::npos;
        bool bpftraceAllowed = backends.find("bpftrace") != std::string::npos;
        bool perfAllowed = backends.find("perf") != std::string::npos;

        if (perfAllowed)
        {
            // 生产路径：perf 直接并发启动，不再等 bpftrace 失败再 fallback。
            attempted.push_back("perf");
            perfCpuFuture = std::async(std::launch::async, collect_window, cfg);
            cpuStarted = true;
            cpuUsePerf = true;
        }
        else if (bpftraceAllowed && ebpfEnabled && command_available("bpftrace"))
        {
            // 实验性路径：bpftrace user/kernel（不参与本轮生产验收）。
            attempted.push_back("bpftrace");
            bpftraceUserFuture = std::async(std::launch::async, collect_bpftrace_cpu_profile, cfg, "user");
            bpftraceKernelFuture = std::async(std::launch::async, collect_bpftrace_cpu_profile, cfg, "kernel");
            cpuStarted = true;
        }
        else if (coreAllowed && ebpfEnabled)
        {
            attempted.push_back("core");
            std::cout << "[native-cp] CO-RE CPU backend unavailable: " << core_unavailable_reason() << std::endl;
        }
        else
        {
            window.backendStatus = "failed";
            window.backendReason = "no CPU backend enabled";
        }
    }

    // IO/sched histograms（bpftrace）与 CPU 同窗并发。
    std::future<HistogramPayload> ioFuture;
	std::future<HistogramPayload> ioSyscallFuture;
    std::future<HistogramPayload> schedFuture;
    bool ioStarted = false;
	bool ioSyscallStarted = false;
    bool schedStarted = false;
    if (ebpfEnabled && signal_enabled(signals, "io"))
    {
        ioFuture = std::async(std::launch::async, collect_bpftrace_latency_histogram, cfg, "io_latency");
        ioStarted = true;
    }
	if (ebpfEnabled && signal_enabled(signals, "io_syscall"))
	{
		ioSyscallFuture = std::async(std::launch::async, collect_bpftrace_latency_histogram, cfg, "io_syscall_latency");
		ioSyscallStarted = true;
	}
    if (ebpfEnabled && signal_enabled(signals, "sched"))
    {
        schedFuture = std::async(std::launch::async, collect_bpftrace_latency_histogram, cfg, "sched_latency");
        schedStarted = true;
    }

    // ============================================================
    // 等待所有本窗采集任务完成
    // ============================================================
    uint64_t cpuSamples = 0;
    if (cpuStarted)
    {
        if (cpuUsePerf)
        {
            WindowPayload perfWindow = perfCpuFuture.get();
            window.samples = perfWindow.samples;
            window.profiles = std::move(perfWindow.profiles);
            window.metrics = std::move(perfWindow.metrics);
            window.rssTruncated = perfWindow.rssTruncated;
            if (perfWindow.endMs > 0)
                captureEndMs = perfWindow.endMs;
            for (auto &sample : window.samples)
                sample.backend = "perf";
            window.symbolRefsJson = perfWindow.symbolRefsJson; // runtime map 诊断
            for (const auto &sample : window.samples)
                cpuSamples = add_count(cpuSamples, sample.count);
            window.selectedBackend = "perf";
        }
        else
        {
            ProfilePayload user = bpftraceUserFuture.get();
            if (!user.samples.empty())
            {
                window.profiles.push_back(user);
                for (const auto &s : user.samples)
                    cpuSamples = add_count(cpuSamples, s.count);
            }
            ProfilePayload kernel = bpftraceKernelFuture.get();
            if (!kernel.samples.empty())
            {
                window.profiles.push_back(kernel);
                for (const auto &s : kernel.samples)
                    cpuSamples = add_count(cpuSamples, s.count);
            }
            if (cpuSamples > 0)
                window.selectedBackend = "bpftrace";
        }

        if (cpuSamples > 0)
        {
            window.backendStatus = "ok";
        }
        else
        {
            window.backendStatus = "failed";
            window.backendReason = "no CPU samples collected by any backend";
            std::cout << "[native-cp] no CPU profile samples collected in this window" << std::endl;
        }
    }
    else if (window.backendStatus.empty())
    {
        window.backendStatus = "failed";
        window.backendReason = "CPU backend not enabled";
    }

    // IO/sched 结果；各自带预算等待，超预算不阻塞 CPU 窗口（标记 unavailable）。
    uint64_t ioEvents = 0;
	uint64_t ioSyscallEvents = 0;
    uint64_t schedEvents = 0;
    int64_t ioMs = 0;
    int64_t schedMs = 0;
    if (ioStarted)
    {
        int64_t t0 = now_ms();
        HistogramPayload hist = wait_histogram_budgeted(ioFuture, "io_latency");
        ioMs = now_ms() - t0;
        ioEvents = hist.eventCount;
        window.histograms.push_back(hist);
    }
	if (ioSyscallStarted)
	{
		HistogramPayload hist = wait_histogram_budgeted(ioSyscallFuture, "io_syscall_latency");
		ioSyscallEvents = hist.eventCount;
		window.histograms.push_back(hist);
	}
    if (schedStarted)
    {
        int64_t t0 = now_ms();
        HistogramPayload hist = wait_histogram_budgeted(schedFuture, "sched_latency");
        schedMs = now_ms() - t0;
        schedEvents = hist.eventCount;
        window.histograms.push_back(hist);
    }

    window.endMs = captureEndMs;
    int64_t elapsedMs = now_ms() - window.startMs;
    std::cout << "[native-cp] window start_ms=" << window.startMs
              << " wall_elapsed_ms=" << elapsedMs
              << " capture_elapsed_ms=" << (window.endMs - window.startMs)
              << " cpu_samples=" << cpuSamples
              << " io_events=" << ioEvents
			  << " io_syscall_events=" << ioSyscallEvents
              << " sched_events=" << schedEvents
              << " io_ms=" << ioMs
              << " sched_ms=" << schedMs
              << " backend=" << window.selectedBackend
              << " status=" << window.backendStatus << std::endl;
    return window;
}

bool RollingPerfRecorder::Start(const ContinuousSamplerConfig &cfg, std::string *error)
{
    consumed.clear();
    directory.clear();
    if (!create_rolling_perf_directory(&directory))
    {
        if (error) *error = "failed to create strict rolling perf directory";
        return false;
    }
    // perf's sample clock is normally CLOCK_MONOTONIC. Capture a wall/mono
    // pair before launching perf so parsed timestamps can be mapped back to
    // Unix milliseconds without using the later Drain() time.
    wallStartMs = now_ms();
    monotonicStartMs = monotonic_ms();
    std::vector<std::string> args{perf_bin(), "record", "--no-buildid-cache", "-q"};
    if (cfg.scope == "process")
    {
        std::string pids = target_pid_csv(cfg);
        if (pids.empty())
        {
            if (error) *error = "strict rolling perf requires at least one target PID";
            ::rmdir(directory.c_str());
            directory.clear();
            return false;
        }
        args.insert(args.end(), {"-p", pids});
    }
    else
        args.push_back("-a");
    args.insert(args.end(), {"-e", env_string_local("DROP_NATIVE_CP_PERF_EVENT", "cpu-clock"),
                             "-F", std::to_string(cfg.sampleRateHz), "-g", "-T",
                             "--timestamp-boundary", "--switch-output=2s", "--timestamp-filename",
                             "-o", directory + "/perf.data", "--", "sleep", "86400"});
    pid = ::fork();
    if (pid < 0)
    {
        if (error) *error = "failed to fork strict rolling perf";
        ::rmdir(directory.c_str());
        directory.clear();
        return false;
    }
    if (pid == 0)
    {
        ::setpgid(0, 0);
        int devnull = ::open("/dev/null", O_WRONLY);
        if (devnull >= 0)
        {
            ::dup2(devnull, STDOUT_FILENO);
            ::dup2(devnull, STDERR_FILENO);
            ::close(devnull);
        }
        std::vector<char *> argv;
        for (auto &arg : args) argv.push_back(const_cast<char *>(arg.c_str()));
        argv.push_back(nullptr);
        ::execvp(argv[0], argv.data());
        _exit(127);
    }
    ::setpgid(pid, pid);
    std::this_thread::sleep_for(std::chrono::milliseconds(250));
    int status = 0;
    if (::waitpid(pid, &status, WNOHANG) == pid)
    {
        if (error) *error = "strict rolling perf exited during startup";
        pid = -1;
        ::rmdir(directory.c_str());
        directory.clear();
        return false;
    }
    return true;
}

bool RollingPerfRecorder::HasParseableOutput() const
{
    const auto files = rolling_perf_files(directory, false);
    if (files.empty())
        return false;
    std::string ignored;
    return drop::exec_capture({perf_bin(), "script", "-F", "time", "-i", files.front()},
                              &ignored, 1024 * 1024) == 0;
}

std::vector<WindowPayload> RollingPerfRecorder::Drain(const ContinuousSamplerConfig &cfg, bool final)
{
    const auto files = rolling_perf_files(directory, final);
    std::vector<WindowPayload> windows;
    for (const auto &path : files)
    {
        if (!consumed.insert(path).second) continue;
        WindowPayload window;
        window.startMs = now_ms();
        window.attemptedBackends = {"perf_rolling"};
        window.selectedBackend = "perf_rolling";
        std::string output;
        int rc = drop::exec_capture({perf_bin(), "script", "-F", "comm,pid,tid,time,event,ip,sym,dso", "-i", path},
                                    &output, 32 * 1024 * 1024);
        window.endMs = now_ms();
        if (rc != 0)
        {
            window.backendStatus = "failed";
            window.backendReason = "perf script failed for rolling file";
        }
        else
        {
            PerfScriptParseResult parsed = parse_perf_script_result(output);
            if (parsed.hasTimestamp)
            {
                const int64_t parsedStart = perf_timestamp_to_unix_ms(parsed.startTimestampSec,
                                                                        wallStartMs,
                                                                        monotonicStartMs);
                const int64_t parsedEnd = perf_timestamp_to_unix_ms(parsed.endTimestampSec,
                                                                      wallStartMs,
                                                                      monotonicStartMs);
                if (parsedStart > 0 && parsedEnd >= parsedStart)
                {
                    window.startMs = parsedStart;
                    window.endMs = std::max(parsedStart + 1, parsedEnd);
                }
            }
            window.samples = std::move(parsed.samples);
            std::map<int, bool> goCache;
            for (auto &sample : window.samples)
            {
                sample.processStartMs = configured_process_start_ms(cfg, sample.pid);
				if (sample.exe.empty())
					sample.exe = configured_process_exe(cfg, sample.pid);
                sample.backend = "perf_rolling";
                sample.runtime = sample_runtime(sample, {}, &goCache);
            }
            if (cfg.scope == "process")
                window.samples.erase(std::remove_if(window.samples.begin(), window.samples.end(), [&](const auto &sample) {
                    return !process_targeted(cfg, sample.pid, sample.processStartMs, sample.exe);
                }), window.samples.end());
            window.backendStatus = "ok";
        }
        if (window.endMs <= window.startMs)
            window.endMs = std::max(window.startMs + 1, now_ms());
        windows.push_back(std::move(window));
        ::unlink(path.c_str());
    }
    if (final && !directory.empty())
    {
        ::rmdir(directory.c_str());
        directory.clear();
    }
    return windows;
}

void RollingPerfRecorder::Stop()
{
    if (pid <= 0) return;
    ::kill(-pid, SIGINT);
    for (int i = 0; i < 30; ++i)
    {
        int status = 0;
        if (::waitpid(pid, &status, WNOHANG) == pid)
        {
            pid = -1;
            return;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }
    ::kill(-pid, SIGKILL);
    ::waitpid(pid, nullptr, 0);
    pid = -1;
}

static double strict_histogram_low(uint32_t slot)
{
    static const double bounds[] = {0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384};
    return bounds[std::min<uint32_t>(slot, 15)];
}

static void append_core_histograms(WindowPayload *window,
                                   const ContinuousSamplerConfig &cfg,
                                   const std::vector<CoreHistogramSample> &samples,
                                   uint64_t lost)
{
    std::set<uint32_t> pids;
    for (const auto &sample : samples) pids.insert(sample.tgid);
    if (cfg.scope != "process" && pids.empty()) pids.insert(0);
    for (uint32_t pid : pids)
        for (uint32_t signal = 1; signal <= 3; ++signal)
        {
            HistogramPayload hist;
            hist.pid = static_cast<int>(pid);
            hist.signalType = signal == 1 ? "io_latency" : signal == 2 ? "io_syscall_latency" : "sched_latency";
            hist.backend = "libbpf-co-re";
            hist.unit = "us";
            std::map<uint32_t, HistogramBucket> buckets;
            for (const auto &sample : samples)
            {
                if (sample.signal != signal || sample.tgid != pid) continue;
                auto &bucket = buckets[sample.slot];
                if (bucket.range.empty())
                {
                    bucket.low = strict_histogram_low(sample.slot);
                    bucket.high = sample.slot >= 15 ? bucket.low : strict_histogram_low(sample.slot + 1);
                    bucket.range = "[" + std::to_string(static_cast<int>(bucket.low)) + ", " +
                                   std::to_string(static_cast<int>(bucket.high)) + ")";
                }
                bucket.count = add_count(bucket.count, sample.count);
            }
            for (auto &entry : buckets)
            {
                hist.eventCount = add_count(hist.eventCount, entry.second.count);
                hist.buckets.push_back(entry.second);
            }
            summarize_histogram(&hist);
            if (hist.buckets.empty())
            {
                hist.unavailable = true;
                hist.reason = "no events observed in strict CO-RE slice";
            }
            if (!hist.buckets.empty() || cfg.scope != "process")
                window->histograms.push_back(std::move(hist));
        }
    if (lost > 0)
        window->backendReason = "CO-RE lost events=" + std::to_string(lost);
}

static void queue_core_histograms(std::vector<WindowPayload> *windows,
                                  const ContinuousSamplerConfig &cfg,
                                  std::vector<CoreHistogramSample> *pendingSamples,
                                  uint64_t *pendingLost,
                                  std::vector<CoreHistogramSample> samples,
                                  uint64_t lost)
{
    if (!windows || !pendingSamples || !pendingLost)
        return;
    pendingSamples->insert(pendingSamples->end(),
                           std::make_move_iterator(samples.begin()),
                           std::make_move_iterator(samples.end()));
    *pendingLost = add_count(*pendingLost, lost);
    if (windows->empty())
        return;
    if (!pendingSamples->empty() || *pendingLost > 0)
        append_core_histograms(&windows->front(), cfg, *pendingSamples, *pendingLost);
    pendingSamples->clear();
    *pendingLost = 0;
}

} // namespace (anonymous)

// 阶段一：逻辑信号集合 → 物理采集信号集合字符串（公开 API）。
std::string PhysicalSignalsFromRequested(const std::vector<std::string> &requested)
{
    return physical_signals_from_requested(requested);
}

// 阶段五：服务器存储压力全局开关（公开 API，见 ContinuousSampler.h）。
void SetContinuousServerPressure(bool halted)
{
    g_continuousServerPressureHalted.store(halted);
}
bool ContinuousServerPressureHalted()
{
    return g_continuousServerPressureHalted.load();
}

bool ContinuousSessionHasPendingSpool(const ContinuousSamplerConfig &config)
{
    return !list_session_spool_files(config, ".json").empty() ||
           !list_session_spool_files(config, ".journal").empty();
}

bool DrainOneContinuousSessionBatch(const ContinuousSamplerConfig &config)
{
    recover_session_spool_journals(config);
    std::vector<std::string> files = list_session_spool_files(config, ".json");
    if (files.empty())
        return true;
    const std::string &path = files.front();
    const std::string batchID = batch_id_from_spool_path(path);
    const SpoolPostResult result = post_spooled_batch(config, path, batchID);
    if (result == SpoolPostResult::PermanentlyRejected)
    {
        // 阶段一：内容冲突/不可重试拒绝 → 移入 .rejected 隔离区，绝不换 ID 重传。
        if (!quarantine_rejected_spooled_batch(path, batchID))
            return false;
        return !list_session_spool_files(config, ".json").empty()
                   ? DrainOneContinuousSessionBatch(config)
                   : true;
    }
    if (result != SpoolPostResult::Acknowledged)
        return false;
    if (::unlink(path.c_str()) != 0)
    {
        std::cout << "[native-cp] stopped Session ACK received but spool removal failed path="
                  << path << " errno=" << errno << std::endl;
        return false;
    }
    return list_session_spool_files(config, ".json").empty();
}

PerfEventSampler::~PerfEventSampler()
{
    Stop();
}

std::string PerfEventSampler::Name() const
{
    return "perf_event";
}

bool PerfEventSampler::Start(const ContinuousSamplerConfig &config, std::string *error)
{
    if (config.sampleRateHz <= 0 || config.aggregationWindowSec <= 0 || config.uploadBatchSec <= 0)
    {
        if (error)
            *error = "invalid native continuous sampler config";
        return false;
    }
    if (config.sessionSID.empty() || config.apiBaseURL.empty() || config.authUID.empty())
    {
        if (error)
            *error = "missing native continuous session/api/auth config";
        return false;
    }
    if (!ensure_directory(session_spool_directory(config)))
    {
        if (error)
            *error = "continuous spool directory is not writable";
        return false;
    }
    if (running_.load())
        return true;
    config_ = config;
    // 阶段一：独立采集器实例分配独立 generation（重启/重建即变），
    // 目标指纹为空时补算（供 window_id 稳定使用）。
    if (config_.collectorGeneration.empty())
        config_.collectorGeneration = collector_generation_id();
    if (config_.targetFingerprint.empty())
        config_.targetFingerprint = target_fingerprint_for(config_);
    running_ = true;
    worker_ = std::thread(&PerfEventSampler::Loop, this);
    return true;
}

void PerfEventSampler::Stop()
{
    running_ = false;
    if (worker_.joinable())
        worker_.join();
}

bool PerfEventSampler::Running() const
{
    return running_.load();
}

template <typename Collector>
static void run_continuous_spool_loop(std::atomic<bool> &running,
                                      const ContinuousSamplerConfig &config,
                                      Collector collector)
{
    ensure_directory(session_spool_directory(config));
    recover_spool_journals(config);
    // 阶段一：本地可变副本，用于维护单调递增的 batch_sequence。
    ContinuousSamplerConfig cfg = config;
    SpoolRetryState retry;
    while (running.load() && drain_one_spooled_batch(cfg, &retry, true))
    {
        if (list_session_spool_files(cfg, ".json").empty())
            break;
    }

    int windowsPerBatch = std::max(1, cfg.uploadBatchSec / cfg.aggregationWindowSec);
    std::vector<WindowPayload> batch;
    batch.reserve(static_cast<size_t>(windowsPerBatch));
    std::string batchID;
    while (running.load())
    {
        drain_one_spooled_batch(cfg, &retry, false);
        if (!spool_has_collection_capacity(cfg))
        {
            std::cout << "[native-cp] spool backpressure usage_bytes=" << spool_usage_bytes(cfg)
                      << " max_bytes=" << cfg.spoolMaxBytes
                      << " free_bytes=" << spool_free_bytes(cfg)
                      << " min_free_bytes=" << cfg.spoolMinFreeBytes << std::endl;
            interruptible_wait(running, 1000);
            continue;
        }

        WindowPayload window = collector(cfg);
        // 阶段一：补齐稳定 window_id / 内容摘要。
        if (window.windowID.empty())
            window.windowID = make_window_id(cfg, window);
        if (window.contentSHA256.empty())
            window.contentSHA256 = window_content_digest(window);
        batch.push_back(window);
        if (batchID.empty())
        {
            cfg.batchSequence += 1;
            batchID = make_batch_id(cfg);
        }
        std::string body = build_batch_json(cfg, batchID, batch);
        bool persisted = persist_batch(cfg, batchID, body);
        while (!persisted && running.load())
        {
            std::cout << "[native-cp] failed to persist batch journal batch=" << batchID
                      << " errno=" << errno << ", retrying without collecting" << std::endl;
            drain_one_spooled_batch(cfg, &retry, false);
            interruptible_wait(running, 1000);
            persisted = persist_batch(cfg, batchID, body);
        }
        if (!persisted)
        {
            std::cout << "[native-cp] failed to persist batch journal batch=" << batchID
                      << " errno=" << errno << std::endl;
            release_window_profiles(window);
            running = false;
            break;
        }
        acknowledge_batch_profiles({window});

        if (static_cast<int>(batch.size()) >= windowsPerBatch)
        {
            if (!finalize_batch(cfg, batchID))
            {
                std::cout << "[native-cp] failed to finalize batch journal batch=" << batchID
                          << " errno=" << errno << std::endl;
                running = false;
                break;
            }
            batch.clear();
            batchID.clear();
            drain_one_spooled_batch(cfg, &retry, false);
        }
    }

    if (!batch.empty() && !batchID.empty())
    {
        std::string body = build_batch_json(cfg, batchID, batch);
        if (persist_batch(cfg, batchID, body) && finalize_batch(cfg, batchID))
            drain_one_spooled_batch(cfg, &retry, true);
        else
            release_batch_profiles(batch);
    }
}

void PerfEventSampler::Loop()
{
    run_continuous_spool_loop(running_, config_, collect_window);
}

DualTrackContinuousSampler::~DualTrackContinuousSampler()
{
    Stop();
}

std::string DualTrackContinuousSampler::Name() const
{
    return "dual_track";
}

bool DualTrackContinuousSampler::Start(const ContinuousSamplerConfig &config, std::string *error)
{
    if (config.sampleRateHz <= 0 || config.aggregationWindowSec <= 0 || config.uploadBatchSec <= 0)
    {
        if (error)
            *error = "invalid native continuous sampler config";
        return false;
    }
    if (config.sessionSID.empty() || config.apiBaseURL.empty() || config.authUID.empty())
    {
        if (error)
            *error = "missing native continuous session/api/auth config";
        return false;
    }
    if (!ensure_directory(session_spool_directory(config)))
    {
        if (error)
            *error = "continuous spool directory is not writable";
        return false;
    }
    if (running_.load())
        return true;
    config_ = config;
    // 阶段一：独立采集器实例分配独立 generation / 目标指纹。
    if (config_.collectorGeneration.empty())
        config_.collectorGeneration = collector_generation_id();
    if (config_.targetFingerprint.empty())
        config_.targetFingerprint = target_fingerprint_for(config_);
    running_ = true;
    worker_ = std::thread(&DualTrackContinuousSampler::Loop, this);
    return true;
}

void DualTrackContinuousSampler::Stop()
{
    running_ = false;
    if (worker_.joinable())
        worker_.join();
}

bool DualTrackContinuousSampler::Running() const
{
    return running_.load();
}

void DualTrackContinuousSampler::Loop()
{
    run_continuous_spool_loop(running_, config_, collect_dual_track_window);
}

// ---------- DBSnapshotSampler：阶段一（标量健康指标） ----------
// 借鉴 mysqld_exporter/postgres_exporter 的采集口径（查哪些系统视图能反映
// 数据库健康度），但不复用它们的代码/二进制：自己拼查询、自己解析输出、
// 自己决定窗口聚合方式。阶段二会在这个函数里追加 digest/锁查询，复用同一
// 个连接建立/超时熔断骨架，不是另起一套。

// 密码只在真正发起查询前，从 target.passwordRef 指向的本机文件读取，
// 从不经服务端 Labels 明文中转、不落 Postgres——与 AgentDiscoveryConfig
// 现有的"不存密码"约定一致。
static bool read_db_target_password(const DBTargetConfig &target, std::string *password)
{
    if (target.passwordRef.empty())
    {
        password->clear();
        return true;
    }
    std::string body;
    if (!read_file(target.passwordRef, &body))
        return false;
    while (!body.empty() && (body.back() == '\n' || body.back() == '\r'))
        body.pop_back();
    *password = body;
    return true;
}

// mysql 客户端不接受把密码明文放进 argv（会出现在 `ps`），标准做法是写一个
// 0600 的 --defaults-extra-file 临时文件，用完立即删除。
static bool write_mysql_defaults_file(const std::string &path, const std::string &user, const std::string &password)
{
    std::ostringstream body;
    body << "[client]\nuser=" << user << "\npassword=" << password << "\n";
    int fd = ::open(path.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0600);
    if (fd < 0)
        return false;
    const std::string content = body.str();
    size_t written = 0;
    bool ok = true;
    while (written < content.size())
    {
        ssize_t count = ::write(fd, content.data() + written, content.size() - written);
        if (count < 0 && errno == EINTR)
            continue;
        if (count <= 0)
        {
            ok = false;
            break;
        }
        written += static_cast<size_t>(count);
    }
    ::close(fd);
    return ok;
}

// 单个 MySQL 目标的一次标量指标轮询：SHOW GLOBAL STATUS 一次性拿全量
// 计数器，本地过滤出关心的几项，换算成 MetricPayload。查询超时/失败只
// 标记 unavailable，不能拖垮整个采集循环（沿用 HistogramPayload 已有的
// unavailable/reason 模式）。
static void collect_mysql_target_metrics(const ContinuousSamplerConfig &cfg, const DBTargetConfig &target,
                                          WindowPayload *window)
{
    std::string password;
    if (!read_db_target_password(target, &password))
    {
        std::cout << "[native-cp] db target=" << target.instanceLabel
                  << " failed to read password_ref=" << target.passwordRef << std::endl;
        return;
    }
    std::string defaultsPath = "/tmp/mini_drop_db_" + target.instanceLabel + "_" + std::to_string(now_ms()) + ".cnf";
    if (!write_mysql_defaults_file(defaultsPath, target.user, password))
    {
        std::cout << "[native-cp] db target=" << target.instanceLabel << " failed to stage mysql defaults file"
                  << std::endl;
        return;
    }
    std::vector<std::string> argv = {
        "mysql", "--defaults-extra-file=" + defaultsPath,
        "-h", target.host, "-P", std::to_string(target.port),
        "--connect-timeout=2", "--batch", "--skip-column-names",
        "-e", "SHOW GLOBAL STATUS"};
    std::string output;
    int rc = drop::exec_capture(argv, &output, 64 * 1024);
    ::unlink(defaultsPath.c_str());
    if (rc != 0)
    {
        std::cout << "[native-cp] db target=" << target.instanceLabel << " SHOW GLOBAL STATUS failed rc=" << rc
                  << std::endl;
        return;
    }

    std::unordered_map<std::string, uint64_t> status;
    std::istringstream lines(output);
    std::string line;
    while (std::getline(lines, line))
    {
        size_t tab = line.find('\t');
        if (tab == std::string::npos)
            continue;
        const std::string key = line.substr(0, tab);
        const std::string value = line.substr(tab + 1);
        char *end = nullptr;
        uint64_t parsed = std::strtoull(value.c_str(), &end, 10);
        if (end != value.c_str())
            status[key] = parsed;
    }

    const int64_t nowMillis = now_ms();
    auto pushMetric = [&](const std::string &name, uint64_t value, const std::string &unit) {
        MetricPayload metric;
        metric.metric = name;
        metric.unit = unit;
        metric.runtime = "mysql:" + target.instanceLabel;
        metric.timestampMs = nowMillis;
        metric.value = value;
        window->metrics.push_back(std::move(metric));
    };
    if (status.count("Threads_connected"))
        pushMetric("db_active_connections", status["Threads_connected"], "count");
    if (status.count("Questions"))
        pushMetric("db_questions_total", status["Questions"], "count");
    if (status.count("Innodb_buffer_pool_read_requests") && status.count("Innodb_buffer_pool_reads"))
    {
        const uint64_t requests = status["Innodb_buffer_pool_read_requests"];
        const uint64_t reads = status["Innodb_buffer_pool_reads"];
        // 命中率放大 10000 倍存成整数（0~10000 代表 0%~100%），MetricPayload.value 是
        // uint64_t，不支持小数；前端展示时按 /100.0 还原成百分比。
        uint64_t hitRatioBps = requests == 0 ? 10000
                                              : static_cast<uint64_t>(
                                                    (1.0 - static_cast<double>(reads) / static_cast<double>(requests)) *
                                                    10000.0);
        pushMetric("db_innodb_buffer_pool_hit_ratio_bps", hitRatioBps, "ratio_bps");
    }
}

// 阶段二状态：SQL digest 表（events_statements_summary_by_digest）里的计数
// 器是自服务器启动（或上次 TRUNCATE）以来的累计值，要换算成"本窗口新发生
// 的调用"，需要保存上一轮的累计值做差分——这是跨采集轮次的状态，只能用
// 进程内静态 map 存（不同 Session 可能采同一实例，key 里带 instanceLabel
// 区分；不同 DBSnapshotSampler 实例跑在各自线程里，故用 mutex 保护）。
struct DigestCounterState
{
    uint64_t countStar = 0;
    uint64_t sumTimerWaitPs = 0;
    uint64_t sumRowsExamined = 0;
};
static std::mutex g_digestStateMutex;
static std::unordered_map<std::string, DigestCounterState> g_digestState;

// ---- digest 增量状态的纯逻辑（与 mysql 命令行解耦，便于单测） ----

// 解析 events_statements_summary_by_digest 的 --batch 输出行（6 列制表符分隔）：
// SCHEMA_NAME, DIGEST, DIGEST_TEXT, COUNT_STAR, SUM_TIMER_WAIT, SUM_ROWS_EXAMINED。
// 列数不对或任一计数列不是数字时返回 false（调用方跳过该行，不做状态变更）。
struct ParsedDigestRow
{
    std::string schemaName;
    std::string digest;
    std::string digestText;
    uint64_t countStar = 0;
    uint64_t sumTimerWaitPs = 0;
    uint64_t sumRowsExamined = 0;
};

static bool parse_digest_row(const std::string &line, ParsedDigestRow *out)
{
    std::vector<std::string> cols;
    size_t start = 0;
    for (size_t i = 0; i <= line.size(); ++i)
    {
        if (i == line.size() || line[i] == '\t')
        {
            cols.push_back(line.substr(start, i - start));
            start = i + 1;
        }
    }
    if (cols.size() != 6)
        return false;
    char *end = nullptr;
    const uint64_t countStar = std::strtoull(cols[3].c_str(), &end, 10);
    if (end == cols[3].c_str())
        return false;
    const uint64_t sumTimerWaitPs = std::strtoull(cols[4].c_str(), &end, 10);
    if (end == cols[4].c_str())
        return false;
    const uint64_t sumRowsExamined = std::strtoull(cols[5].c_str(), &end, 10);
    if (end == cols[5].c_str())
        return false;
    out->schemaName = cols[0];
    out->digest = cols[1];
    out->digestText = cols[2];
    out->countStar = countStar;
    out->sumTimerWaitPs = sumTimerWaitPs;
    out->sumRowsExamined = sumRowsExamined;
    return true;
}

// 增量状态机的结果类型。
enum class DigestDeltaKind
{
    FirstSeen, // 首次见到该 digest：只建立基线，本轮不上报
    Reset,     // 任一累计计数器回退（TRUNCATE/服务重启）：重建基线，本轮不上报
    Increment, // 正常窗口增量（deltaCalls 可能为 0，调用方据此决定是否输出）
};

struct DigestDeltaResult
{
    DigestDeltaKind kind = DigestDeltaKind::FirstSeen;
    uint64_t deltaCalls = 0;
    uint64_t deltaLatencyUs = 0; // 增量总耗时（us），仅 Increment 有意义
    uint64_t deltaRows = 0;      // 增量扫描行数，仅 Increment 有意义
};

// 纯函数：由上一轮状态与当前计数计算窗口增量。prev 为空指针表示首轮
// （FirstSeen，只建立基线）。任一累计计数器回退都视为新基线（Reset），
// 避免无符号整数下溢把 (cur - prev) 变成一个天文数字。
static DigestDeltaResult compute_digest_delta(const DigestCounterState *prev, const DigestCounterState &cur)
{
    if (prev == nullptr)
        return DigestDeltaResult{};
    if (cur.countStar < prev->countStar || cur.sumTimerWaitPs < prev->sumTimerWaitPs ||
        cur.sumRowsExamined < prev->sumRowsExamined)
        return DigestDeltaResult{DigestDeltaKind::Reset, 0, 0, 0};
    DigestDeltaResult out;
    out.kind = DigestDeltaKind::Increment;
    out.deltaCalls = cur.countStar - prev->countStar;
    out.deltaLatencyUs = (cur.sumTimerWaitPs - prev->sumTimerWaitPs) / 1000000ULL; // ps -> us
    out.deltaRows = cur.sumRowsExamined - prev->sumRowsExamined;
    return out;
}

// digest 增量状态的隔离 key：sessionSID + instanceLabel + digest 三重组合，
// 保证不同 Session 采集同一实例、同一 Session 采集多个实例都互不串扰。
static std::string digest_state_key(const std::string &sessionSID, const std::string &instanceLabel,
                                    const std::string &digest)
{
    return sessionSID + "|" + instanceLabel + "|" + digest;
}

// 借鉴 pg_stat_monitor/PMM Query Analytics 的聚合思路：按 DIGEST 做增量
// diff，只上报本窗口内真正新增的调用次数/耗时，不是累计值。SQL 只存归一
// 化 digest_text（占位符形式），不落原始参数。查询里用 REPLACE 把
// digest_text/query 里可能出现的 \n \t 换成空格——这两个字符会破坏
// --batch 输出的行/列分隔，比在 C++ 侧做转义解析更简单可靠。
static void collect_mysql_target_digests(const ContinuousSamplerConfig &cfg, const DBTargetConfig &target,
                                          WindowPayload *window)
{
    std::string password;
    if (!read_db_target_password(target, &password))
        return;
    std::string defaultsPath = "/tmp/mini_drop_db_" + target.instanceLabel + "_" + std::to_string(now_ms()) + ".cnf";
    if (!write_mysql_defaults_file(defaultsPath, target.user, password))
        return;
    const std::string query =
        "SELECT SCHEMA_NAME, DIGEST, "
        "REPLACE(REPLACE(DIGEST_TEXT, '\\n', ' '), '\\t', ' '), "
        "COUNT_STAR, SUM_TIMER_WAIT, SUM_ROWS_EXAMINED "
        "FROM performance_schema.events_statements_summary_by_digest "
        "WHERE SCHEMA_NAME IS NOT NULL AND DIGEST IS NOT NULL "
        "ORDER BY SUM_TIMER_WAIT DESC LIMIT 50";
    std::vector<std::string> argv = {
        "mysql", "--defaults-extra-file=" + defaultsPath,
        "-h", target.host, "-P", std::to_string(target.port),
        "--connect-timeout=2", "--batch", "--skip-column-names",
        "-e", query};
    std::string output;
    int rc = drop::exec_capture(argv, &output, 256 * 1024);
    ::unlink(defaultsPath.c_str());
    if (rc != 0)
    {
        std::cout << "[native-cp] db target=" << target.instanceLabel << " digest query failed rc=" << rc
                  << std::endl;
        return;
    }

    const int64_t nowMillis = now_ms();
    std::istringstream lines(output);
    std::string line;
    std::lock_guard<std::mutex> lock(g_digestStateMutex);
    while (std::getline(lines, line))
    {
        ParsedDigestRow row;
        if (!parse_digest_row(line, &row))
            continue;

        // 状态 key 同时带上 sessionSID 与 instanceLabel：不同 Session 采集同一
        // 实例、同一 Session 采集多个实例都能正确隔离，互不串扰。
        const std::string stateKey = digest_state_key(cfg.sessionSID, target.instanceLabel, row.digest);
        auto it = g_digestState.find(stateKey);
        if (it == g_digestState.end())
        {
            // 第一次见到这个 digest：只记基线，不上报（否则会把"自服务器启动
            // 以来的全部历史调用"当成本窗口发生的调用，数字会失真）。
            g_digestState[stateKey] = {row.countStar, row.sumTimerWaitPs, row.sumRowsExamined};
            continue;
        }
        DigestCounterState prev = it->second;
        const DigestCounterState cur{row.countStar, row.sumTimerWaitPs, row.sumRowsExamined};
        it->second = cur;
        DigestDeltaResult delta = compute_digest_delta(&prev, cur);
        if (delta.kind != DigestDeltaKind::Increment)
            continue; // 首轮基线或任一计数器回退（TRUNCATE/重启）：本轮不上报
        if (delta.deltaCalls == 0)
            continue; // 零增量：不上报，但状态已更新为当前累计值

        DBSnapshotSample sample;
        sample.kind = "digest";
        sample.instanceLabel = target.instanceLabel;
        sample.timestampMs = nowMillis;
        sample.schemaName = row.schemaName;
        sample.digestText = row.digestText;
        sample.callCount = delta.deltaCalls;
        sample.totalLatencyUs = delta.deltaLatencyUs;
        sample.rowsExaminedTotal = delta.deltaRows;
        window->dbSnapshots.push_back(std::move(sample));
    }
}

// 锁等待链：直接查 MySQL 内置的 sys.innodb_lock_waits 视图（5.7.7+ 默认启
// 用的系统 schema，不是第三方工具），它已经把 blocking/waiting 事务、SQL、
// 等待时长、锁定的表拼好了，比自己手写 data_locks/data_lock_waits/
// innodb_trx 三表 join 更不容易出错，且同样是"自己写 SQL、自己解析"，
// 不是部署 pg_wait_tracer 这类第三方工具。
static void collect_mysql_target_lock_waits(const ContinuousSamplerConfig &cfg, const DBTargetConfig &target,
                                             WindowPayload *window)
{
    std::string password;
    if (!read_db_target_password(target, &password))
        return;
    std::string defaultsPath = "/tmp/mini_drop_db_" + target.instanceLabel + "_" + std::to_string(now_ms()) + ".cnf";
    if (!write_mysql_defaults_file(defaultsPath, target.user, password))
        return;
    const std::string query =
        "SELECT waiting_pid, "
        "REPLACE(REPLACE(IFNULL(waiting_query,''), '\\n', ' '), '\\t', ' '), "
        "blocking_pid, "
        "REPLACE(REPLACE(IFNULL(blocking_query,''), '\\n', ' '), '\\t', ' '), "
        "wait_age_secs, IFNULL(locked_table,'') "
        "FROM sys.innodb_lock_waits";
    std::vector<std::string> argv = {
        "mysql", "--defaults-extra-file=" + defaultsPath,
        "-h", target.host, "-P", std::to_string(target.port),
        "--connect-timeout=2", "--batch", "--skip-column-names",
        "-e", query};
    std::string output;
    int rc = drop::exec_capture(argv, &output, 128 * 1024);
    ::unlink(defaultsPath.c_str());
    if (rc != 0)
    {
        // sys schema 可能被 DBA 删掉/禁用，这种情况只记日志，不影响标量指
        // 标和 digest 采集继续跑。
        std::cout << "[native-cp] db target=" << target.instanceLabel << " lock wait query failed rc=" << rc
                  << std::endl;
        return;
    }

    const int64_t nowMillis = now_ms();
    std::istringstream lines(output);
    std::string line;
    while (std::getline(lines, line))
    {
        std::vector<std::string> cols;
        size_t start = 0;
        for (size_t i = 0; i <= line.size(); ++i)
        {
            if (i == line.size() || line[i] == '\t')
            {
                cols.push_back(line.substr(start, i - start));
                start = i + 1;
            }
        }
        if (cols.size() != 6)
            continue;
        char *end = nullptr;
        DBSnapshotSample sample;
        sample.kind = "lock_wait";
        sample.instanceLabel = target.instanceLabel;
        sample.timestampMs = nowMillis;
        sample.waitingPid = std::strtoll(cols[0].c_str(), &end, 10);
        sample.waitingQuery = cols[1];
        sample.blockingPid = std::strtoll(cols[2].c_str(), &end, 10);
        sample.blockingQuery = cols[3];
        sample.waitSeconds = std::strtoull(cols[4].c_str(), &end, 10);
        sample.lockedTable = cols[5];
        window->dbSnapshots.push_back(std::move(sample));
    }
}

static WindowPayload collect_db_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    window.selectedBackend = "db_snapshot";
    window.backendStatus = "ok";
    for (const auto &target : cfg.dbTargets)
    {
        if (target.engine == "mysql")
        {
            collect_mysql_target_metrics(cfg, target, &window);
            collect_mysql_target_digests(cfg, target, &window);
            collect_mysql_target_lock_waits(cfg, target, &window);
        }
        // PostgreSQL 标量指标 + digest/锁查询留待后续子任务，架构上两者共
        // 用同一个 collect_db_window 入口，按 engine 分支接入即可。
    }

    // run_continuous_spool_loop 对所有 collector 一视同仁地紧邻着循环调
    // 用，没有任何节流；CPU/eBPF 的 collector 天然靠内部阻塞采样撑满
    // aggregationWindowSec，这里的 SQL 查询是毫秒级返回，不补一个 sleep
    // 就会以 CPU 能跑多快就跑多快的频率狂轮询目标数据库。睡到凑满窗口时
    // 长再收窗，也让 window.endMs 落在下一个采集周期开始之前，跟其他
    // 信号"一个 window = 一个采集周期"的语义对齐。
    const int64_t elapsedMs = now_ms() - window.startMs;
    const int64_t targetMs = static_cast<int64_t>(std::max(1, cfg.aggregationWindowSec)) * 1000;
    if (elapsedMs < targetMs)
        std::this_thread::sleep_for(std::chrono::milliseconds(targetMs - elapsedMs));

    window.endMs = now_ms();
    // 兜底：即使节流到位，系统时钟粒度理论上仍可能让两次 now_ms() 撞在同一
    // 毫秒——服务端要求 start < end（continuous.go 的 !StartTime.Before(EndTime)
    // 检查），撞上就会导致整个 batch 被拒收，db_snapshot 数据零入库。
    if (window.endMs <= window.startMs)
        window.endMs = window.startMs + 1;
    return window;
}

DBSnapshotSampler::~DBSnapshotSampler()
{
    Stop();
}

std::string DBSnapshotSampler::Name() const
{
    return "db_snapshot";
}

bool DBSnapshotSampler::Start(const ContinuousSamplerConfig &config, std::string *error)
{
    if (config.aggregationWindowSec <= 0 || config.uploadBatchSec <= 0)
    {
        if (error)
            *error = "invalid db snapshot sampler config";
        return false;
    }
    if (config.sessionSID.empty() || config.apiBaseURL.empty() || config.authUID.empty())
    {
        if (error)
            *error = "missing db snapshot session/api/auth config";
        return false;
    }
    if (config.dbTargets.empty())
    {
        if (error)
            *error = "db snapshot sampler started with no db targets";
        return false;
    }
    if (!ensure_directory(session_spool_directory(config)))
    {
        if (error)
            *error = "continuous spool directory is not writable";
        return false;
    }
    if (running_.load())
        return true;
    config_ = config;
    // 阶段一：独立采集器实例分配独立 generation / 目标指纹。
    if (config_.collectorGeneration.empty())
        config_.collectorGeneration = collector_generation_id();
    if (config_.targetFingerprint.empty())
        config_.targetFingerprint = target_fingerprint_for(config_);
    running_ = true;
    worker_ = std::thread(&DBSnapshotSampler::Loop, this);
    return true;
}

void DBSnapshotSampler::Stop()
{
    running_ = false;
    if (worker_.joinable())
        worker_.join();
}

bool DBSnapshotSampler::Running() const
{
    return running_.load();
}

void DBSnapshotSampler::Loop()
{
    run_continuous_spool_loop(running_, config_, collect_db_window);
}

namespace
{

struct SharedSessionAccumulator
{
    ContinuousSamplerConfig config;
    std::vector<WindowPayload> slices;
    std::vector<WindowPayload> batch;
    std::string batchID;
    // 阶段一：该 Session 在本次 collector generation 内单调递增的批次序号。
    uint64_t batchSequence = 0;
};

static ContinuousSamplerConfig shared_physical_config(const std::vector<ContinuousSamplerConfig> &sessions)
{
    ContinuousSamplerConfig physical = sessions.front();
    physical.sessionSID = "__shared_continuous_engine__";
    physical.sampleRateHz = 1;
    physical.aggregationWindowSec = 5;
    physical.uploadBatchSec = 5;
    physical.selectorExe.clear();
    physical.targetProcesses.clear();
    bool hostScope = false;
    std::map<std::pair<int, int64_t>, ContinuousTargetProcess> targets;
    for (const auto &session : sessions)
    {
        physical.sampleRateHz = std::max(physical.sampleRateHz, session.sampleRateHz);
        if (session.scope != "process")
            hostScope = true;
        for (const auto &target : session.targetProcesses)
            if (target.pid > 0)
                targets[{target.pid, target.processStartMs}] = target;
    }
    physical.scope = hostScope ? "host" : "process";
    for (const auto &entry : targets)
        physical.targetProcesses.push_back(entry.second);

    // 阶段一：物理采集信号 = 所有活动 Session 请求信号的并集。逻辑层在
    // filter_shared_window 严格按各自 requestedSignals 分流（多进程选择器 +
    // 滚动 bpftrace fallback 无法安全归属直方图时，fanout 标记 unavailable，
    // 但物理层仍按并集采集，strict CO-RE 路径可正确按 PID 归属）。
    std::vector<std::string> unionRequested;
    for (const auto &session : sessions)
        for (const auto &signal : session.requestedSignals)
            if (std::find(unionRequested.begin(), unionRequested.end(), signal) == unionRequested.end())
                unionRequested.push_back(signal);
    physical.signals = physical_signals_from_requested(unionRequested);
    // 阶段一：共享采集器实例拥有独立 collector generation（新实例 = 新 generation）。
    physical.collectorGeneration = collector_generation_id();
    return physical;
}

static HistogramPayload unavailable_shared_histogram(const std::string &signalType)
{
    HistogramPayload histogram;
    histogram.signalType = signalType;
    histogram.backend = "shared-bpftrace-fallback";
    histogram.unavailable = true;
    histogram.reason = "shared rolling fallback cannot attribute this histogram across multiple process selectors";
    return histogram;
}

static WindowPayload filter_shared_window(const WindowPayload &source,
                                          const ContinuousSamplerConfig &session,
                                          bool histogramAttributionSafe)
{
    // 阶段一：逻辑层严格按该 Session 请求的信号分流（对 host/process 两种
    // scope 都生效）：未请求的信号从窗口 payload 中剔除，后续 build_batch_json
    // 按剔除后的内容重算分信号计数。
    const bool cpuRequested = logical_signal_requested(session.requestedSignals, "cpu_profile");
    const bool ioRequested = logical_signal_requested(session.requestedSignals, "io_latency");
    const bool ioSyscallRequested = logical_signal_requested(session.requestedSignals, "io_syscall_latency");
    const bool schedRequested = logical_signal_requested(session.requestedSignals, "sched_latency");
    const bool rssRequested = logical_signal_requested(session.requestedSignals, "python_rss");
    const bool dbRequested = logical_signal_requested(session.requestedSignals, "db_snapshot");

    WindowPayload out = source;
    if (!cpuRequested)
    {
        out.samples.clear();
        out.profiles.clear();
    }
    else if (session.scope == "process")
    {
        out.samples.erase(std::remove_if(out.samples.begin(), out.samples.end(), [&](const auto &sample) {
                              return !process_targeted(session, sample.pid, sample.processStartMs, sample.exe);
                          }),
                          out.samples.end());
        for (auto &profile : out.profiles)
        {
            profile.readyPath.clear();
            profile.samples.erase(std::remove_if(profile.samples.begin(), profile.samples.end(), [&](const auto &sample) {
                                      return !process_targeted(session, sample.pid, sample.processStartMs, sample.exe);
                                  }),
                                  profile.samples.end());
        }
        out.profiles.erase(std::remove_if(out.profiles.begin(), out.profiles.end(), [](const auto &profile) {
                               return profile.samples.empty();
                           }),
                           out.profiles.end());
    }
    if (!rssRequested)
        out.metrics.clear();
    else if (session.scope == "process")
    {
        out.metrics.erase(std::remove_if(out.metrics.begin(), out.metrics.end(), [&](const auto &metric) {
                              return !process_targeted(session, metric.pid, metric.processStartMs, metric.exe);
                          }),
                          out.metrics.end());
    }
    if (!dbRequested)
        out.dbSnapshots.clear();
    if (session.scope == "process")
    {
        out.histograms.erase(std::remove_if(out.histograms.begin(), out.histograms.end(), [&](const auto &histogram) {
                                  return histogram.pid > 0 && !process_targeted(session, histogram.pid, 0, "");
                              }),
                             out.histograms.end());
        // Runtime symbol diagnostics can contain paths/PIDs for another selector in
        // the physical union. Per-sample symbols are already resolved, so omit the
        // cross-target diagnostic blob from process Session payloads.
        out.symbolRefsJson.clear();
    }
    // 信号级过滤：直方图按请求的信号类型剔除（host/process 均生效）。
    out.histograms.erase(std::remove_if(out.histograms.begin(), out.histograms.end(), [&](const auto &histogram) {
                             const std::string &signal = histogram.signalType;
                             if (signal == "io_latency")
                                 return !ioRequested;
                             if (signal == "io_syscall_latency")
                                 return !ioSyscallRequested;
                             if (signal == "sched_latency")
                                 return !schedRequested;
                             return true; // 未知直方图信号类型一律剔除
                         }),
                         out.histograms.end());
    if (!histogramAttributionSafe)
    {
        // 多进程选择器 + 滚动 bpftrace fallback：直方图无法安全归属到单个
        // selector，仅对请求了该信号的 Session 打 unavailable 标记。
        out.histograms.clear();
        if (ioRequested)
            out.histograms.push_back(unavailable_shared_histogram("io_latency"));
        if (ioSyscallRequested)
            out.histograms.push_back(unavailable_shared_histogram("io_syscall_latency"));
        if (schedRequested)
            out.histograms.push_back(unavailable_shared_histogram("sched_latency"));
    }
    return out;
}

static HistogramPayload merge_histograms(const std::vector<HistogramPayload> &parts,
                                         const std::string &signalType)
{
    HistogramPayload merged;
    merged.signalType = signalType;
    std::map<std::string, HistogramBucket> buckets;
    bool sawAvailable = false;
    for (const auto &part : parts)
    {
        if (merged.backend.empty() && !part.backend.empty())
            merged.backend = part.backend;
        if (!part.unavailable)
            sawAvailable = true;
        else if (merged.reason.empty())
            merged.reason = part.reason;
        for (const auto &bucket : part.buckets)
        {
            std::string key = bucket.range + "|" + std::to_string(bucket.low) + "|" + std::to_string(bucket.high);
            auto &target = buckets[key];
            if (target.range.empty())
                target = bucket;
            else
                target.count = add_count(target.count, bucket.count);
        }
    }
    for (auto &entry : buckets)
        merged.buckets.push_back(entry.second);
    std::sort(merged.buckets.begin(), merged.buckets.end(), [](const auto &left, const auto &right) {
        return left.low == right.low ? left.high < right.high : left.low < right.low;
    });
    merged.unavailable = !sawAvailable;
    summarize_histogram(&merged);
    return merged;
}

static WindowPayload merge_shared_slices(const std::vector<WindowPayload> &slices)
{
    WindowPayload merged;
    if (slices.empty())
        return merged;
    merged.startMs = slices.front().startMs;
    merged.endMs = slices.front().endMs;
    std::map<std::string, std::vector<HistogramPayload>> histograms;
    std::set<std::string> attempted;
    bool anyFailed = false;
    bool anyDegraded = false;
    for (const auto &slice : slices)
    {
        merged.startMs = std::min(merged.startMs, slice.startMs);
        merged.endMs = std::max(merged.endMs, slice.endMs);
        merged.samples.insert(merged.samples.end(), slice.samples.begin(), slice.samples.end());
        merged.profiles.insert(merged.profiles.end(), slice.profiles.begin(), slice.profiles.end());
        merged.metrics.insert(merged.metrics.end(), slice.metrics.begin(), slice.metrics.end());
        merged.rssTruncated += slice.rssTruncated;
        for (const auto &histogram : slice.histograms)
            histograms[histogram.signalType].push_back(histogram);
        attempted.insert(slice.attemptedBackends.begin(), slice.attemptedBackends.end());
        if (!slice.selectedBackend.empty())
            merged.selectedBackend = slice.selectedBackend;
        if (!slice.symbolRefsJson.empty())
            merged.symbolRefsJson = slice.symbolRefsJson;
        if (slice.backendStatus == "failed")
            anyFailed = true;
        else if (slice.backendStatus == "degraded")
            anyDegraded = true;
        if (merged.backendReason.empty() && !slice.backendReason.empty())
            merged.backendReason = slice.backendReason;
    }
    for (const auto &entry : histograms)
        merged.histograms.push_back(merge_histograms(entry.second, entry.first));
    merged.attemptedBackends.assign(attempted.begin(), attempted.end());
    merged.backendStatus = anyFailed ? (slices.size() == 1 ? "failed" : "degraded") : (anyDegraded ? "degraded" : "ok");
    return merged;
}

static std::vector<WindowPayload> merge_shared_slices_preserving_gaps(
    const std::vector<WindowPayload> &slices,
    int64_t continuityToleranceMs = 100)
{
    std::vector<WindowPayload> ordered = slices;
    std::sort(ordered.begin(), ordered.end(), [](const auto &left, const auto &right) {
        return left.startMs == right.startMs ? left.endMs < right.endMs : left.startMs < right.startMs;
    });
    std::vector<WindowPayload> merged;
    std::vector<WindowPayload> contiguous;
    for (const auto &slice : ordered)
    {
        if (!contiguous.empty() && slice.startMs > contiguous.back().endMs + continuityToleranceMs)
        {
            merged.push_back(merge_shared_slices(contiguous));
            contiguous.clear();
        }
        contiguous.push_back(slice);
    }
    if (!contiguous.empty())
        merged.push_back(merge_shared_slices(contiguous));
    return merged;
}

static bool persist_shared_aggregate(SharedSessionAccumulator *session)
{
    if (!session)
        return false;
    session->slices.erase(std::remove_if(session->slices.begin(), session->slices.end(), [](const auto &slice) {
        const bool hasPayload = !slice.samples.empty() || !slice.profiles.empty() ||
                                !slice.metrics.empty() || !slice.histograms.empty();
        return slice.endMs <= slice.startMs || !hasPayload;
    }), session->slices.end());
    if (!session || session->slices.empty())
        return true;
    std::vector<WindowPayload> aggregates = merge_shared_slices_preserving_gaps(session->slices);
    session->slices.clear();
    // 阶段一：为合并后的窗口补齐稳定 window_id / 内容摘要（保证同逻辑窗口
    // 重传一致；内容摘要不参与 ID，冲突时仍是同一 ID）。
    for (auto &window : aggregates)
    {
        if (window.windowID.empty())
            window.windowID = make_window_id(session->config, window);
        if (window.contentSHA256.empty())
            window.contentSHA256 = window_content_digest(window);
    }
    session->batch.insert(session->batch.end(), aggregates.begin(), aggregates.end());
    if (session->batchID.empty())
    {
        session->config.batchSequence = ++session->batchSequence;
        session->batchID = make_batch_id(session->config);
    }
    std::string body = build_batch_json(session->config, session->batchID, session->batch);
    if (!persist_batch(session->config, session->batchID, body))
        return false;
    acknowledge_batch_profiles(aggregates);

    const int windowsPerBatch = std::max(1, (session->config.uploadBatchSec + session->config.aggregationWindowSec - 1) /
                                               session->config.aggregationWindowSec);
    if (static_cast<int>(session->batch.size()) >= windowsPerBatch)
    {
        if (!finalize_batch(session->config, session->batchID))
            return false;
        session->batch.clear();
        session->batchID.clear();
    }
    return true;
}

static bool finalize_shared_session(SharedSessionAccumulator *session)
{
    if (!persist_shared_aggregate(session))
        return false;
    if (session->batch.empty() || session->batchID.empty())
        return true;
    if (!finalize_batch(session->config, session->batchID))
        return false;
    session->batch.clear();
    session->batchID.clear();
    return true;
}

} // namespace

struct SharedDualTrackContinuousSampler::Impl
{
    std::atomic<bool> running{false};
    std::atomic<bool> ready{false};
    std::atomic<bool> strict{false};
    std::atomic<bool> failed{false};
    mutable std::mutex statusMutex;
    std::string degradationReason;
    std::thread worker;
    std::vector<ContinuousSamplerConfig> sessions;
    ContinuousSamplerConfig physical;
    std::vector<SpoolRetryState> spoolRetries;
    // 阶段一：无重叠采集器切换。
    //   owning=false 表示 ready-but-not-owning（replacement 已就绪但不输出正式窗口）。
    //   cutoverWatermarkMs / cutoverKeepBefore 实现唯一 cutover watermark：
    //     旧 generation 只保留 start < watermark（keepBefore=true），
    //     新 generation 只保留 start >= watermark（keepBefore=false）。
    std::atomic<bool> owning{true};
    std::atomic<int64_t> cutoverWatermarkMs{0};
    std::atomic<bool> cutoverKeepBefore{false};

    // 该窗口是否被允许正式输出（owning + cutover watermark 过滤）。
    bool WindowAllowed(const WindowPayload &window) const
    {
        if (!owning.load())
            return false; // ready-but-not-owning：不输出正式窗口
        const int64_t watermark = cutoverWatermarkMs.load();
        if (watermark <= 0)
            return true; // 首次启动无 cutover
        if (cutoverKeepBefore.load())
            // 跨越切点的聚合窗口无法在不伪造样本时间的情况下拆分，必须丢弃；
            // 只保留完整结束于切点前的旧代窗口，确保与新代绝不重叠。
            return window.endMs <= watermark;
        return window.startMs >= watermark;    // 新 generation 只保留切点后
    }

    bool DrainAllSessionSpools(bool force)
    {
        if (spoolRetries.size() != sessions.size())
            spoolRetries.assign(sessions.size(), {});
        bool allSucceeded = true;
        for (size_t index = 0; index < sessions.size(); ++index)
            if (!drain_one_spooled_batch(sessions[index], &spoolRetries[index], force))
                allSucceeded = false;
        return allSucceeded;
    }

    bool AllSessionSpoolsEmpty() const
    {
        for (const auto &session : sessions)
            if (!list_session_spool_files(session, ".json").empty() ||
                !list_session_spool_files(session, ".journal").empty())
                return false;
        return true;
    }

    bool RunStrict(std::vector<SharedSessionAccumulator> &accumulators)
    {
        if (!CoreContinuousSamplerAvailable())
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            degradationReason = "strict persistent CO-RE object is unavailable";
            return false;
        }
        CoreEbpfCollector core;
        RollingPerfRecorder recorder;
        std::string error;
        if (!core.Start(physical.targetProcesses, &error) || !recorder.Start(physical, &error))
        {
            std::cout << "[native-cp] strict engine unavailable: " << error << std::endl;
            {
                std::lock_guard<std::mutex> lock(statusMutex);
                degradationReason = "strict persistent perf/CO-RE engine unavailable: " + error;
            }
            core.Stop();
            recorder.Stop();
            return false;
        }
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            degradationReason = core.DegradationReason();
        }
        std::cout << "[native-cp] strict engine started backend=perf_rolling,libbpf-co-re" << std::endl;
        std::vector<CoreHistogramSample> pendingCoreSamples;
        uint64_t pendingCoreLost = 0;
        while (running.load())
        {
            DrainAllSessionSpools(false);
            // Refresh the TID-to-TGID registry in place so sched latency keeps
            // following threads created after the Session assignment.
            if (!core.UpdateTargets(physical.targetProcesses, &error))
            {
                std::cout << "[native-cp] strict target refresh failed: " << error << std::endl;
                running = false;
                break;
            }
            // A successful attach is not enough for a gap-free handoff: keep
            // the previous recorder alive until this generation has produced
            // and successfully parsed at least one immutable switch-output
            // segment. Probe does not consume the file, so backpressure cannot
            // discard the first window merely to establish readiness.
            if (!ready.load() && recorder.HasParseableOutput())
            {
                strict.store(true);
                failed.store(false);
                ready.store(true);
                std::cout << "[native-cp] strict engine ready after first parseable rolling file" << std::endl;
            }
            else if (!strict.load() && recorder.HasParseableOutput())
            {
                // The sampler may already have been adopted in an explicit
                // disk-backpressure state. Promote its observed status only
                // after the resumed recorder proves it can parse a real file.
                strict.store(true);
                failed.store(false);
            }
            if (!spool_has_collection_capacity(sessions.front()))
            {
                interruptible_wait(running, 500);
                continue;
            }
            auto windows = recorder.Drain(physical, false);
            uint64_t lost = 0;
            auto coreSamples = core.Drain(&lost);
            queue_core_histograms(&windows, physical, &pendingCoreSamples, &pendingCoreLost,
                                  std::move(coreSamples), lost);
            for (auto &physicalWindow : windows)
            {
                physicalWindow.attemptedBackends.push_back("libbpf-co-re");
                physicalWindow.selectedBackend = "perf_rolling+libbpf-co-re";
                physicalWindow.backendStatus = "ok";
                // 阶段一：cutover watermark 过滤（新 generation 切点前不输出）。
                if (!WindowAllowed(physicalWindow))
                    continue;
                for (auto &accumulator : accumulators)
                {
                    accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, true));
                    const int64_t coveredMs = accumulator.slices.back().endMs - accumulator.slices.front().startMs;
                    if (coveredMs >= static_cast<int64_t>(accumulator.config.aggregationWindowSec) * 1000 &&
                        !persist_shared_aggregate(&accumulator))
                    {
                        std::cout << "[native-cp] strict engine failed to persist sid=" << accumulator.config.sessionSID << std::endl;
                        running = false;
                        break;
                    }
                }
            }
            interruptible_wait(running, 250);
        }
        recorder.Stop();
        auto finalWindows = recorder.Drain(physical, true);
        uint64_t lost = 0;
        auto finalCore = core.StopAndDrain(&lost);
        queue_core_histograms(&finalWindows, physical, &pendingCoreSamples, &pendingCoreLost,
                              std::move(finalCore), lost);
        for (auto &physicalWindow : finalWindows)
        {
            physicalWindow.attemptedBackends.push_back("libbpf-co-re");
            physicalWindow.selectedBackend = "perf_rolling+libbpf-co-re";
            physicalWindow.backendStatus = "ok";
            // 阶段一：旧 generation 最终 drain 只保留切点前窗口。
            if (!WindowAllowed(physicalWindow))
                continue;
            for (auto &accumulator : accumulators)
                accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, true));
        }
        for (auto &accumulator : accumulators)
            if (!finalize_shared_session(&accumulator))
                std::cout << "[native-cp] strict engine final flush failed sid=" << accumulator.config.sessionSID << std::endl;
        DrainAllSessionSpools(true);
        return true;
    }

    void Loop()
    {
        std::vector<SharedSessionAccumulator> accumulators;
        accumulators.reserve(sessions.size());
        for (const auto &config : sessions)
        {
            recover_spool_journals(config);
            accumulators.push_back({config, {}, {}, ""});
        }
        spoolRetries.assign(sessions.size(), {});
        while (running.load() && DrainAllSessionSpools(true))
        {
            if (AllSessionSpoolsEmpty())
                break;
        }

        if (running.load() && !spool_has_collection_capacity(sessions.front()))
        {
            strict.store(false);
            {
                std::lock_guard<std::mutex> lock(statusMutex);
                degradationReason = "shared spool backpressure: free disk is below the configured reserve; collection is paused and will resume automatically";
            }
            // Readiness here means the replacement has entered an explicit,
            // observable paused state. No perf recorder is launched, so low
            // disk cannot accumulate unconsumed switch-output files.
            ready.store(true);
            while (running.load() && !spool_has_collection_capacity(sessions.front()))
            {
                DrainAllSessionSpools(false);
                interruptible_wait(running, 1000);
            }
        }

        if (!running.load())
            return;

        const bool degradedFallbackAllowed = std::all_of(
            sessions.begin(), sessions.end(), [](const auto &session) { return session.allowDegraded; });
        while (running.load() && !RunStrict(accumulators))
        {
            if (degradedFallbackAllowed)
                break;
            strict.store(false);
            failed.store(true);
            {
                std::lock_guard<std::mutex> lock(statusMutex);
                if (degradationReason.find("degraded fallback is not allowed") == std::string::npos)
                    degradationReason += degradationReason.empty() ? "strict collector is unavailable and degraded fallback is not allowed"
                                                                   : "; degraded fallback is not allowed";
            }
            ready.store(true);
            interruptible_wait(running, 5000);
        }

        if (!running.load() || strict.load())
            return;

        const bool histogramAttributionSafe = physical.scope != "process" || sessions.size() == 1;
        // The degraded path is still a valid attached collector. Mark it
        // ready before entering its first collection window so a manager
        // handoff never tears down the previous recorder during startup.
        strict.store(false);
        failed.store(false);
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            if (degradationReason.empty())
                degradationReason = "strict persistent perf/CO-RE engine unavailable";
            if (degradationReason.find("using shared rolling perf/bpftrace fallback") == std::string::npos)
                degradationReason += "; using shared rolling perf/bpftrace fallback";
        }
        ready.store(true);
        while (running.load())
        {
            DrainAllSessionSpools(false);
            if (!spool_has_collection_capacity(sessions.front()))
            {
                std::cout << "[native-cp] shared spool backpressure usage_bytes=" << spool_usage_bytes(sessions.front())
                          << " max_bytes=" << sessions.front().spoolMaxBytes << std::endl;
                interruptible_wait(running, 1000);
                continue;
            }
            WindowPayload physicalWindow = collect_dual_track_window(physical);
            // 阶段一：cutover watermark 过滤（新 generation 切点前不输出；
            // 旧 generation 切点后不输出）。
            if (!WindowAllowed(physicalWindow))
                continue;
            for (auto &accumulator : accumulators)
            {
                accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, histogramAttributionSafe));
                const int64_t coveredMs = accumulator.slices.back().endMs - accumulator.slices.front().startMs;
                if (coveredMs >= static_cast<int64_t>(accumulator.config.aggregationWindowSec) * 1000 &&
                    !persist_shared_aggregate(&accumulator))
                {
                    std::cout << "[native-cp] shared engine failed to persist Session batch sid="
                              << accumulator.config.sessionSID << " errno=" << errno << std::endl;
                    running = false;
                    break;
                }
            }
        }

        for (auto &accumulator : accumulators)
            if (!finalize_shared_session(&accumulator))
                std::cout << "[native-cp] shared engine failed final Session flush sid="
                          << accumulator.config.sessionSID << " errno=" << errno << std::endl;
        DrainAllSessionSpools(true);
    }
};

SharedDualTrackContinuousSampler::SharedDualTrackContinuousSampler()
    : impl_(new Impl)
{
}

SharedDualTrackContinuousSampler::~SharedDualTrackContinuousSampler()
{
    Stop();
}

bool SharedDualTrackContinuousSampler::Start(const std::vector<ContinuousSamplerConfig> &sessions,
                                             std::string *error)
{
    if (sessions.empty())
    {
        if (error)
            *error = "shared continuous engine requires at least one Session";
        return false;
    }
    if (impl_->running.load())
        return true;
    impl_->ready = false;
    impl_->strict = false;
    impl_->failed = false;
    {
        std::lock_guard<std::mutex> lock(impl_->statusMutex);
        impl_->degradationReason.clear();
    }
    std::set<std::string> sids;
    for (const auto &config : sessions)
    {
        if (config.sessionSID.empty() || config.apiBaseURL.empty() || config.authUID.empty() ||
            config.sampleRateHz <= 0 || config.aggregationWindowSec <= 0 || config.uploadBatchSec <= 0)
        {
            if (error)
                *error = "invalid shared continuous Session config";
            return false;
        }
        if (!sids.insert(config.sessionSID).second)
        {
            if (error)
                *error = "duplicate Session in shared continuous engine";
            return false;
        }
        if (!ensure_directory(session_spool_directory(config)))
        {
            if (error)
                *error = "continuous spool directory is not writable";
            return false;
        }
    }
    impl_->sessions = sessions;
    impl_->physical = shared_physical_config(sessions);
    // 阶段一：把本采集器实例的 collector generation 与每个 Session 的目标
    // 指纹传播进 session config，供 window_id / batch_id / 内容摘要稳定使用。
    for (auto &config : impl_->sessions)
    {
        config.collectorGeneration = impl_->physical.collectorGeneration;
        if (config.targetFingerprint.empty())
            config.targetFingerprint = target_fingerprint_for(config);
        if (config.requestedSignals.empty())
            config.requestedSignals = {"cpu_profile", "io_latency", "io_syscall_latency", "sched_latency"};
    }
    impl_->running = true;
    impl_->worker = std::thread(&SharedDualTrackContinuousSampler::Impl::Loop, impl_.get());
    std::cout << "[native-cp] shared engine started sessions=" << sessions.size()
              << " targets=" << impl_->physical.targetProcesses.size()
              << " rate_hz=" << impl_->physical.sampleRateHz
              << " generation=" << impl_->physical.collectorGeneration << std::endl;
    return true;
}

void SharedDualTrackContinuousSampler::BeginHandoff()
{
    if (!impl_)
        return;
    impl_->owning.store(false);
    impl_->cutoverWatermarkMs.store(0);
}

void SharedDualTrackContinuousSampler::SetCutoverWatermark(int64_t watermarkMs, bool keepBefore)
{
    if (!impl_)
        return;
    impl_->cutoverWatermarkMs.store(watermarkMs);
    impl_->cutoverKeepBefore.store(keepBefore);
}

void SharedDualTrackContinuousSampler::Own(int64_t watermarkMs)
{
    if (!impl_)
        return;
    impl_->cutoverWatermarkMs.store(watermarkMs);
    impl_->cutoverKeepBefore.store(false);
    impl_->owning.store(true);
}

bool SharedDualTrackContinuousSampler::Owning() const
{
    return impl_ && impl_->owning.load();
}

void SharedDualTrackContinuousSampler::Stop()
{
    if (!impl_)
        return;
    impl_->running = false;
    impl_->ready = false;
    impl_->strict = false;
    impl_->failed = false;
    if (impl_->worker.joinable())
        impl_->worker.join();
}

bool SharedDualTrackContinuousSampler::Running() const
{
    return impl_ && impl_->running.load();
}

bool SharedDualTrackContinuousSampler::Ready() const
{
    return impl_ && impl_->ready.load();
}

bool SharedDualTrackContinuousSampler::Strict() const
{
    return impl_ && impl_->strict.load();
}

bool SharedDualTrackContinuousSampler::Failed() const
{
    return impl_ && impl_->failed.load();
}

std::string SharedDualTrackContinuousSampler::DegradationReason() const
{
    if (!impl_)
        return {};
    std::lock_guard<std::mutex> lock(impl_->statusMutex);
    return impl_->degradationReason;
}

} // namespace drop
