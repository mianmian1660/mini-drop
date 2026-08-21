// ============================================================
// common/ContinuousSampler.h — Native CP sampler abstraction
// ============================================================

#pragma once

#include <cstdint>
#include <atomic>
#include <memory>
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

struct ContinuousTargetProcess
{
    int pid = 0;
    int64_t processStartMs = 0;
    std::string comm;
    std::string exe;
};

struct ContinuousSamplerConfig
{
    int sampleRateHz = 19;
    int aggregationWindowSec = 10;
    int uploadBatchSec = 60;
    int retentionHours = 24;
    uint64_t spoolMaxBytes = 5ULL * 1024 * 1024 * 1024;
    uint64_t spoolMinFreeBytes = 2ULL * 1024 * 1024 * 1024;
    int retryMaxSec = 300;
    std::string spoolDirectory = "/var/lib/mini-drop/continuous-spool";
    std::string sessionSID;
    std::string targetIP;
    std::string hostname;
    std::string apiBaseURL;
    std::string authUID;
    std::string scope = "host";
    std::string selectorExe;
    std::string signals = "cpu,io,io_syscall,sched";
    bool allowDegraded = false;
    std::vector<ContinuousTargetProcess> targetProcesses;
};

// Returns true only when this binary includes the libbpf loader and the
// configured CO-RE object can be opened. Runtime attach is still validated when
// a shared engine starts; callers use this for capability reporting only.
bool CoreContinuousSamplerAvailable();

struct ContinuousSampleWindow
{
    int64_t windowStartUnixMs = 0;
    int64_t windowEndUnixMs = 0;
    ContinuousSampleLabel label;
    std::vector<std::string> stack;
    uint64_t count = 0;
};

// A stopped Session is acknowledged only after its finalized batches have
// been accepted by the API. These helpers are scoped to one Session directory.
bool ContinuousSessionHasPendingSpool(const ContinuousSamplerConfig &config);
bool DrainOneContinuousSessionBatch(const ContinuousSamplerConfig &config);

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

// One physical degraded collector shared by all active user Sessions. Host and
// process scopes are mutually exclusive at the control plane, so process mode
// records the union of assigned TGIDs once and fans samples out by stable
// PID/start-time identity before writing each Session's spool.
class SharedDualTrackContinuousSampler
{
public:
    SharedDualTrackContinuousSampler();
    ~SharedDualTrackContinuousSampler();

    bool Start(const std::vector<ContinuousSamplerConfig> &sessions, std::string *error);
    void Stop();
    bool Running() const;
    // Start() is asynchronous. In strict mode reconciliation keeps the
    // previous recorder alive until the replacement has parsed its first
    // immutable perf switch-output file. Degraded collectors retain their
    // attach-level readiness and are always surfaced as degraded.
    bool Ready() const;
    bool Strict() const;
    bool Failed() const;
    std::string DegradationReason() const;

private:
    struct Impl;
    std::unique_ptr<Impl> impl_;
};

} // namespace drop
