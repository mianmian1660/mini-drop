// ============================================================
// common/BpfProfiler.cpp — eBPF 采集器 实现 (profilerType=3)
// ============================================================
#include "common/BpfProfiler.h"
#include "common/Process.h"
#include "common/Utils.h"

#include <iostream>
#include <fstream>
#include <string>
#include <vector>
#include <thread>
#include <chrono>
#include <unistd.h>
#include <sys/wait.h>
#include <signal.h>
#include <cstring>
#include <cerrno>
#include <cstdio>
#include <cctype>

using namespace std;
using namespace std::chrono;

namespace drop
{

    BpfMode parse_bpf_mode(const string &event)
    {
        if (event.empty())
            return BpfMode::CPU;
        string e = event;
        for (auto &c : e)
            c = tolower(c);
        if (e == "io" || e == "blk" || e == "block" || e == "disk")
            return BpfMode::IO_LATENCY;
        if (e == "sched" || e == "schedule" || e == "latency" || e == "wakeup")
            return BpfMode::SCHED_LATENCY;
        return BpfMode::CPU;
    }

    // ---- 内部函数前向声明 ----
    static string make_script(BpfMode mode, const hotmethod::TaskDesc &taskDesc);
    static void postprocess(const string &raw, const string &out, BpfMode mode);
    static int exec_bpftrace(const string &scriptPath, const string &outputPath,
                             const hotmethod::TaskDesc &taskDesc, BpfMode mode);
    static bool has_histogram_bucket(const string &path);
    static bool has_folded_stack(const string &path);

    static bool tracepoint_available(const string &name)
    {
        ifstream events("/sys/kernel/tracing/available_events");
        string line;
        while (getline(events, line))
            if (line == name)
                return true;
        return false;
    }

