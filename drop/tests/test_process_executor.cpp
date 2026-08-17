// ============================================================
// tests/test_process_executor.cpp — TimedProcessPoller 超时/取消状态机单测
// ============================================================
// 用 FakeProcessExecutor + FakeClock 驱动，不真的 fork/sleep，覆盖：
// 正常退出不发信号、超时后 SIGTERM->grace->SIGKILL 两阶段升级、
// ForceStop 幂等。
// ============================================================

#include "common/ProcessExecutor.h"

#include <csignal>
#include <gtest/gtest.h>
#include <vector>

using namespace drop;

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

    // 按顺序回放预设的 Poll() 结果；序列耗尽后重复最后一个元素。
    class FakeProcessExecutor : public ProcessExecutor
    {
    public:
        std::vector<PollOutcome> pollSequence;
        size_t pollIndex = 0;
        std::vector<int> signalsSent;
        int waitBlockingCalls = 0;
        PollOutcome waitBlockingResult{PollState::kExited, 0, 0};

        bool Start(const ExecArgs &, ExecHandle *out, std::string *) override
        {
            out->pid = 4242;
            return true;
        }
        PollOutcome Poll(const ExecHandle &) override
        {
            if (pollIndex < pollSequence.size())
                return pollSequence[pollIndex++];
            return pollSequence.empty() ? PollOutcome{PollState::kRunning, -1, 0} : pollSequence.back();
        }
        void SendSignal(const ExecHandle &, int sig) override
        {
            signalsSent.push_back(sig);
        }
        PollOutcome WaitBlocking(const ExecHandle &) override
        {
            waitBlockingCalls++;
            return waitBlockingResult;
        }
    };

} // namespace

TEST(TimedProcessPoller, NormalExitDoesNotSendSignal)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    exec.pollSequence = {{PollState::kRunning, -1, 0}, {PollState::kExited, 0, 0}};

    TimedProcessPoller poller(&exec, &clock, /*timeoutSec=*/60, /*gracePeriodSec=*/5);
    ExecHandle handle;
    std::string err;
    ASSERT_TRUE(exec.Start({}, &handle, &err));
    poller.Attach(handle);

    auto o1 = poller.Poll();
    EXPECT_EQ(o1.state, PollState::kRunning);
    auto o2 = poller.Poll();
    EXPECT_EQ(o2.state, PollState::kExited);
    EXPECT_EQ(o2.exitCode, 0);
    EXPECT_TRUE(exec.signalsSent.empty());
    EXPECT_FALSE(poller.TimedOut());
}

TEST(TimedProcessPoller, TimeoutEscalatesToSigtermThenSigkill)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    exec.pollSequence = {{PollState::kRunning, -1, 0}, {PollState::kRunning, -1, 0}};
    exec.waitBlockingResult = {PollState::kSignaled, -1, SIGKILL};

    TimedProcessPoller poller(&exec, &clock, /*timeoutSec=*/10, /*gracePeriodSec=*/5);
    ExecHandle handle;
    std::string err;
    ASSERT_TRUE(exec.Start({}, &handle, &err));
    poller.Attach(handle);

    auto o1 = poller.Poll(); // 还没到 deadline
    EXPECT_EQ(o1.state, PollState::kRunning);
    EXPECT_TRUE(exec.signalsSent.empty());

    clock.Advance(std::chrono::seconds(11)); // 超过 timeoutSec
    auto o2 = poller.Poll();                 // 触发 SIGTERM
    EXPECT_EQ(o2.state, PollState::kRunning);
    EXPECT_TRUE(poller.TimedOut());
    ASSERT_EQ(exec.signalsSent.size(), 1u);
    EXPECT_EQ(exec.signalsSent[0], SIGTERM);

    clock.Advance(std::chrono::seconds(6)); // 超过 gracePeriodSec
    auto o3 = poller.Poll();                // 触发 SIGKILL + WaitBlocking
    EXPECT_EQ(o3.state, PollState::kSignaled);
    ASSERT_EQ(exec.signalsSent.size(), 2u);
    EXPECT_EQ(exec.signalsSent[1], SIGKILL);
    EXPECT_EQ(exec.waitBlockingCalls, 1);

    // 已终态后再 Poll：直接返回缓存结果，不重复发信号/wait
    auto o4 = poller.Poll();
    EXPECT_EQ(o4.state, PollState::kSignaled);
    EXPECT_EQ(exec.signalsSent.size(), 2u);
    EXPECT_EQ(exec.waitBlockingCalls, 1);
}

TEST(TimedProcessPoller, ForceStopIsIdempotent)
{
    FakeClock clock;
    FakeProcessExecutor exec;
    // ForceStop 第一次内部 Poll 就发现已退出：不应该再发任何信号。
    exec.pollSequence = {{PollState::kExited, 0, 0}};

    TimedProcessPoller poller(&exec, &clock, /*timeoutSec=*/60, /*gracePeriodSec=*/0);
    ExecHandle handle;
    std::string err;
    ASSERT_TRUE(exec.Start({}, &handle, &err));
    poller.Attach(handle);

    auto o1 = poller.ForceStop();
    EXPECT_EQ(o1.state, PollState::kExited);
    EXPECT_TRUE(exec.signalsSent.empty());

    auto o2 = poller.ForceStop(); // 幂等：直接返回缓存，不再调用 executor
    EXPECT_EQ(o2.state, PollState::kExited);
    EXPECT_EQ(exec.waitBlockingCalls, 0);
}
