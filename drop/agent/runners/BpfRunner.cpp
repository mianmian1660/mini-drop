// ============================================================
// agent/runners/BpfRunner.cpp — 实现
// ============================================================
// 逐字对齐旧 common/BpfProfiler.cpp。两处刻意保留的行为怪癖(为了 Phase 2
// 的逐字段回归比对)：
//   1. 子进程被信号杀死时 resultCode=-2（其余 3 个 Runner 是 -5）
//   2. bpftrace exitCode==127(execvp 失败) 才映射到 -5(BPFTRACE_UNAVAILABLE)
// 唯一的行为简化：不再用 "timeoutSec+5" 的自定义阈值和 1 秒 grace period
// 分别硬编码，而是复用 TimedProcessPoller，gracePeriodSec 显式传 1 以保留
// 原来"信号升级比其他采集器更快"的特性。
// ============================================================

#include "agent/runners/BpfRunner.h"
#include "agent/RunnerUtils.h"
#include "common/Process.h" // drop::pid_exists

#include <cctype>
#include <cstdio>
#include <fstream>
#include <unistd.h> // getpid

using namespace std;

namespace drop_agent
{

    BpfMode ParseBpfMode(const string &event)
    {
        if (event.empty())
            return BpfMode::CPU;
        string e = event;
        for (auto &c : e)
            c = tolower(static_cast<unsigned char>(c));
        if (e == "io" || e == "blk" || e == "block" || e == "disk")
            return BpfMode::IO_LATENCY;
        if (e == "sched" || e == "schedule" || e == "latency" || e == "wakeup")
            return BpfMode::SCHED_LATENCY;
        return BpfMode::CPU;
    }

    namespace
    {
        bool TracepointAvailable(const string &name)
        {
            ifstream events("/sys/kernel/tracing/available_events");
            string line;
            while (getline(events, line))
                if (line == name)
                    return true;
            return false;
        }

        string MakeScript(BpfMode mode, const hotmethod::TaskDesc &task)
        {
            string s;
            uint32_t hz = task.sampleargv().hz();
            if (hz == 0)
                hz = 99;
            if (mode == BpfMode::CPU && hz < 19)
                hz = 19;
            uint64_t dur = task.sampleargv().duration();
            if (dur == 0)
                dur = 10;
            int pid = task.sampleargv().pid();

            switch (mode)
            {
            case BpfMode::CPU:
            {
                string cg = task.sampleargv().callgraph();
                for (auto &c : cg)
                    c = tolower(static_cast<unsigned char>(c));
                string stackExpr = (cg == "ustack" || cg == "user") ? "ustack" : "kstack";
                if (pid > 0)
                    s += "profile:hz:" + to_string(hz) + "\n/pid == " + to_string(pid) + "/\n{\n    @samples[" + stackExpr + "] = count();\n}\n\n";
                else
                    s += "profile:hz:" + to_string(hz) + "\n{\n    @samples[" + stackExpr + "] = count();\n}\n\n";
                s += "interval:s:" + to_string(dur) + "\n{\n    exit();\n}\n";
                break;
            }
            case BpfMode::IO_LATENCY:
                s += "#define dev_t unsigned int\n";
                s += "#define sector_t unsigned long\n\n";
                s += "tracepoint:block:block_rq_issue\n{\n";
                s += "    @rq_start[args->dev, args->sector] = nsecs;\n";
                s += "}\n\n";
                s += "tracepoint:block:block_rq_complete\n/@rq_start[args->dev, args->sector]/\n{\n";
                s += "    $lat = (nsecs - @rq_start[args->dev, args->sector]) / 1000;\n";
                s += "    @io_lat_us = hist($lat);\n";
                s += "    @io_events = count();\n";
                s += "    delete(@rq_start[args->dev, args->sector]);\n";
                s += "}\n\n";
                s += "interval:s:" + to_string(dur) + "\n{\n";
                s += "    printf(\"# Mini-Drop eBPF IO Latency\\n\");\n";
                s += "    print(@io_lat_us);\n";
                s += "    print(@io_events);\n";
                s += "    clear(@rq_start); clear(@io_events); clear(@io_lat_us);\n";
                s += "    exit();\n";
                s += "}\n";
                break;
            case BpfMode::SCHED_LATENCY:
                s += "#define pid_t int\n\n";
                s += "tracepoint:sched:sched_wakeup\n{\n";
                s += "    @wake[args->pid] = nsecs;\n";
                s += "}\n\n";
                s += "tracepoint:sched:sched_wakeup_new\n{\n";
                s += "    @wake[args->pid] = nsecs;\n";
                s += "}\n\n";
                s += "tracepoint:sched:sched_switch\n/@wake[args->next_pid]/\n{\n";
                s += "    $lat = (nsecs - @wake[args->next_pid]) / 1000;\n";
                s += "    @sched_lat_us = hist($lat);\n";
                s += "    @sched_events = count();\n";
                s += "    delete(@wake[args->next_pid]);\n";
                s += "}\n\n";
                s += "interval:s:" + to_string(dur) + "\n{\n";
                s += "    printf(\"# Mini-Drop eBPF Scheduler Latency\\n\");\n";
                s += "    print(@sched_lat_us);\n";
                s += "    print(@sched_events);\n";
                s += "    clear(@wake); clear(@sched_events); clear(@sched_lat_us);\n";
                s += "    exit();\n";
                s += "}\n";
                break;
            }
            return s;
        }

