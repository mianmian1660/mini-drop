// ============================================================
// common/ContinuousSampler.h — Native CP sampler abstraction
// ============================================================

#pragma once

#include <cstdint>
#include <atomic>
#include <string>
#include <thread>
#include <vector>

namespace drop
{

struct ContinuousSampleLabel
{
    std::string comm;
    int pid = 0;
    std::string exe;
};

struct ContinuousSamplerConfig
{
    int sampleRateHz = 19;
    int aggregationWindowSec = 10;
    int uploadBatchSec = 60;
    int retentionHours = 24;
    std::string sessionSID;
    std::string targetIP;
    std::string hostname;
    std::string apiBaseURL;
    std::string authUID;
};

struct ContinuousSampleWindow
{
    int64_t windowStartUnixMs = 0;
    int64_t windowEndUnixMs = 0;
    ContinuousSampleLabel label;
    std::vector<std::string> stack;
    uint64_t count = 0;
};

class Sampler
{
public:
    virtual ~Sampler() {}
    virtual std::string Name() const = 0;
    virtual bool Start(const ContinuousSamplerConfig &config, std::string *error) = 0;
    virtual void Stop() = 0;
    virtual bool Running() const = 0;
};

class PerfEventSampler : public Sampler
{
public:
    ~PerfEventSampler() override;
    std::string Name() const override;
    bool Start(const ContinuousSamplerConfig &config, std::string *error) override;
    void Stop() override;
    bool Running() const override;

private:
    void Loop();

    std::atomic<bool> running_{false};
    ContinuousSamplerConfig config_;
    std::thread worker_;
};

class DualTrackContinuousSampler : public Sampler
{
public:
    ~DualTrackContinuousSampler() override;
    std::string Name() const override;
    bool Start(const ContinuousSamplerConfig &config, std::string *error) override;
    void Stop() override;
    bool Running() const override;

private:
    void Loop();

    std::atomic<bool> running_{false};
    ContinuousSamplerConfig config_;
    std::thread worker_;
};

} // namespace drop
