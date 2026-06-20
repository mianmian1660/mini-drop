// ============================================================
// common/BpfProfiler.h — eBPF 采集器 声明 (profilerType=3)
// ============================================================
// 使用 bpftrace 实现内核态探针，支持：
//   - CPU 性能分析（profile:hz provider）
//   - IO 延迟分布追踪（kprobe:blk_account_io_*）
//   - 调度延迟追踪（kprobe:schedule / wakeup）
//
// 输出格式：
//   - CPU 模式：折叠栈格式（兼容现有火焰图管线）
//   - IO 模式：延迟直方图 (microseconds:count 格式)
// ============================================================

#pragma once

#include <string>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop
{

    /// eBPF 采集模式
    enum class BpfMode
    {
        CPU = 0,          // CPU 性能分析 (profile:hz)
        IO_LATENCY = 1,   // IO 延迟分布 (kprobe blk_*)
        SCHED_LATENCY = 2 // 调度延迟 (kprobe schedule/wakeup)
    };

    /// 执行 eBPF 采集（profilerType=3）
    /// 使用 bpftrace 执行内核态探针脚本
    /// @param taskDesc  任务描述
    ///                   event 字段用于指定模式："cpu"/"io"/"sched"（默认 cpu）
    /// @param outputPath 输出文件路径（.txt 直方图 或 .collapsed 折叠栈）
    /// @return 0=成功, -1=fork失败, -2=exec/wait异常, -3=超时, -4=PID不存在/target不可达
    int run_bpf(const hotmethod::TaskDesc &taskDesc, const std::string &outputPath);

    /// 将 event 字符串解析为 BpfMode 枚举
    BpfMode parse_bpf_mode(const std::string &event);

} // namespace drop
