// ============================================================
// server/TaskQueue.h — 任务队列（供各 Service 共用）
// ============================================================

#pragma once

#include <string>
#include <deque>
#include <mutex>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>
#include "common/proto/hotmethod.pb.h" // hotmethod::TaskDesc

namespace drop_server
{
    struct QueuedTask
    {
        hotmethod::TaskDesc task;
        int priority = 0;
        int64_t deadline_unix_ms = 0;
        int64_t enqueued_unix_ms = 0;
        bool canceled = false;
    };

    struct EnqueueResult
    {
        bool ok = false;
        bool duplicate = false;
        std::string message;
    };

    /// 全局任务队列（按 targetIP 索引）
    extern std::mutex tasks_mutex;
    extern std::unordered_map<std::string, std::deque<QueuedTask>> tasks_;
    extern std::unordered_set<std::string> completed_attempts_;

    /// Phase 7：已派发（不在 tasks_ 队列里）、待取消的 attempt，按 targetIP
    /// 索引，值是 task_attempt_key() 集合。CancelTask 对已派发任务写入这里，
    /// HealthCheckService::Do 每次心跳都从这里取出并下发给 Agent，直到
    /// Agent 通过 completed_attempts 明确确认任务结束才摘除。
    extern std::unordered_map<std::string, std::unordered_set<std::string>> pending_cancels_;

    std::string task_attempt_key(const std::string &taskID, uint64_t attemptID);
    std::string task_attempt_key(const hotmethod::TaskDesc &task);
    EnqueueResult enqueue_task_locked(const std::string &targetIP, const hotmethod::TaskDesc &task);
    bool cancel_task_locked(const std::string &targetIP, const std::string &taskID, uint64_t attemptID);
    bool pop_next_dispatchable_task_locked(const std::string &targetIP, hotmethod::TaskDesc *task);
    void mark_attempt_completed_locked(const hotmethod::TaskResult &result);
    bool is_attempt_completed_locked(const hotmethod::TaskDesc &task);

    /// Phase 7：登记"这个已派发 attempt 需要被取消"，供下次心跳下发。
    void request_cancel_locked(const std::string &targetIP, const std::string &taskID, uint64_t attemptID);

    /// Phase 7：返回这次要下发给 Agent 的 (taskID, attemptID) 取消列表。
    /// currentlyRunningKeys 保留在接口中用于兼容调用方，但不能作为删除
    /// 依据：派发后首次 running 心跳前集合也会为空。取消项只在收到 Agent
    /// completed_attempts 后由 mark_attempt_completed_locked 清理。
    std::vector<std::pair<std::string, uint64_t>> collect_cancel_attempts_locked(
        const std::string &targetIP,
        const std::unordered_set<std::string> &currentlyRunningKeys);

    /// A3：把当前队列快照写入本地文件，供 Server 重启后恢复未派发任务。
    /// 由 main.cpp 里的后台线程定期调用，不是每次入队/出队都写。
    void snapshot_tasks_to_disk(const std::string &path);

    /// A3：启动时从磁盘快照恢复队列。找不到文件或文件为空都是正常情况（首次启动）。
    void restore_tasks_from_disk(const std::string &path);

} // namespace drop_server
