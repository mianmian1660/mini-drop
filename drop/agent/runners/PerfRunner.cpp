// ============================================================
// agent/runners/PerfRunner.cpp — 实现
// ============================================================
// argv 构建逻辑逐字对齐旧 common/Perf.cpp::run_perf()，只是从
// "子进程内构建 argv" 改成"fork 之前、父进程内构建 argv"（fork 安全性
// 要求），并把阻塞 waitpid 换成 TimedProcessPoller 的非阻塞轮询。
// ============================================================

#include "agent/runners/PerfRunner.h"
#include "agent/RunnerUtils.h"
#include "common/Process.h" // drop::pid_exists

#include <cstdlib>

using namespace std;

namespace drop_agent
{

    namespace
    {
        string ResolvePerfBin()
        {
            const char *configured = getenv("DROP_PERF_BIN");
            if (configured && configured[0] != '\0')
                return string(configured);
            return "perf";
        }
    } // namespace

    ValidationResult PerfRunner::Validate(const TaskContext &ctx)
    {
        int targetPid = ctx.task.sampleargv().pid();
        if (targetPid > 0 && !drop::pid_exists(targetPid))
        {
            ValidationResult r;
            r.ok = false;
            r.resultCode = -4;
            r.errorCode = GetErrorCode(-4, "perf");
            r.errorMessage = GetErrorMessage(-4, "perf", ctx.task);
            return r;
        }
        ValidationResult r;
        r.ok = true;
        return r;
    }

    PrepareResult PerfRunner::Prepare(TaskContext &ctx)
    {
        outputPath_ = ctx.taskDir;
        PrepareResult r;
        r.ok = true;
        return r;
    }

    StartResult PerfRunner::Start(TaskContext &ctx)
    {
        drop::ExecArgs args;
        string perfBin = ResolvePerfBin();
        args.argv.push_back(perfBin);
        args.argv.push_back("record");
        args.argv.push_back("--no-buildid-cache");

        string event = ctx.task.sampleargv().event();
        if (!event.empty())
        {
            args.argv.push_back("-e");
            args.argv.push_back(event);
        }
        args.argv.push_back("-F");
        args.argv.push_back(to_string(ctx.task.sampleargv().hz()));

        string cg = ctx.task.sampleargv().callgraph();
        if (!cg.empty())
        {
            args.argv.push_back("--call-graph");
            args.argv.push_back(cg);
        }

        args.argv.push_back("-o");
        args.argv.push_back(outputPath_);

        int targetPid = ctx.task.sampleargv().pid();
        if (targetPid > 0)
        {
            args.argv.push_back("-p");
            args.argv.push_back(to_string(targetPid));
        }
        else
        {
            args.argv.push_back("-a");
        }

        args.argv.push_back("--");
        args.argv.push_back("sleep");
        uint64_t dur = ctx.task.sampleargv().duration();
        if (dur == 0)
            dur = 10;
        args.argv.push_back(to_string(dur));

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
        poller_.reset(new drop::TimedProcessPoller(ctx.executor, ctx.clock, timeout));
        poller_->Attach(handle);
        StartResult r;
        r.ok = true;
        return r;
    }

    PollResult PerfRunner::Poll(TaskContext &)
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

    StopResult PerfRunner::Stop(TaskContext &, StopReason)
    {
        if (poller_)
            poller_->ForceStop();
        return {true};
    }

    CollectResult PerfRunner::Collect(TaskContext &)
    {
        CollectResult r;
        r.resultCode = lastResultCode_;
        r.outputPath = outputPath_;
        r.profilerName = "perf";
        r.remoteKeyHint = RemoteKeyFor(r.profilerName);
        r.contentType = ContentTypeFor(r.profilerName);
        r.partial = lastResultCode_ != 0 && FileExistsNonEmpty(outputPath_);
        return r;
    }

    CleanupResult PerfRunner::Cleanup(TaskContext &) { return {true}; }

} // namespace drop_agent
