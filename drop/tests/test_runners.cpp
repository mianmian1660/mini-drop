// ============================================================
// tests/test_runners.cpp — 4 个 Runner 子类的行为回归测试
// ============================================================
// 用 FakeProcessExecutor 驱动，不真的 fork perf/asprof/curl/bpftrace，
// 断言 resultCode/remoteKeyHint 映射和旧 run_*() 的约定一致，作为
// Phase 2 切换调用点时的回归防护网。
// ============================================================

#include "agent/RunnerUtils.h"
#include "agent/runners/AsyncProfilerRunner.h"
#include "agent/runners/BpfRunner.h"
#include "agent/runners/PerfRunner.h"
#include "agent/runners/PprofRunner.h"

#include <cstdio>
#include <fstream>
#include <gtest/gtest.h>

using namespace drop;
using namespace drop_agent;

namespace
{

    class FakeClock : public Clock
    {
    public:
        std::chrono::steady_clock::time_point Now() const override { return now_; }
        void Advance(std::chrono::seconds d) { now_ += d; }

    private:
        std::chrono::steady_clock::time_point now_ = std::chrono::steady_clock::now();
    };

    class FakeProcessExecutor : public ProcessExecutor
    {
    public:
        std::vector<PollOutcome> pollSequence;
        size_t pollIndex = 0;

        bool Start(const ExecArgs &, ExecHandle *out, std::string *) override
        {
            out->pid = 9999;
            return true;
        }
        PollOutcome Poll(const ExecHandle &) override
        {
            if (pollIndex < pollSequence.size())
                return pollSequence[pollIndex++];
            return pollSequence.empty() ? PollOutcome{PollState::kRunning, -1, 0} : pollSequence.back();
        }
        void SendSignal(const ExecHandle &, int) override {}
        PollOutcome WaitBlocking(const ExecHandle &) override
        {
            return {PollState::kExited, 0, 0};
        }
    };

    std::string WriteTempFile(const std::string &path, const std::string &content)
    {
        std::ofstream f(path);
        f << content;
        f.close();
        return path;
    }

} // namespace

TEST(PerfRunner, HappyPathProducesExpectedRemoteKey)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    exec.pollSequence = {{PollState::kExited, 0, 0}};

    TaskContext ctx;
    ctx.taskDir = "/tmp/test_perf_runner_output";
    ctx.executor = &exec;
    ctx.clock = &clock;
    ctx.task.set_taskid("t1");
    ctx.task.mutable_sampleargv()->set_hz(99);
    ctx.task.mutable_sampleargv()->set_duration(5);

    PerfRunner runner;
    ASSERT_TRUE(runner.Validate(ctx).ok);
    ASSERT_TRUE(runner.Prepare(ctx).ok);
    WriteTempFile(ctx.taskDir, "fake-perf-data"); // 模拟子进程已经产出文件
    ASSERT_TRUE(runner.Start(ctx).ok);

    auto poll = runner.Poll(ctx);
    EXPECT_EQ(poll.status, PollStatus::kSucceeded);

    auto collect = runner.Collect(ctx);
    EXPECT_EQ(collect.resultCode, 0);
    EXPECT_EQ(collect.remoteKeyHint, "perf.data");
    EXPECT_EQ(collect.contentType, "application/octet-stream");
    EXPECT_FALSE(collect.partial);

    std::remove(ctx.taskDir.c_str());
}

TEST(PerfRunner, TimeoutMapsToDashThree)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    exec.pollSequence = {{PollState::kRunning, -1, 0}};

    TaskContext ctx;
    ctx.taskDir = "/tmp/test_perf_runner_timeout";
    ctx.executor = &exec;
    ctx.clock = &clock;
    ctx.task.set_taskid("t2");
    ctx.task.set_timeoutsec(10);

    PerfRunner runner;
    ASSERT_TRUE(runner.Validate(ctx).ok);
    ASSERT_TRUE(runner.Prepare(ctx).ok);
    ASSERT_TRUE(runner.Start(ctx).ok);

    clock.Advance(std::chrono::seconds(11));
    auto poll = runner.Poll(ctx); // 触发 SIGTERM，仍 running（FakeExecutor 序列耗尽后重复 Running）
    EXPECT_EQ(poll.status, PollStatus::kRunning);

    clock.Advance(std::chrono::seconds(6)); // 超过 grace period(默认5s)
    poll = runner.Poll(ctx);                // 触发 SIGKILL + WaitBlocking
    // 对齐旧代码：一旦走了超时升级路径，无论最终 wait 到什么状态都固定
    // 报 -3（TASK_TIMEOUT），不看 WaitBlocking 具体返回的退出码。
    EXPECT_EQ(poll.status, PollStatus::kFailed);
    EXPECT_EQ(poll.resultCode, -3);

    auto collect = runner.Collect(ctx);
    EXPECT_EQ(collect.resultCode, -3);
}

