// ============================================================
// agent/runners/PprofRunner.cpp — 实现
// ============================================================
// 逐字对齐旧 common/PprofProfiler.cpp::run_pprof()，包括 curl 专属的
// exitCode 7(连接失败)/28(超时) 特判和 gzip magic byte 校验。
// ============================================================

#include "agent/runners/PprofRunner.h"
#include "agent/RunnerUtils.h"

#include <fstream>

using namespace std;

namespace drop_agent
{

    namespace
    {
        bool HasGzipMagic(const string &path)
        {
            ifstream profile(path, ios::binary);
            unsigned char magic[2]{};
            profile.read(reinterpret_cast<char *>(magic), sizeof(magic));
            return profile && magic[0] == 0x1f && magic[1] == 0x8b;
        }
    } // namespace

    string PprofRunner::BuildUrl(const TaskContext &ctx) const
    {
        uint64_t duration = ctx.task.sampleargv().duration();
        if (duration == 0)
            duration = 10;

        string fullUrl = ctx.task.pprofurl();
        if (fullUrl.empty())
            fullUrl = ctx.task.sampleargv().event(); // 兼容 pprofUrl 字段引入之前创建的任务
        if (fullUrl.empty())
            return "";
        const string seconds = "seconds=";
        if (fullUrl.find(seconds) == string::npos)
            fullUrl += (fullUrl.find('?') == string::npos ? "?" : "&") + seconds + to_string(duration);
        return fullUrl;
    }

    ValidationResult PprofRunner::Validate(const TaskContext &ctx)
    {
        if (BuildUrl(ctx).empty())
            return {false, GetErrorCode(-4, "pprof"), GetErrorMessage(-4, "pprof", ctx.task)};
        return {true, "", ""};
    }

    PrepareResult PprofRunner::Prepare(TaskContext &ctx)
    {
        outputPath_ = ctx.taskDir + ".pb.gz";
        url_ = BuildUrl(ctx);
        return {true, "", ""};
    }

    StartResult PprofRunner::Start(TaskContext &ctx)
    {
        uint64_t duration = ctx.task.sampleargv().duration();
        if (duration == 0)
            duration = 10;
        string maxTime = to_string(duration + 10);

        drop::ExecArgs args;
        args.argv.push_back("curl");
        args.argv.push_back("-s");
        args.argv.push_back("--fail");
        args.argv.push_back("--show-error");
        args.argv.push_back("-o");
        args.argv.push_back(outputPath_);
        args.argv.push_back("--connect-timeout");
        args.argv.push_back("5");
        args.argv.push_back("--max-time");
        args.argv.push_back(maxTime);
        args.argv.push_back("-L");
        args.argv.push_back(url_);

        drop::ExecHandle handle;
        string err;
        if (!ctx.executor->Start(args, &handle, &err))
            return {false, "RUNNER_NOT_AVAILABLE", err};

        uint32_t timeout = ctx.task.timeoutsec();
        if (timeout == 0)
            timeout = 60;
        poller_.reset(new drop::TimedProcessPoller(ctx.executor, ctx.clock, timeout));
        poller_->Attach(handle);
        return {true, "", ""};
    }

    PollResult PprofRunner::Poll(TaskContext &)
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
            int exitCode = o.exitCode;
            if (exitCode == 7) // curl: 无法连接
            {
                lastResultCode_ = -4;
                return {PollStatus::kFailed, lastResultCode_};
            }
            if (exitCode == 28) // curl: --max-time 超时
            {
                lastResultCode_ = -3;
                return {PollStatus::kFailed, lastResultCode_};
            }
            if (exitCode != 0)
            {
                lastResultCode_ = exitCode;
                return {PollStatus::kFailed, lastResultCode_};
            }
            if (!HasGzipMagic(outputPath_))
            {
                lastResultCode_ = -6;
                return {PollStatus::kFailed, lastResultCode_};
            }
            lastResultCode_ = 0;
            return {PollStatus::kSucceeded, 0};
        }
        lastResultCode_ = -6;
        return {PollStatus::kFailed, lastResultCode_};
    }

    StopResult PprofRunner::Stop(TaskContext &, StopReason)
    {
        if (poller_)
            poller_->ForceStop();
        return {true};
    }

    CollectResult PprofRunner::Collect(TaskContext &)
    {
        CollectResult r;
        r.resultCode = lastResultCode_;
        r.outputPath = outputPath_;
        r.profilerName = "pprof";
        r.remoteKeyHint = RemoteKeyFor(r.profilerName);
        r.contentType = ContentTypeFor(r.profilerName);
        r.partial = lastResultCode_ != 0 && FileExistsNonEmpty(outputPath_);
        return r;
    }

    CleanupResult PprofRunner::Cleanup(TaskContext &) { return {true}; }

} // namespace drop_agent
