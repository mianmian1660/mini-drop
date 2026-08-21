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
    // 来自 session.labels.db_targets（服务端零迁移透传的 jsonb），驱动
    // DBSnapshotSampler。为空时该 Session 不做数据库巡检。
    std::vector<drop::DBTargetConfig> dbTargets;
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
        // 数据库巡检不走 SharedDualTrackContinuousSampler 的"整机唯一物理采集
        // 器+按 PID 分流"模型——那套模型是为了绕开 perf_event/eBPF 只能有一个
        // 物理挂载点的限制。数据库短连接查询没有这个硬件级约束，每个 Session
        // 独立持有自己的 DBSnapshotSampler 更简单，生命周期直接跟着 Runtime 走。
        std::unique_ptr<drop::DBSnapshotSampler> dbSampler;
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
    void ReconcileDBSampler(Runtime &runtime);
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