    // ---- 生成 bpftrace 脚本 ----
    static string make_script(BpfMode mode, const hotmethod::TaskDesc &taskDesc)
    {
        string s;
        uint32_t hz = taskDesc.sampleargv().hz();
        if (hz == 0)
            hz = 99;
        if (mode == BpfMode::CPU && hz < 19)
            hz = 19;
        uint64_t dur = taskDesc.sampleargv().duration();
        if (dur == 0)
            dur = 10;
        int pid = taskDesc.sampleargv().pid();

        switch (mode)
        {
        case BpfMode::CPU:
        {
            string cg = taskDesc.sampleargv().callgraph();
            for (auto &c : cg)
                c = tolower(c);
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

    // ---- 后处理 ----
    static void postprocess(const string &raw, const string &out, BpfMode mode)
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
                // bpftrace v0.14/v0.20 文本输出可能是单行或多行：
                // @samples[func1;func2]: 42
                // @[
                //     leaf+7
                //     parent+9
                // ]: 42
                // 转换为标准折叠栈: parent;leaf 42
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

    static bool has_histogram_bucket(const string &path)
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

    static bool has_folded_stack(const string &path)
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

    // ---- fork+exec bpftrace ----
    static int exec_bpftrace(const string &scriptPath, const string &outputPath,
                             const hotmethod::TaskDesc &taskDesc, BpfMode mode)
    {
        int targetPid = taskDesc.sampleargv().pid();
        uint64_t timeoutSec = taskDesc.timeoutsec();
        if (timeoutSec == 0)
            timeoutSec = 60;

        if (mode == BpfMode::CPU && targetPid > 0 && !pid_exists(targetPid))
        {
            cerr << "[bpf] PID " << targetPid << " 不存在!" << endl;
            return -4;
        }

        pid_t p = fork();
        if (p < 0)
        {
            cerr << "[bpf] fork 失败!" << endl;
            return -1;
        }

        if (p == 0)
        {
            setpgid(0, 0);
            FILE *f = fopen(outputPath.c_str(), "w");
            if (f)
            {
                dup2(fileno(f), STDOUT_FILENO);
                dup2(fileno(f), STDERR_FILENO);
                fclose(f);
            }

            vector<string> as;
            vector<const char *> av;
            as.push_back("bpftrace");
            as.push_back(scriptPath);
            for (auto &a : as)
                av.push_back(a.c_str());
            av.push_back(nullptr);
            execvp("bpftrace", const_cast<char *const *>(av.data()));
            perror("[bpf] execvp bpftrace 失败");
            _exit(127);
        }

        pid_t pgid = p;
        auto t0 = steady_clock::now();
        bool to = false;
        int st = 0;
        pid_t wp = 0;

        while (true)
        {
            wp = waitpid(p, &st, WNOHANG);
            if (wp > 0)
                break;
            if (wp < 0)
            {
                cerr << "[bpf] waitpid err: " << strerror(errno) << endl;
                return -2;
            }
            auto el = duration_cast<seconds>(steady_clock::now() - t0).count();
            if ((uint64_t)el >= timeoutSec + 5)
            {
                to = true;
                break;
            }
            this_thread::sleep_for(milliseconds(100));
        }

        if (to && wp <= 0)
        {
            cerr << "[bpf] 超时 " << timeoutSec << "s, 强制终止" << endl;
            killpg(pgid, SIGTERM);
            this_thread::sleep_for(seconds(1));
            killpg(pgid, SIGKILL);
            waitpid(p, &st, 0);
            return -3;
        }

        if (WIFEXITED(st))
        {
            int ec = WEXITSTATUS(st);
            if (ec != 0)
            {
                cerr << "[bpf] 退出码=" << ec << endl;
                if (ec == 127)
                    return -5;
                return ec;
            }
            return 0;
        }
        if (WIFSIGNALED(st))
        {
            cerr << "[bpf] 信号=" << WTERMSIG(st) << endl;
            return -2;
        }
        return 0;
    }

    // ============================================================
    // 公开入口
    // ============================================================
    int run_bpf(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
    {
        BpfMode mode = parse_bpf_mode(taskDesc.sampleargv().event());
        if (mode == BpfMode::IO_LATENCY &&
            (!tracepoint_available("block:block_rq_issue") || !tracepoint_available("block:block_rq_complete")))
        {
            cerr << "[bpf] 当前内核缺少 block_rq_issue/block_rq_complete tracepoint" << endl;
            return -2;
        }
        if (mode == BpfMode::SCHED_LATENCY &&
            (!tracepoint_available("sched:sched_wakeup") || !tracepoint_available("sched:sched_switch")))
        {
            cerr << "[bpf] 当前内核缺少 sched_wakeup/sched_switch tracepoint" << endl;
            return -2;
        }
        string script = make_script(mode, taskDesc);

        string tmp = "/tmp/bpf_script_" + to_string(getpid()) + ".bt";
        {
            ofstream f(tmp);
            if (!f.is_open())
            {
                cerr << "[bpf] 无法写脚本" << endl;
                return -2;
            }
            f << script;
        }

        string raw = outputPath + ".raw";
        int r = exec_bpftrace(tmp, raw, taskDesc, mode);

        if (r == 0)
            postprocess(raw, outputPath, mode);
        remove(tmp.c_str());
        if (r == 0)
            remove(raw.c_str());

        if (r == 0)
        {
            ifstream ck(outputPath);
            if (!ck.is_open() || ck.peek() == ifstream::traits_type::eof())
            {
                if (mode == BpfMode::CPU)
                    cerr << "[bpf] NO_EBPF_SAMPLES: CPU profile 没有 folded stack；请提高频率/加长 duration 或确认 kstack/ustack 可用" << endl;
                else
                    cerr << "[bpf] NO_EBPF_SAMPLES: 输出文件为空；请确认采集窗口内存在 IO/调度负载" << endl;
                remove(outputPath.c_str());
                return -2;
            }
            ck.close();

            if (mode == BpfMode::CPU && !has_folded_stack(outputPath))
            {
                cerr << "[bpf] NO_EBPF_SAMPLES: CPU 输出中没有有效 folded stack" << endl;
                remove(outputPath.c_str());
                return -2;
            }

            if (mode != BpfMode::CPU && !has_histogram_bucket(outputPath))
            {
                cerr << "[bpf] NO_EBPF_SAMPLES: 未采到直方图桶，请确认现场负载存在并适当加长 duration" << endl;
                remove(outputPath.c_str());
                return -2;
            }
        }

        cout << "[bpf] 完成 mode=" << (int)mode << " result=" << r << endl;
        return r;
    }

} // namespace drop
