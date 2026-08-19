// ============================================================
// agent/CleanupWorker.h — 兜底清理线程（Phase 5）
// ============================================================
// 独立线程，每隔 cfg.cleanupIntervalSec 醒一次，做两件事：
//   1. 扫 PidRegistry：登记超过 cfg.orphanPidGraceSec 仍未被摘牌的 pid，
//      判定为孤儿进程（大概率是 Worker 线程卡死/崩溃丢下的），直接
//      SIGKILL + 阻塞 waitpid 强制回收，避免僵尸进程堆积。
//   2. 扫 /tmp/drop_agent/tasks/<taskID>/<attemptID>/：删除超过
//      cfg.taskDirRetentionSec 的旧任务目录，避免本地磁盘无限增长。
//
// 和 HeartbeatThread/WorkerThread 一样用 atomic<bool>& runningFlag +
// Start/Stop 的线程类范式（参照 common/ContinuousSampler.h）。
// ============================================================

#pragma once

#include <atomic>
#include <thread>
#include "agent/Config.h"
#include "common/PidRegistry.h"

namespace drop_agent
{

    class CleanupWorker
    {
    public:
        CleanupWorker(const AgentConfig &cfg,
                      drop::PidRegistry &pidRegistry,
                      std::atomic<bool> &runningFlag);

        void Start();

        /// 等 runningFlag 变为 false 后线程自然退出的循环收尾，join 线程。
        /// 幂等。不做"排空"语义——孤儿回收/目录清理下次心跳或下次启动
        /// 还会再扫一遍，不需要在关闭时抢最后一次。
        void Stop();

    private:
        void Loop();
        void ReapOrphanPids();
        void CleanStaleTaskDirs();

        const AgentConfig &cfg_;
        drop::PidRegistry &pidRegistry_;
        std::atomic<bool> &running_;
        std::thread thread_;
    };

} // namespace drop_agent