TEST(AsyncProfilerRunner, ValidateRejectsMissingPid)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    TaskContext ctx;
    ctx.executor = &exec;
    ctx.clock = &clock;
    ctx.task.set_taskid("t3");
    // 不设置 pid（默认 0）

    AsyncProfilerRunner runner;
    auto v = runner.Validate(ctx);
    EXPECT_FALSE(v.ok);
    EXPECT_EQ(v.errorCode, "TARGET_NOT_FOUND");
    EXPECT_EQ(v.errorMessage, GetErrorMessage(-4, "async-profiler", ctx.task));
}

TEST(PprofRunner, ValidateRejectsEmptyUrl)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    TaskContext ctx;
    ctx.executor = &exec;
    ctx.clock = &clock;
    ctx.task.set_taskid("t4");
    // 不设置 pprofurl/event，URL 应该解析为空串

    PprofRunner runner;
    auto v = runner.Validate(ctx);
    EXPECT_FALSE(v.ok);
    EXPECT_EQ(v.errorCode, GetErrorCode(-4, "pprof"));
    EXPECT_EQ(v.errorMessage, GetErrorMessage(-4, "pprof", ctx.task));
}

TEST(BpfRunner, SignaledMapsToDashTwoNotDashFive)
{
    // 刻意保留的行为怪癖：BPF 被信号杀死映射到 -2，其余 3 个 Runner 是 -5。
    FakeClock clock;
    FakeProcessExecutor exec;
    exec.pollSequence = {{PollState::kSignaled, -1, 9}};

    TaskContext ctx;
    ctx.taskDir = "/tmp/test_bpf_runner_signaled";
    ctx.executor = &exec;
    ctx.clock = &clock;
    ctx.task.set_taskid("t5");
    ctx.task.mutable_sampleargv()->set_hz(99);
    ctx.task.mutable_sampleargv()->set_duration(5);

    BpfRunner runner;
    ASSERT_TRUE(runner.Validate(ctx).ok);
    ASSERT_TRUE(runner.Prepare(ctx).ok);
    ASSERT_TRUE(runner.Start(ctx).ok);

    auto poll = runner.Poll(ctx);
    EXPECT_EQ(poll.status, PollStatus::kFailed);
    EXPECT_EQ(poll.resultCode, -2);

    auto collect = runner.Collect(ctx);
    EXPECT_EQ(collect.resultCode, -2);
    EXPECT_EQ(collect.remoteKeyHint, "raw.bpf");
}

TEST(RawObjectKey, V2LayoutByDefault)
{
    EXPECT_EQ(drop_agent::RawObjectKey("t1", 7, "perf.data"),
              "tasks/t1/attempts/7/raw/perf.data");
    EXPECT_EQ(drop_agent::RawObjectKey("t1", 7, "profile.pb.gz"),
              "tasks/t1/attempts/7/raw/profile.pb.gz");
    EXPECT_EQ(drop_agent::RawObjectKey("t1", 7, "raw.bpf"),
              "tasks/t1/attempts/7/raw/raw.bpf");
    EXPECT_EQ(drop_agent::ManifestObjectKey("t1", 7),
              "tasks/t1/attempts/7/manifest.json");
}

TEST(RawObjectKey, FallsBackToLegacyWhenAttemptZeroOrInvalidBasename)
{
    // attempt_id=0：旧布局回退
    EXPECT_EQ(drop_agent::RawObjectKey("t1", 0, "perf.data"), "t1/perf.data");
    EXPECT_EQ(drop_agent::ManifestObjectKey("t1", 0), "t1/manifest.json");
    // 非法 basename：旧布局回退（不拼接 v2 路径）
    EXPECT_EQ(drop_agent::RawObjectKey("t1", 7, "../evil"), "t1/../evil");
}

TEST(ValidRemoteBasename, RejectsDotsSlashesAndEmpty)
{
    EXPECT_TRUE(drop_agent::ValidRemoteBasename("perf.data"));
    EXPECT_TRUE(drop_agent::ValidRemoteBasename("profile.pb.gz"));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename(""));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename("."));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename(".."));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename("a/b"));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename("a\\b"));
    EXPECT_FALSE(drop_agent::ValidRemoteBasename("a b"));
}
