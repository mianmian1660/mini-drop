// ============================================================
// agent/Config.h — Agent 配置文件读取 声明
// ============================================================
// 功能：
//   - 读取 JSON 配置文件
//   - 支持多 Server 地址列表（故障转移）
//   - Agent 身份信息（hostname、uid、version）
//   - 心跳间隔、超时等运行时参数
// ============================================================

#pragma once

#include <string>
#include <vector>
#include <cstdint>

namespace drop_agent
{

    /// Agent 配置结构
    struct AgentConfig
    {
        // Agent 身份
        std::string hostname;
        std::string uid;
        std::string agentVersion;

        // 多 Server 故障转移列表（按优先级顺序尝试）
        std::vector<std::string> serverAddrs;

        // 运行时参数
        uint32_t heartbeatIntervalSec = 5; // 心跳间隔（秒）
        uint32_t registerTimeoutSec = 10;  // 注册超时（秒）

        /// 从 JSON 文件加载配置
        static AgentConfig LoadFromFile(const std::string &configPath);

        /// 使用默认配置 + 命令行 server 地址回退
        static AgentConfig Default(const std::string &serverAddr = "localhost:50051");
    };

} // namespace drop_agent
