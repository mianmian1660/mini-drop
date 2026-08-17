// ============================================================
// tests/test_task_queue.cpp — TaskQueue 优先级/去重/取消逻辑单元测试
// ============================================================
// 只测试不依赖真实进程/网络的纯逻辑：优先级排序、attempt 去重、
// 取消/终态判断。每个用例用独立的 targetIP + taskID，避免和其他
// 用例互相污染全局队列状态（TaskQueue.cpp 的 tasks_/completed_attempts_
// 是进程级全局变量，测试之间不会自动重置）。
// ============================================================

#include "server/TaskQueue.h"

#include <gtest/gtest.h>
#include <mutex>
#include <string>

using drop_server::EnqueueResult;

namespace
{

    hotmethod::TaskDesc make_task(const std::string &taskID, uint64_t attemptID,
                                  const std::string &priorityJson = "")
    {
        hotmethod::TaskDesc task;
        task.set_taskid(taskID);
        task.set_attempt_id(attemptID);
        if (!priorityJson.empty())
            task.set_resource_budget(priorityJson);
        return task;
    }

} // namespace

TEST(TaskQueue, EnqueueOrdersByPriorityDescending)
{
    const std::string ip = "10.0.0.101";
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    auto low = make_task("tq-order-low", 1, R"({"priority": 1})");
    auto high = make_task("tq-order-high", 1, R"({"priority": 10})");
    auto mid = make_task("tq-order-mid", 1, R"({"priority": 5})");

    ASSERT_TRUE(drop_server::enqueue_task_locked(ip, low).ok);
    ASSERT_TRUE(drop_server::enqueue_task_locked(ip, high).ok);
    ASSERT_TRUE(drop_server::enqueue_task_locked(ip, mid).ok);

    hotmethod::TaskDesc out;
    ASSERT_TRUE(drop_server::pop_next_dispatchable_task_locked(ip, &out));
    EXPECT_EQ(out.taskid(), "tq-order-high");
    ASSERT_TRUE(drop_server::pop_next_dispatchable_task_locked(ip, &out));
    EXPECT_EQ(out.taskid(), "tq-order-mid");
    ASSERT_TRUE(drop_server::pop_next_dispatchable_task_locked(ip, &out));
    EXPECT_EQ(out.taskid(), "tq-order-low");
}

TEST(TaskQueue, EnqueueDedupsSameAttempt)
{
    const std::string ip = "10.0.0.102";
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    auto task = make_task("tq-dedup", 1);
    EnqueueResult first = drop_server::enqueue_task_locked(ip, task);
    EnqueueResult second = drop_server::enqueue_task_locked(ip, task);

    EXPECT_TRUE(first.ok);
    EXPECT_FALSE(first.duplicate);
    EXPECT_TRUE(second.ok);
    EXPECT_TRUE(second.duplicate);

    hotmethod::TaskDesc out;
    ASSERT_TRUE(drop_server::pop_next_dispatchable_task_locked(ip, &out));
    EXPECT_FALSE(drop_server::pop_next_dispatchable_task_locked(ip, &out)); // 只有一条真正入队
}

TEST(TaskQueue, EnqueueRejectsAlreadyCompletedAttempt)
{
    const std::string ip = "10.0.0.103";
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    hotmethod::TaskResult result;
    result.set_taskid("tq-completed");
    result.set_attempt_id(1);
    drop_server::mark_attempt_completed_locked(result);

    auto task = make_task("tq-completed", 1);
    EnqueueResult enqueueResult = drop_server::enqueue_task_locked(ip, task);

    EXPECT_TRUE(enqueueResult.ok);
    EXPECT_TRUE(enqueueResult.duplicate);

    hotmethod::TaskDesc out;
    EXPECT_FALSE(drop_server::pop_next_dispatchable_task_locked(ip, &out)); // 不会真正入队
}

TEST(TaskQueue, CancelQueuedTaskMarksCanceledAndSkipsDispatch)
{
    const std::string ip = "10.0.0.104";
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    auto task = make_task("tq-cancel", 1);
    ASSERT_TRUE(drop_server::enqueue_task_locked(ip, task).ok);

    EXPECT_TRUE(drop_server::cancel_task_locked(ip, "tq-cancel", 1));

    hotmethod::TaskDesc out;
    EXPECT_FALSE(drop_server::pop_next_dispatchable_task_locked(ip, &out)); // 已取消，不会派发
}

TEST(TaskQueue, CancelUnknownTaskReturnsFalse)
{
    const std::string ip = "10.0.0.105";
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    EXPECT_FALSE(drop_server::cancel_task_locked(ip, "tq-never-existed", 1));
}

TEST(TaskQueue, IsAttemptCompletedLockedReflectsMarking)
{
    std::lock_guard<std::mutex> lock(drop_server::tasks_mutex);

    auto task = make_task("tq-terminal-check", 1);
    EXPECT_FALSE(drop_server::is_attempt_completed_locked(task));

    hotmethod::TaskResult result;
    result.set_taskid("tq-terminal-check");
    result.set_attempt_id(1);
    drop_server::mark_attempt_completed_locked(result);

    EXPECT_TRUE(drop_server::is_attempt_completed_locked(task));
}
