// ============================================================
// common/AsyncProfilerProfiler.cpp — async-profiler（Java）采集器 实现
// ============================================================
// profilerType=1: 调用 asprof 工具对 Java 进程进行 CPU 采样
//
// asprof 用法: asprof -e <event> -d <duration> -f <output> <pid>
//   示例: asprof -e cpu -d 10 -f /tmp/profile.jfr 12345
//   也可输出折叠栈: asprof -e cpu -d 10 -o collapsed 12345 > /tmp/profile.collapsed
//
// 采集流程:
//   1. 检查目标 PID 是否存在
//   2. fork+execvp 调用 asprof
//   3. 等待子进程结束（带超时保护）
//   4. 返回结果状态码
// ============================================================

#include "common/AsyncProfilerProfiler.h"
#include "common/Process.h" // pid_exists

#include <iostream>
#include <string>
#include <vector>
#include <thread>
#include <chrono>
#include <unistd.h>   // fork, execvp, setpgid
#include <sys/wait.h> // waitpid, WIFEXITED, WEXITSTATUS, WIFSIGNALED, WTERMSIG
#include <signal.h>   // kill, SIGTERM, SIGKILL
#include <cstring>    // strerror
#include <cerrno>     // errno

using namespace std;
using namespace std::chrono;

namespace drop
{

    int run_async_profiler(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
    {
        // 检查目标 PID 是否存在
        int targetPid = taskDesc.sampleargv().pid();
        if (targetPid <= 0)
        {
            cerr << "[async-profiler] 需要指定有效的目标 PID!" << endl;
            return -4;
        }
        if (!pid_exists(targetPid))
        {
            cerr << "[async-profiler] 目标 PID " << targetPid << " 不存在!" << endl;
            return -4;
        }

        pid_t pid = fork();
        if (pid < 0)
        {
            cerr << "[async-profiler] fork 失败!" << endl;
            return -1;
        }

        if (pid == 0)
        {
            // ==== 子进程 ====
            setpgid(0, 0); // 独立进程组，方便超时 kill

            vector<string> args_storage;
            args_storage.reserve(12);

            args_storage.push_back("asprof");

            // 事件类型（从 event 字段获取，默认 cpu）
            string event = taskDesc.sampleargv().event();
            if (event.empty())
                event = "cpu";
            args_storage.push_back("-e");
            args_storage.push_back(event);

            // 采集时长
            uint64_t dur = taskDesc.sampleargv().duration();
            if (dur == 0)
                dur = 10;
            args_storage.push_back("-d");
            args_storage.push_back(to_string(dur));

            // 输出格式：默认 jfr，也可指定 collapsed
            // 根据 outputPath 后缀决定格式
            string fmt = "jfr";
            if (outputPath.find(".collapsed") != string::npos)
                fmt = "collapsed";
            args_storage.push_back("-o");
            args_storage.push_back(fmt);

            args_storage.push_back("-f");
            args_storage.push_back(outputPath);

            // 目标 PID
            args_storage.push_back(to_string(targetPid));

            // 构建 char* 数组
            vector<const char *> args;
            args.reserve(args_storage.size() + 1);
            for (const auto &s : args_storage)
                args.push_back(s.c_str());
            args.push_back(nullptr);

            cout << "[async-profiler] 执行: ";
            for (size_t i = 0; i < args.size() - 1; i++)
                cout << args[i] << " ";
            cout << endl;

            execvp("asprof", const_cast<char *const *>(args.data()));
            cerr << "[async-profiler] execvp 失败!（请确认 asprof 已安装）" << endl;
            _exit(127);
        }

        // ==== 父进程 ====
        cout << "[async-profiler] 子进程 PID=" << pid
             << ", 等待 " << taskDesc.sampleargv().duration() << "s..." << endl;

        uint32_t timeout = taskDesc.timeoutsec();
        if (timeout == 0)
            timeout = 60;

        int status;
        time_t start = time(nullptr);

        while (true)
        {
            pid_t result = waitpid(pid, &status, WNOHANG);
            if (result == pid)
                break;
            if (result < 0)
            {
                cerr << "[async-profiler] waitpid 出错: " << strerror(errno) << endl;
                return -2;
            }

            if (time(nullptr) - start > timeout)
            {
                cerr << "[async-profiler] 超时（" << timeout << "秒），强制终止" << endl;
                killpg(pid, SIGTERM);
                this_thread::sleep_for(seconds(5));
                killpg(pid, SIGKILL);
                waitpid(pid, &status, 0);
                return -3;
            }

            this_thread::sleep_for(milliseconds(500));
        }

        if (WIFEXITED(status))
        {
            cout << "[async-profiler] 子进程退出, exitCode=" << WEXITSTATUS(status) << endl;
            return WEXITSTATUS(status);
        }
        if (WIFSIGNALED(status))
        {
            cerr << "[async-profiler] 子进程被信号杀死: " << WTERMSIG(status) << endl;
            return -5;
        }

        return -6;
    }

} // namespace drop
