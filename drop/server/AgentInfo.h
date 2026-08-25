// ============================================================
// server/AgentInfo.h — Agent 信息追踪（供各 Service 共用）
// ============================================================

#pragma once

#include <string>
#include <vector>
#include <chrono>
#include <mutex>
#include <unordered_map>
#include "common/proto/common.pb.h" // common::PidStats, common::HostStats

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
        // 整机资源：hasHostStats 区分"旧 Agent 未上报"（false）与
        // "已上报但部分指标不可用"（true，靠各 *_available 标志降级）
        common::HostStats lastHostStats;
        bool hasHostStats = false;
        // 主机身份与系统信息：hasHostMetadata 区分"旧 Agent 未上报"（false）与
        // "已上报但部分字段缺失"（true，靠空字段降级）
        common::HostMetadata lastHostMetadata;
        bool hasHostMetadata = false;
        std::vector<common::AttemptStatus> runningAttempts;
        std::vector<common::AttemptStatus> completedAttempts;
    };

    /// 全局 Agent 信息表（按 uid 索引）
    extern std::mutex agents_mutex;
    extern std::unordered_map<std::string, AgentInfo> agents_;
    extern const int AGENT_TIMEOUT_SEC;

} // namespace drop_server
