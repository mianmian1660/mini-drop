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

    static std::string BuildReconcileBody(const ContinuousSessionManager &manager,
                                          const std::vector<drop::ContinuousTargetProcess> &processes = {})
    {
        return manager.BuildReconcileBody(processes);
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

// ============================================================
// 阶段六：selector 模型（pid_instance / exe_all_instances / cgroup /
// container_id）
// ============================================================

TEST(ContinuousSessionManager, PidInstanceMatchesExactTripleAndRejectsPidReuse)
{
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "api", "/opt/api"},
        // PID 复用：同 PID 但 start time 不同（新进程实例）。
        {42, 5000, "api", "/opt/api"},
        {43, 2000, "api", "/opt/api"},
    };
    ContinuousAssignment assignment;
    assignment.scope = "process";
    assignment.selectorMode = "pid_instance";
    assignment.selectorPid = 42;
    assignment.selectorProcessStartMs = 1000;
    assignment.selectorExe = "/opt/api";

    auto matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 1u);
    EXPECT_EQ(matches[0].pid, 42);
    EXPECT_EQ(matches[0].processStartMs, 1000);

    // 目标进程退出后：同 PID 的新进程（start time 不同）不得匹配。
    std::vector<drop::ContinuousTargetProcess> afterExit = {
        {42, 5000, "api", "/opt/api"},
    };
    EXPECT_TRUE(MatchContinuousProcessesBySelector(afterExit, assignment).empty());
}

TEST(ContinuousSessionManager, ExeAllInstancesFollowsNewInstances)
{
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "api", "/opt/api"},
        {43, 2000, "api", "/opt/api"},
    };
    ContinuousAssignment assignment;
    assignment.scope = "process";
    assignment.selectorMode = "exe_all_instances";
    assignment.selectorExe = "/opt/api";
    auto matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 2u);

    // 新实例出现后自动跟随。
    processes.push_back({44, 3000, "api", "/opt/api"});
    matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 3u);
}

TEST(ContinuousSessionManager, CgroupSelectorMatchesGroupMembers)
{
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "python", "/usr/bin/python3", "/system.slice/docker-abc123.scope", "abc123def456"},
        {43, 2000, "python", "/usr/bin/python3", "/system.slice/docker-abc123.scope", "abc123def456"},
        {44, 3000, "nginx", "/usr/sbin/nginx", "/system.slice/nginx.service", ""},
    };
    ContinuousAssignment assignment;
    assignment.scope = "process";
    assignment.selectorMode = "cgroup";
    assignment.selectorCgroup = "/system.slice/docker-abc123.scope";
    auto matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 2u);
    EXPECT_EQ(matches[0].pid, 42);
    EXPECT_EQ(matches[1].pid, 43);

    // A nested cgroup remains part of the selected cgroup subtree.
    processes.push_back({45, 4000, "worker", "/system.slice/docker-abc123.scope/child", "abc123def456"});
    matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 3u);
}

TEST(ContinuousSessionManager, ContainerIdSelectorMatchesParsedContainer)
{
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "python", "/usr/bin/python3", "/docker/abc123def456", "abc123def456"},
        {43, 2000, "nginx", "/usr/sbin/nginx", "/system.slice/nginx.service", ""},
    };
    ContinuousAssignment assignment;
    assignment.scope = "process";
    assignment.selectorMode = "container_id";
    assignment.selectorContainerId = "abc123def456";
    auto matches = MatchContinuousProcessesBySelector(processes, assignment);
    ASSERT_EQ(matches.size(), 1u);
    EXPECT_EQ(matches[0].pid, 42);
}

// 阶段六：ParseAssignments 解析 selector_mode 与 base64 的 selector_params。
TEST(ContinuousSessionManager, ParsesSelectorModeAndParams)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    // selector_params 是 Go []byte jsonb 的 base64（{"pid":42,"process_start_ms":1000,"exe":"/opt/api"}）。
    const std::string paramsB64 = "eyJwaWQiOjQyLCJwcm9jZXNzX3N0YXJ0X21zIjoxMDAwLCJleGUiOiIvb3B0L2FwaSJ9";
    const std::string response =
        R"({"code":0,"data":{"revision":11,"assignments":[{"sid":"cps-sel","scope":"process","selector_exe":"/opt/api","selector_mode":"pid_instance","selector_params":")" +
        paramsB64 +
        R"(","desired_state":"running","revision":11}]}})";
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::ParseAssignments(manager, response, &assignments, &revision));
    ASSERT_EQ(assignments.size(), 1u);
    EXPECT_EQ(assignments[0].selectorMode, "pid_instance");
    EXPECT_EQ(assignments[0].selectorPid, 42);
    EXPECT_EQ(assignments[0].selectorProcessStartMs, 1000);
    EXPECT_EQ(assignments[0].selectorExe, "/opt/api");
}

