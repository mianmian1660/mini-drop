#include "agent/ContinuousSessionManager.h"

#include <gtest/gtest.h>

#include <atomic>
#include <cstdlib>
#include <fstream>
#include <sstream>
#include <thread>
#include <sys/stat.h>
#include <unistd.h>

namespace drop_agent
{

struct ContinuousSessionManagerTestAccess
{
    static std::vector<drop::ContinuousTargetProcess> ScanProcesses(const ContinuousSessionManager &manager)
    {
        return manager.ScanProcesses();
    }

    static bool ParseAssignments(const ContinuousSessionManager &manager,
                                 const std::string &body,
                                 std::vector<ContinuousAssignment> *assignments,
                                 uint64_t *revision)
    {
        return manager.ParseAssignments(body, assignments, revision);
    }

    static void SaveAssignmentCache(const ContinuousSessionManager &manager, const std::string &body)
    {
        manager.SaveAssignmentCache(body);
    }

    static bool LoadAssignmentCache(const ContinuousSessionManager &manager,
                                    std::vector<ContinuousAssignment> *assignments,
                                    uint64_t *revision)
    {
        return manager.LoadAssignmentCache(assignments, revision);
    }

    static void AddStoppingRuntime(ContinuousSessionManager &manager,
                                   const std::string &sid,
                                   const drop::ContinuousSamplerConfig &config)
    {
        ContinuousSessionManager::StoppingRuntime stopping;
        stopping.samplerConfig = config;
        manager.stoppingRuntimes_[sid] = std::move(stopping);
    }

    static void AdvanceStoppingSessions(ContinuousSessionManager &manager)
    {
        manager.AdvanceStoppingSessions();
    }

    static std::string BuildReconcileBody(const ContinuousSessionManager &manager)
    {
        return manager.BuildReconcileBody({});
    }

    static void AddRuntimeReport(ContinuousSessionManager &manager,
                                 const std::string &sid,
                                 const std::string &observedState,
                                 const std::string &continuityMode,
                                 const std::string &degradationReason)
    {
        ContinuousSessionManager::Runtime runtime;
        runtime.assignment.sid = sid;
        runtime.observedState = observedState;
        runtime.effectiveContinuityMode = continuityMode;
        runtime.degradationReason = degradationReason;
        manager.runtimes_[sid] = std::move(runtime);
    }

    static bool StartSharedSampler(ContinuousSessionManager &manager,
                                   const drop::ContinuousSamplerConfig &config,
                                   std::string *error)
    {
        manager.sharedSampler_ = std::make_unique<drop::SharedDualTrackContinuousSampler>();
        if (!manager.sharedSampler_->Start({config}, error))
            return false;
        for (int attempt = 0; !manager.sharedSampler_->Ready() && attempt < 100; ++attempt)
            std::this_thread::sleep_for(std::chrono::milliseconds(10));
        return manager.sharedSampler_->Ready() && manager.sharedSampler_->Running();
    }

    static void StopSharedSampler(ContinuousSessionManager &manager)
    {
        if (manager.sharedSampler_)
        {
            manager.sharedSampler_->Stop();
            manager.sharedSampler_.reset();
        }
    }
};

class ScopedEnvOverride
{
public:
    ScopedEnvOverride(std::string name, const std::string &value)
        : name_(std::move(name))
    {
        const char *existing = ::getenv(name_.c_str());
        if (existing)
        {
            hadValue_ = true;
            oldValue_ = existing;
        }
        ::setenv(name_.c_str(), value.c_str(), 1);
    }

    ~ScopedEnvOverride()
    {
        if (hadValue_)
            ::setenv(name_.c_str(), oldValue_.c_str(), 1);
        else
            ::unsetenv(name_.c_str());
    }

private:
    std::string name_;
    std::string oldValue_;
    bool hadValue_ = false;
};

TEST(ContinuousSessionManager, FollowsEveryInstanceWithExactExeIdentity)
{
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "api", "/opt/api"},
        {43, 2000, "api", "/opt/api"},
        {44, 3000, "api-helper", "/opt/api-helper"},
    };
    auto matches = MatchContinuousProcessesByExe(processes, "/opt/api");
    ASSERT_EQ(matches.size(), 2u);
    EXPECT_EQ(matches[0].pid, 42);
    EXPECT_EQ(matches[1].pid, 43);
    EXPECT_TRUE(MatchContinuousProcessesByExe(processes, "/missing").empty());
}

