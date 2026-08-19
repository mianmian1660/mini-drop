// ============================================================
// agent/WorkerThread.h — 消费 TaskQueue，跑 Runner 全生命周期
// ============================================================
// Phase 3 从 main.cpp 拆出：原来单线程循环里"收到任务后原地执行采集→
// 上传→NotifyResult"的那一段，独立成一个线程，靠 TaskQueue 和
// HeartbeatThread 解耦，这样心跳不再被采集过程阻塞。
//
// Phase 4：上传/NotifyResult 进一步拆到 UploadWorker。WorkerThread 采集
// 一结束就把 UploadJob 丢进 UploadQueue，立刻去拿下一个任务——不再需要
// ServerChannelHolder（上传/NotifyResult 用的 stub/CosConfig 也移到
// UploadWorker）。
//
// Phase 7：AttemptTracker 又加回来了，但用途和 Phase 3 之前不同——不是为
// 了 MarkCompleted（那还在 UploadWorker），而是只读查询
// IsCancelRequested()：HeartbeatThread 收到 Server 下发的取消指令后调用
// AttemptTracker::RequestCancel()，这里的 Poll 循环每轮检查一次，一旦命中
// 就调用 Runner::Stop(kCancel) 中断采集。
//
// 内部仍然复用 Phase 2 落地的 drop_agent::Runner 生命周期（Validate->
// Prepare->Start->Poll->Collect），只是编排它的调用方从 main() 的循环体
// 换成了这个线程的 Loop()。
// ============================================================

#pragma once

#include <atomic>
#include <thread>
#include "agent/TaskQueue.h"
#include "agent/UploadQueue.h"
#include "agent/AttemptTracker.h"
#include "common/PidRegistry.h"

namespace drop_agent
{

    class WorkerThread
    {
    public:
        WorkerThread(TaskQueue &taskQueue,
                     UploadQueue &uploadQueue,
                     drop::PidRegistry &pidRegistry,
                     AttemptTracker &attemptTracker,
                     std::atomic<bool> &runningFlag);

        void Start();

        /// 等待 runningFlag 变为 false 后线程自然退出的循环收尾，
        /// join 线程。幂等（重复调用安全）。
        void Stop();

    private:
        void Loop();

        TaskQueue &taskQueue_;
        UploadQueue &uploadQueue_;
        drop::PidRegistry &pidRegistry_;
        AttemptTracker &attemptTracker_;
        std::atomic<bool> &running_;
        std::thread thread_;
    };

} // namespace drop_agent
