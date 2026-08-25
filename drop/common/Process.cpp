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
#include <algorithm>   // std::min
#include <unistd.h>    // getpid, sysconf
#include <sys/stat.h>      // stat, S_ISDIR
#include <sys/statvfs.h>   // statvfs
#include <sys/utsname.h>   // uname
#include <dirent.h>        // opendir, readdir
#include <cstdlib>         // atoi, atol, strtoull

namespace drop
{

    // 前向声明：把一次采样窗口的整机数据填充进 protobuf HostStats
    static void fill_host_stats(common::HostStats &out,
                                const HostCpuSample &before,
                                const HostCpuSample &after,
                                const std::string &diskMountPath);

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

    common::PidStats collect_self_pidstats(common::HostStats *hostStatsOut,
                                           const std::string &diskMountPath)
    {
        common::PidStats ps;
        int mypid = getpid();
        ps.set_pid(mypid);

        ProcStat s1 = read_proc_stat(mypid);
        ProcIO io1 = read_proc_io(mypid);
        HostCpuSample hostCpu1 = read_host_cpu_jiffies();
        long hz = sysconf(_SC_CLK_TCK);

        // 等 1 秒（同时作为整机 CPU 两次采样的间隔，不额外拖慢心跳）
        std::this_thread::sleep_for(std::chrono::seconds(1));

        ProcStat s2 = read_proc_stat(mypid);
        ProcIO io2 = read_proc_io(mypid);
        HostCpuSample hostCpu2 = read_host_cpu_jiffies();

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

        if (hostStatsOut)
        {
            fill_host_stats(*hostStatsOut, hostCpu1, hostCpu2, diskMountPath);
        }
        return ps;
    }

    // ------------------------------------------------------------
    // 整机（宿主机）资源采集实现
    // ------------------------------------------------------------

    HostCpuSample read_host_cpu_jiffies(const std::string &path)
    {
        HostCpuSample sample;
        std::ifstream f(path);
        if (!f.is_open())
            return sample;
        std::string line;
        while (std::getline(f, line))
        {
            if (line.rfind("cpu ", 0) == 0)
            {
                // 第一行 "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
                // total 统计所有字段，idle 统计 idle+iowait（guest/guest_nice 已含在 user/nice 中）
                std::istringstream iss(line);
                std::string tag;
                iss >> tag;
                uint64_t value = 0;
                uint64_t total = 0;
                uint64_t idle = 0;
                int fieldIdx = 0;
                while (iss >> value)
                {
                    total += value;
                    // 字段顺序：user=0 nice=1 system=2 idle=3 iowait=4 ...
                    if (fieldIdx == 3 || fieldIdx == 4)
                        idle += value;
                    fieldIdx++;
                }
                if (fieldIdx >= 4)
                {
                    sample.total_jiffies = total;
                    sample.idle_jiffies = idle;
                    sample.valid = true;
                }
                break;
            }
        }
        return sample;
    }

    double compute_host_cpu_percent(const HostCpuSample &before,
                                    const HostCpuSample &after)
    {
        // 任一采样无效：无法计算，返回 0（由调用方结合 *_available 判定不可用）
        if (!before.valid || !after.valid)
            return 0.0;
        // 零增量或计数器异常（after 不增反减）：不能算出负数，返回 0%
        if (after.total_jiffies <= before.total_jiffies || after.idle_jiffies < before.idle_jiffies)
            return 0.0;

        uint64_t totalDelta = after.total_jiffies - before.total_jiffies;
        uint64_t idleDelta = after.idle_jiffies - before.idle_jiffies;
        if (idleDelta > totalDelta)
            idleDelta = totalDelta;
        uint64_t busyDelta = totalDelta - idleDelta;
        if (totalDelta == 0)
            return 0.0;

        double percent = (double)busyDelta / (double)totalDelta * 100.0;
        if (percent < 0.0)
            percent = 0.0;
        if (percent > 100.0)
            percent = 100.0;
        return percent;
    }

    HostMemInfo read_host_meminfo(const std::string &path)
    {
        HostMemInfo mem;
        std::ifstream f(path);
        if (!f.is_open())
            return mem;
        std::string line;
        while (std::getline(f, line))
        {
            if (line.rfind("MemTotal:", 0) == 0)
            {
                mem.total_bytes = (uint64_t)strtoull(line.c_str() + 9, nullptr, 10) * 1024;
            }
            else if (line.rfind("MemAvailable:", 0) == 0)
            {
                mem.available_bytes = (uint64_t)strtoull(line.c_str() + 13, nullptr, 10) * 1024;
            }
        }
        mem.valid = (mem.total_bytes > 0);
        return mem;
    }

