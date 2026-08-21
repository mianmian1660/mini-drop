#include "common/PythonRuntimeProfiler.h"
#include "common/Utils.h"

#include <algorithm>
#include <climits>
#include <chrono>
#include <cstdlib>
#include <dirent.h>
#include <fstream>
#include <mutex>
#include <sstream>
#include <sys/stat.h>
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

    std::string path = "/tmp/mini_drop_pyspy_" + std::to_string(candidate.pid) + "_" +
                       std::to_string(wall_now_ms()) + ".raw";
    std::string output;
    int rc = exec_capture({"timeout", "-s", "INT", "-k", "2", std::to_string(durationSec + 2),
                           "/usr/local/bin/py-spy", "record", "--pid", std::to_string(candidate.pid),
                           "--duration", std::to_string(durationSec), "--rate", std::to_string(rateHz),
                           "--format", "raw", "--function", "--output", path},
                          &output, 16384);
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
    if (!result.ready && rc != 0)
        result.reason = "py-spy failed rc=" + std::to_string(rc) + ": " + trim(output);
    else if (!result.ready)
        result.reason = "py-spy returned no samples";
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
    std::vector<PythonFallbackResult> out;
    out.reserve(futures_.size());
    for (auto &future : futures_)
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
    futures_.clear();
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
