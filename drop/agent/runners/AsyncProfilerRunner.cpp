// ============================================================
// agent/runners/AsyncProfilerRunner.cpp — 实现
// ============================================================
// 逐字对齐旧 common/AsyncProfilerProfiler.cpp::run_async_profiler()。
// ============================================================

#include "agent/runners/AsyncProfilerRunner.h"
#include "agent/RunnerUtils.h"
#include "common/Process.h" // drop::pid_exists

using namespace std;

namespace drop_agent
{

    ValidationResult AsyncProfilerRunner::Validate(const TaskContext &ctx)
    {
        int targetPid = ctx.task.sampleargv().pid();
        if (targetPid <= 0 || !drop::pid_exists(targetPid))
        {
            ValidationResult r;
            r.ok = false;
            r.resultCode = -4;
            r.errorCode = GetErrorCode(-4, "async-profiler");
            r.errorMessage = GetErrorMessage(-4, "async-profiler", ctx.task);
            return r;
        }
        ValidationResult r;
        r.ok = true;
        return r;
    }

    PrepareResult AsyncProfilerRunner::Prepare(TaskContext &ctx)
    {
        outputPath_ = ctx.taskDir + ".collapsed";
        PrepareResult r;
        r.ok = true;
        return r;
    }

    StartResult AsyncProfilerRunner::Start(TaskContext &ctx)
    {
        drop::ExecArgs args;
        args.argv.push_back("asprof");

        string event = ctx.task.sampleargv().event();
        if (event.empty())
            event = "cpu";
        args.argv.push_back("-e");
        args.argv.push_back(event);

        uint64_t dur = ctx.task.sampleargv().duration();
        if (dur == 0)
            dur = 10;
        args.argv.push_back("-d");
        args.argv.push_back(to_string(dur));

        // 统一输出折叠栈：便于分析容器解析，不依赖匹配版本的 JDK/jfr 工具。
        args.argv.push_back("-o");
        args.argv.push_back("collapsed");

        args.argv.push_back("-f");
        args.argv.push_back(outputPath_);

        args.argv.push_back(to_string(ctx.task.sampleargv().pid()));

        drop::ExecHandle handle;
        string err;
        if (!ctx.executor->Start(args, &handle, &err))
        {
            StartResult r;
            r.ok = false;
            r.resultCode = -1;
            r.errorCode = "RUNNER_NOT_AVAILABLE";
            r.errorMessage = err;
            return r;
        }

        uint32_t timeout = ctx.task.timeoutsec();
        if (timeout == 0)
            timeout = 60;
        poller_.reset(new drop::TimedProcessPoller(ctx.executor, ctx.clock, timeout, /*gracePeriodSec=*/5, ctx.pidRegistry));
        poller_->Attach(handle);
        StartResult r;
        r.ok = true;
        return r;
    }

    PollResult AsyncProfilerRunner::Poll(TaskContext &)
    {
        auto o = poller_->Poll();
        if (o.state == drop::PollState::kRunning)
            return {PollStatus::kRunning, 0};
        if (poller_->TimedOut())
        {
            lastResultCode_ = -3;
            return {PollStatus::kFailed, lastResultCode_};
        }
        if (o.state == drop::PollState::kSignaled)
        {
            lastResultCode_ = -5;
            return {PollStatus::kFailed, lastResultCode_};
        }
        if (o.state == drop::PollState::kExited)
        {
            if (o.exitCode == 0 && !FileExistsNonEmpty(outputPath_))
            {
                lastResultCode_ = -6;
                return {PollStatus::kFailed, lastResultCode_};
            }
            lastResultCode_ = o.exitCode;
            return {o.exitCode == 0 ? PollStatus::kSucceeded : PollStatus::kFailed, lastResultCode_};
        }
        lastResultCode_ = -2;
        return {PollStatus::kFailed, lastResultCode_};
    }

    StopResult AsyncProfilerRunner::Stop(TaskContext &, StopReason)
    {
        if (poller_)
            poller_->ForceStop();
        return {true};
    }

    CollectResult AsyncProfilerRunner::Collect(TaskContext &)
    {
        CollectResult r;
        r.resultCode = lastResultCode_;
        r.outputPath = outputPath_;
        r.profilerName = "async-profiler";
        r.remoteKeyHint = RemoteKeyFor(r.profilerName);
        r.contentType = ContentTypeFor(r.profilerName);
        r.partial = lastResultCode_ != 0 && FileExistsNonEmpty(outputPath_);
        return r;
    }

    CleanupResult AsyncProfilerRunner::Cleanup(TaskContext &) { return {true}; }

} // namespace drop_agent
