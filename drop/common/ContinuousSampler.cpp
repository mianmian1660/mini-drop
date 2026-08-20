// ============================================================
// common/ContinuousSampler.cpp — perf_event-first continuous sampler
// ============================================================

#include "common/ContinuousSampler.h"
#include "common/BuildId.h"
#include "common/GoSymbolizer.h"
#include "common/PythonRuntimeProfiler.h"
#include "common/MemrayProfileIngest.h"
#include "common/RuntimeSymbolMap.h"
#include "common/Utils.h"

#include <algorithm>
#include <chrono>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cctype>
#include <ctime>
#include <fstream>
#include <future>
#include <iostream>
#include <map>
#include <mutex>
#include <regex>
#include <set>
#include <sstream>
#include <dirent.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <unistd.h>
#include <unordered_map>

namespace drop
{

namespace
{
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

static std::string make_batch_id(const ContinuousSamplerConfig &cfg, int64_t windowStartMs)
{
    std::ostringstream id;
    id << "cpb-" << std::hex << stable_string_hash(cfg.sessionSID)
       << std::dec << '-' << windowStartMs << '-' << ::getpid();
    return id.str();
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
        else if (name.size() > suffix.size() && name.compare(name.size() - suffix.size(), suffix.size(), suffix) == 0)
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

static bool spool_has_collection_capacity(const ContinuousSamplerConfig &cfg)
{
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
    std::string comm;
    int pid = 0;
    std::string exe;
    std::string stackScope;
    std::string backend;
    std::string runtime;
    uint64_t count = 0;
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
};

struct WindowPayload
{
    int64_t startMs = 0;
    int64_t endMs = 0;
    std::vector<AggregatedSample> samples;
    std::vector<ProfilePayload> profiles;
    std::vector<HistogramPayload> histograms;
    std::vector<MetricPayload> metrics;
    size_t rssTruncated = 0;
    // Backend metadata (5-8: strict fallback strategy)
    std::string backendStatus;                   // "ok" | "degraded" | "failed"
    std::string backendReason;                   // human-readable reason
    std::vector<std::string> attemptedBackends;  // ["core","bpftrace","perf"]
    std::string selectedBackend;                 // "bpftrace" | "perf" | ""
    // runtime map 符号化诊断（Java/Node/Python JIT perf map），序列化后写入
    // 批次/窗口 JSON 的 symbol_refs，服务端据此推断 symbol_status。
    std::string symbolRefsJson;
};

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
    std::tm tm{};
    gmtime_r(&sec, &tm);
    char buf[32];
    strftime(buf, sizeof(buf), "%Y-%m-%dT%H:%M:%SZ", &tm);
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
    return std::string(buf);
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

static bool parse_sample_header(const std::string &line, std::string *comm, int *pid)
{
    std::string trimmed = drop::trim(line);
    if (trimmed.empty() || trimmed[0] == '#' || trimmed[0] == '\t')
        return false;
    if (trimmed.find(':') == std::string::npos || trimmed.find('[') == std::string::npos)
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
    return true;
}

static void add_sample(std::map<std::string, AggregatedSample> *out,
                       const std::string &comm,
                       int pid,
                       const std::vector<std::string> &rawStack,
                       const std::string &stackScope = "",
                       const std::string &backend = "")
{
    if (rawStack.empty())
        return;
    std::vector<std::string> stack = rawStack;
    std::reverse(stack.begin(), stack.end());
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
        sample.stackScope = stackScope;
        sample.backend = backend;
    }
    sample.count++;
}

static std::vector<AggregatedSample> parse_perf_script(const std::string &script)
{
    std::map<std::string, AggregatedSample> byKey;
    std::istringstream iss(script);
    std::string line;
    std::string currentComm;
    int currentPid = 0;
    std::vector<std::string> currentStack;
    auto flush = [&]() {
        add_sample(&byKey, currentComm, currentPid, currentStack);
        currentComm.clear();
        currentPid = 0;
        currentStack.clear();
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
        if (parse_sample_header(line, &comm, &pid))
        {
            flush();
            currentComm = comm;
            currentPid = pid;
            continue;
        }
        if (!currentComm.empty() && (line[0] == ' ' || line[0] == '\t'))
        {
            std::string frame = parse_frame_name(line, currentPid);
            if (!frame.empty())
                currentStack.push_back(frame);
        }
    }
    flush();
    std::vector<AggregatedSample> out;
    out.reserve(byKey.size());
    for (auto &kv : byKey)
        out.push_back(kv.second);
    return out;
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
    int64_t retryAfterMs; // 仅 TransientFail 有意义
};

static std::mutex g_buildIdAttemptMutex;
static std::map<std::string, BuildIdAttempt> g_buildIdAttempts;
static constexpr int64_t kBuildIdTransientRetryMs = 5 * 60 * 1000; // 5 分钟

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
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::TransientFail, nowMs + kBuildIdTransientRetryMs};
}

