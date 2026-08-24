// ============================================================
// common/KernelSymbols.h — 内核符号(kallsyms)快照与去重上传（共享）
// ============================================================
// 持续采集链路复用一次性任务已验证的 kallsyms 快照/上传能力。这里提供一份
// 自包含实现，供 drop/common 下的采集器调用，避免依赖 drop/agent 内部的
// 匿名命名空间私有函数。与 drop/agent/UploadWorker.cpp 里的一次性任务实现
// 逻辑等价（同一套 /proc/kallsyms 快照 + kernel-symbols 去重上传协议），
// 后续若要统一，可把 UploadWorker.cpp 也切换到这里以消除重复。
// ============================================================

#pragma once

#include <string>

namespace drop
{

    /// 快照 /proc/kallsyms 到 outPath。地址全为 0（缺少 CAP_SYSLOG 或
    /// kptr_restrict 受限）或文件为空时返回 false 并拒绝写无效快照。
    bool snapshot_kallsyms(const std::string &outPath);

    /// 按 sha256 去重上传 kallsyms 快照到服务端符号库。
    /// @param apiBaseURL apiserver 基础地址
    /// @param tid         用于登记引用的任务/会话标识（持续采集传 session_sid）
    /// @param kallsymsPath 本地快照文件路径
    /// @return 成功（服务端已有或本次上传成功）时返回 sha256，失败返回空串
    std::string ensure_kernel_symbol_uploaded(const std::string &apiBaseURL,
                                              const std::string &tid,
                                              const std::string &kallsymsPath);

} // namespace drop
