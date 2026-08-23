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

    /// 探测 basePath/basePath+".collapsed"/".pb.gz"/".bpf"/".bpf.raw" 里哪个
    /// 真实存在，找不到就回退成 basePath 本身——逐字对齐旧 class Runner::
    /// ResolveOutputPath()，用于兜底/mock 路径写到了哪个变体文件的场景。
    std::string ResolveOutputPath(const std::string &basePath);

    /// 采集产物在 MinIO 里的相对文件名（不含 "<tid>/" 前缀，调用方自己拼）。
    std::string RemoteKeyFor(const std::string &profilerName);

    /// 阶段 4：RAW 对象 key。
    /// v2 布局（DROP_AGENT_LAYOUT_V2 默认开启）：tasks/{tid}/attempts/{attempt_id}/raw/{basename}
    /// 旧布局回退：{tid}/{basename}。attemptId<=0 或 basename 非法时回退旧布局。
    std::string RawObjectKey(const std::string &tid, uint64_t attemptId,
                             const std::string &basename);

    /// 阶段 4：采集 manifest key。
    /// v2 布局：tasks/{tid}/attempts/{attempt_id}/manifest.json；否则 {tid}/manifest.json。
    std::string ManifestObjectKey(const std::string &tid, uint64_t attemptId);

    /// basename 合法性校验：只允许 [A-Za-z0-9._-]，禁止空、"."、".." 与目录分隔符。
    bool ValidRemoteBasename(const std::string &basename);

    /// 阶段 4 布局开关（环境变量 DROP_AGENT_LAYOUT_V2，1/true/yes/on 视为开启，
    /// 默认开启；关闭时保持旧 {tid}/... 写入）。
    bool LayoutV2Enabled();

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
