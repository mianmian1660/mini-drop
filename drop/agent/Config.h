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
        std::string ipAddr;
        std::string uid;
        std::string agentVersion;
        std::string platform;
        std::vector<std::string> capabilities;
        std::vector<std::string> labels;
        std::string resourceBudget;

        // 多 Server 故障转移列表（按优先级顺序尝试）
        std::vector<std::string> serverAddrs;

        // 运行时参数
        uint32_t heartbeatIntervalSec = 5; // 心跳间隔（秒）
        uint32_t registerTimeoutSec = 10;  // 注册超时（秒）
        uint32_t heartbeatTimeoutSec = 10; // 心跳 RPC 超时（秒），Phase 3：优雅关闭时避免心跳线程无限阻塞

        // Phase 5：CleanupWorker 参数
        uint32_t cleanupIntervalSec = 60;    // CleanupWorker 扫描周期（秒）
        uint32_t taskDirRetentionSec = 3600; // 任务目录保留时长（秒），超过此时长的 /tmp/drop_agent/tasks/<taskID>/<attemptID>/ 会被清理
        uint32_t orphanPidGraceSec = 300;    // pid 登记表宽限期（秒），远大于任何 Runner 自身的 timeoutSec+gracePeriodSec，超过仍未摘牌视为孤儿进程

        // 整机磁盘采集的 statvfs 路径。Docker 部署下容器 overlay 容量没有
        // 意义，这里应指向宿主机根分区上的绑定目录（如 /tmp），页面统一
        // 标记为"系统盘 /"。默认 "/"（裸机部署时即宿主机根分区）。
        std::string hostDiskMount = "/";

        /// 从 JSON 文件加载配置
        static AgentConfig LoadFromFile(const std::string &configPath);

        /// 使用默认配置 + 命令行 server 地址回退
        static AgentConfig Default(const std::string &serverAddr = "localhost:50051");
    };

} // namespace drop_agent
