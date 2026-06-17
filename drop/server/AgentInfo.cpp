// ============================================================
// server/AgentInfo.cpp — Agent 信息追踪 全局变量
// ============================================================

#include "server/AgentInfo.h"

namespace drop_server
{

    std::mutex agents_mutex;
    std::unordered_map<std::string, AgentInfo> agents_;
    const int AGENT_TIMEOUT_SEC = 30;

} // namespace drop_server