TEST(ContinuousSessionManager, ScansOneTargetPerMultithreadedProcess)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);

    std::thread worker([] { ::usleep(250000); });
    const std::string selfExe = "/proc/self/exe";
    char resolved[4096] = {};
    const ssize_t size = ::readlink(selfExe.c_str(), resolved, sizeof(resolved) - 1);
    ASSERT_GT(size, 0);
    resolved[size] = '\0';

    auto processes = ContinuousSessionManagerTestAccess::ScanProcesses(manager);
    worker.join();
    size_t selfInstances = 0;
    for (const auto &process : processes)
        if (process.exe == resolved)
            ++selfInstances;
    EXPECT_EQ(selfInstances, 1u);
}

TEST(ContinuousSessionManager, PersistsAndRestoresAuthoritativeAssignments)
{
    char directoryTemplate[] = "/tmp/mini-drop-assignment-cache-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    ASSERT_EQ(::setenv("DROP_NATIVE_CP_SPOOL_DIR", directory, 1), 0);

    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    const std::string response =
        R"({"code":0,"data":{"revision":7,"assignments":[{"sid":"cps-cache","scope":"process","selector_exe":"/opt/api","desired_state":"running","continuity_mode":"strict","allow_degraded":true,"revision":7,"sample_rate_hz":49,"aggregation_window_sec":10,"upload_batch_sec":60,"retention_hours":24}]}})";
    ContinuousSessionManagerTestAccess::SaveAssignmentCache(manager, response);

    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::LoadAssignmentCache(manager, &assignments, &revision));
    ASSERT_EQ(assignments.size(), 1u);
    EXPECT_EQ(revision, 7u);
    EXPECT_EQ(assignments[0].sid, "cps-cache");
    EXPECT_EQ(assignments[0].selectorExe, "/opt/api");
    EXPECT_EQ(assignments[0].sampleRateHz, 49);
    EXPECT_TRUE(assignments[0].allowDegraded);

    ::unlink((std::string(directory) + "/assignments.json").c_str());
    ::rmdir(directory);
    ::unsetenv("DROP_NATIVE_CP_SPOOL_DIR");
}

TEST(ContinuousSessionManager, RejectsMalformedAssignmentEnvelope)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    EXPECT_FALSE(ContinuousSessionManagerTestAccess::ParseAssignments(
        manager, R"({"code":500,"data":{"assignments":[]}})", &assignments, &revision));
    EXPECT_FALSE(ContinuousSessionManagerTestAccess::ParseAssignments(
        manager, "not-json", &assignments, &revision));
}

// 阶段一：Reconcile assignment DTO 的 signals 字符串数组解析。
TEST(ContinuousSessionManager, ParsesAssignmentSignalsArray)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    const std::string response =
        R"({"code":0,"data":{"revision":9,"assignments":[{"sid":"cps-sig","scope":"host","desired_state":"running","revision":9,"signals":["cpu_profile"],"labels":null}]}})";
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::ParseAssignments(manager, response, &assignments, &revision));
    ASSERT_EQ(assignments.size(), 1u);
    EXPECT_EQ(revision, 9u);
    ASSERT_EQ(assignments[0].requestedSignals.size(), 1u);
    EXPECT_EQ(assignments[0].requestedSignals[0], "cpu_profile");
}

// 阶段一：assignment 信号 → 物理采集信号换算。
TEST(ContinuousSessionManager, BuildSamplerConfigCarriesRequestedSignals)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    ContinuousSessionManagerTestAccess::AddRuntimeReport(manager, "cps-sig2", "running", "strict", "");
    const std::string response =
        R"({"code":0,"data":{"revision":1,"assignments":[{"sid":"cps-sig2","scope":"host","desired_state":"running","revision":1,"signals":["cpu_profile"]}]}})";
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::ParseAssignments(manager, response, &assignments, &revision));
    ASSERT_EQ(assignments.size(), 1u);
    // 直接验证 requestedSignals 与物理信号集合换算。
    EXPECT_EQ(assignments[0].requestedSignals.size(), 1u);
    EXPECT_EQ(drop::PhysicalSignalsFromRequested(assignments[0].requestedSignals), "cpu");
}

TEST(ContinuousSessionManager, ReportsRuntimeContinuityInsteadOfStaticObjectAvailability)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    ContinuousSessionManagerTestAccess::AddRuntimeReport(
        manager, "cps-degraded", "degraded", "degraded", "runtime attach failed");

    const std::string body = ContinuousSessionManagerTestAccess::BuildReconcileBody(manager);
    EXPECT_NE(body.find(R"("continuity_mode":"degraded")"), std::string::npos);
    EXPECT_NE(body.find(R"("degradation_reason":"runtime attach failed")"), std::string::npos);
    EXPECT_EQ(body.find("shared rolling perf/bpftrace fallback; strict persistent CO-RE engine unavailable"),
              std::string::npos);
}

