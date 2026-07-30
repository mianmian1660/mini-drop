// ============================================================
// common/PprofProfiler.cpp — pprof（Go）采集器 实现
// ============================================================
// profilerType=2: 通过 HTTP 端点采集 Go 程序的 pprof profile
//
// 方式1: curl 拉取 profile（推荐，简单可靠）
//   curl -o <output> "http://<host>:<port>/debug/pprof/profile?seconds=<duration>"
//
// 方式2: go tool pprof（需要 Go 环境）
//   go tool pprof -seconds <duration> -output <output> http://<host>:<port>/debug/pprof/profile
//
// pprof 端点信息传递:
//   - taskDesc.sampleargv().event() 可指定 "host:port" 或完整 URL
//   - 若 event 为空，尝试使用 localhost:pid（pid 作为端口号）的约定
//   - 若 pid<=0 且 event 为空，则使用默认 localhost:6060
//
// 采集流程:
//   1. 解析 pprof 目标 URL
//   2. fork+execvp 调用 curl 拉取 profile
//   3. 等待子进程结束（带超时保护）
//   4. 返回结果状态码
// ============================================================

#include "common/PprofProfiler.h"

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
#include <fstream>

using namespace std;
using namespace std::chrono;

namespace drop
{

    /// 构建 pprof 的完整 URL
    static string build_pprof_url(const hotmethod::TaskDesc &taskDesc)
    {
        uint64_t duration = taskDesc.sampleargv().duration();
        if (duration == 0)
            duration = 10;

        string fullUrl = taskDesc.pprofurl();
        // Compatibility with tasks created before pprofUrl existed.
        if (fullUrl.empty())
            fullUrl = taskDesc.sampleargv().event();
        if (fullUrl.empty())
            return "";
        const string seconds = "seconds=";
        if (fullUrl.find(seconds) == string::npos)
            fullUrl += (fullUrl.find('?') == string::npos ? "?" : "&") + seconds + to_string(duration);
        return fullUrl;
    }

    int run_pprof(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
    {
        string url = build_pprof_url(taskDesc);

        if (url.empty())
        {
            cerr << "[pprof] 未提供 pprof URL" << endl;
            return -4;
        }

        cout << "[pprof] 目标 URL: " << url << endl;

        pid_t pid = fork();
        if (pid < 0)
        {
            cerr << "[pprof] fork 失败!" << endl;
            return -1;
        }

        if (pid == 0)
        {
            // ==== 子进程 ====
            setpgid(0, 0); // 独立进程组，方便超时 kill

            // 使用 curl 拉取 pprof profile
            // curl -s -o <output> --connect-timeout 5 --max-time <duration+10> <url>
            uint64_t duration = taskDesc.sampleargv().duration();
            if (duration == 0)
                duration = 10;

            string maxTime = to_string(duration + 10);

            vector<string> args_storage;
            args_storage.push_back("curl");
            args_storage.push_back("-s"); // 静默模式
            args_storage.push_back("--fail"); // reject HTTP error pages
            args_storage.push_back("--show-error");
            args_storage.push_back("-o"); // 输出到文件
            args_storage.push_back(outputPath);
            args_storage.push_back("--connect-timeout");
            args_storage.push_back("5");
            args_storage.push_back("--max-time");
            args_storage.push_back(maxTime);
            args_storage.push_back("-L"); // 跟随重定向
            args_storage.push_back(url);

            vector<const char *> args;
            args.reserve(args_storage.size() + 1);
            for (const auto &s : args_storage)
                args.push_back(s.c_str());
            args.push_back(nullptr);

            cout << "[pprof] 执行: ";
            for (size_t i = 0; i < args.size() - 1; i++)
                cout << args[i] << " ";
            cout << endl;

            execvp("curl", const_cast<char *const *>(args.data()));
            cerr << "[pprof] execvp curl 失败!" << endl;
            _exit(127);
        }

        // ==== 父进程 ====
        cout << "[pprof] 子进程 PID=" << pid
             << ", 等待 HTTP 响应（约 " << taskDesc.sampleargv().duration() << "s）..." << endl;

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
                cerr << "[pprof] waitpid 出错: " << strerror(errno) << endl;
                return -2;
            }

            if (time(nullptr) - start > timeout)
            {
                cerr << "[pprof] 超时（" << timeout << "秒），强制终止" << endl;
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
            int exitCode = WEXITSTATUS(status);
            cout << "[pprof] 子进程退出, exitCode=" << exitCode << endl;

            // curl exit code: 0=成功, 7=连接失败, 28=超时
            if (exitCode == 7)
            {
                cerr << "[pprof] 无法连接到 pprof 端点: " << url << endl;
                return -4;
            }
            if (exitCode == 28)
            {
                cerr << "[pprof] curl 请求超时" << endl;
                return -3;
            }
            if (exitCode != 0)
                return exitCode;
            std::ifstream profile(outputPath, std::ios::binary);
            unsigned char magic[2]{};
            profile.read(reinterpret_cast<char *>(magic), sizeof(magic));
            if (!profile || magic[0] != 0x1f || magic[1] != 0x8b)
            {
                cerr << "[pprof] 返回内容不是 gzip pprof profile" << endl;
                return -6;
            }
            return 0;
        }
        if (WIFSIGNALED(status))
        {
            cerr << "[pprof] 子进程被信号杀死: " << WTERMSIG(status) << endl;
            return -5;
        }

        return -6;
    }

} // namespace drop
