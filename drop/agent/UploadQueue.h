// ============================================================
// agent/UploadQueue.h — WorkerThread → UploadWorker 上传任务缓冲队列
// ============================================================
// Phase 4：把"上传产物 + NotifyResult"从采集路径里拆出来，让 WorkerThread
// 采集一结束就能去拿下一个任务，不用在上传/符号处理这类慢 IO 上干等。
// 结构上和 TaskQueue 完全对称（mutex+condvar），只是搬运的载荷换成
// UploadJob。
// ============================================================

#pragma once

#include <deque>
#include <mutex>
#include <condition_variable>
#include <vector>
#include "agent/RunnerOutcome.h"
#include "common/proto/hotmethod.pb.h"  // hotmethod::TaskDesc
#include "common/proto/common.pb.h"     // common::PidStats

namespace drop_agent
{

    struct UploadJob
    {
        hotmethod::TaskDesc task;
        RunnerOutcome outcome;
        common::PidStats selfPs;
        std::vector<common::PidStats> childrenPs;
    };

    class UploadQueue
    {
    public:
        void Push(UploadJob job);

        /// 语义和 TaskQueue::WaitPop 完全一致：最多阻塞 timeoutMs 毫秒；
        /// 取到任务返回 true，队列为空则返回 false。Shutdown() 不清空
        /// 队列，只是唤醒等待者——已入队的上传任务仍然可以被 WaitPop
        /// 取走，供 UploadWorker 排空。
        bool WaitPop(int timeoutMs, UploadJob *outJob);

        void Shutdown();

    private:
        mutable std::mutex mutex_;
        std::condition_variable cv_;
        std::deque<UploadJob> queue_;
        bool shuttingDown_ = false;
    };

} // namespace drop_agent
