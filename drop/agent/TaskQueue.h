// ============================================================
// agent/TaskQueue.h — HeartbeatThread → WorkerThread 任务缓冲队列
// ============================================================
// mutex + condition_variable，参照 common/ContinuousSampler.h 的线程类
// 风格。HeartbeatThread 收到 Server 派发的任务就 Push 进来，WorkerThread
// 阻塞式 WaitPop 消费——这样心跳不再被采集过程阻塞（新复刻指南 5.7 节）。
// ============================================================

#pragma once

#include <deque>
#include <mutex>
#include <condition_variable>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop_agent
{

    class TaskQueue
    {
    public:
        void Push(const hotmethod::TaskDesc &task);

        /// 最多阻塞等待 timeoutMs 毫秒；取到任务返回 true 并写入 *outTask，
        /// 超时（队列一直为空）返回 false。Shutdown() 不会清空队列，
        /// 只是唤醒等待者——已入队的任务仍然可以被 WaitPop 取走，
        /// 交由调用方（WorkerThread 的运行标志）决定是否继续处理。
        bool WaitPop(int timeoutMs, hotmethod::TaskDesc *outTask);

        void Shutdown();

        // Phase 7：从队列里移除匹配的任务（还没被 WorkerThread 取走）。命中
        // 返回 true。已经被取走、正在跑的任务不受影响——那种情况走
        // AttemptTracker::RequestCancel，由 WorkerThread 的 Poll 循环感知。
        bool CancelQueued(const std::string &taskID, uint64_t attemptID);

    private:
        mutable std::mutex mutex_;
        std::condition_variable cv_;
        std::deque<hotmethod::TaskDesc> queue_;
        bool shuttingDown_ = false;
    };

} // namespace drop_agent