        void Postprocess(const string &raw, const string &out, BpfMode mode)
        {
            ifstream in(raw);
            if (!in.is_open())
                return;
            ofstream of(out);
            if (!of.is_open())
            {
                in.close();
                return;
            }

            string line;
            while (getline(in, line))
            {
                if (line.find("Attaching") == 0)
                    continue;
                if (line.empty())
                    continue;
                if (mode == BpfMode::CPU)
                {
                    string trimmed = line;
                    size_t first = trimmed.find_first_not_of(" \t");
                    if (first != string::npos)
                        trimmed = trimmed.substr(first);
                    if (trimmed.find("@samples[") == 0 || trimmed.find("@[") == 0)
                    {
                        vector<string> frames;
                        string count;

                        size_t endBr = trimmed.rfind("]:");
                        if (endBr != string::npos)
                        {
                            size_t startBr = trimmed.find("[");
                            string stack = trimmed.substr(startBr + 1, endBr - startBr - 1);
                            count = trimmed.substr(endBr + 2);
                            string frame;
                            for (char c : stack)
                            {
                                if (c == ';')
                                {
                                    if (!frame.empty())
                                    {
                                        frames.push_back(frame);
                                        frame.clear();
                                    }
                                }
                                else
                                {
                                    frame += c;
                                }
                            }
                            if (!frame.empty())
                                frames.push_back(frame);
                        }
                        else
                        {
                            string firstFrame = trimmed.substr(trimmed.find("[") + 1);
                            if (!firstFrame.empty())
                                frames.push_back(firstFrame);
                            while (getline(in, line))
                            {
                                if (line.find("]:") != string::npos)
                                {
                                    size_t cb = line.rfind("]:");
                                    if (cb != string::npos)
                                    {
                                        string frame = line.substr(0, cb);
                                        if (!frame.empty())
                                            frames.push_back(frame);
                                        count = line.substr(cb + 2);
                                    }
                                    break;
                                }
                                frames.push_back(line);
                            }
                        }

                        vector<string> cleanFrames;
                        for (string frame : frames)
                        {
                            size_t fs = frame.find_first_not_of(" \t\n\r");
                            size_t fe = frame.find_last_not_of(" \t\n\r");
                            if (fs == string::npos || fe == string::npos)
                                continue;
                            frame = frame.substr(fs, fe - fs + 1);
                            if (frame.empty() || frame == "@[" || frame == "]")
                                continue;
                            cleanFrames.push_back(frame);
                        }

                        string finalStack;
                        for (auto it = cleanFrames.rbegin(); it != cleanFrames.rend(); ++it)
                        {
                            if (!finalStack.empty())
                                finalStack += ";";
                            finalStack += *it;
                        }

                        size_t cs = count.find_first_not_of(" \t\n\r");
                        size_t ce = count.find_last_not_of(" \t\n\r");
                        if (cs != string::npos && ce != string::npos)
                            count = count.substr(cs, ce - cs + 1);

                        if (!finalStack.empty() && !count.empty())
                            of << finalStack << " " << count << "\n";
                    }
                }
                else
                {
                    of << line << "\n";
                }
            }
            in.close();
            of.close();
        }

        bool HasHistogramBucket(const string &path)
        {
            ifstream in(path);
            if (!in.is_open())
                return false;
            string line;
            while (getline(in, line))
            {
                size_t pos = line.find_first_not_of(" \t");
                if (pos == string::npos)
                    continue;
                char c = line[pos];
                if (c == '[' || c == '(')
                    return true;
            }
            return false;
        }

        bool HasFoldedStack(const string &path)
        {
            ifstream in(path);
            if (!in.is_open())
                return false;
            string line;
            while (getline(in, line))
            {
                size_t pos = line.find_first_not_of(" \t");
                if (pos == string::npos)
                    continue;
                if (line.find(';') != string::npos)
                    return true;
            }
            return false;
        }
    } // namespace

