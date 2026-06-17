// ============================================================
// common/AsyncProfilerProfiler.h — async-profiler（Java）采集器 声明
// ============================================================
// profilerType=1: 调用 asprof 工具对 Java 进程进行 CPU/内存采样
// 输出折叠栈格式 (.collapsed) 或 JFR 格式
// ============================================================

#pragma once

#include <string>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop
{

    /// 执行 async-profiler 采集（profilerType=1）
    /// 调用 asprof 命令行工具对目标 Java 进程采样
    /// @param taskDesc  任务描述（PID、频率、时长等）
    /// @param outputPath 输出文件路径（折叠栈 .collapsed 或 .jfr）
    /// @return 0=成功, -1=fork失败, -2=exec/wait异常, -3=超时, -4=PID不存在, >0=退出码
    int run_async_profiler(const hotmethod::TaskDesc &taskDesc, const std::string &outputPath);

} // namespace drop
