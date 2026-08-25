// ============================================================
// common/Process.h — /proc 文件系统读取 声明
// ============================================================
// 提供：
//   - read_proc_stat()  读取 /proc/[pid]/stat
//   - read_proc_io()    读取 /proc/[pid]/io
//   - collect_self_pidstats()  采集自身进程的 PidStats
//   - collect_children_pidstats() 采集子进程的 PidStats
//   - pid_exists()      检查 PID 是否存在
// 整机维度（宿主机资源，随心跳上报）：
//   - read_host_cpu_jiffies()  /proc/stat 整机 CPU 计数
//   - compute_host_cpu_percent() 两次采样增量计算整机 CPU 使用率
//   - read_host_meminfo()     /proc/meminfo 整机内存
//   - read_host_disk()        statvfs 宿主机根分区容量
// ============================================================

#pragma once

#include <string>
#include <vector>
#include <cstdint>
#include "common/proto/common.pb.h" // common::PidStats, common::HostStats

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

    /// 采集当前进程的 PidStats（两次采样间隔 1 秒计算速率）。
    /// 可选：hostStatsOut 非空时，复用同一个 1 秒采样窗口同时填充整机
    /// HostStats（宿主机 CPU/内存/磁盘），不额外拖慢心跳。
    /// diskMountPath 是 Docker 部署下指向宿主机根分区绑定目录的 statvfs 路径。
    common::PidStats collect_self_pidstats(
        common::HostStats *hostStatsOut = nullptr,
        const std::string &diskMountPath = "/");

    /// 采集宿主机身份与系统信息（/etc/os-release、uname、/proc/cpuinfo、
    /// /proc/uptime）并填充 common::HostMetadata。
    /// 单个字段失败不影响其他字段；失败字段留空或为 0，不伪造默认值。
    /// 路径可注入用于单元测试；默认读取真实系统文件。
    void collect_host_metadata(common::HostMetadata *out,
                               const std::string &osReleasePath = "/etc/os-release",
                               const std::string &cpuInfoPath = "/proc/cpuinfo",
                               const std::string &uptimePath = "/proc/uptime");

    /// 遍历 /proc 找到 PPID=当前进程的子进程，返回其 PidStats
    std::vector<common::PidStats> collect_children_pidstats();

    /// 检查 PID 是否存在（/proc/<pid> 目录是否存在）
    bool pid_exists(int pid);

    // ------------------------------------------------------------
    // 整机（宿主机）资源采集
    // ------------------------------------------------------------

    /// /proc/stat 第一行 "cpu " 的一次采样
    struct HostCpuSample
    {
        uint64_t total_jiffies = 0; // 所有字段之和（含 idle/iowait）
        uint64_t idle_jiffies = 0;  // idle + iowait
        bool valid = false;         // /proc/stat 是否成功读取
    };

    /// 读取 /proc/stat 整机 CPU 累计计数（宿主命名空间）。
    /// path 可注入用于单元测试；默认 /proc/stat。
    HostCpuSample read_host_cpu_jiffies(const std::string &path = "/proc/stat");

    /// 由两次采样增量计算整机 CPU 使用率（0-100）。
    /// 处理零增量（采样间隔内无变化）与计数器异常（after 小于 before）：
    /// 返回 0% 而不是负数或异常值；任一采样无效时返回 0。
    double compute_host_cpu_percent(const HostCpuSample &before,
                                    const HostCpuSample &after);

    /// /proc/meminfo 的解析结果（字节）
    struct HostMemInfo
    {
        uint64_t total_bytes = 0;
        uint64_t available_bytes = 0;
        bool valid = false;
    };

    /// 读取 /proc/meminfo，使用 MemTotal - MemAvailable 计算已用量。
    /// path 可注入用于单元测试；默认 /proc/meminfo。
    HostMemInfo read_host_meminfo(const std::string &path = "/proc/meminfo");

    /// statvfs 磁盘容量结果（字节）
    struct HostDiskInfo
    {
        uint64_t used_bytes = 0;
        uint64_t total_bytes = 0;
        bool valid = false;
        std::string mount; // 逻辑挂载点标签（如 "/"）
    };

    /// 读取挂载点容量（默认 "/"；Docker 下可传宿主机绑定目录，如 "/tmp"）。
    /// 页面统一把结果标记为"系统盘 /"，避免误读容器 overlay 容量。
    HostDiskInfo read_host_disk(const std::string &mountPath = "/");

} // namespace drop
