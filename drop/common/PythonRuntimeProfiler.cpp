#include "common/PythonRuntimeProfiler.h"
#include "common/Utils.h"

#include <algorithm>
#include <climits>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <dirent.h>
#include <fstream>
#include <mutex>
#include <sstream>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

namespace drop
{
namespace
{

std::mutex g_candidatesMutex;
std::map<std::string, std::vector<PythonCandidate>> g_candidatesBySession;
std::map<std::pair<int, int64_t>, int64_t> g_failureCooldown;
constexpr int64_t kFailureCooldownMs = 60 * 1000;

int64_t wall_now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

int64_t boot_epoch_ms()
{
    static int64_t cached = 0;
    if (cached > 0)
        return cached;
    std::ifstream statFile("/proc/stat");
    std::string key;
    int64_t seconds = 0;
    while (statFile >> key)
    {
        if (key == "btime")
        {
            statFile >> seconds;
            break;
        }
        std::string rest;
        std::getline(statFile, rest);
    }
    cached = seconds * 1000;
    return cached;
}

std::string read_link(const std::string &path)
{
    char buf[4096];
    ssize_t n = ::readlink(path.c_str(), buf, sizeof(buf) - 1);
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

std::string sanitize_python_frame(const std::string &raw)
{
    // py-spy emits "function (/absolute/path/module.py:line)". Keep the
    // function, short file name and line while preventing host/container
    // paths from entering uploaded batches.
    size_t open = raw.rfind(" (");
    if (open == std::string::npos || raw.back() != ')')
        return raw;
    std::string location = raw.substr(open + 2, raw.size() - open - 3);
    size_t slash = location.find_last_of("/\\");
    if (slash != std::string::npos)
        location = location.substr(slash + 1);
    return raw.substr(0, open) + " (" + location + ")";
}

bool is_python_pid(int pid, std::string *exe)
{
    *exe = read_link("/proc/" + std::to_string(pid) + "/exe");
    return basename_of(*exe).rfind("python", 0) == 0;
}

std::string process_comm(int pid)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/comm");
    std::string value;
    std::getline(in, value);
    return trim(value);
}

PythonFallbackResult capture_one(PythonCandidate candidate, int durationSec, int rateHz)
{
    PythonFallbackResult result;
    result.pid = candidate.pid;
    result.startMs = candidate.startMs;
    result.comm = candidate.comm;
    result.exe = candidate.exe;

    if (!python_process_is_same(candidate.pid, candidate.startMs))
    {
        result.reason = "process exited or PID was reused";
        return result;
    }

    // 阶段四：记录真实 capture 区间；结果只能替换时间重叠且身份一致的
    // perf 样本（防跨窗口双计数）。
    result.captureStartMs = wall_now_ms();
    std::string path = "/tmp/mini_drop_pyspy_" + std::to_string(candidate.pid) + "_" +
                       std::to_string(result.captureStartMs) + ".raw";
    std::vector<std::string> args{"timeout", "-s", "INT", "-k", "2", std::to_string(durationSec + 2),
                                  "/usr/local/bin/py-spy", "record", "--pid", std::to_string(candidate.pid),
                                  "--duration", std::to_string(durationSec), "--rate", std::to_string(rateHz),
                                  "--format", "raw", "--function"};
    // 阶段四：--native 采集 C 扩展调用链（保留 Python 入口与 native C 热点）。
    if (pyspy_native_enabled())
        args.push_back("--native");
    args.insert(args.end(), {"--output", path});
    std::string output;
    int rc = exec_capture(args, &output, 16384);
    result.captureEndMs = wall_now_ms();
    if (!python_process_is_same(candidate.pid, candidate.startMs))
    {
        ::remove(path.c_str());
        result.reason = "process exited or PID was reused during capture";
        return result;
    }
    std::ifstream in(path, std::ios::binary);
    std::string raw((std::istreambuf_iterator<char>(in)), std::istreambuf_iterator<char>());
    ::remove(path.c_str());
    result.samples = parse_pyspy_raw(raw);
    result.ready = !result.samples.empty();
    // GNU timeout can return 124 after py-spy has already flushed a complete
    // raw profile. Valid samples are authoritative; use rc only to diagnose
    // an empty capture.
    if (!result.ready)
    {
        if (rc != 0)
        {
            const std::string trimmed = trim(output);
            if (trimmed.find("Permission denied") != std::string::npos ||
                trimmed.find("Operation not permitted") != std::string::npos)
                result.reason = "py-spy attach permission denied: " + trimmed;
            else if (rc == 124 || rc == 137)
                result.reason = "py-spy capture timed out";
            else
                result.reason = "py-spy failed rc=" + std::to_string(rc) + ": " + trimmed;
        }
        else
            result.reason = "py-spy returned no samples";
    }
    return result;
}

} // namespace

bool python_process_start_ms(int pid, int64_t *out)
{
    if (pid <= 0 || !out)
        return false;
    std::ifstream statFile("/proc/" + std::to_string(pid) + "/stat");
    std::string content;
    if (!std::getline(statFile, content))
        return false;
    size_t close = content.rfind(')');
    if (close == std::string::npos)
        return false;
    std::istringstream fields(content.substr(close + 1));
    std::vector<std::string> values;
    std::string value;
    while (fields >> value)
        values.push_back(value);
    if (values.size() < 20)
        return false;
    unsigned long long ticks = std::strtoull(values[19].c_str(), nullptr, 10);
    long hz = ::sysconf(_SC_CLK_TCK);
    if (hz <= 0)
        return false;
    int64_t bootMs = boot_epoch_ms();
    if (bootMs <= 0)
        return false;
    *out = bootMs + static_cast<int64_t>(ticks * 1000ULL / static_cast<unsigned long long>(hz));
    return true;
}

bool python_process_is_same(int pid, int64_t expectedStartMs)
{
    int64_t currentStartMs = 0;
    return expectedStartMs > 0 && python_process_start_ms(pid, &currentStartMs) && currentStartMs == expectedStartMs;
}

bool pyspy_native_enabled()
{
    const char *value = std::getenv("DROP_NATIVE_CP_PYSPY_NATIVE");
    if (!value || !*value)
        return true; // 默认开启：C 扩展调用链是两级策略的一部分
    std::string text(value);
    std::transform(text.begin(), text.end(), text.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return !(text == "0" || text == "false" || text == "no" || text == "off");
}

namespace
{

// 读取 /proc/<pid>/cmdline（NUL 分隔）。
std::string read_cmdline(int pid)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/cmdline", std::ios::binary);
    std::string content((std::istreambuf_iterator<char>(in)), std::istreambuf_iterator<char>());
    std::replace(content.begin(), content.end(), '\0', ' ');
    return trim(content);
}

} // namespace

PythonRuntimeProbe probe_python_runtime(int pid)
{
    PythonRuntimeProbe probe;
    if (pid <= 0)
        return probe;
    probe.exe = read_link("/proc/" + std::to_string(pid) + "/exe");
    if (probe.exe.empty() || basename_of(probe.exe).rfind("python", 0) != 0)
        return probe;
    probe.valid = true;
    probe.cmdline = read_cmdline(pid);
    // -X perf 兼容 "-Xperf"、"-X perf"、"‑Xperf=..." 写法。
    probe.hasPerfFlag = probe.cmdline.find("-X perf") != std::string::npos ||
                        probe.cmdline.find("-Xperf") != std::string::npos;
    // 从 maps 的 libpython3.<minor> 推断解释器次版本（不执行解释器）。
    std::ifstream maps("/proc/" + std::to_string(pid) + "/maps");
    std::string line;
    while (std::getline(maps, line))
    {
        const size_t pos = line.find("libpython3.");
        if (pos == std::string::npos)
            continue;
        const size_t minorStart = pos + strlen("libpython3.");
        char *end = nullptr;
        long minor = std::strtol(line.c_str() + minorStart, &end, 10);
        if (end && end != line.c_str() + minorStart && minor > 0)
            probe.pythonMinor = static_cast<int>(minor);
        break;
    }
    return probe;
}

std::vector<PythonCandidate> hottest_python_candidates_by_cpu_ticks(size_t limit)
{
    struct TickSnapshot
    {
        int pid = 0;
        uint64_t ticks = 0;
        int64_t startMs = 0;
        std::string comm;
        std::string exe;
    };
    auto snapshot = []() {
        std::vector<TickSnapshot> out;
        DIR *dir = ::opendir("/proc");
        if (!dir)
            return out;
        struct dirent *entry;
        while ((entry = ::readdir(dir)) != nullptr)
        {
            char *end = nullptr;
            const long parsed = std::strtol(entry->d_name, &end, 10);
            if (!end || *end != '\0' || parsed <= 0)
                continue;
            TickSnapshot snap;
            snap.pid = static_cast<int>(parsed);
            std::string exe;
            if (!is_python_pid(snap.pid, &exe))
                continue;
            snap.exe = exe;
            std::ifstream statFile("/proc/" + std::to_string(snap.pid) + "/stat");
            std::string content;
            if (!std::getline(statFile, content))
                continue;
            const size_t close = content.rfind(')');
            if (close == std::string::npos)
                continue;
            std::istringstream fields(content.substr(close + 1));
            std::vector<std::string> values;
            std::string value;
            while (fields >> value)
                values.push_back(value);
            // state(3) 之后：utime=14, stime=15 → 索引 11/12（相对 fields[0]）
            if (values.size() < 13)
                continue;
            snap.ticks = std::strtoull(values[11].c_str(), nullptr, 10) +
                         std::strtoull(values[12].c_str(), nullptr, 10);
            python_process_start_ms(snap.pid, &snap.startMs);
            snap.comm = process_comm(snap.pid);
            out.push_back(std::move(snap));
        }
        ::closedir(dir);
        return out;
    };

    const auto first = snapshot();
    struct timespec pause{0, 200 * 1000 * 1000}; // 200ms 增量窗口
    ::nanosleep(&pause, nullptr);
    const auto second = snapshot();

    std::map<int, TickSnapshot> baseline;
    for (const auto &before : first)
        baseline[before.pid] = before;

    std::vector<PythonCandidate> out;
    for (const auto &after : second)
    {
        PythonCandidate candidate;
        candidate.pid = after.pid;
        candidate.startMs = after.startMs;
        candidate.comm = after.comm;
        candidate.exe = after.exe;
        uint64_t current = after.ticks;
        auto found = baseline.find(after.pid);
        if (found != baseline.end() && found->second.startMs == after.startMs &&
            current >= found->second.ticks)
            current -= found->second.ticks; // 短周期增量（PID 复用时退化为总量）
        candidate.samples = static_cast<int>(current);
        out.push_back(std::move(candidate));
    }
    std::sort(out.begin(), out.end(), [](const auto &a, const auto &b) {
        if (a.samples == b.samples)
            return a.pid < b.pid;
        return a.samples > b.samples;
    });
    if (out.size() > limit)
        out.resize(limit);
    return out;
}

std::vector<PythonStackSample> parse_pyspy_raw(const std::string &raw)
{
    std::map<std::string, PythonStackSample> merged;
    std::istringstream input(raw);
    std::string line;
    while (std::getline(input, line))
    {
        line = trim(line);
        size_t split = line.rfind(' ');
        if (split == std::string::npos)
            continue;
        std::string countText = trim(line.substr(split + 1));
        char *end = nullptr;
        unsigned long long parsed = std::strtoull(countText.c_str(), &end, 10);
        if (!end || *end != '\0' || parsed == 0)
            continue;
        std::string folded = trim(line.substr(0, split));
        std::vector<std::string> stack;
        std::stringstream frames(folded);
        std::string frame;
        while (std::getline(frames, frame, ';'))
        {
            frame = trim(frame);
            if (!frame.empty())
                stack.push_back(sanitize_python_frame(frame));
        }
        if (stack.empty())
            continue;
        auto &sample = merged[folded];
        sample.stack = std::move(stack);
        uint64_t count = static_cast<uint64_t>(parsed);
        sample.count = UINT64_MAX - sample.count < count ? UINT64_MAX : sample.count + count;
    }
    std::vector<PythonStackSample> out;
    out.reserve(merged.size());
    for (auto &item : merged)
        out.push_back(std::move(item.second));
    return out;
}

void schedule_python_fallback(const std::string &sessionSID,
                              const std::vector<PythonCandidate> &candidates)
{
    std::vector<PythonCandidate> next = candidates;
    std::sort(next.begin(), next.end(), [](const auto &a, const auto &b) {
        if (a.samples == b.samples)
            return a.pid < b.pid;
        return a.samples > b.samples;
    });
    std::lock_guard<std::mutex> lock(g_candidatesMutex);
    g_candidatesBySession[sessionSID] = std::move(next);
}

PythonFallbackCapture start_python_fallback_capture(const std::string &sessionSID,
                                                    int durationSec,
                                                    int rateHz,
                                                    int maxProcesses)
{
    PythonFallbackCapture capture;
    if (durationSec <= 0 || rateHz <= 0 || maxProcesses <= 0)
        return capture;
    std::vector<PythonCandidate> candidates;
    {
        std::lock_guard<std::mutex> lock(g_candidatesMutex);
        auto found = g_candidatesBySession.find(sessionSID);
        if (found != g_candidatesBySession.end())
            candidates = found->second;
        int64_t now = wall_now_ms();
        candidates.erase(std::remove_if(candidates.begin(), candidates.end(), [&](const auto &candidate) {
                             auto it = g_failureCooldown.find({candidate.pid, candidate.startMs});
                             return it != g_failureCooldown.end() && it->second > now;
                         }),
                         candidates.end());
    }
    if (candidates.size() > static_cast<size_t>(maxProcesses))
    {
        capture.limitedCount_ = candidates.size() - static_cast<size_t>(maxProcesses);
        candidates.resize(static_cast<size_t>(maxProcesses));
    }
    for (const auto &candidate : candidates)
        capture.futures_.push_back(std::async(std::launch::async, capture_one, candidate, durationSec, rateHz));
    return capture;
}

std::vector<PythonFallbackResult> PythonFallbackCapture::Finish()
{
    return Poll(INT64_MAX);
}

std::vector<PythonFallbackResult> PythonFallbackCapture::Poll(int64_t maxWaitMs)
{
    std::vector<PythonFallbackResult> out;
    out.reserve(futures_.size());
    if (maxWaitMs < 0)
        maxWaitMs = 0;
    const int64_t deadline = wall_now_ms() + maxWaitMs;
    std::vector<std::future<PythonFallbackResult>> stillRunning;
    for (auto &future : futures_)
    {
        int64_t remaining = deadline - wall_now_ms();
        if (remaining > 0 && future.wait_for(std::chrono::milliseconds(remaining)) == std::future_status::ready)
        {
            try
            {
                out.push_back(future.get());
            }
            catch (const std::exception &e)
            {
                PythonFallbackResult failed;
                failed.reason = std::string("py-spy future failed: ") + e.what();
                out.push_back(std::move(failed));
            }
        }
        else
        {
            // 仍未就绪：保留 future，下一轮继续等待（不阻塞滚动 perf 解析）。
            stillRunning.push_back(std::move(future));
        }
    }
    futures_ = std::move(stillRunning);
    {
        std::lock_guard<std::mutex> lock(g_candidatesMutex);
        int64_t now = wall_now_ms();
        for (const auto &result : out)
        {
            auto key = std::make_pair(result.pid, result.startMs);
            if (result.ready)
                g_failureCooldown.erase(key);
            else if (result.pid > 0 && result.startMs > 0)
                g_failureCooldown[key] = now + kFailureCooldownMs;
        }
        for (auto it = g_failureCooldown.begin(); it != g_failureCooldown.end();)
            it = it->second <= now ? g_failureCooldown.erase(it) : std::next(it);
    }
    return out;
}

std::vector<PythonRSSMetric> collect_python_rss(size_t maxProcesses, size_t *truncated)
{
    std::vector<PythonRSSMetric> out;
    DIR *dir = ::opendir("/proc");
    if (!dir)
        return out;
    long pageSize = ::sysconf(_SC_PAGESIZE);
    struct dirent *entry;
    while ((entry = ::readdir(dir)) != nullptr)
    {
        char *end = nullptr;
        long parsed = std::strtol(entry->d_name, &end, 10);
        if (!end || *end != '\0' || parsed <= 0)
            continue;
        int pid = static_cast<int>(parsed);
        std::string exe;
        if (!is_python_pid(pid, &exe))
            continue;
        std::ifstream statm("/proc/" + std::to_string(pid) + "/statm");
        uint64_t totalPages = 0, residentPages = 0;
        if (!(statm >> totalPages >> residentPages))
            continue;
        PythonRSSMetric metric;
        metric.pid = pid;
        python_process_start_ms(pid, &metric.startMs);
        metric.timestampMs = wall_now_ms();
        metric.valueBytes = residentPages * static_cast<uint64_t>(std::max<long>(pageSize, 0));
        metric.comm = process_comm(pid);
        metric.exe = exe;
        out.push_back(std::move(metric));
    }
    ::closedir(dir);
    std::sort(out.begin(), out.end(), [](const auto &a, const auto &b) {
        if (a.valueBytes == b.valueBytes)
            return a.pid < b.pid;
        return a.valueBytes > b.valueBytes;
    });
    if (truncated)
        *truncated = out.size() > maxProcesses ? out.size() - maxProcesses : 0;
    if (out.size() > maxProcesses)
        out.resize(maxProcesses);
    return out;
}

} // namespace drop
