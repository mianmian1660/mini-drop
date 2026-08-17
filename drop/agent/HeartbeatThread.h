// ============================================================
// agent/HeartbeatThread.h — 心跳 + 故障转移 + 派发任务入队
// ============================================================
// Phase 3 从 main.cpp 拆出：原来单线程循环里"发心跳→故障转移重连→收到
// 任务"的那一段独立成一个线程，不再和任务执行共享同一个循环——这样心跳
// 不会被 WorkerThread 正在跑的采集阻塞（新复刻指南 5.7 节）。
// ============================================================

#pragma once

#include <atomic>
#include <memory>
#include <string>
#include <thread>
#include <chrono>
#include <grpcpp/grpcpp.h>
#include "agent/Config.h"
#include "agent/TaskQueue.h"
#include "agent/AttemptTracker.h"
#include "agent/ServerChannelHolder.h"
#include "common/ContinuousSampler.h" // drop::DualTrackContinuousSampler
#include "common/proto/common.pb.h"   // common::CosConfig
#include "common/proto/healthcheck.grpc.pb.h"
#include "common/proto/hotmethod.grpc.pb.h"

namespace drop_agent
{

    /// 遍历 cfg.serverAddrs，注册到第一个可用 Server 并拉取 COS 配置。
    /// 注册失败返回 nullptr；registered 输出注册结果。main() 用它做启动时
    /// 的首次连接，HeartbeatThread 内部故障转移时也复用同一份逻辑。
    std::shared_ptr<grpc::Channel> ConnectToServer(
        const AgentConfig &cfg,
        common::CosConfig &cosConfig,
        bool &registered);

    class HeartbeatThread
    {
    public:
        HeartbeatThread(const AgentConfig &cfg,
                         std::shared_ptr<grpc::Channel> initialChannel,
                         const common::CosConfig &initialCosConfig,
                         TaskQueue &taskQueue,
                         AttemptTracker &attemptTracker,
                         ServerChannelHolder &channelHolder,
                         std::atomic<bool> &runningFlag);

        void Start();

        /// join 线程并停止内部持有的 Native CP 采样器。幂等。
        void Stop();

    private:
        void Loop();

        const AgentConfig &cfg_;
        std::shared_ptr<grpc::Channel> channel_;
        common::CosConfig cosConfig_;
        bool registered_ = true; // 构造时传入的 initialChannel 已经是注册成功的

        std::shared_ptr<healthcheck::HealthCheck::Stub> healthStub_;
        std::shared_ptr<hotmethod::Hotmethod::Stub> hotmethodStub_;

        TaskQueue &taskQueue_;
        AttemptTracker &attemptTracker_;
        ServerChannelHolder &channelHolder_;
        std::atomic<bool> &running_;
        std::thread thread_;

        drop::DualTrackContinuousSampler nativeSampler_;
        std::string nativeCPAPIBaseURL_;
        std::string nativeCPAuthUID_;
        std::string nativeCPSessionSID_;
        std::chrono::steady_clock::time_point nativeCPNextRetryAt_{};
    };

} // namespace drop_agent
