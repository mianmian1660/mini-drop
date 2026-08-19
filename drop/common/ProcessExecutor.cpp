// ============================================================
// common/ProcessExecutor.cpp — 实现
// ============================================================

#include "common/ProcessExecutor.h"

#include <cerrno>
#include <cstring>
#include <fcntl.h>
#include <iostream>
#include <signal.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>

using namespace std;

namespace drop
{

    bool RealProcessExecutor::Start(const ExecArgs &args, ExecHandle *out, std::string *err)
    {
        // argv/重定向路径已经在调用方（父进程）构建完毕，这里只做
        // fork + 子进程内的 async-signal-safe 操作。
        vector<const char *> argv;
        argv.reserve(args.argv.size() + 1);
        for (const auto &a : args.argv)
            argv.push_back(a.c_str());
        argv.push_back(nullptr);

        int stdoutFd = -1;
        int stderrFd = -1;
        if (!args.stdoutRedirectPath.empty())
        {
            stdoutFd = ::open(args.stdoutRedirectPath.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
            if (stdoutFd < 0)
            {
                if (err)
                    *err = "打开 stdout 重定向文件失败: " + args.stdoutRedirectPath + " (" + strerror(errno) + ")";
                return false;
            }
        }
        if (!args.stderrRedirectPath.empty())
        {
            if (args.stderrRedirectPath == args.stdoutRedirectPath && stdoutFd >= 0)
            {
                stderrFd = stdoutFd;
            }
            else
            {
                stderrFd = ::open(args.stderrRedirectPath.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
                if (stderrFd < 0)
                {
                    if (err)
                        *err = "打开 stderr 重定向文件失败: " + args.stderrRedirectPath + " (" + strerror(errno) + ")";
                    if (stdoutFd >= 0)
                        ::close(stdoutFd);
                    return false;
                }
            }
        }

        pid_t pid = fork();
        if (pid < 0)
        {
            if (err)
                *err = string("fork 失败: ") + strerror(errno);
            if (stdoutFd >= 0)
                ::close(stdoutFd);
            if (stderrFd >= 0 && stderrFd != stdoutFd)
                ::close(stderrFd);
            return false;
        }

        if (pid == 0)
        {
            // ==== 子进程：只允许 async-signal-safe 调用，不做任何
            //      std::string/std::vector 构造、malloc、iostream。 ====
            setpgid(0, 0);
            if (stdoutFd >= 0)
                dup2(stdoutFd, STDOUT_FILENO);
            if (stderrFd >= 0)
                dup2(stderrFd, STDERR_FILENO);
            execvp(argv[0], const_cast<char *const *>(argv.data()));
            _exit(127); // execvp 失败（可执行文件不存在等）
        }

        // ==== 父进程 ====
        if (stdoutFd >= 0)
            ::close(stdoutFd);
        if (stderrFd >= 0 && stderrFd != stdoutFd)
            ::close(stderrFd);

        out->pid = pid;
        out->startedAt = chrono::steady_clock::now();
        return true;
    }

    PollOutcome RealProcessExecutor::Poll(const ExecHandle &handle)
    {
        int status = 0;
        pid_t result = waitpid(handle.pid, &status, WNOHANG);
        if (result == 0)
            return {PollState::kRunning, -1, 0};
        if (result < 0)
            return {PollState::kWaitError, -1, 0};
        if (WIFEXITED(status))
            return {PollState::kExited, WEXITSTATUS(status), 0};
        if (WIFSIGNALED(status))
            return {PollState::kSignaled, -1, WTERMSIG(status)};
        return {PollState::kWaitError, -1, 0};
    }

    void RealProcessExecutor::SendSignal(const ExecHandle &handle, int sig)
    {
        killpg(handle.pid, sig);
    }

    PollOutcome RealProcessExecutor::WaitBlocking(const ExecHandle &handle)
    {
        int status = 0;
        pid_t result = waitpid(handle.pid, &status, 0);
        if (result < 0)
            return {PollState::kWaitError, -1, 0};
        if (WIFEXITED(status))
            return {PollState::kExited, WEXITSTATUS(status), 0};
        if (WIFSIGNALED(status))
            return {PollState::kSignaled, -1, WTERMSIG(status)};
        return {PollState::kWaitError, -1, 0};
    }

    TimedProcessPoller::TimedProcessPoller(ProcessExecutor *executor, Clock *clock,
                                            uint32_t timeoutSec, uint32_t gracePeriodSec,
                                            PidRegistry *pidRegistry)
        : executor_(executor), clock_(clock), pidRegistry_(pidRegistry), gracePeriodSec_(gracePeriodSec)
    {
        deadline_ = clock_->Now() + chrono::seconds(timeoutSec);
    }

    void TimedProcessPoller::Attach(ExecHandle handle)
    {
        handle_ = handle;
        if (pidRegistry_)
            pidRegistry_->Register(handle_.pid);
    }

    PollOutcome TimedProcessPoller::MarkReaped(PollOutcome o)
    {
        reaped_ = true;
        lastOutcome_ = o;
        if (pidRegistry_)
            pidRegistry_->Unregister(handle_.pid);
        return lastOutcome_;
    }

    PollOutcome TimedProcessPoller::Poll()
    {
        if (reaped_)
            return lastOutcome_;

        if (sigtermSent_)
        {
            auto o = executor_->Poll(handle_);
            if (o.state != PollState::kRunning)
                return MarkReaped(o);
            if (clock_->Now() >= sigtermSentAt_ + chrono::seconds(gracePeriodSec_))
            {
                executor_->SendSignal(handle_, SIGKILL);
                return MarkReaped(executor_->WaitBlocking(handle_));
            }
            return o;
        }

        auto o = executor_->Poll(handle_);
        if (o.state != PollState::kRunning)
            return MarkReaped(o);
        if (clock_->Now() >= deadline_)
        {
            timedOut_ = true;
            executor_->SendSignal(handle_, SIGTERM);
            sigtermSent_ = true;
            sigtermSentAt_ = clock_->Now();
        }
        return o;
    }

    PollOutcome TimedProcessPoller::ForceStop()
    {
        if (reaped_)
            return lastOutcome_;

        auto o = executor_->Poll(handle_);
        if (o.state != PollState::kRunning)
            return MarkReaped(o);

        executor_->SendSignal(handle_, SIGTERM);
        auto graceDeadline = clock_->Now() + chrono::seconds(gracePeriodSec_);
        while (clock_->Now() < graceDeadline)
        {
            o = executor_->Poll(handle_);
            if (o.state != PollState::kRunning)
                return MarkReaped(o);
            this_thread::sleep_for(chrono::milliseconds(200));
        }

        executor_->SendSignal(handle_, SIGKILL);
        return MarkReaped(executor_->WaitBlocking(handle_));
    }

} // namespace drop
