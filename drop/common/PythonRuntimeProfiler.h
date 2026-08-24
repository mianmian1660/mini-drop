#pragma once

#include <cstddef>
#include <cstdint>
#include <future>
#include <map>
#include <string>
#include <vector>

namespace drop
{

struct PythonCandidate
{
    int pid = 0;
    int samples = 0;
    int64_t startMs = 0;
    std::string comm;
    std::string exe;
};

struct PythonStackSample
{
    std::vector<std::string> stack;
    uint64_t count = 0;
};

struct PythonFallbackResult
{
    int pid = 0;
    int64_t startMs = 0;
    std::string comm;
    std::string exe;
    bool ready = false;
    std::string reason;
    std::vector<PythonStackSample> samples;
};

struct PythonRSSMetric
{
    int pid = 0;
    int64_t startMs = 0;
    int64_t timestampMs = 0;
    uint64_t valueBytes = 0;
    std::string comm;
    std::string exe;
};

class PythonFallbackCapture
{
public:
    PythonFallbackCapture() = default;
    PythonFallbackCapture(PythonFallbackCapture &&) = default;
    PythonFallbackCapture &operator=(PythonFallbackCapture &&) = default;
    PythonFallbackCapture(const PythonFallbackCapture &) = delete;
    PythonFallbackCapture &operator=(const PythonFallbackCapture &) = delete;

    /// 阻塞等待全部 fallback 结果（覆盖真实 capture 区间后合并）。
    std::vector<PythonFallbackResult> Finish();

    /// 等待最多 maxWaitMs，返回本轮已就绪的 fallback 结果；仍在后台运行的
    /// future 保留在 capture 内（后续可再次 Poll），不阻塞滚动 perf 解析。
    std::vector<PythonFallbackResult> Poll(int64_t maxWaitMs);

    /// 是否还有尚未完成的后台 fallback future。
    bool AnyPending() const { return !futures_.empty(); }

    /// 因 maxProcesses 上限被截断的候选数（诊断用）。
    size_t LimitedCount() const { return limitedCount_; }

private:
    friend PythonFallbackCapture start_python_fallback_capture(const std::string &sessionSID,
                                                                int durationSec,
                                                                int rateHz,
                                                                int maxProcesses);
    std::vector<std::future<PythonFallbackResult>> futures_;
    size_t limitedCount_ = 0;
};

/// Replaces the candidate set for the next window. PID identity includes startMs.
void schedule_python_fallback(const std::string &sessionSID,
                              const std::vector<PythonCandidate> &candidates);

/// Starts py-spy for candidates learned from the preceding perf window.
PythonFallbackCapture start_python_fallback_capture(const std::string &sessionSID,
                                                    int durationSec,
                                                    int rateHz,
                                                    int maxProcesses);

/// Parses py-spy folded/raw output. Frames remain root-to-leaf.
std::vector<PythonStackSample> parse_pyspy_raw(const std::string &raw);

/// Reads current Python RSS points, sorted descending and capped.
std::vector<PythonRSSMetric> collect_python_rss(size_t maxProcesses, size_t *truncated);

/// Reads /proc/<pid>/stat start time as stable Unix epoch milliseconds.
bool python_process_start_ms(int pid, int64_t *out);
bool python_process_is_same(int pid, int64_t expectedStartMs);

} // namespace drop
