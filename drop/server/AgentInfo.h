// ============================================================
// server/AgentInfo.h — Agent 信息追踪（供各 Service 共用）
// ============================================================

#pragma once

#include <string>
#include <vector>
#include <chrono>
#include <mutex>
#include <unordered_map>
#include "common/proto/common.pb.h" // common::PidStats

namespace drop_server
{

    struct AgentInfo
    {
        std::string hostname;
        std::string ipAddr;
        std::string uid;
        std::string version;
        bool online = false;
        std::chrono::steady_clock::time_point lastHeartbeat;
        common::PidStats lastSelfPstats;
        std::vector<common::PidStats> lastChildrenPstats;
    };

    /// 全局 Agent 信息表（按 uid 索引）
    extern std::mutex agents_mutex;
    extern std::unordered_map<std::string, AgentInfo> agents_;
    extern const int AGENT_TIMEOUT_SEC;

} // namespace drop_server