// 阶段六：历史 all_instances 归一化为 exe_all_instances。
TEST(ContinuousSessionManager, NormalizesLegacyAllInstancesMode)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    const std::string response =
        R"({"code":0,"data":{"revision":12,"assignments":[{"sid":"cps-legacy","scope":"process","selector_exe":"/opt/api","selector_mode":"all_instances","desired_state":"running","revision":12}]}})";
    std::vector<ContinuousAssignment> assignments;
    uint64_t revision = 0;
    ASSERT_TRUE(ContinuousSessionManagerTestAccess::ParseAssignments(manager, response, &assignments, &revision));
    ASSERT_EQ(assignments.size(), 1u);
    EXPECT_EQ(assignments[0].selectorMode, "exe_all_instances");
}

// 阶段六：Reconcile 上报进程快照携带 cgroup/container_id。
TEST(ContinuousSessionManager, ReconcileBodyCarriesCgroupAndContainerId)
{
    AgentConfig config;
    config.ipAddr = "127.0.0.1";
    config.hostname = "test-host";
    config.uid = "agent-test";
    std::atomic<bool> agentRunning{true};
    ContinuousSessionManager manager(config, "http://127.0.0.1:8191", "agent-test", agentRunning);
    ContinuousSessionManagerTestAccess::AddRuntimeReport(manager, "cps-cg", "running", "strict", "");
    std::vector<drop::ContinuousTargetProcess> processes = {
        {42, 1000, "python", "/usr/bin/python3", "/system.slice/docker-abc123def456.scope", "abc123def456"},
        {43, 2000, "nginx", "/usr/sbin/nginx", "/system.slice/nginx.service", ""},
    };
    const std::string body = ContinuousSessionManagerTestAccess::BuildReconcileBody(manager, processes);
    // 进程条目必须携带 cgroup_path / container_id（Agent 上报契约）。
    EXPECT_NE(body.find("\"cgroup_path\":\"/system.slice/docker-abc123def456.scope\""), std::string::npos);
    EXPECT_NE(body.find("\"container_id\":\"abc123def456\""), std::string::npos);
    EXPECT_NE(body.find("\"cgroup_path\":\"/system.slice/nginx.service\""), std::string::npos);
    EXPECT_NE(body.find("\"container_id\":\"\""), std::string::npos);
}

// 阶段六：/proc/<pid>/cgroup 解析（v2 与 v1 格式）。
TEST(ContinuousSessionManager, ParsesCgroupPathFromProc)
{
    // 当前测试进程的 cgroup 一定存在且以 / 开头（v2）或可解析（v1）。
    const std::string path = process_cgroup_path(static_cast<int>(::getpid()));
    EXPECT_FALSE(path.empty());
    EXPECT_EQ(path[0], '/');
}

// 阶段六：container ID 提取（docker/containerd/CRI-O/kubepods/systemd scope）。
TEST(ContinuousSessionManager, ExtractsContainerIdFromCgroupPaths)
{
    EXPECT_EQ(extract_container_id("/docker/abc123def456"), "abc123def456");
    EXPECT_EQ(extract_container_id("/kubepods/burstable/pod123/docker/abc123def456"), "abc123def456");
    EXPECT_EQ(extract_container_id("/kubepods/burstable/pod123/containerd/abc123def456"), "abc123def456");
    EXPECT_EQ(extract_container_id("/kubepods/burstable/pod123/cri-o-abc123def456"), "abc123def456");
    EXPECT_EQ(extract_container_id("/system.slice/docker-abc123def456.scope"), "abc123def456");
    EXPECT_EQ(extract_container_id("/system.slice/containerd-abc123def456.scope"), "abc123def456");
    EXPECT_EQ(extract_container_id("/system.slice/crio-abc123def456.scope"), "abc123def456");
    // 非容器路径 / 无法识别 → 空。
    EXPECT_EQ(extract_container_id("/system.slice/nginx.service"), "");
    EXPECT_EQ(extract_container_id("/user.slice/user-1000.slice/session-1.scope"), "");
    EXPECT_EQ(extract_container_id(""), "");
    // 过短 ID 不识别。
    EXPECT_EQ(extract_container_id("/docker/abc"), "");
}

} // namespace drop_agent
