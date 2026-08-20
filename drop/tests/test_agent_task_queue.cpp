// ============================================================
// tests/test_agent_task_queue.cpp — drop_agent::TaskQueue 单元测试
// ============================================================
// 纯逻辑测试，不依赖真实进程/网络：验证 Phase 3 拆出的
// HeartbeatThread -> WorkerThread 任务缓冲队列的基本行为。
// ============================================================

#include <gtest/gtest.h>
#include "agent/TaskQueue.h"

#include <set>
#include <thread>
#include <vector>

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

TEST(TaskQueue, ShutdownWakesBlockedConsumer)
{
    TaskQueue q;
    bool popped = true;
    std::thread consumer([&]
                         {
                             hotmethod::TaskDesc out;
                             popped = q.WaitPop(5000, &out);
                         });
    q.Shutdown();
    consumer.join();
    EXPECT_FALSE(popped);
}

TEST(TaskQueue, ConcurrentProducersDoNotLoseTasks)
{
    TaskQueue q;
    constexpr int kProducers = 4;
    constexpr int kTasksPerProducer = 25;
    std::vector<std::thread> producers;
    for (int producer = 0; producer < kProducers; ++producer)
    {
        producers.emplace_back([&, producer]
                               {
                                   for (int i = 0; i < kTasksPerProducer; ++i)
                                   {
                                       hotmethod::TaskDesc task;
                                       task.set_taskid(std::to_string(producer) + "-" + std::to_string(i));
                                       task.set_attempt_id(static_cast<uint64_t>(i + 1));
                                       q.Push(task);
                                   }
                               });
    }
    for (auto &producer : producers)
        producer.join();

    std::set<std::string> ids;
    for (int i = 0; i < kProducers * kTasksPerProducer; ++i)
    {
        hotmethod::TaskDesc out;
        ASSERT_TRUE(q.WaitPop(1000, &out));
        ids.insert(out.taskid());
    }
    EXPECT_EQ(ids.size(), static_cast<size_t>(kProducers * kTasksPerProducer));
}
