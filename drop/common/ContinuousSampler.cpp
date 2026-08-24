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
// 阶段二：统一 strict/degraded 的 perf 段处理流水线（payload 类型、解析辅助、
// ContinuousSegmentProcessor）。连续采样器的 strict/degraded 引擎都通过它处理
// 不可变 perf.data 段，共享符号准备、解析、runtime 分类与诊断生成。
#include "common/ContinuousSegmentProcessor.h"
#include "common/LanguageStatus.h"

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
#include <tuple>
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

struct RollingPerfRecorder
{
    pid_t pid = -1;
    std::string directory;
    std::set<std::string> consumed;
    int64_t wallStartMs = 0;
    int64_t monotonicStartMs = 0;
    bool Start(const ContinuousSamplerConfig &, std::string *error);
    // 阶段二：RollingPerfRecorder 只负责启动、轮转、枚举与确认删除不可变
    // segment，不再自行执行 perf script 或 runtime 分类（统一交给
    // ContinuousSegmentProcessor）。Drain 返回尚未交付的不可变段，不删除；
    // 处理成功后调用 Confirm(path)（删除并记录），最终失败调用 Abandon(path)
    // （删除并记录，形成真实 coverage gap）。
    std::vector<PerfSegment> Drain(const ContinuousSamplerConfig &, bool final);
    void Confirm(const std::string &path);
    void Abandon(const std::string &path);
    // 积压保护：已关闭滚动文件组成的磁盘有界队列。
    size_t PendingSegmentCount() const;
    uint64_t PendingSegmentBytes() const;
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
// 取并集。空集合回退四类全开（兼容旧客户端）。阶段三：只映射实际匹配的
// 物理信号——纯 python_rss/python_memory/db_snapshot 请求不产生 CPU/IO/sched
// 物理采集（"不选就不采"），返回空字符串。
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
        return "";
    std::string out;
    for (size_t i = 0; i < physical.size(); ++i)
    {
        if (i)
            out += ",";
        out += physical[i];
    }
    return out;
}

// logical_signal_requested 现由 common/ContinuousSegmentProcessor.h 提供
// （drop::logical_signal_requested，inline），连续采样器 fan-out 与 processor 共用。

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

// clamp_count / add_count / kMaxDBCount 现由 common/ContinuousSegmentProcessor.h
// 提供（drop::，inline），连续采样器与 processor 共用同一份实现。

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

// ============================================================
// 阶段二：共享 payload 类型（AggregatedSample / ProfilePayload /
// HistogramPayload / MetricPayload / DBSnapshotSample / WindowPayload /
// PhysicalDiagnostics / PerfSegment / ContinuousSegmentProcessor）已统一移至
// common/ContinuousSegmentProcessor.h（drop::），strict/degraded/测试共用同一套。
// ============================================================

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
        content << hist.pid << ':' << hist.processStartMs << ':' << hist.eventCount << ':' << hist.unavailable << ':';
        text(hist.exe); text(hist.comm);
        content
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
    content << "meta:" << window.rssTruncated << ':' << window.identityUnavailableCount << ':'
            << window.physicalSampleRateHz << ':' << window.effectiveSampleRateHz << ':';
    text(window.backendStatus); text(window.backendReason); text(window.selectedBackend);
    for (const auto &backend : window.attemptedBackends)
        text(backend);
    for (const auto &entry : window.signalStatuses)
    {
        text(entry.first);
        content << signal_status_name(entry.second.status) << ':' << entry.second.lostEvents << ':';
        text(entry.second.reason);
    }
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

// now_ms / monotonic_ms / rfc3339_from_ms / json_escape / perf_bin / read_exe /
// process_tgid / process_targeted / configured_process_start_ms /
// configured_process_exe / env_enabled_default / env_positive_int 已统一移至
// common/ContinuousSegmentProcessor.h（drop::inline），连续采样器与 processor
// 共用同一份实现，避免 strict/degraded 各写一套。

