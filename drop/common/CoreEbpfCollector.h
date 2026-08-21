#pragma once

#include "common/ContinuousSampler.h"

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace drop
{

struct CoreHistogramSample
{
    uint32_t signal = 0;
    uint32_t tgid = 0;
    uint32_t slot = 0;
    uint64_t count = 0;
};

class CoreEbpfCollector
{
public:
    CoreEbpfCollector();
    ~CoreEbpfCollector();

    bool Start(const std::vector<ContinuousTargetProcess> &targets, std::string *error);
    bool UpdateTargets(const std::vector<ContinuousTargetProcess> &targets, std::string *error);
    std::vector<CoreHistogramSample> Drain(uint64_t *lost);
    std::vector<CoreHistogramSample> StopAndDrain(uint64_t *lost);
    void Stop();
    bool Running() const;
    bool BlockAvailable() const;
    std::string DegradationReason() const;
#ifdef DROP_NATIVE_CP_TESTING
    bool SetLostForTesting(uint64_t value);
#endif

private:
    struct Impl;
    std::unique_ptr<Impl> impl_;
};

} // namespace drop