static void record_build_id_permanent_fail(const std::string &buildId)
{
    std::lock_guard<std::mutex> lock(g_buildIdAttemptMutex);
    g_buildIdAttempts[buildId] = {BuildIdAttemptState::PermanentFail, 0};
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
                                             const std::vector<AggregatedSample> &samples)
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
        pythonCapture = drop::start_python_fallback_capture(cfg.aggregationWindowSec, pythonRateHz, pythonMaxProcesses);
    // 云 VM 上硬件 cycles 计数器可能冻结（perf stat 读到 2^50 固定值），
    // perf 默认的 cycles 事件会采不到任何样本。默认改用软件事件 cpu-clock，
    // 可用 DROP_NATIVE_CP_PERF_EVENT 覆盖（Step 1 实测结论）。
    std::string perfEvent = "cpu-clock";
    if (const char *env = std::getenv("DROP_NATIVE_CP_PERF_EVENT"))
        if (*env)
            perfEvent = env;
    std::string recordOutput;
    int64_t tRecordStart = now_ms();
    int rc = drop::exec_capture({perf, "record", "--no-buildid-cache", "-q", "-a", "-e", perfEvent, "-F", std::to_string(cfg.sampleRateHz), "-g", "-o", dataPath, "--", "sleep", std::to_string(cfg.aggregationWindowSec)}, &recordOutput, 4096);
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
    rc = drop::exec_capture({perf, "script", "-i", dataPath}, &scriptOutput, 32 * 1024 * 1024);
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
        sample.backend = "perf";
        sample.runtime = sample_runtime(sample, goReport, &goBuildInfoCache);
        if (sample.runtime == "python")
            for (auto &frame : sample.stack)
                frame = sanitize_python_perf_frame(frame);
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
    drop::schedule_python_fallback(nextCandidates);

    if (env_enabled_default("DROP_NATIVE_CP_PYTHON_RSS_ENABLED", true))
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
    if (env_enabled_default("DROP_NATIVE_CP_MEMRAY_INGEST_ENABLED", true))
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
    window.symbolRefsJson = combined_symbol_refs_json(runtimeReport, goReport, pythonResults, pythonLimitedCount, memrayResults, window.samples);
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
        out << "profile:hz:" << cfg.sampleRateHz << "\n{\n  @samples[" << stackExpr << "] = count();\n}\n";
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
    if (!command_available("bpftrace"))
    {
        hist.unavailable = true;
        hist.reason = "bpftrace unavailable";
        return hist;
    }
    if (!sched && (!tracepoint_exists("block:block_rq_issue") || !tracepoint_exists("block:block_rq_complete")))
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
        if (sched)
        {
            out << "#define pid_t int\n"
                << "tracepoint:sched:sched_wakeup { @wake[args->pid] = nsecs; }\n"
                << "tracepoint:sched:sched_wakeup_new { @wake[args->pid] = nsecs; }\n"
                << "tracepoint:sched:sched_switch /@wake[args->next_pid]/ { $lat = (nsecs - @wake[args->next_pid]) / 1000; @lat = hist($lat); @events = count(); delete(@wake[args->next_pid]); }\n";
        }
        else
        {
            out << "#define dev_t unsigned int\n#define sector_t unsigned long\n"
                << "tracepoint:block:block_rq_issue { @rq_start[args->dev, args->sector] = nsecs; }\n"
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
    uint64_t sampleCount = 0;
    for (const auto &window : windows)
    {
        for (const auto &sample : window.samples)
            sampleCount = add_count(sampleCount, sample.count);
        for (const auto &profile : window.profiles)
            for (const auto &sample : profile.samples)
                sampleCount = add_count(sampleCount, sample.count);
        for (const auto &hist : window.histograms)
            sampleCount = add_count(sampleCount, hist.eventCount);
    }

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
    }

    std::string body = "{";
    body += "\"session_sid\":\"" + json_escape(cfg.sessionSID) + "\",";
    body += "\"batch_id\":\"" + json_escape(batchID) + "\",";
    body += "\"target_ip\":\"" + json_escape(cfg.targetIP) + "\",";
    body += "\"schema_version\":2,";
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
    body += "\"sample_count\":" + std::to_string(sampleCount) + ",";
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
        body += "{";
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
            body += "\"exe\":\"" + json_escape(sample.exe) + "\",";
            body += "\"runtime\":\"" + json_escape(sample.runtime) + "\",";
            body += "\"count\":" + std::to_string(sample.count) + ",";
            body += "\"stack_scope\":\"" + json_escape(sample.stackScope) + "\",";
            body += "\"backend\":\"" + json_escape(sample.backend) + "\",";
            body += "\"stack\":[";
            for (size_t fi = 0; fi < sample.stack.size(); ++fi)
            {
                if (fi)
                    body += ",";
                body += "\"" + json_escape(sample.stack[fi]) + "\"";
            }
            body += "]}";
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
                body += "\"exe\":\"" + json_escape(sample.exe) + "\",";
                body += "\"runtime\":\"" + json_escape(sample.runtime) + "\",";
                body += "\"count\":" + std::to_string(sample.count) + ",";
                body += "\"stack_scope\":\"" + json_escape(profile.stackScope) + "\",";
                body += "\"backend\":\"" + json_escape(profile.backend) + "\",";
                body += "\"stack\":[";
                for (size_t fi = 0; fi < sample.stack.size(); ++fi)
                {
                    if (fi)
                        body += ",";
                    body += "\"" + json_escape(sample.stack[fi]) + "\"";
                }
                body += "]}";
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

