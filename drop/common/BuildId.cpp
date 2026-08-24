// ============================================================
// common/BuildId.cpp — build-id 清单解析与本地符号缓存（一次性任务 + 持续采集共用）
// ============================================================
#include "common/BuildId.h"

#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <dirent.h>
#include <fstream>
#include <map>
#include <sstream>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#include <thread>

using namespace std;

namespace drop
{

    namespace
    {
        bool file_readable(const string &path)
        {
            struct stat st;
            return ::stat(path.c_str(), &st) == 0 && S_ISREG(st.st_mode);
        }

        string home_dir()
        {
            const char *home = getenv("HOME");
            return (home && *home) ? string(home) : "/root";
        }

        // ~/.debug/.build-id/<id[:2]>/<id[2:]>/elf —— perf 自身的 build-id
        // 缓存布局，perf script 解析失败时会自动回退查这里。
        string build_id_cache_path(const string &buildId)
        {
            if (buildId.size() < 3)
                return "";
            return home_dir() + "/.debug/.build-id/" + buildId.substr(0, 2) + "/" + buildId.substr(2) + "/elf";
        }

        // mkdir -p 的最小实现：逐级创建，已存在的目录忽略错误（::mkdir 失败
        // 不代表整体失败，最后用 stat 校验目标目录确实可用）。
        bool make_dirs(const string &path)
        {
            for (size_t i = 1; i < path.size(); ++i)
            {
                if (path[i] == '/')
                    ::mkdir(path.substr(0, i).c_str(), 0755);
            }
            ::mkdir(path.c_str(), 0755);
            struct stat st;
            return ::stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
        }

        bool is_pid_dir(const char *name, int *pid)
        {
            if (!*name)
                return false;
            for (const char *p = name; *p; ++p)
            {
                if (!isdigit(static_cast<unsigned char>(*p)))
                    return false;
            }
            *pid = atoi(name);
            return *pid > 0;
        }
    } // namespace

    vector<BuildIdEntry> parse_buildid_list(const string &output)
    {
        vector<BuildIdEntry> entries;
        map<string, bool> seen;
        istringstream iss(output);
        string line;
        while (getline(iss, line))
        {
            while (!line.empty() && (line.back() == '\r' || line.back() == '\n'))
                line.pop_back();
            size_t sp = line.find(' ');
            if (sp == string::npos)
                continue;
            string buildId = line.substr(0, sp);
            string dsoPath = line.substr(sp + 1);
            if (buildId.empty() || dsoPath.empty())
                continue;
            if (dsoPath[0] != '/') // 过滤 [kernel.kallsyms]/[vdso] 等伪 DSO
                continue;
            if (seen.count(buildId))
                continue;
            seen[buildId] = true;
            entries.push_back({buildId, dsoPath});
        }
        return entries;
    }

    bool build_id_cached_locally(const string &buildId)
    {
        string path = build_id_cache_path(buildId);
        return !path.empty() && file_readable(path);
    }

    string build_id_local_cache_path(const string &buildId)
    {
        return build_id_cache_path(buildId);
    }

    bool cache_build_id_locally(const string &buildId, const string &srcPath)
    {
        string dest = build_id_cache_path(buildId);
        if (dest.empty() || !file_readable(srcPath))
            return false;
        size_t slash = dest.rfind('/');
        if (slash == string::npos || !make_dirs(dest.substr(0, slash)))
            return false;

        ifstream in(srcPath, ios::binary);
        if (!in.is_open())
            return false;

        // 先写临时文件再 rename，避免和"同一 build-id 并发预热两次"
        // 竞态时读到写了一半的文件（rename 是原子操作）。临时文件名必须
        // 带上 pid+线程 id：同一进程内两个线程（比如 PerfEventSampler 和
        // DualTrackContinuousSampler）同时预热同一个 build-id 时，如果
        // 共用固定的 "<dest>.tmp"，会并发写同一个临时文件——rename 的
        // 原子性保护不了这一步，写坏的内容仍可能被 rename 进最终缓存。
        std::ostringstream tmpSuffix;
        tmpSuffix << ".tmp." << ::getpid() << "." << std::this_thread::get_id();
        string tmp = dest + tmpSuffix.str();
        {
            ofstream out(tmp, ios::binary);
            if (!out.is_open())
                return false;
            out << in.rdbuf();
            if (!out.good())
            {
                ::remove(tmp.c_str());
                return false;
            }
        }
        if (::rename(tmp.c_str(), dest.c_str()) != 0)
        {
            ::remove(tmp.c_str());
            return false;
        }
        return true;
    }

    unordered_map<string, int> build_dso_path_index()
    {
        unordered_map<string, int> index;
        DIR *proc = opendir("/proc");
        if (!proc)
            return index;

        struct dirent *entry;
        while ((entry = readdir(proc)) != nullptr)
        {
            int pid = 0;
            if (!is_pid_dir(entry->d_name, &pid))
                continue;

            ifstream maps("/proc/" + string(entry->d_name) + "/maps");
            if (!maps.is_open())
                continue;

            string line;
            while (getline(maps, line))
            {
                // /proc/<pid>/maps 每行格式：
                // "<start>-<end> <perms> <offset> <dev> <inode>  <path>"
                // 匿名映射(heap/stack/无路径)没有以 '/' 开头的字段，天然跳过。
                size_t slash = line.find('/');
                if (slash == string::npos)
                    continue;
                string path = line.substr(slash);
                if (path.empty() || path[0] != '/')
                    continue;
                if (index.count(path))
                    continue; // 只记第一个能提供该路径的 pid
                index[path] = pid;
            }
        }
        closedir(proc);
        return index;
    }

    string resolve_via_pid(const string &dsoPath, int pid)
    {
        if (file_readable(dsoPath))
            return dsoPath;
        if (pid > 0)
        {
            string candidate = "/proc/" + to_string(pid) + "/root" + dsoPath;
            if (file_readable(candidate))
                return candidate;
        }
        return "";
    }

} // namespace drop