// 阶段五：frames-only 模式（DROP_CONTINUOUS_FRAMES_ONLY=1）。
// shadow/prefer 阶段同时发送 stack+frames；进入 v2-only 且回滚窗口结束后
// 仅发送 frames（服务端按 frames 生成展示名称）。默认关闭保持兼容。
static bool frames_only_mode()
{
    static const bool enabled = env_enabled_default("DROP_CONTINUOUS_FRAMES_ONLY", false);
    return enabled;
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

// 阶段四：栈回溯模式。默认 Frame Pointer（-g）；DROP_NATIVE_CP_CALL_GRAPH=dwarf
// 时统一为 --call-graph dwarf,8192。
static std::string unwind_mode_from_env()
{
    const char *env = std::getenv("DROP_NATIVE_CP_CALL_GRAPH");
    if (!env || !*env)
        return "fp";
    std::string value(env);
    std::transform(value.begin(), value.end(), value.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return value == "dwarf" ? "dwarf" : "fp";
}

static std::vector<std::string> call_graph_args(const std::string &unwindMode)
{
    if (unwindMode == "dwarf")
        return {"--call-graph", "dwarf,8192"};
    return {"-g"};
}

static std::vector<std::string> perf_record_args(const ContinuousSamplerConfig &cfg,
                                                 const std::string &perf,
                                                 const std::string &perfEvent,
                                                 const std::string &dataPath,
                                                 const std::string &unwindMode = "fp")
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
    args.insert(args.end(), {"-e", perfEvent, "-F", std::to_string(cfg.sampleRateHz)});
    for (auto &arg : call_graph_args(unwindMode))
        args.push_back(std::move(arg));
    args.insert(args.end(), {"-o", dataPath, "--", "sleep",
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

// 前向声明（定义在 collect_window 之后）。
static bool signal_enabled(const std::string &signals, const std::string &name);

// ============================================================
// 阶段四：Python 两级策略（perf-map → py-spy）候选构建
// ============================================================

// 单 PID 的 Python 语义帧覆盖率（%）。只统计 perf backend 的 python 样本
// （py-spy 样本本身即语义帧），排除内核帧。
static double python_semantic_percent_for_pid(const std::vector<AggregatedSample> &samples, int pid)
{
    uint64_t total = 0;
    uint64_t semantic = 0;
    for (const auto &sample : samples)
    {
        if (sample.pid != pid || sample.runtime != "python" || sample.backend == "py-spy")
            continue;
        size_t index = 0;
        for (const auto &frame : sample.stack)
        {
            const drop::ContinuousStackFrame structured =
                index < sample.frames.size() ? sample.frames[index] : drop::ContinuousStackFrame{};
            ++index;
            if (drop::is_kernel_frame(structured) || drop::is_kernel_frame_text(frame))
                continue;
            total = add_count(total, sample.count);
            if (drop::is_python_semantic_frame(structured) || drop::is_python_semantic_frame_text(frame))
                semantic = add_count(semantic, sample.count);
        }
    }
    if (total == 0)
        return 100.0; // 无 perf Python 样本时不用覆盖率触发 fallback
    return static_cast<double>(semantic) * 100.0 / static_cast<double>(total);
}

// 候选构建规则见 collect_window 内注释。firstWindowDetected=false 时（首窗
// runtime map 尚未识别出 Python）用进程预检兜底，保证第一窗就能决定 fallback。
static std::vector<drop::PythonCandidate> build_python_candidates(
    const ContinuousSamplerConfig &cfg,
    const drop::RuntimeSymbolReport &runtimeReport,
    const std::vector<AggregatedSample> &samples,
    bool firstWindowDetected)
{
    std::map<int, drop::PythonCandidate> chosen;

    auto addCandidate = [&](int pid) {
        int64_t startMs = 0;
        if (!drop::python_process_start_ms(pid, &startMs))
            return;
        auto it = chosen.find(pid);
        if (it != chosen.end())
        {
            it->second.startMs = startMs;
            return;
        }
        drop::PythonCandidate candidate;
        candidate.pid = pid;
        candidate.startMs = startMs;
        candidate.samples = 0;
        for (const auto &sample : samples)
            if (sample.pid == pid && !sample.comm.empty())
            {
                candidate.comm = sample.comm;
                break;
            }
        if (candidate.comm.empty())
            candidate.comm = "python";
        candidate.exe = read_exe(pid);
        chosen.emplace(pid, std::move(candidate));
    };

    // 1) perf-map 缺失的采样进程
    for (int pid : runtimeReport.python.missingPids)
        addCandidate(pid);

    // 2) 有 map 但语义覆盖 <70%（perf-map 模式质量不足）
    std::set<int> checkedPids;
    for (const auto &sample : samples)
    {
        if (sample.runtime != "python" || sample.backend == "py-spy")
            continue;
        if (!checkedPids.insert(sample.pid).second)
            continue;
        if (runtimeReport.python.readyPids.empty() ||
            std::find(runtimeReport.python.readyPids.begin(), runtimeReport.python.readyPids.end(),
                      sample.pid) != runtimeReport.python.readyPids.end())
        {
            if (python_semantic_percent_for_pid(samples, sample.pid) < 70.0)
                addCandidate(sample.pid);
        }
    }

    // 3) 首窗预检兜底：runtime map 未识别到 Python 时主动探测目标。
    //    节流：探测含短周期 CPU ticks 采样（约 200ms）与全 /proc 扫描，
    //    全局最多每 5 秒一次，避免空跑机器上每个段都付这笔开销。
    if (chosen.empty() && !firstWindowDetected)
    {
        static std::atomic<int64_t> lastProbeMonoMs{0};
        const int64_t nowMono = monotonic_ms();
        int64_t last = lastProbeMonoMs.load();
        if (nowMono - last >= 5000 && lastProbeMonoMs.compare_exchange_strong(last, nowMono))
        {
            if (cfg.scope == "process")
            {
                for (const auto &target : cfg.targetProcesses)
                {
                    drop::PythonRuntimeProbe probe = drop::probe_python_runtime(target.pid);
                    // 无 -X perf 真实 map 的解释器直接进入 py-spy；带 flag 且
                    // map 就绪的交给 perf-map 路径。
                    if (probe.valid && !probe.hasPerfMap)
                        addCandidate(target.pid);
                }
            }
            else
            {
                // host Session：短周期 CPU ticks 增量排序，默认只 attach 最热实例。
                for (auto &candidate :
                     drop::hottest_python_candidates_by_cpu_ticks(8))
                {
                    drop::PythonRuntimeProbe probe = drop::probe_python_runtime(candidate.pid);
                    if (probe.valid && !probe.hasPerfMap)
                        chosen.emplace(candidate.pid, std::move(candidate));
                }
            }
        }
    }

    std::vector<drop::PythonCandidate> out;
    out.reserve(chosen.size());
    for (auto &kv : chosen)
        out.push_back(std::move(kv.second));
    return out;
}


static WindowPayload collect_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    // 阶段二：wall/mono 锚点必须在 perf record 之前成对捕获（perf 样本时钟是
    // CLOCK_MONOTONIC，解析时用这对锚点把样本时间映射回 Unix 毫秒；若 mono
    // 锚点取在 record 之后，映射结果会整体前移一个录制时长，导致窗口时间
    // 错位/负 capture_elapsed）。
    const int64_t monoAnchorMs = monotonic_ms();
    std::string dataPath = "/tmp/mini_drop_native_cp_" + std::to_string(window.startMs) + ".data";
    std::string perf = perf_bin();
    // 阶段二：degraded 的 collect_window 只负责录制原始 perf 段 + 模式特有的
    // sidecar 数据（py-spy / RSS / Memray）；perf.data 交给统一的
    // ContinuousSegmentProcessor 解析（符号准备/解析/runtime 分类/诊断）。
    // 阶段三：py-spy 是 CPU fallback，只有请求 cpu_profile 才启用。
    // 阶段四：独立开关 DROP_CONTINUOUS_PYSPY_FALLBACK（默认开启，可与旧
    // 开关 DROP_NATIVE_CP_PYTHON_FALLBACK_ENABLED 单独关闭）。
    const bool pythonFallbackEnabled =
        logical_signal_requested(cfg.requestedSignals, "cpu_profile") &&
        env_enabled_default("DROP_CONTINUOUS_PYSPY_FALLBACK", true) &&
        env_enabled_default("DROP_NATIVE_CP_PYTHON_FALLBACK_ENABLED", true);
    // 阶段四：host Session 默认只 attach 最热 2 个 Python 实例（CPU ticks
    // 增量排序）；process Session 精确目标最多 4 个。
    int pythonMaxProcesses = env_positive_int("DROP_NATIVE_CP_PYTHON_MAX_PROCESSES", 4);
    if (cfg.scope != "process")
        pythonMaxProcesses = env_positive_int("DROP_NATIVE_CP_PYTHON_HOST_MAX_PROCESSES", 2);
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
    // 阶段四：栈回溯模式 DROP_NATIVE_CP_CALL_GRAPH=fp|dwarf（默认 fp）。
    const std::string unwindMode = unwind_mode_from_env();
    std::string recordOutput;
    // 阶段三：纯 python/db 请求（signals 为空）不启动 CPU perf record。
    const bool cpuRequested = signal_enabled(cfg.signals, "cpu");
    std::vector<std::string> recordArgs;
    if (cpuRequested)
        recordArgs = perf_record_args(cfg, perf, perfEvent, dataPath, unwindMode);
    int rc = 0;
    if (!recordArgs.empty())
    {
        rc = drop::exec_capture(recordArgs, &recordOutput, 4096);
        window.endMs = now_ms();
    }
    else
    {
        window.endMs = now_ms();
        // 阶段三：CPU 未请求不是失败——窗口仍承载 RSS/Memray sidecar。
        window.backendStatus = "ok";
        window.backendReason = "CPU not requested by Session signals";
    }
    if (rc != 0)
    {
        std::cout << "[native-cp] perf record failed rc=" << rc << " output=" << recordOutput << std::endl;
        ::remove(dataPath.c_str());
        pythonCapture.Finish();
        window.backendStatus = "failed";
        window.backendReason = "perf record failed rc=" + std::to_string(rc);
        return window;
    }
    // degraded 已同步等满 capture 区间，直接收尾（与旧行为一致）。
    std::vector<drop::PythonFallbackResult> pythonResults = pythonCapture.Finish();
    const size_t pythonLimitedCount = pythonCapture.LimitedCount();
    for (auto &result : pythonResults)
    {
        if (result.ready && !drop::python_process_is_same(result.pid, result.startMs))
        {
            result.ready = false;
            result.samples.clear();
            result.reason = "process exited or PID was reused before stack replacement";
        }
    }
    std::vector<drop::MemrayProfileResult> memrayResults;
    // 阶段三：RSS/Memray 按 Session 请求信号决定（不选就不采、不存）。
    const bool rssRequested = logical_signal_requested(cfg.requestedSignals, "python_rss");
    const bool memrayRequested = logical_signal_requested(cfg.requestedSignals, "python_memory");
    const bool memrayEnabled = memrayRequested &&
                               env_enabled_default("DROP_NATIVE_CP_MEMRAY_INGEST_ENABLED", true);
    if (memrayEnabled)
        memrayResults = drop::collect_memray_profiles();

    drop::RuntimeCapabilitySet capabilities;
    capabilities.pythonFallback = pythonFallbackEnabled;
    capabilities.pythonRss = rssRequested &&
                             env_enabled_default("DROP_NATIVE_CP_PYTHON_RSS_ENABLED", true);
    capabilities.memray = memrayEnabled;
    // 阶段四：逐语言能力开关（默认全开，可单独关闭单语言）。
    capabilities.goReSym = env_enabled_default("DROP_CONTINUOUS_GORESYM", true);
    capabilities.goSymbols = capabilities.goReSym;
    capabilities.javaPerfMap = env_enabled_default("DROP_CONTINUOUS_JAVA_PERFMAP", true);
    capabilities.nodePerfMap = env_enabled_default("DROP_CONTINUOUS_NODE_PERFMAP", true);
    capabilities.pythonPerf = env_enabled_default("DROP_CONTINUOUS_PYTHON_PERF", true);
    capabilities.unwindMode = unwindMode;

    // 阶段三：CPU 未请求时无 perf.data 段，跳过 processor（窗口只承载
    // RSS/Memray sidecar）。
    SegmentProcessResult processed;
    if (cpuRequested)
    {
        PerfSegment segment;
        segment.path = dataPath;
        segment.sourceBackend = "perf";
        segment.collectorGeneration = cfg.collectorGeneration;
        segment.targetFingerprint = cfg.targetFingerprint;
        segment.wallStartMs = window.startMs;
        segment.monotonicStartMs = monoAnchorMs;

        ContinuousSegmentProcessor processor;
        processed = processor.Process(segment, cfg, capabilities,
                                      pythonResults, pythonLimitedCount, memrayResults);
        // degraded 单窗段处理完即删（成功或最终失败都删除，无重试队列）。
        ::remove(dataPath.c_str());
        if (!processed.success)
        {
            window.backendStatus = "failed";
            window.backendReason = processed.failureReason;
            std::cout << "[native-cp] degraded segment processing failed: "
                      << processed.failureReason << std::endl;
            return window;
        }
        window = std::move(processed.windows.front());
        window.backendStatus = "ok";
    }

    // RSS：观测时间生成独立 metric（不伪装成 perf 段时间）。
    if (capabilities.pythonRss)
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
    // Memray：自身采集时间生成 python_memory window。
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
            // 阶段三：Memray sample 携带完整进程身份。
            sample.processStartMs = result.processStartMs;
            sample.exe = result.exe;
            sample.backend = "memray";
            sample.runtime = "python";
            sample.count = clamp_count(raw.count);
            profile.samples.push_back(std::move(sample));
        }
        window.profiles.push_back(std::move(profile));
    }
    if (cfg.scope == "process")
    {
        window.metrics.erase(std::remove_if(window.metrics.begin(), window.metrics.end(), [&](const auto &metric) {
                                  return !process_targeted(cfg, metric.pid, metric.processStartMs, metric.exe);
                              }),
                             window.metrics.end());
        for (auto &profile : window.profiles)
            profile.samples.erase(std::remove_if(profile.samples.begin(), profile.samples.end(), [&](const auto &sample) {
                                      return !process_targeted(cfg, sample.pid, sample.processStartMs, sample.exe);
                                  }),
                                  profile.samples.end());
        window.profiles.erase(std::remove_if(window.profiles.begin(), window.profiles.end(), [](const auto &profile) {
                                  return profile.samples.empty();
                              }),
                              window.profiles.end());
    }

    // 阶段四：两级 Python 策略的候选调度（物理级异步 sidecar，不阻塞解析）。
    //   1) perf-map 缺失 PID；
    //   2) 有 perf 样本但 Python 语义覆盖 <70% 的 PID（解释器/native 帧占比过高）；
    //   3) 首窗预检：process Session 对目标逐个探测（身份/-X perf/真实 map），
    //      host Session 按 CPU ticks 增量排序只取最热实例。
    if (pythonFallbackEnabled && cpuRequested)
    {
        drop::schedule_python_fallback(
            cfg.sessionSID,
            build_python_candidates(cfg, processed.diagnostics.runtimeReport, window.samples,
                                    processed.diagnostics.runtimeReport.python.detected));
    }
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
    // 阶段三：空字符串 = 无物理信号（纯 python/db 请求），不启用任何信号。
    // 旧调用方（未设置 signals）由 BuildSamplerConfig 回退四类默认。
    if (all.empty())
        return false;
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
        for (const auto &entry : window.signalStatuses)
            signalTypes.insert(entry.first);
    }

    std::string body = "{";
    body += "\"session_sid\":\"" + json_escape(cfg.sessionSID) + "\",";
    body += "\"batch_id\":\"" + json_escape(batchID) + "\",";
    body += "\"target_ip\":\"" + json_escape(cfg.targetIP) + "\",";
    // 阶段一：协议 v3。batch 层 sample_count 废弃写 0；分信号计数
    // signal_counts、collector_generation、batch_sequence、content_sha256
    // 是服务端幂等/冲突校验与统计的新事实来源。
    // 阶段三：协议 v4。窗口新增 signal_statuses（每 signal 采集状态）、
    // physical/effective_sample_rate_hz、identity_unavailable_count；
    // histogram 携带完整进程身份（pid/process_start_ms/exe/comm）。
    body += "\"schema_version\":4,";
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
        // 阶段三：身份不完整被丢弃的样本数（process Session 无法归属时记录）。
        body += "\"identity_unavailable_count\":" + std::to_string(window.identityUnavailableCount) + ",";
        // 阶段三：物理/生效采样率（v4）。低频 Session 降采样后 effective < physical。
        body += "\"physical_sample_rate_hz\":" + std::to_string(window.physicalSampleRateHz) + ",";
        body += "\"effective_sample_rate_hz\":" + std::to_string(window.effectiveSampleRateHz) + ",";
        // 阶段三：每 signal 采集状态（collected/target_idle/no_events/
        // unavailable/failed + reason + lost events）。零计数窗口也登记，
        // 使 coverage 能区分 idle/no-events、backend unavailable 和真实 gap。
        body += "\"signal_statuses\":{";
        {
            size_t statusIndex = 0;
            for (const auto &entry : window.signalStatuses)
            {
                if (statusIndex++)
                    body += ",";
                body += "\"" + json_escape(entry.first) + "\":{\"status\":\"" +
                        signal_status_name(entry.second.status) + "\",\"reason\":\"" +
                        json_escape(entry.second.reason) + "\",\"lost_events\":" +
                        std::to_string(entry.second.lostEvents) + "}";
            }
        }
        body += "},";
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
            // 阶段三：histogram 完整进程身份（strict CO-RE 按 TGID 归属；
            // degraded 无法安全归属时 pid=0 且 unavailable）。
            body += "\"process_start_ms\":" + std::to_string(hist.processStartMs) + ",";
            body += "\"exe\":\"" + json_escape(hist.exe) + "\",";
            body += "\"comm\":\"" + json_escape(hist.comm) + "\",";
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
    window.physicalSampleRateHz = cfg.sampleRateHz;
    window.effectiveSampleRateHz = cfg.sampleRateHz;
    int64_t captureEndMs = window.startMs + static_cast<int64_t>(std::max(1, cfg.aggregationWindowSec)) * 1000;
    // 回收上一轮超预算被搁置的 io/sched future（它们已完成后台任务则清理）
    reap_abandoned_hist_futures();
    // 阶段三：signals 为空字符串 = 该 Session 未请求任何 CPU/IO/sched 物理信号
    //（纯 python_rss/python_memory/db_snapshot），不启动 CPU 采集。旧路径
    //（signals 未设置时）由 BuildSamplerConfig 回退四类默认，这里不再兜底
    // env 默认，避免纯 python 请求错误启动 CPU。
    std::string signals = cfg.signals;
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
            // 阶段二：结构化物理诊断随窗口一起 fan-out，供 filter_shared_window
            // 按 Session 重算 symbol_refs。
            window.physicalDiagnostics = perfWindow.physicalDiagnostics;
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
        // 阶段三：CPU 未请求（纯 python/db 信号）不是失败——窗口仍承载
        // RSS/Memray/db sidecar 数据，状态由 fan-out 按信号登记。
        if (signal_enabled(signals, "cpu"))
        {
            window.backendStatus = "failed";
            window.backendReason = "CPU backend not enabled";
        }
        else
        {
            window.backendStatus = "ok";
            window.backendReason = "CPU not requested by any Session signal";
        }
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
                             "-F", std::to_string(cfg.sampleRateHz), "-T"});
    // 阶段四：FP（默认）或 DWARF（--call-graph dwarf,8192）统一入口。
    for (auto &arg : call_graph_args(unwind_mode_from_env()))
        args.push_back(std::move(arg));
    args.insert(args.end(), {"--timestamp-boundary", "--switch-output=2s", "--timestamp-filename",
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

std::vector<PerfSegment> RollingPerfRecorder::Drain(const ContinuousSamplerConfig &cfg, bool final)
{
    const auto files = rolling_perf_files(directory, final);
    std::vector<PerfSegment> segments;
    for (const auto &path : files)
    {
        if (consumed.count(path) > 0)
            continue;
        PerfSegment segment;
        segment.path = path;
        segment.sourceBackend = "perf_rolling";
        segment.collectorGeneration = cfg.collectorGeneration;
        segment.targetFingerprint = cfg.targetFingerprint;
        segment.wallStartMs = wallStartMs;
        segment.monotonicStartMs = monotonicStartMs;
        segments.push_back(std::move(segment));
    }
    return segments;
}

// 处理成功后确认删除该不可变段并记入已消费集合。
void RollingPerfRecorder::Confirm(const std::string &path)
{
    if (path.empty())
        return;
    consumed.insert(path);
    ::unlink(path.c_str());
}

// 最终失败（重试 3 次仍失败）后放弃：删除该段并记入已消费集合，形成真实
// coverage gap（时间缺失，不伪造成功）。
void RollingPerfRecorder::Abandon(const std::string &path)
{
    if (path.empty())
        return;
    consumed.insert(path);
    ::unlink(path.c_str());
}

size_t RollingPerfRecorder::PendingSegmentCount() const
{
    return rolling_perf_files(directory, false).size();
}

uint64_t RollingPerfRecorder::PendingSegmentBytes() const
{
    const auto files = rolling_perf_files(directory, false);
    uint64_t total = 0;
    for (const auto &path : files)
    {
        struct stat st = {};
        if (::stat(path.c_str(), &st) == 0)
            total = add_count(total, static_cast<uint64_t>(st.st_size));
    }
    return total;
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
    // host 查询可以在服务端聚合逐 TGID 直方图；同时生成 pid=0 整机副本会让
    // 无过滤查询把同一事件计算两次。只有确实没有事件时才保留 pid=0 状态项。
    if (cfg.scope != "process" && pids.empty())
        pids.insert(0);
    for (uint32_t pid : pids)
        for (uint32_t signal = 1; signal <= 3; ++signal)
        {
            HistogramPayload hist;
            hist.pid = static_cast<int>(pid);
            // 阶段三：histogram 完整进程身份（strict CO-RE 按 TGID 归属）。
            // 从物理配置的 targetProcesses 补齐 start/exe/comm；host scope
            // 的 pid=0 直方图（整机）身份留空。
            if (pid > 0)
            {
                hist.processStartMs = configured_process_start_ms(cfg, static_cast<int>(pid));
                hist.exe = configured_process_exe(cfg, static_cast<int>(pid));
                for (const auto &target : cfg.targetProcesses)
                    if (target.pid == static_cast<int>(pid))
                    {
                        hist.comm = target.comm;
                        break;
                    }
            }
            hist.signalType = signal == 1 ? "io_latency" : signal == 2 ? "io_syscall_latency" : "sched_latency";
            hist.backend = "libbpf-co-re";
            hist.unit = "us";
            std::map<uint32_t, HistogramBucket> buckets;
            for (const auto &sample : samples)
            {
                // pid=0 聚合所有 TGID 的样本；否则只取本 TGID。
                if (sample.signal != signal) continue;
                if (pid > 0 && sample.tgid != pid) continue;
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
        // 非共享采样器（尤其 DBSnapshotSampler）同样写 v4，必须经过统一投影器
        // 生成 signal_statuses；否则 API 会收到“有数据但无状态”的非法 v4。
        SessionContract contract;
        contract.sid = cfg.sessionSID;
        contract.scope = cfg.scope;
        contract.targets = cfg.targetProcesses;
        contract.signals = cfg.requestedSignals;
        contract.requestedSampleRateHz = cfg.sampleRateHz;
        contract.aggregationWindowSec = cfg.aggregationWindowSec;
        contract.allowDegraded = cfg.allowDegraded;
        window = SessionFanoutProjector().Project(window, contract, cfg.sampleRateHz, true);
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

// A Memray .ready file is a physical capture shared by every Session that
// requested python_memory.  Keep it claimed until every projected copy has
// reached that Session's durable spool; acknowledging it for the first
// Session would make a crash lose the remaining deliveries.
std::mutex g_sharedProfileMutex;
std::map<std::string, std::set<std::string>> g_sharedProfileDeliveries;

static void register_shared_profile_deliveries(const WindowPayload &window,
                                               const std::string &sessionSID)
{
    std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
    for (const auto &profile : window.profiles)
        if (!profile.readyPath.empty())
            g_sharedProfileDeliveries[profile.readyPath].insert(sessionSID);
}

static void acknowledge_shared_profile_deliveries(const std::vector<WindowPayload> &windows,
                                                   const std::string &sessionSID)
{
    std::set<std::string> ready;
    {
        std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
        for (const auto &window : windows)
            for (const auto &profile : window.profiles)
            {
                if (profile.readyPath.empty())
                    continue;
                auto delivery = g_sharedProfileDeliveries.find(profile.readyPath);
                if (delivery == g_sharedProfileDeliveries.end())
                    continue;
                delivery->second.erase(sessionSID);
                if (delivery->second.empty())
                {
                    ready.insert(delivery->first);
                    g_sharedProfileDeliveries.erase(delivery);
                }
            }
    }
    for (const auto &path : ready)
        if (!drop::acknowledge_memray_profile(path))
            std::cout << "[native-cp] failed to mark shared Memray profile done: " << path << std::endl;
}

static void release_shared_profile_deliveries()
{
    std::vector<std::string> pending;
    {
        std::lock_guard<std::mutex> lock(g_sharedProfileMutex);
        for (const auto &delivery : g_sharedProfileDeliveries)
            pending.push_back(delivery.first);
        g_sharedProfileDeliveries.clear();
    }
    for (const auto &path : pending)
        drop::release_memray_profile(path);
}

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
    physical.requestedSignals = unionRequested;
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

// 阶段三：把 ContinuousSamplerConfig 转成 SessionContract（纯逻辑投影合同）。
static drop::SessionContract session_contract_from_config(const ContinuousSamplerConfig &session)
{
    drop::SessionContract contract;
    contract.sid = session.sessionSID;
    contract.scope = session.scope;
    contract.targets = session.targetProcesses;
    contract.signals = session.requestedSignals;
    contract.requestedSampleRateHz = session.sampleRateHz;
    contract.aggregationWindowSec = session.aggregationWindowSec;
    contract.allowDegraded = session.allowDegraded;
    return contract;
}

// 阶段三：统一 Session 分流入口。所有共享采集循环（strict/degraded）都通过
// SessionFanoutProjector 投影物理窗口，不再分别手写过滤规则。
static WindowPayload filter_shared_window(const WindowPayload &source,
                                          const ContinuousSamplerConfig &session,
                                          bool histogramAttributionSafe)
{
    static const drop::SessionFanoutProjector projector;
    return projector.Project(source, session_contract_from_config(session),
                             source.physicalSampleRateHz > 0 ? source.physicalSampleRateHz
                                                             : session.sampleRateHz,
                             histogramAttributionSafe);
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
        // 阶段三：合并时保留完整进程身份（pid/start/exe/comm）。同一
        // (signal, pid) 的 slice 合并；pid=0 的整机直方图身份留空。
        if (part.pid > 0 && merged.pid == 0)
        {
            merged.pid = part.pid;
            merged.processStartMs = part.processStartMs;
            merged.exe = part.exe;
            merged.comm = part.comm;
        }
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

static std::shared_ptr<const PhysicalDiagnostics> merge_physical_diagnostics(
    const std::vector<WindowPayload> &slices)
{
    auto merged = std::make_shared<PhysicalDiagnostics>();
    bool found = false;
    std::set<std::string> buildIDs;
    std::set<std::string> buildEntries;
    std::map<std::string, drop::PythonFallbackResult> python;
    std::map<std::string, drop::MemrayProfileResult> memray;

    auto mergePids = [](std::vector<int> *target, const std::vector<int> &source) {
        for (int pid : source)
            if (std::find(target->begin(), target->end(), pid) == target->end())
                target->push_back(pid);
    };
    auto mergeRuntimeInfo = [&](drop::RuntimeMapInfo *target, const drop::RuntimeMapInfo &source) {
        target->detected = target->detected || source.detected;
        mergePids(&target->readyPids, source.readyPids);
        mergePids(&target->missingPids, source.missingPids);
        target->ready = target->detected && !target->readyPids.empty() && target->missingPids.empty();
        if (!source.reason.empty()) target->reason = source.reason;
        if (!source.requiredFlag.empty()) target->requiredFlag = source.requiredFlag;
    };
    auto mergeGoItems = [](std::vector<drop::GoSymbolItem> *target,
                           const std::vector<drop::GoSymbolItem> &source) {
        for (const auto &item : source)
        {
            const auto same = [&](const auto &existing) {
                return existing.buildId == item.buildId && existing.dsoPath == item.dsoPath;
            };
            if (std::none_of(target->begin(), target->end(), same))
                target->push_back(item);
        }
    };

    for (const auto &slice : slices)
    {
        if (!slice.physicalDiagnostics)
            continue;
        found = true;
        const auto &source = *slice.physicalDiagnostics;
        std::cout << "[native-cp][dbg] merge slice unwind=[" << source.unwindMode << "]"
                  << " totalWeight=" << source.totalFrameWeight << std::endl;
        for (const auto &id : source.buildIds)
            if (buildIDs.insert(id).second) merged->buildIds.push_back(id);
        for (const auto &entry : source.buildIdEntries)
        {
            const std::string key = entry.buildId + "|" + entry.dsoPath;
            if (buildEntries.insert(key).second) merged->buildIdEntries.push_back(entry);
        }
        if (!source.kallsymsSha256.empty()) merged->kallsymsSha256 = source.kallsymsSha256;
        // 阶段四：栈回溯模式与 build-id 预热报告必须随合并保留，否则
        // persist_shared_aggregate 重建 symbol_refs 时会退化成默认 fp。
        if (merged->unwindMode.empty())
            merged->unwindMode = source.unwindMode;
        std::map<std::string, bool> warmSeen;
        for (const auto &warm : merged->buildIdWarmReport)
            warmSeen[warm.buildId + "|" + warm.dsoPath] = true;
        for (const auto &warm : source.buildIdWarmReport)
            if (!warmSeen[warm.buildId + "|" + warm.dsoPath])
                merged->buildIdWarmReport.push_back(warm);
        mergeRuntimeInfo(&merged->runtimeReport.java, source.runtimeReport.java);
        mergeRuntimeInfo(&merged->runtimeReport.node, source.runtimeReport.node);
        mergeRuntimeInfo(&merged->runtimeReport.python, source.runtimeReport.python);
        merged->runtimeReport.skippedRefresh += source.runtimeReport.skippedRefresh;
        for (const auto &entry : source.runtimeReport.sampledPids)
            merged->runtimeReport.sampledPids[entry.first] += entry.second;
        mergeGoItems(&merged->goReport.ready, source.goReport.ready);
        mergeGoItems(&merged->goReport.pending, source.goReport.pending);
        mergeGoItems(&merged->goReport.failed, source.goReport.failed);
        if (!source.goReport.disabled.empty())
            merged->goReport.disabled = source.goReport.disabled;
        for (const auto &item : source.pythonFallback)
            python[std::to_string(item.pid) + "|" + std::to_string(item.startMs) + "|" + item.exe] = item;
        for (const auto &item : source.memrayResults)
            memray[item.profileID + "|" + std::to_string(item.pid) + "|" +
                   std::to_string(item.processStartMs) + "|" + item.exe] = item;
        merged->pythonFallbackLimitedCount += source.pythonFallbackLimitedCount;
        merged->runtimeEnrichmentDisabled = merged->runtimeEnrichmentDisabled || source.runtimeEnrichmentDisabled;
        if (!source.enrichmentDisabledReason.empty())
            merged->enrichmentDisabledReason = source.enrichmentDisabledReason;
        merged->enrichmentApplied = merged->enrichmentApplied && source.enrichmentApplied;
    }
    if (!found)
        return {};
    std::cout << "[native-cp][dbg] merge_physical_diagnostics merged unwind=["
              << merged->unwindMode << "]" << std::endl;
    merged->runtimeReport.status = drop::runtime_aggregate_status(merged->runtimeReport);
    for (auto &entry : python) merged->pythonFallback.push_back(std::move(entry.second));
    for (auto &entry : memray) merged->memrayResults.push_back(std::move(entry.second));
    return merged;
}

static WindowPayload merge_shared_slices(const std::vector<WindowPayload> &slices)
{
    WindowPayload merged;
    if (slices.empty())
        return merged;
    merged.startMs = slices.front().startMs;
    merged.endMs = slices.front().endMs;
    using HistogramIdentity = std::tuple<std::string, int, int64_t, std::string>;
    std::map<HistogramIdentity, std::vector<HistogramPayload>> histograms;
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
        merged.dbSnapshots.insert(merged.dbSnapshots.end(), slice.dbSnapshots.begin(), slice.dbSnapshots.end());
        merged.rssTruncated += slice.rssTruncated;
        merged.identityUnavailableCount = add_count(merged.identityUnavailableCount, slice.identityUnavailableCount);
        if (!slice.collectorGeneration.empty())
            merged.collectorGeneration = slice.collectorGeneration;
        if (slice.physicalSampleRateHz > 0)
            merged.physicalSampleRateHz = slice.physicalSampleRateHz;
        if (slice.effectiveSampleRateHz > 0)
            merged.effectiveSampleRateHz = slice.effectiveSampleRateHz;
        // 阶段三：每 signal 状态合并——任一 slice 有数据则 collected；否则
        // 取最严重状态（failed > unavailable > no_events > target_idle）。
        for (const auto &entry : slice.signalStatuses)
        {
            auto &target = merged.signalStatuses[entry.first];
            const uint64_t mergedLost = add_count(target.lostEvents, entry.second.lostEvents);
            const auto severity = [](SignalCollectionStatus status) {
                switch (status)
                {
                case SignalCollectionStatus::Failed: return 4;
                case SignalCollectionStatus::Unavailable: return 3;
                case SignalCollectionStatus::NoEvents: return 2;
                case SignalCollectionStatus::TargetIdle: return 1;
                default: return 0;
                }
            };
            if (target.status == SignalCollectionStatus::Collected)
            {
                target.lostEvents = mergedLost;
                continue;
            }
            if (entry.second.status == SignalCollectionStatus::Collected ||
                severity(entry.second.status) > severity(target.status))
            {
                target = entry.second;
            }
            target.lostEvents = mergedLost;
        }
        // 完整身份参与分组，防止 PID 复用跨 slice 合并。
        for (const auto &histogram : slice.histograms)
            histograms[{histogram.signalType, histogram.pid, histogram.processStartMs,
                        histogram.exe}].push_back(histogram);
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
        merged.histograms.push_back(merge_histograms(entry.second, std::get<0>(entry.first)));
    merged.physicalDiagnostics = merge_physical_diagnostics(slices);
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
    // 阶段三：不再删除"无 payload"的窗口——零计数窗口携带每 signal 状态
    // （target_idle/no_events/unavailable），是 coverage 区分目标空闲与真实
    // 采集缺口的事实来源。只丢弃时间无效的窗口。
    session->slices.erase(std::remove_if(session->slices.begin(), session->slices.end(), [](const auto &slice) {
        return slice.endMs <= slice.startMs;
    }), session->slices.end());
    if (!session || session->slices.empty())
        return true;
    std::vector<WindowPayload> aggregates = merge_shared_slices_preserving_gaps(session->slices);
    // 阶段一：为合并后的窗口补齐稳定 window_id / 内容摘要（保证同逻辑窗口
    // 重传一致；内容摘要不参与 ID，冲突时仍是同一 ID）。
    for (auto &window : aggregates)
    {
        if (window.physicalDiagnostics)
            window.symbolRefsJson = build_session_symbol_refs(*window.physicalDiagnostics,
                                                              window.samples, session->config);
        if (window.windowID.empty())
            window.windowID = make_window_id(session->config, window);
        if (window.contentSHA256.empty())
            window.contentSHA256 = window_content_digest(window);
    }
    const size_t previousBatchSize = session->batch.size();
    session->batch.insert(session->batch.end(), aggregates.begin(), aggregates.end());
    bool createdBatch = false;
    if (session->batchID.empty())
    {
        session->config.batchSequence = ++session->batchSequence;
        session->batchID = make_batch_id(session->config);
        createdBatch = true;
    }
    std::string body = build_batch_json(session->config, session->batchID, session->batch);
    if (!persist_batch(session->config, session->batchID, body))
    {
        session->batch.resize(previousBatchSize);
        if (createdBatch)
            session->batchID.clear();
        return false;
    }
    session->slices.clear();
    acknowledge_shared_profile_deliveries(aggregates, session->config.sessionSID);

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

// 阶段二：strict 运行结果分级，驱动 Loop 的状态转换（灰度）。
enum class StrictRunResult
{
    Succeeded,       // strict 正常运行至 Stop（保持 strict）
    Unavailable,     // strict 引擎不可用：允许 degraded 时降级，否则 failed
    Backlogged,      // strict 滚动段队列积压：允许 degraded 时降级，否则 failed
    ProcessorFatal,  // 公共 processor 持续致命错误：不尝试 degraded，直接 failed
};

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

    StrictRunResult RunStrict(std::vector<SharedSessionAccumulator> &accumulators)
    {
        if (!CoreContinuousSamplerAvailable())
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            degradationReason = "strict persistent CO-RE object is unavailable";
            return StrictRunResult::Unavailable;
        }
        CoreEbpfCollector core;
        RollingPerfRecorder recorder;
        std::string error;
        // 阶段三：host/process 混合共享——存在 host Session 时启用整机
        // wildcard，同时保留 process targets 的 TID 映射，使 process Session
        // 的 sched histogram 仍能归属到 TGID。
        core.SetHostWildcard(physical.scope == "host" && !physical.targetProcesses.empty());
        // 阶段三：纯 python/db 请求（signals 为空）不启动 perf/CO-RE 物理
        // 采集——strict 引擎直接不可用，走 degraded 路径只做 sidecar。
        const bool anyPhysicalRequested = !physical.signals.empty();
        if (anyPhysicalRequested &&
            (!core.Start(physical.targetProcesses, &error) || !recorder.Start(physical, &error)))
        {
            std::cout << "[native-cp] strict engine unavailable: " << error << std::endl;
            {
                std::lock_guard<std::mutex> lock(statusMutex);
                degradationReason = "strict persistent perf/CO-RE engine unavailable: " + error;
            }
            core.Stop();
            recorder.Stop();
            return StrictRunResult::Unavailable;
        }
        if (!anyPhysicalRequested)
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            degradationReason = "no CPU/IO/sched signal requested by any Session; strict engine idle";
            return StrictRunResult::Unavailable;
        }
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            degradationReason = core.DegradationReason();
        }
        std::cout << "[native-cp] strict engine started backend=perf_rolling,libbpf-co-re"
                  << " unwind=" << unwind_mode_from_env() << std::endl;

        std::vector<CoreHistogramSample> pendingCoreSamples;
        uint64_t pendingCoreLost = 0;
        ContinuousSegmentProcessor processor;
        const int segmentMaxRetries = std::max(1, env_positive_int("DROP_STRICT_SEGMENT_MAX_RETRIES", 3));
        const size_t segmentQueueLimit = static_cast<size_t>(
            std::max(1, env_positive_int("DROP_STRICT_SEGMENT_QUEUE_LIMIT", 30)));
        const uint64_t segmentQueueBytesLimit = static_cast<uint64_t>(
            std::max<int64_t>(1, env_positive_int("DROP_STRICT_SEGMENT_QUEUE_BYTES",
                                                  512 * 1024 * 1024)));
        // 阶段三：物理级 sidecar 按"所有活动 Session 请求信号的并集"决定
        // 是否启动（不选就不采、不存）：
        //   - py-spy 是 CPU fallback：任一 Session 请求 cpu_profile 才启用。
        //   - RSS/Memray：任一 Session 请求 python_rss/python_memory 才启用。
        //   - fan-out 再按各自 Session signals 严格过滤。
        const bool anyCpuRequested = std::any_of(sessions.begin(), sessions.end(), [](const auto &session) {
            return logical_signal_requested(session.requestedSignals, "cpu_profile");
        });
        const bool anyRssRequested = std::any_of(sessions.begin(), sessions.end(), [](const auto &session) {
            return logical_signal_requested(session.requestedSignals, "python_rss");
        });
        const bool anyMemrayRequested = std::any_of(sessions.begin(), sessions.end(), [](const auto &session) {
            return logical_signal_requested(session.requestedSignals, "python_memory");
        });
        const bool pythonFallbackEnabled = anyCpuRequested &&
                                           env_enabled_default("DROP_CONTINUOUS_PYSPY_FALLBACK", true) &&
                                           env_enabled_default("DROP_NATIVE_CP_PYTHON_FALLBACK_ENABLED", true);
        const int pythonRateHz = env_positive_int("DROP_NATIVE_CP_PYTHON_RATE_HZ", 19);
        // 阶段四：host Session 默认只 attach 最热 2 个 Python 实例；process
        // Session 精确目标最多 4 个。
        const int pythonMaxProcesses =
            physical.scope == "process"
                ? env_positive_int("DROP_NATIVE_CP_PYTHON_MAX_PROCESSES", 4)
                : env_positive_int("DROP_NATIVE_CP_PYTHON_HOST_MAX_PROCESSES", 2);
        const int pythonSidecarPollMs = std::max(0, env_positive_int("DROP_STRICT_PYTHON_SIDECAR_POLL_MS", 350));
        // 阶段四：栈回溯模式与逐语言能力开关。
        const std::string unwindMode = unwind_mode_from_env();
        RuntimeCapabilitySet caps;
        caps.pythonFallback = pythonFallbackEnabled;
        caps.pythonRss = anyRssRequested &&
                         env_enabled_default("DROP_NATIVE_CP_PYTHON_RSS_ENABLED", true);
        caps.memray = anyMemrayRequested &&
                      env_enabled_default("DROP_NATIVE_CP_MEMRAY_INGEST_ENABLED", true);
        caps.goReSym = env_enabled_default("DROP_CONTINUOUS_GORESYM", true);
        caps.goSymbols = caps.goReSym;
        caps.javaPerfMap = env_enabled_default("DROP_CONTINUOUS_JAVA_PERFMAP", true);
        caps.nodePerfMap = env_enabled_default("DROP_CONTINUOUS_NODE_PERFMAP", true);
        caps.pythonPerf = env_enabled_default("DROP_CONTINUOUS_PYTHON_PERF", true);
        caps.unwindMode = unwindMode;
        bool sidecarInFlight = false;
        drop::PythonFallbackCapture sidecarCapture;
        std::vector<drop::PythonFallbackResult> pendingSidecarResults;
        size_t sidecarLimitedCount = 0;

        size_t processedSegments = 0;
        int consecutiveProcessorFailures = 0;
        bool strictBacklogged = false;
        int64_t lastSidecarCollectMs = 0;
        const int64_t sidecarIntervalMs = static_cast<int64_t>(std::max(1, physical.aggregationWindowSec)) * 1000;

        // 物理级内存 sidecar：RSS / Memray 用观测/自身采集时间生成独立窗口，
        // 不伪装成 perf segment 的时间。Memray 在全部 Session fan-out 持久化
        // 成功后才被 ACK（见 persist_shared_aggregate → acknowledge_batch_profiles）。
        auto collectMemorySidecars = [&](WindowPayload *window, int64_t nowM) {
            if (!window)
                return;
            if (caps.pythonRss && nowM - lastSidecarCollectMs >= sidecarIntervalMs)
            {
                size_t truncated = 0;
                auto rss = drop::collect_python_rss(
                    static_cast<size_t>(env_positive_int("DROP_NATIVE_CP_PYTHON_RSS_MAX_PROCESSES", 128)), &truncated);
                window->rssTruncated = truncated;
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
                    window->metrics.push_back(std::move(metric));
                }
            }
            if (caps.memray && nowM - lastSidecarCollectMs >= sidecarIntervalMs)
            {
                auto memrayResults = drop::collect_memray_profiles();
                if (!memrayResults.empty())
                {
                    auto diagnostics = window->physicalDiagnostics
                        ? std::make_shared<PhysicalDiagnostics>(*window->physicalDiagnostics)
                        : std::make_shared<PhysicalDiagnostics>();
                    diagnostics->memrayResults.insert(diagnostics->memrayResults.end(),
                                                      memrayResults.begin(), memrayResults.end());
                    window->physicalDiagnostics = diagnostics;
                }
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
                        // 阶段三：Memray sample 携带完整进程身份，process
                        // Session fan-out 才能按实例精确归属。
                        sample.processStartMs = result.processStartMs;
                        sample.exe = result.exe;
                        sample.backend = "memray";
                        sample.runtime = "python";
                        sample.count = clamp_count(raw.count);
                        profile.samples.push_back(std::move(sample));
                    }
                    window->profiles.push_back(std::move(profile));
                }
            }
            if (nowM - lastSidecarCollectMs >= sidecarIntervalMs)
                lastSidecarCollectMs = nowM;
        };

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

            // 阶段二：积压保护——已关闭滚动文件组成的磁盘有界队列（默认
            // 30 段 / 512 MiB），任一达到即停止认定 strict 并停止产生新段。
            const size_t pendingCount = recorder.PendingSegmentCount();
            const uint64_t pendingBytes = recorder.PendingSegmentBytes();
            if (pendingCount > segmentQueueLimit || pendingBytes > segmentQueueBytesLimit)
            {
                std::cout << "[native-cp] strict segment backlog count=" << pendingCount
                          << " bytes=" << pendingBytes << std::endl;
                strictBacklogged = true;
                {
                    std::lock_guard<std::mutex> lock(statusMutex);
                    degradationReason = "strict rolling segment queue exceeded (" +
                                        std::to_string(pendingCount) + " segments / " +
                                        std::to_string(pendingBytes / (1024 * 1024)) +
                                        " MiB); no longer certifying strict";
                }
                break;
            }

            if (!spool_has_collection_capacity(sessions.front()))
            {
                interruptible_wait(running, 500);
                continue;
            }

            // 物理级 Python sidecar：上一批已发现候选异步覆盖 capture 区间。
            if (pythonFallbackEnabled)
            {
                if (!sidecarInFlight && pendingSidecarResults.empty())
                {
                    sidecarCapture = drop::start_python_fallback_capture(
                        physical.sessionSID, std::max(1, physical.aggregationWindowSec),
                        pythonRateHz, pythonMaxProcesses);
                    sidecarInFlight = true;
                }
                if (sidecarInFlight)
                {
                    auto ready = sidecarCapture.Poll(pythonSidecarPollMs);
                    if (!ready.empty())
                    {
                        pendingSidecarResults = std::move(ready);
                        sidecarLimitedCount = sidecarCapture.LimitedCount();
                    }
                    if (!sidecarCapture.AnyPending())
                        sidecarInFlight = false;
                }
            }

            auto segments = recorder.Drain(physical, false);
            if (segments.empty())
            {
                interruptible_wait(running, 200);
                continue;
            }
            uint64_t lost = 0;
            auto coreSamples = core.Drain(&lost);
            std::vector<WindowPayload> iterationWindows;
            for (auto &segment : segments)
            {
                SegmentProcessResult processed;
                int attempts = 0;
                bool segmentOk = false;
                while (attempts < segmentMaxRetries)
                {
                    ++attempts;
                    processed = processor.Process(segment, physical, caps,
                                                  pendingSidecarResults, sidecarLimitedCount, {});
                    if (processed.success)
                    {
                        segmentOk = true;
                        break;
                    }
                    std::cout << "[native-cp] strict segment process failed (attempt "
                              << attempts << "/" << segmentMaxRetries << ") reason="
                              << processed.failureReason << std::endl;
                    interruptible_wait(running, 250);
                }
                if (!segmentOk)
                {
                    // 确定失败：记录诊断、形成真实 coverage gap（不伪造成功）。
                    consecutiveProcessorFailures++;
                    recorder.Abandon(segment.path);
                    std::cout << "[native-cp] strict segment abandoned (gap) path="
                              << segment.path << std::endl;
                    continue;
                }
                consecutiveProcessorFailures = 0;
                processedSegments++;
                recorder.Confirm(segment.path); // 仅处理成功后删除
                // py-spy sidecar 结果只应用于这一个 segment 的窗口（不跨段
                // 双计数）；下一轮 capture 完成后再产生新结果。
                if (!pendingSidecarResults.empty())
                {
                    pendingSidecarResults.clear();
                    sidecarLimitedCount = 0;
                }

                // 阶段二：strict readiness = 至少一个真实 segment 已被统一
                // processor 成功处理（保证 cutover 后不进入缺失多语言能力的
                // 伪 strict 状态）。
                if (!ready.load())
                {
                    strict.store(true);
                    failed.store(false);
                    ready.store(true);
                    std::cout << "[native-cp] strict engine ready after "
                              << processedSegments << " processed segment(s)" << std::endl;
                }

                // 阶段四修复：必须在把 windows move 进 iterationWindows 之前
                // 收集 py-spy 候选元数据——move 之后 processed.windows 的样本
                // 已被搬空，旧实现读到空 metadata 导致 py-spy 候选为空。
                if (pythonFallbackEnabled)
                {
                    std::vector<AggregatedSample> allSamples;
                    for (const auto &w : processed.windows)
                        allSamples.insert(allSamples.end(), w.samples.begin(), w.samples.end());
                    bool pythonDetected = false;
                    for (const auto &w : processed.windows)
                        if (w.physicalDiagnostics &&
                            w.physicalDiagnostics->runtimeReport.python.detected)
                        {
                            pythonDetected = true;
                            break;
                        }
                    drop::schedule_python_fallback(
                        physical.sessionSID,
                        build_python_candidates(physical, processed.diagnostics.runtimeReport,
                                                allSamples, pythonDetected));
                }

                for (auto &physicalWindow : processed.windows)
                {
                    physicalWindow.attemptedBackends.push_back("libbpf-co-re");
                    physicalWindow.selectedBackend = "perf_rolling+libbpf-co-re";
                    physicalWindow.backendStatus = "ok";
                    iterationWindows.push_back(std::move(physicalWindow));
                }
            }
            // 本迭代 CO-RE 直方图附加到首个物理窗口（与旧行为一致）。
            queue_core_histograms(&iterationWindows, physical, &pendingCoreSamples, &pendingCoreLost,
                                  std::move(coreSamples), lost);
            for (auto &physicalWindow : iterationWindows)
            {
                collectMemorySidecars(&physicalWindow, now_ms());
                // 阶段一：cutover watermark 过滤（新 generation 切点前不输出）。
                if (!WindowAllowed(physicalWindow))
                    continue;
                // Register every recipient before any Session can persist and ACK
                // the shared Memray file.
                for (auto &accumulator : accumulators)
                {
                    accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, true));
                    register_shared_profile_deliveries(accumulator.slices.back(),
                                                       accumulator.config.sessionSID);
                }
                for (auto &accumulator : accumulators)
                {
                    const int64_t coveredMs = accumulator.slices.back().endMs - accumulator.slices.front().startMs;
                    if (coveredMs >= static_cast<int64_t>(accumulator.config.aggregationWindowSec) * 1000 &&
                        !persist_shared_aggregate(&accumulator))
                    {
                        std::cout << "[native-cp] strict engine failed to persist sid="
                                  << accumulator.config.sessionSID << std::endl;
                        running = false;
                        break;
                    }
                }
            }
            interruptible_wait(running, 50);
        }
        recorder.Stop();
        // 最终 drain：处理所有已接收段（处理成功才删除）。
        auto finalSegments = recorder.Drain(physical, true);
        uint64_t lost = 0;
        auto finalCore = core.StopAndDrain(&lost);
        // 先把最终 CO-RE 直方图并入 pending（首窗附加）。
        {
            std::vector<WindowPayload> emptyWindows;
            queue_core_histograms(&emptyWindows, physical, &pendingCoreSamples, &pendingCoreLost,
                                  std::move(finalCore), lost);
        }
        for (auto &segment : finalSegments)
        {
            SegmentProcessResult processed = processor.Process(segment, physical, caps,
                                                               pendingSidecarResults, sidecarLimitedCount, {});
            if (processed.success)
            {
                recorder.Confirm(segment.path);
                // sidecar 结果只应用于首个成功处理的最终段（防跨段双计数）。
                if (!pendingSidecarResults.empty())
                {
                    pendingSidecarResults.clear();
                    sidecarLimitedCount = 0;
                }
                for (auto &physicalWindow : processed.windows)
                {
                    physicalWindow.attemptedBackends.push_back("libbpf-co-re");
                    physicalWindow.selectedBackend = "perf_rolling+libbpf-co-re";
                    physicalWindow.backendStatus = "ok";
                    std::vector<WindowPayload> tmp{physicalWindow};
                    queue_core_histograms(&tmp, physical, &pendingCoreSamples, &pendingCoreLost,
                                          std::vector<CoreHistogramSample>(), 0);
                    physicalWindow = std::move(tmp.front());
                    if (!WindowAllowed(physicalWindow))
                        continue;
                    for (auto &accumulator : accumulators)
                    {
                        accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, true));
                        register_shared_profile_deliveries(accumulator.slices.back(),
                                                           accumulator.config.sessionSID);
                    }
                }
            }
            else
            {
                recorder.Abandon(segment.path);
                std::cout << "[native-cp] strict final segment abandoned (gap) path="
                          << segment.path << std::endl;
            }
        }
        if (sidecarInFlight)
            sidecarCapture.Finish();
        for (auto &accumulator : accumulators)
            if (!finalize_shared_session(&accumulator))
                std::cout << "[native-cp] strict engine final flush failed sid="
                          << accumulator.config.sessionSID << std::endl;
        release_shared_profile_deliveries();
        DrainAllSessionSpools(true);

        if (strictBacklogged)
            return StrictRunResult::Backlogged;
        if (consecutiveProcessorFailures >= segmentMaxRetries)
        {
            std::lock_guard<std::mutex> lock(statusMutex);
            if (degradationReason.find("processor") == std::string::npos)
                degradationReason += degradationReason.empty()
                                         ? "unified segment processor persistent failure"
                                         : "; unified segment processor persistent failure";
            return StrictRunResult::ProcessorFatal;
        }
        return StrictRunResult::Succeeded;
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
        // 阶段二：strict 运行结果分级驱动状态转换。
        //   - Succeeded：保持 strict。
        //   - Unavailable / Backlogged 且全部 Session 允许 degraded：降级。
        //   - 任一 Session 禁止 degraded 或公共 processor 致命：进入 failed，
        //     按现有 reconcile 周期重试 strict，不暗中降级。
        while (running.load())
        {
            StrictRunResult result = RunStrict(accumulators);
            if (result == StrictRunResult::Succeeded)
                break;
            if (degradedFallbackAllowed &&
                (result == StrictRunResult::Unavailable || result == StrictRunResult::Backlogged))
                break; // 切换到低频 degraded 录制；切换期空白按真实 gap 展示
            strict.store(false);
            failed.store(true);
            {
                std::lock_guard<std::mutex> lock(statusMutex);
                if (result != StrictRunResult::ProcessorFatal &&
                    degradationReason.find("degraded fallback is not allowed") == std::string::npos)
                    degradationReason += degradationReason.empty()
                                             ? "strict collector is unavailable and degraded fallback is not allowed"
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
            // 阶段三：degraded 共享路径也按 Session 信号并集采集 RSS/Memray
            // sidecar（与 strict 路径的 collectMemorySidecars 一致）。
            {
                const bool anyRssRequested = std::any_of(sessions.begin(), sessions.end(), [](const auto &session) {
                    return logical_signal_requested(session.requestedSignals, "python_rss");
                });
                const bool anyMemrayRequested = std::any_of(sessions.begin(), sessions.end(), [](const auto &session) {
                    return logical_signal_requested(session.requestedSignals, "python_memory");
                });
                if (anyRssRequested && env_enabled_default("DROP_NATIVE_CP_PYTHON_RSS_ENABLED", true))
                {
                    size_t truncated = 0;
                    auto rss = drop::collect_python_rss(
                        static_cast<size_t>(env_positive_int("DROP_NATIVE_CP_PYTHON_RSS_MAX_PROCESSES", 128)), &truncated);
                    physicalWindow.rssTruncated = truncated;
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
                        physicalWindow.metrics.push_back(std::move(metric));
                    }
                }
                if (anyMemrayRequested && env_enabled_default("DROP_NATIVE_CP_MEMRAY_INGEST_ENABLED", true))
                {
                    auto memrayResults = drop::collect_memray_profiles();
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
                            sample.processStartMs = result.processStartMs;
                            sample.exe = result.exe;
                            sample.backend = "memray";
                            sample.runtime = "python";
                            sample.count = clamp_count(raw.count);
                            profile.samples.push_back(std::move(sample));
                        }
                        physicalWindow.profiles.push_back(std::move(profile));
                    }
                }
            }
            // 阶段一：cutover watermark 过滤（新 generation 切点前不输出；
            // 旧 generation 切点后不输出）。
            if (!WindowAllowed(physicalWindow))
                continue;
            // Complete fan-out registration before the first Session is allowed
            // to persist and ACK shared sidecar files.
            for (auto &accumulator : accumulators)
            {
                accumulator.slices.push_back(filter_shared_window(physicalWindow, accumulator.config, histogramAttributionSafe));
                register_shared_profile_deliveries(accumulator.slices.back(),
                                                   accumulator.config.sessionSID);
            }
            for (auto &accumulator : accumulators)
            {
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
        // Any delivery still pending belongs to a failed Session spool.  Release
        // the claim so a later collector generation can ingest the .ready file.
        release_shared_profile_deliveries();
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
