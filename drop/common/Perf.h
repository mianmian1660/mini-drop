// ============================================================
// common/Perf.h — perf 采集执行 声明
// ============================================================
// 封装 Linux perf 工具的 fork+execvp 调用
// 支持：PID 检查、超时杀进程组、进程状态收集
// ============================================================

#pragma once

#include <string>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop
{

    /// 执行 perf record 采集
    /// @param taskDesc  任务描述（PID、频率、时长等）
    /// @param outputPath 输出文件路径
    /// @return 0=成功, -1=fork失败, -2=exec/wait异常, -3=超时, -4=PID不存在, >0=perf退出码
    int run_perf(const hotmethod::TaskDesc &taskDesc, const std::string &outputPath);

} // namespace drop
