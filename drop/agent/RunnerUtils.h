// ============================================================
// agent/RunnerUtils.h — Runner 共用的纯函数工具
// ============================================================
// 从旧 agent/main.cpp 里的 class Runner 私有方法 + 自由函数搬出来，
// 保持逻辑完全不变（只是从私有方法/静态函数变成可独立单测的自由函数）。
// ============================================================

#pragma once

#include "common/proto/hotmethod.pb.h"

#include <string>

namespace drop_agent
{

    /// 采集产物在 MinIO 里的相对文件名（不含 "<tid>/" 前缀，调用方自己拼）。
    std::string RemoteKeyFor(const std::string &profilerName);

    /// 采集产物的 HTTP Content-Type。
    std::string ContentTypeFor(const std::string &profilerName);

    /// resultCode/profilerName -> 面向用户的错误消息。resultCode==0 返回空串。
    std::string GetErrorMessage(int resultCode, const std::string &profilerName,
                                const hotmethod::TaskDesc &task);

    /// resultCode/profilerName -> 稳定错误码字符串。resultCode==0 返回空串。
    std::string GetErrorCode(int resultCode, const std::string &profilerName);

    /// 文件是否存在且非空（fork+exec 型采集器判断"进程说成功但产物无效"用）。
    bool FileExistsNonEmpty(const std::string &path);

} // namespace drop_agent
