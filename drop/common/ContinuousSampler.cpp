// ============================================================
// common/ContinuousSampler.cpp — perf_event-first sampler shell
// ============================================================

#include "common/ContinuousSampler.h"

namespace drop
{

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
    config_ = config;
    running_ = true;
    return true;
}

void PerfEventSampler::Stop()
{
    running_ = false;
}

bool PerfEventSampler::Running() const
{
    return running_;
}

} // namespace drop
