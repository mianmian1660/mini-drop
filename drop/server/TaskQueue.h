// ============================================================
// server/TaskQueue.h — 任务队列（供各 Service 共用）
// ============================================================

#pragma once

#include <string>
#include <queue>
#include <mutex>
#include <unordered_map>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop_server
{

    /// 全局任务队列（按 targetIP 索引）
    extern std::mutex tasks_mutex;
    extern std::unordered_map<std::string, std::queue<hotmethod::TaskDesc>> tasks_;

} // namespace drop_server
