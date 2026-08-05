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
        std::string agentID;
        std::string version;
        std::string platform;
        std::vector<std::string> capabilities;
        std::vector<std::string> labels;
        std::string resourceBudget;
        bool online = false;
        std::chrono::steady_clock::time_point lastHeartbeat;
        int64_t lastSeenUnixMs = 0;
        common::PidStats lastSelfPstats;
        std::vector<common::PidStats> lastChildrenPstats;
        std::vector<common::AttemptStatus> runningAttempts;
        std::vector<common::AttemptStatus> completedAttempts;
    };

    /// 全局 Agent 信息表（按 uid 索引）
    extern std::mutex agents_mutex;
    extern std::unordered_map<std::string, AgentInfo> agents_;
    extern const int AGENT_TIMEOUT_SEC;

} // namespace drop_server
