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
            s += "BEGIN { printf(\"# Mini-Drop eBPF CPU Profiler\\n\"); }\n\n";
            if (pid > 0)
                s += "profile:hz:" + to_string(hz) + "\n/pid == " + to_string(pid) + "/\n{\n    @samples[ustack] = count();\n}\n\n";
            else
                s += "profile:hz:" + to_string(hz) + "\n{\n    @samples[ustack] = count();\n}\n\n";
            s += "interval:s:" + to_string(dur) + "\n{\n    exit();\n}\n";
            break;

        case BpfMode::IO_LATENCY:
            s += "BEGIN { printf(\"# Mini-Drop eBPF IO Latency Profiler\\n\"); }\n\n";
            s += "kprobe:blk_account_io_start\n{\n    @start[tid] = nsecs;\n    @io_cnt = count();\n}\n\n";
            s += "kprobe:blk_account_io_done\n{\n";
            s += "    $ns = nsecs;\n    if (@start[tid]) {\n";
            s += "        $lat = ($ns - @start[tid]) / 1000;\n";
            s += "        @io_lat_us = hist($lat);\n        delete(@start[tid]);\n    }\n}\n\n";
            s += "interval:s:" + to_string(dur) + "\n{\n    exit();\n}\n\n";
            s += "END {\n    print(@io_lat_us);\n    printf(\"# Total IO: %d\\n\", @io_cnt);\n";
            s += "    clear(@start); clear(@io_cnt); clear(@io_lat_us);\n}\n";
            break;

        case BpfMode::SCHED_LATENCY:
            s += "BEGIN { printf(\"# Mini-Drop eBPF Scheduler Latency Profiler\\n\"); }\n\n";
            s += "kprobe:try_to_wake_up\n{\n    $p = (struct task_struct *)arg0;\n    @wake[$p->pid] = nsecs;\n}\n\n";
            s += "kprobe:finish_task_switch\n{\n    $prev = (struct task_struct *)arg0;\n    $ns = nsecs;\n";
            s += "    if (@wake[$prev->pid]) {\n";
            s += "        $lat = ($ns - @wake[$prev->pid]) / 1000;\n";
            s += "        @sched_lat_us = hist($lat);\n        delete(@wake[$prev->pid]);\n    }\n}\n\n";
            s += "interval:s:" + to_string(dur) + "\n{\n    exit();\n}\n\n";
            s += "END {\n    print(@sched_lat_us);\n    clear(@wake); clear(@sched_lat_us);\n}\n";
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
                // bpftrace -f folded 已经是折叠栈格式，直接输出
                of << line << "\n";
            }
            else
            {
                of << line << "\n";
            }
        }
        in.close();
        of.close();
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
            if (mode == BpfMode::CPU)
            {
                as.push_back("-f");
                as.push_back("folded");
            }
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
        }

        cout << "[bpf] 完成 mode=" << (int)mode << " result=" << r << endl;
        return r;
    }

} // namespace drop
