// ============================================================
// tests/test_agent_task_queue.cpp — drop_agent::TaskQueue 单元测试
// ============================================================
// 纯逻辑测试，不依赖真实进程/网络：验证 Phase 3 拆出的
// HeartbeatThread -> WorkerThread 任务缓冲队列的基本行为。
// ============================================================

#include <gtest/gtest.h>
#include "agent/TaskQueue.h"

using drop_agent::TaskQueue;

TEST(TaskQueue, WaitPopTimesOutOnEmptyQueue)
{
    TaskQueue q;
    hotmethod::TaskDesc out;
    EXPECT_FALSE(q.WaitPop(50, &out));
}

TEST(TaskQueue, PushThenWaitPopReturnsTask)
{
    TaskQueue q;
    hotmethod::TaskDesc task;
    task.set_taskid("abc");
    task.set_attempt_id(7);
    q.Push(task);

    hotmethod::TaskDesc out;
    ASSERT_TRUE(q.WaitPop(1000, &out));
    EXPECT_EQ(out.taskid(), "abc");
    EXPECT_EQ(out.attempt_id(), 7u);
}

TEST(TaskQueue, FIFOOrderPreserved)
{
    TaskQueue q;
    hotmethod::TaskDesc t1, t2;
    t1.set_taskid("first");
    t2.set_taskid("second");
    q.Push(t1);
    q.Push(t2);

    hotmethod::TaskDesc out1, out2;
    ASSERT_TRUE(q.WaitPop(1000, &out1));
    ASSERT_TRUE(q.WaitPop(1000, &out2));
    EXPECT_EQ(out1.taskid(), "first");
    EXPECT_EQ(out2.taskid(), "second");
}

TEST(TaskQueue, ShutdownDoesNotDropAlreadyQueuedTask)
{
    TaskQueue q;
    hotmethod::TaskDesc task;
    task.set_taskid("queued-before-shutdown");
    q.Push(task);
    q.Shutdown();

    hotmethod::TaskDesc out;
    ASSERT_TRUE(q.WaitPop(1000, &out));
    EXPECT_EQ(out.taskid(), "queued-before-shutdown");

    hotmethod::TaskDesc out2;
    EXPECT_FALSE(q.WaitPop(50, &out2));
}

// ============================================================
// Phase 7：CancelQueued
// ============================================================

TEST(TaskQueue, CancelQueuedRemovesMatchingTask)
{
    TaskQueue q;
    hotmethod::TaskDesc task;
    task.set_taskid("cancel-me");
    task.set_attempt_id(3);
    q.Push(task);

    EXPECT_TRUE(q.CancelQueued("cancel-me", 3));

    hotmethod::TaskDesc out;
    EXPECT_FALSE(q.WaitPop(50, &out)); // 已摘除，取不到
}

TEST(TaskQueue, CancelQueuedReturnsFalseWhenNotFound)
{
    TaskQueue q;
    EXPECT_FALSE(q.CancelQueued("never-queued", 1));
}

TEST(TaskQueue, CancelQueuedOnlyRemovesMatchingAttempt)
{
    TaskQueue q;
    hotmethod::TaskDesc t1, t2;
    t1.set_taskid("same-task");
    t1.set_attempt_id(1);
    t2.set_taskid("same-task");
    t2.set_attempt_id(2);
    q.Push(t1);
    q.Push(t2);

    EXPECT_TRUE(q.CancelQueued("same-task", 1));

    hotmethod::TaskDesc out;
    ASSERT_TRUE(q.WaitPop(1000, &out));
    EXPECT_EQ(out.attempt_id(), 2u); // attempt 1 被摘除，attempt 2 还在
}