static bool post_spooled_batch(const ContinuousSamplerConfig &cfg,
                               const std::string &path,
                               const std::string &batchID)
{
    std::string response;
    int rc = drop::exec_capture({"curl", "-fsS", "-m", "20", "-X", "POST",
                                 "-H", "Content-Type: application/json",
                                 "-H", "Drop-User-Uid: " + cfg.authUID,
                                 "-H", "X-Mini-Drop-Agent-Time-Ms: " + std::to_string(now_ms()),
                                 "-d", "@" + path,
                                 cfg.apiBaseURL + "/api/v1/internal/continuous/batches"},
                                &response, 8192);
    if (rc != 0 || !response_acknowledges_batch(response, batchID))
    {
        std::cout << "[native-cp] batch upload failed batch=" << batchID
                  << " rc=" << rc << " response=" << response << std::endl;
        return false;
    }
    std::cout << "[native-cp] batch ACK received batch=" << batchID
              << " response=" << response << std::endl;
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
    std::vector<std::string> files = list_spool_files(cfg);
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
    if (post_spooled_batch(cfg, path, batchID))
    {
        if (::unlink(path.c_str()) != 0)
            std::cout << "[native-cp] ACK received but spool removal failed path=" << path
                      << " errno=" << errno << std::endl;
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
    std::string signals = env_string_local("DROP_NATIVE_CP_SIGNALS", "cpu,io,sched");
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
    std::future<HistogramPayload> schedFuture;
    bool ioStarted = false;
    bool schedStarted = false;
    if (ebpfEnabled && signal_enabled(signals, "io"))
    {
        ioFuture = std::async(std::launch::async, collect_bpftrace_latency_histogram, cfg, "io_latency");
        ioStarted = true;
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
              << " sched_events=" << schedEvents
              << " io_ms=" << ioMs
              << " sched_ms=" << schedMs
              << " backend=" << window.selectedBackend
              << " status=" << window.backendStatus << std::endl;
    return window;
}

} // namespace (anonymous)

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
    SpoolRetryState retry;
    while (running.load() && drain_one_spooled_batch(config, &retry, true))
    {
        if (list_spool_files(config).empty())
            break;
    }

    int windowsPerBatch = std::max(1, config.uploadBatchSec / config.aggregationWindowSec);
    std::vector<WindowPayload> batch;
    batch.reserve(static_cast<size_t>(windowsPerBatch));
    std::string batchID;
    while (running.load())
    {
        drain_one_spooled_batch(config, &retry, false);
        if (!spool_has_collection_capacity(config))
        {
            std::cout << "[native-cp] spool backpressure usage_bytes=" << spool_usage_bytes(config)
                      << " max_bytes=" << config.spoolMaxBytes
                      << " free_bytes=" << spool_free_bytes(config)
                      << " min_free_bytes=" << config.spoolMinFreeBytes << std::endl;
            interruptible_wait(running, 1000);
            continue;
        }

        WindowPayload window = collector(config);
        batch.push_back(window);
        if (batchID.empty())
            batchID = make_batch_id(config, window.startMs);
        std::string body = build_batch_json(config, batchID, batch);
        bool persisted = persist_batch(config, batchID, body);
        while (!persisted && running.load())
        {
            std::cout << "[native-cp] failed to persist batch journal batch=" << batchID
                      << " errno=" << errno << ", retrying without collecting" << std::endl;
            drain_one_spooled_batch(config, &retry, false);
            interruptible_wait(running, 1000);
            persisted = persist_batch(config, batchID, body);
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
            if (!finalize_batch(config, batchID))
            {
                std::cout << "[native-cp] failed to finalize batch journal batch=" << batchID
                          << " errno=" << errno << std::endl;
                running = false;
                break;
            }
            batch.clear();
            batchID.clear();
            drain_one_spooled_batch(config, &retry, false);
        }
    }

    if (!batch.empty() && !batchID.empty())
    {
        std::string body = build_batch_json(config, batchID, batch);
        if (persist_batch(config, batchID, body) && finalize_batch(config, batchID))
            drain_one_spooled_batch(config, &retry, true);
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

} // namespace drop
