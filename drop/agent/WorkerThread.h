// ============================================================
// agent/WorkerThread.h — 消费 TaskQueue，跑 Runner 全生命周期
// ============================================================
// Phase 3 从 main.cpp 拆出：原来单线程循环里"收到任务后原地执行采集→
// 上传→NotifyResult"的那一段，现在独立成一个线程，靠 TaskQueue 和
// HeartbeatThread 解耦，这样心跳不再被采集过程阻塞。
//
// 内部仍然复用 Phase 2 落地的 drop_agent::Runner 生命周期（Validate->
// Prepare->Start->Poll->Collect），只是编排它的调用方从 main() 的循环体
// 换成了这个线程的 Loop()。
// ============================================================

#pragma once

#include <atomic>
#include <thread>
#include "agent/TaskQueue.h"
#include "agent/AttemptTracker.h"
#include "agent/ServerChannelHolder.h"

namespace drop_agent
{

    class WorkerThread
    {
    public:
        WorkerThread(TaskQueue &taskQueue,
                     AttemptTracker &attemptTracker,
                     ServerChannelHolder &channelHolder,
                     std::atomic<bool> &runningFlag);

        void Start();

        /// 等待 runningFlag 变为 false 后线程自然退出的循环收尾，
        /// join 线程。幂等（重复调用安全）。
        void Stop();

    private:
        void Loop();

        TaskQueue &taskQueue_;
        AttemptTracker &attemptTracker_;
        ServerChannelHolder &channelHolder_;
        std::atomic<bool> &running_;
        std::thread thread_;
    };

} // namespace drop_agent
