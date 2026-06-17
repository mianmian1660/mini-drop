// ============================================================
// common/Perf.cpp — perf 采集执行 实现
// ============================================================

#include "common/Perf.h"
#include "common/Process.h" // pid_exists

#include <iostream>
#include <string>
#include <vector>
#include <thread>
#include <chrono>
#include <unistd.h>   // fork, execvp, getpid, setpgid
#include <sys/wait.h> // waitpid, WIFEXITED, WEXITSTATUS, WIFSIGNALED, WTERMSIG
#include <signal.h>   // kill, SIGTERM, SIGKILL
#include <cstring>    // strerror
#include <cerrno>     // errno

using namespace std;
using namespace std::chrono;

namespace drop
{

    int run_perf(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
    {
        // 检查目标 PID 是否存在
        int targetPid = taskDesc.sampleargv().pid();
        if (targetPid > 0 && !pid_exists(targetPid))
        {
            cerr << "[perf] 目标 PID " << targetPid << " 不存在!" << endl;
            return -4;
        }

        pid_t pid = fork();
        if (pid < 0)
        {
            cerr << "[perf] fork 失败!" << endl;
            return -1;
        }

        if (pid == 0)
        {
            // ==== 子进程 ====
            setpgid(0, 0); // 独立进程组，方便超时 kill

            vector<string> args_storage;
            args_storage.reserve(16);

            args_storage.push_back("perf");
            args_storage.push_back("record");
            args_storage.push_back("-F");
            args_storage.push_back(to_string(taskDesc.sampleargv().hz()));

            string cg = taskDesc.sampleargv().callgraph();
            if (!cg.empty())
            {
                args_storage.push_back("--call-graph");
                args_storage.push_back(cg);
            }

            args_storage.push_back("-o");
            args_storage.push_back(outputPath);

            if (targetPid > 0)
            {
                args_storage.push_back("-p");
                args_storage.push_back(to_string(targetPid));
            }

            args_storage.push_back("--");
            args_storage.push_back("sleep");
            uint64_t dur = taskDesc.sampleargv().duration();
            if (dur == 0)
                dur = 10;
            args_storage.push_back(to_string(dur));

            // 构建 char* 数组
            vector<const char *> args;
            args.reserve(args_storage.size() + 1);
            for (const auto &s : args_storage)
                args.push_back(s.c_str());
            args.push_back(nullptr);

            cout << "[perf] 执行: ";
            for (size_t i = 0; i < args.size() - 1; i++)
                cout << args[i] << " ";
            cout << endl;

            execvp("perf", const_cast<char *const *>(args.data()));
            cerr << "[perf] execvp 失败!" << endl;
            _exit(127);
        }

        // ==== 父进程 ====
        cout << "[perf] 子进程 PID=" << pid << ", 等待 " << taskDesc.sampleargv().duration() << "s..." << endl;

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
                cerr << "[perf] waitpid 出错: " << strerror(errno) << endl;
                return -2;
            }

            if (time(nullptr) - start > timeout)
            {
                cerr << "[perf] 超时（" << timeout << "秒），强制终止" << endl;
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
            cout << "[perf] 子进程退出, exitCode=" << WEXITSTATUS(status) << endl;
            return WEXITSTATUS(status);
        }
        if (WIFSIGNALED(status))
        {
            cerr << "[perf] 子进程被信号杀死: " << WTERMSIG(status) << endl;
            return -5;
        }

        return -6;
    }

} // namespace drop
