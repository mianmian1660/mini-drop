// ============================================================
// agent/AttemptTracker.h — running/completed attempt 快照（线程安全）
// ============================================================
// 旧代码里裸的 vector<AttemptStatus> runningAttempts/completedAttempts 只
// 被单线程读写，天然没有竞态。Phase 3 拆出 HeartbeatThread（读，填进心跳
// 请求）和 WorkerThread（写，任务开始/结束时更新）后，必须换成加锁的共享
// 状态——这是新复刻指南 5.7 节里心跳线程读取 running/completed 快照的
// 落地方式。
// ============================================================

#pragma once

#include <mutex>
#include <string>
#include <vector>
#include <cstdint>
#include "common/proto/common.pb.h" // common::AttemptStatus

namespace drop_agent
{

    class AttemptTracker
    {
    public:
        void MarkRunning(const std::string &taskID, uint64_t attemptID);

        /// 从 running 快照中移除，并追加进 completed 快照（超过 50 条丢弃最旧的，
        /// 与旧代码 `if (completedAttempts.size() > 50) completedAttempts.erase(begin())` 行为一致）。
        void MarkCompleted(const std::string &taskID, uint64_t attemptID);

        std::vector<common::AttemptStatus> RunningSnapshot() const;
        std::vector<common::AttemptStatus> CompletedSnapshot() const;

    private:
        mutable std::mutex mutex_;
        std::vector<common::AttemptStatus> running_;
        std::vector<common::AttemptStatus> completed_;
    };

} // namespace drop_agent
