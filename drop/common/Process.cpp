// ============================================================
// common/Process.cpp — /proc 文件系统读取 实现
// ============================================================

#include "common/Process.h"
#include <fstream>
#include <sstream>
#include <string>
#include <vector>
#include <thread>
#include <chrono>
#include <unistd.h>   // getpid, sysconf
#include <sys/stat.h> // stat, S_ISDIR
#include <dirent.h>   // opendir, readdir
#include <cstdlib>    // atoi, atol

namespace drop
{

    ProcStat read_proc_stat(int pid)
    {
        ProcStat ps;
        std::string path = "/proc/" + std::to_string(pid) + "/stat";
        std::ifstream f(path);
        if (!f.is_open())
            return ps;
        std::string line;
        std::getline(f, line);

        // 格式: pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime ...
        size_t rparen = line.rfind(')');
        if (rparen == std::string::npos)
            return ps;
        size_t lparen = line.find('(');
        if (lparen == std::string::npos || lparen >= rparen)
            return ps;
        ps.comm = line.substr(lparen + 1, rparen - lparen - 1);

        // 括号后的部分用空格分割
        std::string rest = line.substr(rparen + 2);
        std::istringstream iss(rest);
        std::vector<std::string> fields;
        std::string field;
        while (iss >> field)
            fields.push_back(field);

        // utime = fields[11], stime = fields[12]
        if (fields.size() >= 12)
        {
            ps.utime_ticks = atol(fields[11].c_str());
            ps.stime_ticks = atol(fields[12].c_str());
        }
        // rss = fields[21]
        if (fields.size() >= 22)
        {
            ps.rss_pages = atol(fields[21].c_str());
        }
        ps.valid = true;
        return ps;
    }

    ProcIO read_proc_io(int pid)
    {
        ProcIO io;
        std::string path = "/proc/" + std::to_string(pid) + "/io";
        std::ifstream f(path);
        if (!f.is_open())
            return io;
        std::string line;
        while (std::getline(f, line))
        {
            if (line.find("read_bytes:") == 0)
            {
                io.read_bytes = strtoull(line.c_str() + 11, nullptr, 10);
            }
            else if (line.find("write_bytes:") == 0)
            {
                io.write_bytes = strtoull(line.c_str() + 12, nullptr, 10);
            }
        }
        io.valid = true;
        return io;
    }

    common::PidStats collect_self_pidstats()
    {
        common::PidStats ps;
        int mypid = getpid();
        ps.set_pid(mypid);

        ProcStat s1 = read_proc_stat(mypid);
        ProcIO io1 = read_proc_io(mypid);
        long hz = sysconf(_SC_CLK_TCK);

        // 等 1 秒
        std::this_thread::sleep_for(std::chrono::seconds(1));

        ProcStat s2 = read_proc_stat(mypid);
        ProcIO io2 = read_proc_io(mypid);

        if (s1.valid && s2.valid)
        {
            long total_ticks = (s2.utime_ticks - s1.utime_ticks) + (s2.stime_ticks - s1.stime_ticks);
            if (total_ticks < 0)
                total_ticks = 0;
            double cpuPct = (double)total_ticks / (double)hz * 100.0;
            ps.set_cpupercent(cpuPct);
            ps.set_rsskb((uint64_t)s2.rss_pages * 4);
            ps.set_comm(s2.comm);
        }
        if (io1.valid && io2.valid)
        {
            uint64_t readDelta = (io2.read_bytes > io1.read_bytes) ? (io2.read_bytes - io1.read_bytes) : 0;
            uint64_t writeDelta = (io2.write_bytes > io1.write_bytes) ? (io2.write_bytes - io1.write_bytes) : 0;
            ps.set_readkbpers(readDelta / 1024);
            ps.set_writekbpers(writeDelta / 1024);
        }
        return ps;
    }

    std::vector<common::PidStats> collect_children_pidstats()
    {
        std::vector<common::PidStats> result;
        DIR *dir = opendir("/proc");
        if (!dir)
            return result;
        int mypid = getpid();
        struct dirent *entry;
        while ((entry = readdir(dir)) != nullptr)
        {
            int childPid = atoi(entry->d_name);
            if (childPid <= 0)
                continue;
            ProcStat ps = read_proc_stat(childPid);
            if (!ps.valid)
                continue;
            // 通过 /proc/pid/status 读 PPID
            std::string statusPath = "/proc/" + std::to_string(childPid) + "/status";
            std::ifstream f(statusPath);
            if (!f.is_open())
                continue;
            std::string line;
            int ppid = 0;
            while (std::getline(f, line))
            {
                if (line.find("PPid:") == 0)
                {
                    ppid = atoi(line.c_str() + 5);
                    break;
                }
            }
            if (ppid == mypid)
            {
                common::PidStats childPs;
                childPs.set_pid(childPid);
                childPs.set_comm(ps.comm);
                childPs.set_rsskb((uint64_t)ps.rss_pages * 4);
                result.push_back(childPs);
            }
        }
        closedir(dir);
        return result;
    }

    bool pid_exists(int pid)
    {
        if (pid <= 0)
            return false;
        std::string path = "/proc/" + std::to_string(pid);
        struct ::stat st;
        return ::stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
    }

} // namespace drop
