#include "agent/CleanupWorker.h"

#include <iostream>
#include <chrono>
#include <cerrno>
#include <cstring>
#include <ctime>
#include <string>

#include <dirent.h>
#include <signal.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

using namespace std;
using namespace std::chrono;

namespace drop_agent
{

    namespace
    {

        // 递归删除一个目录及其内容。目录本就不存在视为成功（幂等）。
        bool RemoveDirRecursive(const string &path)
        {
            DIR *dir = opendir(path.c_str());
            if (!dir)
                return errno == ENOENT;

            bool ok = true;
            struct dirent *entry;
            while ((entry = readdir(dir)) != nullptr)
            {
                string name = entry->d_name;
                if (name == "." || name == "..")
                    continue;
                string childPath = path + "/" + name;
                struct stat st;
                if (lstat(childPath.c_str(), &st) != 0)
                {
                    ok = false;
                    continue;
                }
                if (S_ISDIR(st.st_mode))
                {
                    if (!RemoveDirRecursive(childPath))
                        ok = false;
                }
                else if (::unlink(childPath.c_str()) != 0)
                {
                    ok = false;
                }
            }
            closedir(dir);
            if (::rmdir(path.c_str()) != 0 && errno != ENOENT)
                ok = false;
            return ok;
        }

    } // namespace

    CleanupWorker::CleanupWorker(const AgentConfig &cfg,
                                  drop::PidRegistry &pidRegistry,
                                  std::atomic<bool> &runningFlag)
        : cfg_(cfg), pidRegistry_(pidRegistry), running_(runningFlag)
    {
    }

    void CleanupWorker::Start()
    {
        thread_ = std::thread(&CleanupWorker::Loop, this);
    }

    void CleanupWorker::Stop()
    {
        if (thread_.joinable())
            thread_.join();
    }

    void CleanupWorker::Loop()
    {
        // 启动即跑一轮，之后每 cleanupIntervalSec 跑一轮；用 200ms 步长
        // 轮询 running_，这样 SIGTERM 能被及时响应，不会被整个周期卡住。
        auto lastRun = steady_clock::now() - seconds(cfg_.cleanupIntervalSec);
        while (running_)
        {
            auto now = steady_clock::now();
            if (now - lastRun >= seconds(cfg_.cleanupIntervalSec))
            {
                ReapOrphanPids();
                CleanStaleTaskDirs();
                lastRun = now;
            }
            this_thread::sleep_for(milliseconds(200));
        }
    }

    void CleanupWorker::ReapOrphanPids()
    {
        auto snapshot = pidRegistry_.Snapshot();
        auto now = steady_clock::now();
        for (const auto &entry : snapshot)
        {
            pid_t pid = entry.first;
            auto lastSeenAt = entry.second;
            if (now - lastSeenAt < seconds(cfg_.orphanPidGraceSec))
                continue;

            cerr << "[cleanup] 发现孤儿进程 pid=" << pid << "，超过 "
                 << cfg_.orphanPidGraceSec << " 秒仍未被正常回收，强制 SIGKILL" << endl;
            if (::kill(pid, SIGKILL) != 0 && errno != ESRCH)
                cerr << "[cleanup] SIGKILL pid=" << pid << " 失败: " << strerror(errno) << endl;

            int status = 0;
            ::waitpid(pid, &status, 0);
            pidRegistry_.Unregister(pid);
        }
    }

    void CleanupWorker::CleanStaleTaskDirs()
    {
        const string root = "/tmp/drop_agent/tasks";
        DIR *taskDirs = opendir(root.c_str());
        if (!taskDirs)
            return; // 目录还不存在（还没跑过任何任务），无需清理

        struct dirent *taskEntry;
        while ((taskEntry = readdir(taskDirs)) != nullptr)
        {
            string taskID = taskEntry->d_name;
            if (taskID == "." || taskID == "..")
                continue;
            string taskPath = root + "/" + taskID;
            struct stat taskSt;
            if (lstat(taskPath.c_str(), &taskSt) != 0 || !S_ISDIR(taskSt.st_mode))
                continue;

            DIR *attemptDirs = opendir(taskPath.c_str());
            if (!attemptDirs)
                continue;

            bool anyRemaining = false;
            struct dirent *attemptEntry;
            while ((attemptEntry = readdir(attemptDirs)) != nullptr)
            {
                string attemptID = attemptEntry->d_name;
                if (attemptID == "." || attemptID == "..")
                    continue;
                string attemptPath = taskPath + "/" + attemptID;
                struct stat attemptSt;
                if (lstat(attemptPath.c_str(), &attemptSt) != 0 || !S_ISDIR(attemptSt.st_mode))
                    continue;

                time_t ageSec = ::time(nullptr) - attemptSt.st_mtime;
                if (ageSec >= 0 && static_cast<uint32_t>(ageSec) >= cfg_.taskDirRetentionSec)
                {
                    cout << "[cleanup] 清理过期任务目录: " << attemptPath
                         << " (age=" << ageSec << "s)" << endl;
                    if (!RemoveDirRecursive(attemptPath))
                    {
                        cerr << "[cleanup] 清理失败: " << attemptPath << endl;
                        anyRemaining = true;
                    }
                }
                else
                {
                    anyRemaining = true;
                }
            }
            closedir(attemptDirs);

            // attempt 都清完了，顺手把空的 taskID 目录也删掉；还有残留时
            // rmdir 会因为目录非空失败，忽略即可，下一轮再试。
            if (!anyRemaining)
                ::rmdir(taskPath.c_str());
        }
        closedir(taskDirs);
    }

} // namespace drop_agent
