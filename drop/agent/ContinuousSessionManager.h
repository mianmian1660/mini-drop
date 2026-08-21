#pragma once

#include "agent/Config.h"
#include "common/ContinuousSampler.h"

#include <atomic>
#include <chrono>
#include <map>
#include <memory>
#include <string>
#include <thread>
#include <vector>

namespace drop_agent
{

struct ContinuousAssignment
{
    std::string sid;
    std::string scope = "host";
    std::string selectorExe;
    std::string desiredState = "running";
    std::string continuityMode = "degraded";
    bool allowDegraded = false;
    uint64_t revision = 0;
    int sampleRateHz = 19;
    int aggregationWindowSec = 10;
    int uploadBatchSec = 60;
    int retentionHours = 24;
};

std::vector<drop::ContinuousTargetProcess> MatchContinuousProcessesByExe(
    const std::vector<drop::ContinuousTargetProcess> &processes,
    const std::string &selectorExe);

class ContinuousSessionManager
{
public:
    ContinuousSessionManager(const AgentConfig &config,
                             std::string apiBaseURL,
                             std::string authUID,
                             std::atomic<bool> &agentRunning);
    ~ContinuousSessionManager();

    void Start();
    void Stop();

private:
    friend struct ContinuousSessionManagerTestAccess;

    struct Runtime
    {
        ContinuousAssignment assignment;
        std::vector<drop::ContinuousTargetProcess> targets;
        std::string observedState = "pending";
        std::string effectiveContinuityMode = "degraded";
        std::string degradationReason;
        std::string lastError;
    };

    struct StoppingRuntime
    {
        drop::ContinuousSamplerConfig samplerConfig;
        std::string continuityMode = "degraded";
        std::string degradationReason;
        std::string lastError;
    };

    struct StoppedReport
    {
        std::string continuityMode = "degraded";
        std::string degradationReason;
    };

    void Loop();
    std::vector<drop::ContinuousTargetProcess> ScanProcesses() const;
    bool Reconcile(const std::vector<drop::ContinuousTargetProcess> &processes);
    void ApplyAssignments(const std::vector<ContinuousAssignment> &assignments,
                          const std::vector<drop::ContinuousTargetProcess> &processes);
    void RefreshTargets(const std::vector<drop::ContinuousTargetProcess> &processes);
    void RebuildSharedEngine();
    void UpdateRuntimeEngineStatus(const std::vector<drop::ContinuousSamplerConfig> &configs);
    void AdvanceStoppingSessions();
    drop::ContinuousSamplerConfig BuildSamplerConfig(const Runtime &runtime) const;
    void StopRuntime(Runtime &runtime);
    std::string BuildReconcileBody(const std::vector<drop::ContinuousTargetProcess> &processes) const;
    bool ParseAssignments(const std::string &response, std::vector<ContinuousAssignment> *assignments, uint64_t *revision) const;
    void SaveAssignmentCache(const std::string &response) const;
    bool LoadAssignmentCache(std::vector<ContinuousAssignment> *assignments, uint64_t *revision) const;

    const AgentConfig &config_;
    std::string apiBaseURL_;
    std::string authUID_;
    std::string spoolDirectory_;
    std::string cachePath_;
    uint64_t spoolMaxBytes_ = 5ULL * 1024 * 1024 * 1024;
    uint64_t spoolMinFreeBytes_ = 2ULL * 1024 * 1024 * 1024;
    int retryMaxSec_ = 300;
    std::atomic<bool> &agentRunning_;
    std::atomic<bool> running_{false};
    std::thread thread_;
    std::map<std::string, Runtime> runtimes_;
    std::map<std::string, StoppingRuntime> stoppingRuntimes_;
    std::map<std::string, StoppedReport> stoppedReports_;
    std::unique_ptr<drop::SharedDualTrackContinuousSampler> sharedSampler_;
    std::string sharedFingerprint_;
    uint64_t revision_ = 0;
};

} // namespace drop_agent
