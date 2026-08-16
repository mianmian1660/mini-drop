// ============================================================
// common/ContinuousSampler.cpp — perf_event-first continuous sampler
// ============================================================

#include "common/ContinuousSampler.h"
#include "common/Utils.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cctype>
#include <ctime>
#include <fstream>
#include <future>
#include <iostream>
#include <map>
#include <regex>
#include <set>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace drop
{

namespace
{
static constexpr uint64_t kMaxDBCount = (1ULL << 63) - 1;

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
    uint64_t count = 0;
};

struct ProfilePayload
{
    std::string signalType = "cpu_profile";
    std::string backend;
    std::string stackScope;
    std::vector<AggregatedSample> samples;
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
    for (auto &sample : window.samples)
        sample.backend = "perf";
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
    std::string output;
    int rc = drop::exec_capture({"timeout", "-s", "INT", "-k", "2", std::to_string(std::max(1, cfg.aggregationWindowSec)), "bpftrace", scriptPath}, &output, 8 * 1024 * 1024);
    ::remove(scriptPath.c_str());
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
    }

    std::string body = "{";
    body += "\"session_sid\":\"" + json_escape(cfg.sessionSID) + "\",";
    body += "\"batch_id\":\"" + json_escape(batchID) + "\",";
    body += "\"target_ip\":\"" + json_escape(cfg.targetIP) + "\",";
    body += "\"schema_version\":2,";
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
    int rc = drop::exec_capture({"curl", "-fsS", "-m", "20", "-X", "POST",
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

static std::string core_unavailable_reason()
{
    std::string btf = env_string_local("DROP_BTF_PATH", "/sys/kernel/btf/vmlinux");
    if (!file_exists_local(btf))
        return "CO-RE BTF unavailable";
    return "CO-RE CPU sampler object is not enabled in this build";
}

static WindowPayload collect_dual_track_window(const ContinuousSamplerConfig &cfg)
{
    WindowPayload window;
    window.startMs = now_ms();
    int64_t captureEndMs = window.startMs + static_cast<int64_t>(std::max(1, cfg.aggregationWindowSec)) * 1000;
    std::string signals = env_string_local("DROP_NATIVE_CP_SIGNALS", "cpu,io,sched");
    bool ebpfEnabled = env_enabled_local("DROP_NATIVE_CP_EBPF_ENABLED");

    std::future<ProfilePayload> userFuture;
    std::future<ProfilePayload> kernelFuture;
    std::future<WindowPayload> perfFuture;
    std::future<HistogramPayload> ioFuture;
    std::future<HistogramPayload> schedFuture;
    bool userStarted = false;
    bool kernelStarted = false;
    bool perfStarted = false;
    bool ioStarted = false;
    bool schedStarted = false;

    if (signal_enabled(signals, "cpu"))
    {
        std::string backends = env_string_local("DROP_NATIVE_CP_CPU_BACKENDS", "core,bpftrace,perf");
        bool perfAllowed = backends.find("perf") != std::string::npos;
        if (ebpfEnabled && backends.find("core") != std::string::npos)
        {
            std::cout << "[native-cp] CO-RE CPU backend unavailable: " << core_unavailable_reason() << std::endl;
        }
        if (ebpfEnabled && backends.find("bpftrace") != std::string::npos && command_available("bpftrace"))
        {
            userFuture = std::async(std::launch::async, collect_bpftrace_cpu_profile, cfg, "user");
            kernelFuture = std::async(std::launch::async, collect_bpftrace_cpu_profile, cfg, "kernel");
            userStarted = true;
            kernelStarted = true;
        }
        if (perfAllowed)
        {
            perfFuture = std::async(std::launch::async, collect_window, cfg);
            perfStarted = true;
        }
    }

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

    bool cpuCollected = false;
    if (userStarted)
    {
        ProfilePayload user = userFuture.get();
        if (!user.samples.empty())
        {
            window.profiles.push_back(user);
            cpuCollected = true;
        }
    }
    if (kernelStarted)
    {
        ProfilePayload kernel = kernelFuture.get();
        if (!kernel.samples.empty())
        {
            window.profiles.push_back(kernel);
            cpuCollected = true;
        }
    }
    if (perfStarted)
    {
        WindowPayload perfWindow = perfFuture.get();
        if (perfWindow.endMs > 0)
            captureEndMs = std::max(captureEndMs, perfWindow.endMs);
        if (!cpuCollected)
        {
            window.samples = perfWindow.samples;
            for (auto &sample : window.samples)
                sample.backend = "perf";
            cpuCollected = !window.samples.empty();
        }
    }
    if (signal_enabled(signals, "cpu") && !cpuCollected)
        std::cout << "[native-cp] no CPU profile samples collected in this window" << std::endl;
    if (ioStarted)
        window.histograms.push_back(ioFuture.get());
    if (schedStarted)
        window.histograms.push_back(schedFuture.get());

    window.endMs = captureEndMs;
    return window;
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
    if (running_.load())
        return true;
    config_ = config;
    running_ = true;
    worker_ = std::thread(&DualTrackContinuousSampler::Loop, this);
    return true;
}

void DualTrackContinuousSampler::Stop()
{
    if (!running_.load())
        return;
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
    int windowsPerBatch = std::max(1, config_.uploadBatchSec / config_.aggregationWindowSec);
    std::vector<WindowPayload> batch;
    batch.reserve(static_cast<size_t>(windowsPerBatch));
    while (running_.load())
    {
        WindowPayload window = collect_dual_track_window(config_);
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
