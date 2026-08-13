// ============================================================
// common/SymbolCollector.h — 用户态符号按 build-id 去重上传（阶段三）
// ============================================================
// 提取 perf.data 引用的 build-id 清单，问 apiserver 哪些已经有了，
// 只上传服务端缺失的那部分二进制。失败只降级（不影响任务成败）——
// 上传不了的符号就是继续显示 [模块名]，不应该让整个采集任务失败。
// ============================================================

#pragma once

#include <string>

namespace drop
{

    /// @param perfDataPath 本次采集产出的 perf.data 本地路径
    /// @param tid          任务 ID，随 build-id 清单一起写入服务端 task_build_ids 表
    /// @param targetPid    本次采集的目标 PID（>0 时用于跨容器路径解析兜底；
    ///                     0 表示整机模式，无法为所有 DSO 做路径兜底，是已知局限）
    /// @param apiBaseURL   apiserver 基础地址，例如 "http://apiserver:8191"
    /// @return 完成一次检查+尽力上传返回 true；连服务端都联系不上返回 false
    bool collect_and_upload_symbols(const std::string &perfDataPath,
                                     const std::string &tid,
                                     int targetPid,
                                     const std::string &apiBaseURL);

} // namespace drop
