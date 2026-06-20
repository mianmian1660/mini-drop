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

    // ---- 生成 bpftrace 脚本 ----
    static string make_script(BpfMode mode, const hotmethod::TaskDesc &taskDesc)
    {
        string s;
        uint32_t hz = taskDesc.sampleargv().hz();
        if (hz == 0)
            hz = 99;
        uint64_t dur = taskDesc.sampleargv().duration();
        if (dur == 0)
            dur = 10;
        int pid = taskDesc.sampleargv().pid();

        switch (mode)
        {
        case BpfMode::CPU:
            if (pid > 0)
                s += "profile:hz:" + to_string(hz) + "\n/pid == " + to_string(pid) + "/\n{\n    @samples[ustack] = count();\n}\n\n";
            else
                s += "profile:hz:" + to_string(hz) + "\n{\n    @samples[ustack] = count();\n}\n\n";
            s += "interval:s:" + to_string(dur) + "\n{\n    exit();\n}\n";
            break;

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
                // bpftrace v0.14 文本输出可能是单行或多行：
                // 单行: @samples[func1;func2]: 42
                // 多行: @samples[\n  func1\n  func2\n]: 42
                // 转换为标准折叠栈: func1;func2 42
                if (line.find("@samples[") == 0)
                {
                    string stack;
                    string count;

                    // 检查是否单行格式（同一行有 ]:）
                    size_t endBr = line.rfind("]:");
                    if (endBr != string::npos)
                    {
                        size_t startBr = line.find("[");
                        stack = line.substr(startBr + 1, endBr - startBr - 1);
                        count = line.substr(endBr + 2);
                    }
                    else
                    {
                        // 多行格式：收集后续行
                        stack = line.substr(line.find("[") + 1);
                        while (getline(in, line))
                        {
                            if (line.find("]:") != string::npos)
                            {
                                size_t cb = line.rfind("]:");
                                if (cb != string::npos)
                                {
                                    stack += line.substr(0, cb);
                                    count = line.substr(cb + 2);
                                }
                                break;
                            }
                            stack += line;
                        }
                    }

                    // 清理：将 \n 替换为 ;
                    string cleanStack;
                    for (char c : stack)
                    {
                        if (c == '\n' || c == '\r')
                            continue;
                        if (c == ' ' && (cleanStack.empty() || cleanStack.back() == ';'))
                            continue;
                        cleanStack += c;
                    }
                    // 去首尾空格并压缩内部空格
                    size_t ss = cleanStack.find_first_not_of(" \t");
                    size_t se = cleanStack.find_last_not_of(" \t");
                    if (ss != string::npos && se != string::npos)
                        cleanStack = cleanStack.substr(ss, se - ss + 1);
                    // 将空白分隔替换为 ;
                    string finalStack;
                    bool inSpace = false;
                    for (char c : cleanStack)
                    {
                        if (c == ' ' || c == '\t')
                        {
                            if (!inSpace)
                            {
                                finalStack += ';';
                                inSpace = true;
                            }
                        }
                        else
                        {
                            finalStack += c;
                            inSpace = false;
                        }
                    }

                    // 清理 count
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
        remove(raw.c_str());

        if (r == 0)
        {
            ifstream ck(outputPath);
            if (!ck.is_open() || ck.peek() == ifstream::traits_type::eof())
            {
                cerr << "[bpf] 输出文件为空" << endl;
                return -2;
            }
            ck.close();

            if (mode != BpfMode::CPU && !has_histogram_bucket(outputPath))
            {
                cerr << "[bpf] 未采到直方图桶，请确认现场负载存在并适当加长 duration" << endl;
                return -2;
            }
        }

        cout << "[bpf] 完成 mode=" << (int)mode << " result=" << r << endl;
        return r;
    }

} // namespace drop
