// ============================================================
// common/PprofProfiler.h — pprof（Go）采集器 声明
// ============================================================
// profilerType=2: 通过 HTTP 端点采集 Go 程序的 pprof profile
// ============================================================

#pragma once

#include <string>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop
{

    /// 执行 pprof 采集（profilerType=2）
    /// 从 Go 程序的 /debug/pprof/profile HTTP 端点拉取 CPU profile
    /// @param taskDesc  任务描述（pprof 端点信息通过 event 字段传递）
    ///                   event 格式: "http://host:port" 或 ":port"
    ///                   pid 字段可用于端口号（当 event 为空时取 :pid 作为端口）
    /// @param outputPath 输出文件路径（pprof 格式）
    /// @return 0=成功, -1=fork失败, -2=exec/wait异常, -3=超时, -4=连接失败, >0=退出码
    int run_pprof(const hotmethod::TaskDesc &taskDesc, const std::string &outputPath);

} // namespace drop
