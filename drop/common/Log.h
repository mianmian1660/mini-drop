// ============================================================
// common/Log.h — 轻量结构化日志（JSON 行）
// ============================================================
// 只用于关键生命周期事件（注册/心跳失败/收到任务/采集起止/上传结果/
// NotifyResult 上报），不替代原有的调试性 cout/cerr 输出。
// 格式和 apiserver(zap JSON)/analysis(observability.py log_event) 的思路
// 保持一致：component + event + ts(ISO8601) + 任意字段，一行一条打到 stderr，
// 方便和另外两个模块的日志一起被采集/检索。
// ============================================================

#pragma once

#include <string>
#include <utility>
#include <vector>

namespace drop
{

    /// 输出一条结构化 JSON 行日志。
    /// component 固定传 "drop_agent" 或 "drop_server"；event 是事件名
    /// （如 "agent_registered"/"task_received"/"collection_failed"）；
    /// fields 是任意附加的 key-value 字段（值统一按字符串处理，数字/布尔
    /// 由调用方自行 to_string）。
    void log_event(const std::string &component, const std::string &event,
                    const std::vector<std::pair<std::string, std::string>> &fields = {});

} // namespace drop
