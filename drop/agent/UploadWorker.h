// ============================================================
// agent/UploadWorker.h — 消费 UploadQueue，跑"上传产物 + NotifyResult"
// ============================================================
// Phase 4 从 WorkerThread 拆出：原来采集一结束就原地上传/NotifyResult 的
//那一段，现在独立成一个线程。WorkerThread 采完一个任务就把 UploadJob
// 丢进 UploadQueue，立刻去拿下一个任务，不用在慢 IO（MinIO 上传、符号
// 上传、NotifyResult RPC）上阻塞采集节奏。
//
// 关闭语义和 WorkerThread 不同：Stop() 不是简单等 runningFlag 变 false，
// 而是先 Shutdown() 队列、再把线程"排空"到队列真正为空为止——这样
// "采集刚结束、还在上传时收到 SIGTERM"的任务不会被半途丢弃。
// ============================================================

#pragma once

#include <atomic>
#include <thread>
#include "agent/UploadQueue.h"
#include "agent/AttemptTracker.h"
#include "agent/ServerChannelHolder.h"

namespace drop_agent
{

    class UploadWorker
    {
    public:
        UploadWorker(UploadQueue &uploadQueue,
                     AttemptTracker &attemptTracker,
                     ServerChannelHolder &channelHolder);

        void Start();

        /// 关闭队列并等待排空（不是打断），join 线程。幂等。
        void Stop();

    private:
        void Loop();

        UploadQueue &uploadQueue_;
        AttemptTracker &attemptTracker_;
        ServerChannelHolder &channelHolder_;
        std::atomic<bool> draining_{false};
        std::thread thread_;
    };

} // namespace drop_agent