    ValidationResult BpfRunner::Validate(const TaskContext &ctx)
    {
        mode_ = ParseBpfMode(ctx.task.sampleargv().event());
        if (mode_ == BpfMode::IO_LATENCY &&
            (!TracepointAvailable("block:block_rq_issue") || !TracepointAvailable("block:block_rq_complete")))
        {
            ValidationResult r;
            r.ok = false;
            r.resultCode = -2;
            r.errorCode = GetErrorCode(-2, "eBPF");
            r.errorMessage = GetErrorMessage(-2, "eBPF", ctx.task);
            return r;
        }
        if (mode_ == BpfMode::SCHED_LATENCY &&
            (!TracepointAvailable("sched:sched_wakeup") || !TracepointAvailable("sched:sched_switch")))
        {
            ValidationResult r;
            r.ok = false;
            r.resultCode = -2;
            r.errorCode = GetErrorCode(-2, "eBPF");
            r.errorMessage = GetErrorMessage(-2, "eBPF", ctx.task);
            return r;
        }
        int targetPid = ctx.task.sampleargv().pid();
        if (mode_ == BpfMode::CPU && targetPid > 0 && !drop::pid_exists(targetPid))
        {
            ValidationResult r;
            r.ok = false;
            r.resultCode = -4;
            r.errorCode = GetErrorCode(-4, "eBPF");
            r.errorMessage = GetErrorMessage(-4, "eBPF", ctx.task);
            return r;
        }
        ValidationResult r;
        r.ok = true;
        return r;
    }

    PrepareResult BpfRunner::Prepare(TaskContext &ctx)
    {
        outputPath_ = ctx.taskDir + ".bpf";
        rawPath_ = outputPath_ + ".raw";
        scriptPath_ = "/tmp/bpf_script_" + to_string(getpid()) + "_" + ctx.task.taskid() + ".bt";

        string script = MakeScript(mode_, ctx.task);
        ofstream f(scriptPath_);
        if (!f.is_open())
        {
            PrepareResult r;
            r.ok = false;
            r.resultCode = -2;
            r.errorCode = GetErrorCode(-2, "eBPF");
            r.errorMessage = GetErrorMessage(-2, "eBPF", ctx.task);
            return r;
        }
        f << script;
        f.close();
        PrepareResult r;
        r.ok = true;
        return r;
    }

    StartResult BpfRunner::Start(TaskContext &ctx)
    {
        drop::ExecArgs args;
        args.argv.push_back("bpftrace");
        args.argv.push_back(scriptPath_);
        args.stdoutRedirectPath = rawPath_;
        args.stderrRedirectPath = rawPath_;

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
        // gracePeriodSec=1：对齐旧 exec_bpftrace() 里比其他采集器更短的 grace period。
        poller_.reset(new drop::TimedProcessPoller(ctx.executor, ctx.clock, timeout, /*gracePeriodSec=*/1));
        poller_->Attach(handle);
        StartResult r;
        r.ok = true;
        return r;
    }

    PollResult BpfRunner::Poll(TaskContext &)
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
            // 刻意保留：旧代码里 bpftrace 被信号杀死映射到 -2，不是其余
            // 3 个采集器用的 -5。
            lastResultCode_ = -2;
            return {PollStatus::kFailed, lastResultCode_};
        }
        if (o.state == drop::PollState::kExited)
        {
            if (o.exitCode == 0)
            {
                lastResultCode_ = 0;
                return {PollStatus::kSucceeded, 0};
            }
            if (o.exitCode == 127)
            {
                lastResultCode_ = -5; // execvp 失败：bpftrace 不存在
                return {PollStatus::kFailed, lastResultCode_};
            }
            lastResultCode_ = o.exitCode;
            return {PollStatus::kFailed, lastResultCode_};
        }
        // 旧代码对 wait 既非 EXITED 也非 SIGNALED 的情况直接 return 0——
        // 保留这个怪癖以确保 Phase 2 行为逐字段一致。
        lastResultCode_ = 0;
        return {PollStatus::kSucceeded, 0};
    }

    StopResult BpfRunner::Stop(TaskContext &, StopReason)
    {
        if (poller_)
            poller_->ForceStop();
        return {true};
    }

    CollectResult BpfRunner::Collect(TaskContext &)
    {
        CollectResult r;
        r.profilerName = "eBPF";
        int resultCode = lastResultCode_;

        if (resultCode == 0)
        {
            Postprocess(rawPath_, outputPath_, mode_);
            ::remove(rawPath_.c_str());

            ifstream ck(outputPath_);
            bool emptyOrMissing = !ck.is_open() || ck.peek() == ifstream::traits_type::eof();
            ck.close();

            if (emptyOrMissing)
            {
                resultCode = -2;
                ::remove(outputPath_.c_str());
            }
            else if (mode_ == BpfMode::CPU && !HasFoldedStack(outputPath_))
            {
                resultCode = -2;
                ::remove(outputPath_.c_str());
            }
            else if (mode_ != BpfMode::CPU && !HasHistogramBucket(outputPath_))
            {
                resultCode = -2;
                ::remove(outputPath_.c_str());
            }
        }
        ::remove(scriptPath_.c_str());

        r.resultCode = resultCode;
        r.outputPath = outputPath_;
        r.remoteKeyHint = RemoteKeyFor(r.profilerName);
        r.contentType = ContentTypeFor(r.profilerName);
        r.partial = resultCode != 0 && FileExistsNonEmpty(outputPath_);
        return r;
    }

    CleanupResult BpfRunner::Cleanup(TaskContext &) { return {true}; }

} // namespace drop_agent