    HostDiskInfo read_host_disk(const std::string &mountPath)
    {
        HostDiskInfo disk;
        struct statvfs vfs;
        if (statvfs(mountPath.c_str(), &vfs) != 0)
            return disk;
        // 使用 f_frsize（基础块大小）与 f_bavail（非 root 可用块）计算
        uint64_t frsize = vfs.f_frsize > 0 ? (uint64_t)vfs.f_frsize : (uint64_t)vfs.f_bsize;
        if (frsize == 0 || vfs.f_blocks == 0)
            return disk;
        disk.total_bytes = (uint64_t)vfs.f_blocks * frsize;
        uint64_t free_bytes = (uint64_t)vfs.f_bavail * frsize;
        disk.used_bytes = (free_bytes <= disk.total_bytes) ? (disk.total_bytes - free_bytes) : 0;
        disk.valid = true;
        disk.mount = "/"; // 页面统一标记为"系统盘 /"，避免误读容器 overlay 容量
        return disk;
    }

    // 把一次采样窗口的整机数据填充进 protobuf HostStats
    static void fill_host_stats(common::HostStats &out,
                                const HostCpuSample &before,
                                const HostCpuSample &after,
                                const std::string &diskMountPath)
    {
        out.set_cpu_available(before.valid && after.valid);
        if (out.cpu_available())
            out.set_cpu_percent(compute_host_cpu_percent(before, after));

        HostMemInfo mem = read_host_meminfo();
        out.set_memory_available(mem.valid);
        if (mem.valid)
        {
            out.set_memory_total_bytes(mem.total_bytes);
            out.set_memory_used_bytes(mem.total_bytes - std::min(mem.available_bytes, mem.total_bytes));
            out.set_memory_percent(mem.total_bytes > 0
                                       ? (double)out.memory_used_bytes() / (double)mem.total_bytes * 100.0
                                       : 0.0);
        }

        HostDiskInfo disk = read_host_disk(diskMountPath);
        out.set_disk_available(disk.valid);
        if (disk.valid)
        {
            out.set_disk_total_bytes(disk.total_bytes);
            out.set_disk_used_bytes(disk.used_bytes);
            out.set_disk_percent(disk.total_bytes > 0
                                     ? (double)disk.used_bytes / (double)disk.total_bytes * 100.0
                                     : 0.0);
            out.set_disk_mount(disk.mount);
        }

        out.set_collected_at_unix_ms(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::system_clock::now().time_since_epoch())
                .count());
    }

    // ------------------------------------------------------------
    // 宿主机身份与系统信息采集（HostMetadata）
    // ------------------------------------------------------------

    // 去掉 os-release 值两侧的引号（NAME="Ubuntu" → Ubuntu）
    static std::string trim_os_release_value(const std::string &raw)
    {
        std::string value = raw;
        if (value.size() >= 2 && value.front() == '"' && value.back() == '"')
            value = value.substr(1, value.size() - 2);
        return value;
    }

    void collect_host_metadata(common::HostMetadata *out,
                               const std::string &osReleasePath,
                               const std::string &cpuInfoPath,
                               const std::string &uptimePath)
    {
        if (!out)
            return;

        // /etc/os-release：NAME= 与 VERSION_ID=（单个字段失败不影响其他字段）
        {
            std::ifstream f(osReleasePath);
            std::string line;
            while (std::getline(f, line))
            {
                if (line.rfind("NAME=", 0) == 0 && out->os_name().empty())
                {
                    out->set_os_name(trim_os_release_value(line.substr(5)));
                }
                else if (line.rfind("VERSION_ID=", 0) == 0 && out->os_version().empty())
                {
                    out->set_os_version(trim_os_release_value(line.substr(11)));
                }
            }
        }

        // uname：内核版本与架构；os-release 读取失败时用 sysname 兜底 os_name
        {
            struct utsname uts;
            if (uname(&uts) == 0)
            {
                if (out->os_name().empty() && uts.sysname[0])
                    out->set_os_name(uts.sysname);
                if (uts.release[0])
                    out->set_kernel_version(uts.release);
                if (uts.machine[0])
                    out->set_architecture(uts.machine);
            }
        }

        // /proc/cpuinfo：model name 与 processor 条目数（在线 CPU 核数）
        {
            std::ifstream f(cpuInfoPath);
            std::string line;
            int cores = 0;
            while (std::getline(f, line))
            {
                if (line.rfind("model name", 0) == 0 && out->cpu_model().empty())
                {
                    size_t colon = line.find(':');
                    if (colon != std::string::npos)
                    {
                        std::string value = line.substr(colon + 1);
                        size_t start = value.find_first_not_of(" \t");
                        if (start != std::string::npos)
                            out->set_cpu_model(value.substr(start));
                    }
                }
                else if (line.rfind("processor", 0) == 0)
                {
                    cores++;
                }
            }
            if (cores > 0)
                out->set_cpu_cores(cores);
        }

        // /proc/uptime：第一行第一个数字（开机秒数）
        {
            std::ifstream f(uptimePath);
            std::string line;
            if (std::getline(f, line))
            {
                std::istringstream iss(line);
                double uptime = 0.0;
                if (iss >> uptime && uptime >= 0)
                    out->set_uptime_seconds((int64_t)uptime);
            }
        }

        out->set_collected_at_unix_ms(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::system_clock::now().time_since_epoch())
                .count());
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
