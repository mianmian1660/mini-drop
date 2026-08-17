// ============================================================
// agent/AgentUtils.h — HeartbeatThread 与 WorkerThread 共用的小工具函数
// ============================================================
// Phase 3 拆分：这几个纯函数在 main.cpp 单线程时代同时被"心跳"和"任务
// 执行"两段代码用到，拆成两个线程后仍然共用，避免各自维护一份重复实现。
// ============================================================

#pragma once

#include <string>

namespace drop_agent
{

    bool FileExists(const std::string &path);

    /// JSON 字符串转义（引号/反斜杠/换行）
    std::string JsonEscape(const std::string &s);

    /// 读取布尔型环境变量："1"/"true"/"yes"/"on"（大小写不敏感）视为开启
    bool EnvEnabled(const char *name);

    /// 读取字符串型环境变量，未设置或为空时返回 fallback
    std::string EnvString(const char *name, const std::string &fallback = "");

} // namespace drop_agent