TEST(ContinuousSessionManager, ReportsStoppedOnlyAfterFinalSpoolIsAcknowledged)
{
    char directoryTemplate[] = "/tmp/mini-drop-stop-state-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:1", "agent-test", agentRunning);

    drop::ContinuousSamplerConfig sampler;
    sampler.sessionSID = "cps-stopping";
    sampler.spoolDirectory = directory;
    sampler.apiBaseURL = "http://127.0.0.1:1";
    sampler.authUID = "agent-test";
    ASSERT_EQ(::mkdir((std::string(directory) + "/cps-stopping").c_str(), 0700), 0);
    std::ofstream pending(std::string(directory) + "/cps-stopping/cpb-final.json");
    pending << R"({"batch_id":"cpb-final"})";
    pending.close();

    ContinuousSessionManagerTestAccess::AddStoppingRuntime(manager, sampler.sessionSID, sampler);
    ContinuousSessionManagerTestAccess::AdvanceStoppingSessions(manager);
    const std::string waiting = ContinuousSessionManagerTestAccess::BuildReconcileBody(manager);
    EXPECT_NE(waiting.find(R"("observed_state":"stopping")"), std::string::npos);
    EXPECT_EQ(waiting.find(R"("observed_state":"stopped")"), std::string::npos);

    ::unlink((std::string(directory) + "/cps-stopping/cpb-final.json").c_str());
    ContinuousSessionManagerTestAccess::AdvanceStoppingSessions(manager);
    const std::string stopped = ContinuousSessionManagerTestAccess::BuildReconcileBody(manager);
    EXPECT_NE(stopped.find(R"("observed_state":"stopped")"), std::string::npos);

    ::rmdir((std::string(directory) + "/cps-stopping").c_str());
    ::rmdir(directory);
}

TEST(ContinuousSessionManager, RetriesStoppedSpoolWhileAnotherSharedSessionIsActive)
{
    char directoryTemplate[] = "/tmp/mini-drop-stop-active-XXXXXX";
    char *directory = ::mkdtemp(directoryTemplate);
    ASSERT_NE(directory, nullptr);
    const std::string root = directory;
    const std::string binDirectory = root + "/bin";
    const std::string marker = root + "/curl-called";
    ASSERT_EQ(::mkdir(binDirectory.c_str(), 0700), 0);
    {
        std::ofstream fakeCurl(binDirectory + "/curl");
        fakeCurl << "#!/bin/sh\n: > " << marker << "\nexit 1\n";
    }
    ASSERT_EQ(::chmod((binDirectory + "/curl").c_str(), 0700), 0);
    ScopedEnvOverride path("PATH", binDirectory);
    ScopedEnvOverride spool("DROP_NATIVE_CP_SPOOL_DIR", root);
    ScopedEnvOverride missingCoreObject("DROP_NATIVE_CP_CORE_OBJECT", root + "/missing-core.o");

    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:1", "agent-test", agentRunning);

    drop::ContinuousSamplerConfig active;
    active.sessionSID = "cps-active";
    active.spoolDirectory = root;
    active.spoolMinFreeBytes = 0;
    active.apiBaseURL = "http://127.0.0.1:1";
    active.authUID = "agent-test";
    active.allowDegraded = false;
    std::string error;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::StartSharedSampler(manager, active, &error)) << error;

    drop::ContinuousSamplerConfig stopping = active;
    stopping.sessionSID = "cps-stopping-active";
    ASSERT_EQ(::mkdir((root + "/" + stopping.sessionSID).c_str(), 0700), 0);
    {
        std::ofstream pending(root + "/" + stopping.sessionSID + "/cpb-final.json");
        pending << R"({"batch_id":"cpb-final"})";
    }
    ContinuousSessionManagerTestAccess::AddStoppingRuntime(manager, stopping.sessionSID, stopping);
    ContinuousSessionManagerTestAccess::AdvanceStoppingSessions(manager);
    EXPECT_EQ(::access(marker.c_str(), F_OK), 0);

    ContinuousSessionManagerTestAccess::StopSharedSampler(manager);
    EXPECT_EQ(::unlink((root + "/" + stopping.sessionSID + "/cpb-final.json").c_str()), 0);
    EXPECT_EQ(::unlink((binDirectory + "/curl").c_str()), 0);
    EXPECT_EQ(::unlink(marker.c_str()), 0);
    EXPECT_EQ(::rmdir((root + "/" + stopping.sessionSID).c_str()), 0);
    EXPECT_EQ(::rmdir((root + "/" + active.sessionSID).c_str()), 0);
    EXPECT_EQ(::rmdir(binDirectory.c_str()), 0);
    EXPECT_EQ(::rmdir(root.c_str()), 0);
}

} // namespace drop_agent
