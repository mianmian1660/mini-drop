// ============================================================
// common/ProcessExecutor.h — 非阻塞子进程执行 + 超时/取消状态机
// ============================================================
// 指南 5.8 节要求 TaskContext 暴露 ProcessExecutor。这里把 4 个采集器
// （perf/async-profiler/pprof/eBPF）原来各自重复一份的"fork + WNOHANG
// 轮询 + 超时两阶段 SIGTERM/SIGKILL"状态机收敛成一份 TimedProcessPoller，
// 4 个 Runner 子类只需要 compose 一个实例，不用各写一份。
//
// fork 安全性：ExecArgs.argv 必须在调用 Start() 之前、在父进程里构建
// 完毕。RealProcessExecutor::Start() 的子进程分支只做 async-signal-safe
// 调用（setpgid/dup2/execve/_exit），不做任何 std::string/std::vector
// 构造、new/malloc——这是从"只有主线程会 fork"变成"Worker 线程会 fork"
// 之后必须遵守的约束，否则子进程可能卡死在继承的 malloc 锁上。
// ============================================================

#pragma once

#include "common/Clock.h"
#include "common/PidRegistry.h"

#include <cstdint>
#include <string>
#include <vector>

namespace drop
{

    struct ExecArgs
    {
        std::vector<std::string> argv;      // argv[0] = 可执行文件名/路径
        std::string stdoutRedirectPath;     // 空 = 继承父进程 stdout
        std::string stderrRedirectPath;     // 空 = 继承父进程 stderr
    };

    struct ExecHandle
    {
        pid_t pid = -1;
        std::chrono::steady_clock::time_point startedAt{};
    };

    enum class PollState
    {
        kRunning,
        kExited,
        kSignaled,
        kWaitError,
    };

    struct PollOutcome
    {
        PollState state = PollState::kRunning;
        int exitCode = -1;
        int termSignal = 0;
    };

    /// 非阻塞子进程执行的抽象接口。生产代码用 RealProcessExecutor；
    /// 单元测试用 FakeProcessExecutor（见 tests/test_process_executor.cpp）
    /// 驱动 TimedProcessPoller，不需要真的 fork/sleep。
    class ProcessExecutor
    {
    public:
        virtual ~ProcessExecutor() = default;

        /// fork + 子进程内 setpgid(独立进程组)/可选 dup2 重定向/execve，
        /// 非阻塞，立即返回。
        virtual bool Start(const ExecArgs &args, ExecHandle *out, std::string *err) = 0;

        /// 单次 WNOHANG waitpid，不做超时判断/信号升级——那是
        /// TimedProcessPoller 的职责。
        virtual PollOutcome Poll(const ExecHandle &handle) = 0;

        /// killpg(handle.pid, sig)
        virtual void SendSignal(const ExecHandle &handle, int sig) = 0;

        /// 阻塞 waitpid(pid, &status, 0)，仅在 SendSignal(SIGKILL) 之后
        /// 做最终收割时使用。
        virtual PollOutcome WaitBlocking(const ExecHandle &handle) = 0;
    };

    class RealProcessExecutor : public ProcessExecutor
    {
    public:
        bool Start(const ExecArgs &args, ExecHandle *out, std::string *err) override;
        PollOutcome Poll(const ExecHandle &handle) override;
        void SendSignal(const ExecHandle &handle, int sig) override;
        PollOutcome WaitBlocking(const ExecHandle &handle) override;
    };

    /// 收敛"超时两阶段终止"状态机（对应指南 4.4 步骤 1-4：标记 canceling/
    /// timeout → 温和终止信号 → grace period → 强制终止）。
    /// 每个 Runner 子类在 Start() 成功后 Attach() 一次，之后在 Poll() 循环
    /// 里反复调用 Poll()；需要主动终止（取消/关闭）时调用 ForceStop()。
    /// ForceStop() 是幂等的：进程已经被收割后重复调用直接返回上次结果，
    /// 不会重复发信号/重复 waitpid。
    class TimedProcessPoller
    {
    public:
        /// pidRegistry 为 nullptr 时（如单测里的 FakeProcessExecutor 驱动场景）
        /// 完全不登记，行为和 Phase 5 之前一致。
        TimedProcessPoller(ProcessExecutor *executor, Clock *clock,
                            uint32_t timeoutSec, uint32_t gracePeriodSec = 5,
                            PidRegistry *pidRegistry = nullptr);

        void Attach(ExecHandle handle);

        /// 普通 tick：非阻塞检查子进程是否结束；超时后自动发 SIGTERM，
        /// 之后的 tick 里若已过 grace period 仍未退出，则发 SIGKILL 并
        /// 阻塞收割。
        PollOutcome Poll();

        /// 主动终止（取消/Agent 关闭）：立即走 SIGTERM → 轮询等待 grace
        /// period → 仍未退出则 SIGKILL，阻塞直到进程被收割。幂等。
        PollOutcome ForceStop();

        bool TimedOut() const { return timedOut_; }
        pid_t Pid() const { return handle_.pid; }

    private:
        /// 统一的"进程已收割"落点：设 reaped_/lastOutcome_，并在 pidRegistry_
        /// 非空时把 pid 从登记表摘牌。Poll()/ForceStop() 里所有到达终态的
        /// 分支都走这里，不再各自重复设置。
        PollOutcome MarkReaped(PollOutcome o);

        ProcessExecutor *executor_;
        Clock *clock_;
        PidRegistry *pidRegistry_;
        ExecHandle handle_;
        std::chrono::steady_clock::time_point deadline_;
        uint32_t gracePeriodSec_;
        bool sigtermSent_ = false;
        std::chrono::steady_clock::time_point sigtermSentAt_{};
        bool reaped_ = false;
        bool timedOut_ = false;
        PollOutcome lastOutcome_;
    };

} // namespace drop
