// ============================================================
// common/ContinuousSampler.cpp — perf_event-first continuous sampler
// ============================================================

#include "common/ContinuousSampler.h"
#include "common/Utils.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <fstream>
#include <iostream>
#include <map>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace drop
{

namespace
{
struct AggregatedSample
{
    std::vector<std::string> stack;
    std::string comm;
    int pid = 0;
    std::string exe;
    uint64_t count = 0;
};

struct WindowPayload
{
    int64_t startMs = 0;
    int64_t endMs = 0;
    std::vector<AggregatedSample> samples;
};

static int64_t now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
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

static std::string parse_frame_name(const std::string &raw)
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
    if (paren != std::string::npos)
        name = name.substr(0, paren);
    if (name.empty())
        name = first;
    return drop::trim(name);
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
                       const std::vector<std::string> &rawStack)
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
            std::string frame = parse_frame_name(line);
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

static WindowPayload collect_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    std::string dataPath = "/tmp/mini_drop_native_cp_" + std::to_string(window.startMs) + ".data";
    std::string perf = perf_bin();
    std::string recordOutput;
    int rc = drop::exec_capture({perf, "record", "--no-buildid-cache", "-q", "-a", "-F", std::to_string(cfg.sampleRateHz), "-g", "-o", dataPath, "--", "sleep", std::to_string(cfg.aggregationWindowSec)}, &recordOutput, 4096);
    window.endMs = now_ms();
    if (rc != 0)
    {
        std::cout << "[native-cp] perf record failed rc=" << rc << " output=" << recordOutput << std::endl;
        ::remove(dataPath.c_str());
        return window;
    }
    std::string scriptOutput;
    rc = drop::exec_capture({perf, "script", "-i", dataPath}, &scriptOutput, 32 * 1024 * 1024);
    ::remove(dataPath.c_str());
    if (rc != 0)
    {
        std::cout << "[native-cp] perf script failed rc=" << rc << std::endl;
        return window;
    }
    window.samples = parse_perf_script(scriptOutput);
    return window;
}

static std::string build_batch_json(const ContinuousSamplerConfig &cfg,
                                    const std::string &batchID,
                                    const std::vector<WindowPayload> &windows)
{
    std::string start = windows.empty() ? rfc3339_from_ms(now_ms()) : rfc3339_from_ms(windows.front().startMs);
    std::string end = windows.empty() ? start : rfc3339_from_ms(windows.back().endMs);
    uint64_t sampleCount = 0;
    for (const auto &window : windows)
        for (const auto &sample : window.samples)
            sampleCount += sample.count;

    std::string body = "{";
    body += "\"session_sid\":\"" + json_escape(cfg.sessionSID) + "\",";
    body += "\"batch_id\":\"" + json_escape(batchID) + "\",";
    body += "\"target_ip\":\"" + json_escape(cfg.targetIP) + "\",";
    body += "\"start_time\":\"" + start + "\",";
    body += "\"end_time\":\"" + end + "\",";
    body += "\"window_count\":" + std::to_string(windows.size()) + ",";
    body += "\"sample_count\":" + std::to_string(sampleCount) + ",";
    body += "\"windows\":[";
    for (size_t wi = 0; wi < windows.size(); ++wi)
    {
        const auto &window = windows[wi];
        if (wi)
            body += ",";
        uint64_t windowSamples = 0;
        for (const auto &sample : window.samples)
            windowSamples += sample.count;
        body += "{";
        body += "\"window_start\":\"" + rfc3339_from_ms(window.startMs) + "\",";
        body += "\"window_end\":\"" + rfc3339_from_ms(window.endMs) + "\",";
        body += "\"sample_count\":" + std::to_string(windowSamples) + ",";
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
            body += "\"count\":" + std::to_string(sample.count) + ",";
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
    body += "]}";
    return body;
}

static bool post_batch(const ContinuousSamplerConfig &cfg, const std::string &body)
{
    std::string path = "/tmp/mini_drop_native_cp_batch_" + std::to_string(now_ms()) + ".json";
    {
        std::ofstream out(path, std::ios::binary);
        if (!out.is_open())
            return false;
        out << body;
    }
    std::string response;
    int rc = drop::exec_capture({"curl", "-sS", "-m", "20", "-X", "POST",
                                 "-H", "Content-Type: application/json",
                                 "-H", "Drop-User-Uid: " + cfg.authUID,
                                 "-d", "@" + path,
                                 cfg.apiBaseURL + "/api/v1/internal/continuous/batches"},
                                &response, 8192);
    ::remove(path.c_str());
    if (rc != 0)
    {
        std::cout << "[native-cp] batch upload failed rc=" << rc << " response=" << response << std::endl;
        return false;
    }
    std::cout << "[native-cp] batch uploaded response=" << response << std::endl;
    return true;
}
} // namespace

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
    if (running_.load())
        return true;
    config_ = config;
    running_ = true;
    worker_ = std::thread(&PerfEventSampler::Loop, this);
    return true;
}

void PerfEventSampler::Stop()
{
    if (!running_.load())
        return;
    running_ = false;
    if (worker_.joinable())
        worker_.join();
}

bool PerfEventSampler::Running() const
{
    return running_.load();
}

void PerfEventSampler::Loop()
{
    int windowsPerBatch = std::max(1, config_.uploadBatchSec / config_.aggregationWindowSec);
    std::vector<WindowPayload> batch;
    batch.reserve(static_cast<size_t>(windowsPerBatch));
    while (running_.load())
    {
        WindowPayload window = collect_window(config_);
        if (!running_.load())
            break;
        batch.push_back(window);
        if (static_cast<int>(batch.size()) >= windowsPerBatch)
        {
            std::string batchID = "cpb-" + std::to_string(now_ms());
            std::string body = build_batch_json(config_, batchID, batch);
            post_batch(config_, body);
            batch.clear();
        }
    }
}

} // namespace drop
