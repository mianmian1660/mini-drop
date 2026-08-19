// ============================================================
// tests/test_upload_queue.cpp — drop_agent::UploadQueue 单元测试
// ============================================================
// 纯逻辑测试，不依赖真实进程/网络：验证 Phase 4 拆出的
// WorkerThread -> UploadWorker 上传任务缓冲队列的基本行为，
// 和 test_agent_task_queue.cpp 对 TaskQueue 的覆盖完全对称。
// ============================================================

#include <gtest/gtest.h>
#include "agent/UploadQueue.h"

using drop_agent::UploadQueue;
using drop_agent::UploadJob;

TEST(UploadQueue, WaitPopTimesOutOnEmptyQueue)
{
    UploadQueue q;
    UploadJob out;
    EXPECT_FALSE(q.WaitPop(50, &out));
}

TEST(UploadQueue, PushThenWaitPopReturnsJob)
{
    UploadQueue q;
    UploadJob job;
    job.task.set_taskid("abc");
    job.task.set_attempt_id(7);
    job.outcome.resultCode = 0;
    job.outcome.profilerName = "perf";
    q.Push(job);

    UploadJob out;
    ASSERT_TRUE(q.WaitPop(1000, &out));
    EXPECT_EQ(out.task.taskid(), "abc");
    EXPECT_EQ(out.task.attempt_id(), 7u);
    EXPECT_EQ(out.outcome.profilerName, "perf");
}

TEST(UploadQueue, FIFOOrderPreserved)
{
    UploadQueue q;
    UploadJob j1, j2;
    j1.task.set_taskid("first");
    j2.task.set_taskid("second");
    q.Push(j1);
    q.Push(j2);

    UploadJob out1, out2;
    ASSERT_TRUE(q.WaitPop(1000, &out1));
    ASSERT_TRUE(q.WaitPop(1000, &out2));
    EXPECT_EQ(out1.task.taskid(), "first");
    EXPECT_EQ(out2.task.taskid(), "second");
}

TEST(UploadQueue, ShutdownDoesNotDropAlreadyQueuedJob)
{
    UploadQueue q;
    UploadJob job;
    job.task.set_taskid("queued-before-shutdown");
    q.Push(job);
    q.Shutdown();

    UploadJob out;
    ASSERT_TRUE(q.WaitPop(1000, &out));
    EXPECT_EQ(out.task.taskid(), "queued-before-shutdown");

    UploadJob out2;
    EXPECT_FALSE(q.WaitPop(50, &out2));
}
