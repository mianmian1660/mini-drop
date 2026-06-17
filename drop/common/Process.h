// ============================================================
// common/Process.h — /proc 文件系统读取 声明
// ============================================================
// 提供：
//   - read_proc_stat()  读取 /proc/[pid]/stat
//   - read_proc_io()    读取 /proc/[pid]/io
//   - collect_self_pidstats()  采集自身进程的 PidStats
//   - collect_children_pidstats() 采集子进程的 PidStats
//   - pid_exists()      检查 PID 是否存在
// ============================================================

#pragma once

#include <string>
#include <vector>
#include <cstdint>
#include "common/proto/common.pb.h" // common::PidStats

namespace drop
{

    /// /proc/[pid]/stat 的解析结果
    struct ProcStat
    {
        long utime_ticks = 0; // 用户态 CPU 时间（jiffies）
        long stime_ticks = 0; // 内核态 CPU 时间（jiffies）
        long rss_pages = 0;   // 物理内存页数
        std::string comm;     // 进程名
        bool valid = false;
    };

    /// 读取 /proc/[pid]/stat
    ProcStat read_proc_stat(int pid);

    /// /proc/[pid]/io 的解析结果
    struct ProcIO
    {
        uint64_t read_bytes = 0;
        uint64_t write_bytes = 0;
        bool valid = false;
    };

    /// 读取 /proc/[pid]/io
    ProcIO read_proc_io(int pid);

    /// 采集当前进程的 PidStats（两次采样间隔 1 秒计算速率）
    common::PidStats collect_self_pidstats();

    /// 遍历 /proc 找到 PPID=当前进程的子进程，返回其 PidStats
    std::vector<common::PidStats> collect_children_pidstats();

    /// 检查 PID 是否存在（/proc/<pid> 目录是否存在）
    bool pid_exists(int pid);

} // namespace drop
