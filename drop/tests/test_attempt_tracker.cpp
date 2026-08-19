// ============================================================
// tests/test_attempt_tracker.cpp — drop_agent::AttemptTracker 单元测试
// ============================================================

#include <gtest/gtest.h>
#include "agent/AttemptTracker.h"

using drop_agent::AttemptTracker;

TEST(AttemptTracker, MarkRunningAppearsInRunningSnapshot)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-1", 1);

    auto running = tracker.RunningSnapshot();
    ASSERT_EQ(running.size(), 1u);
    EXPECT_EQ(running[0].taskid(), "task-1");
    EXPECT_EQ(running[0].attempt_id(), 1u);
    EXPECT_TRUE(tracker.CompletedSnapshot().empty());
    EXPECT_TRUE(tracker.IsRunning("task-1", 1));
    EXPECT_FALSE(tracker.IsRunning("task-1", 2));
}

TEST(AttemptTracker, MarkCompletedMovesFromRunningToCompleted)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-1", 1);
    tracker.MarkCompleted("task-1", 1);

    EXPECT_TRUE(tracker.RunningSnapshot().empty());
    EXPECT_FALSE(tracker.IsRunning("task-1", 1));
    auto completed = tracker.CompletedSnapshot();
    ASSERT_EQ(completed.size(), 1u);
    EXPECT_EQ(completed[0].taskid(), "task-1");
}

TEST(AttemptTracker, CompletedSnapshotCapsAtFiftyDroppingOldest)
{
    AttemptTracker tracker;
    for (uint64_t i = 1; i <= 51; i++)
        tracker.MarkCompleted("task", i);

    auto completed = tracker.CompletedSnapshot();
    ASSERT_EQ(completed.size(), 50u);
    // 最旧的一条（attempt_id=1）应该已经被丢弃，最新的一条（attempt_id=51）保留
    EXPECT_EQ(completed.front().attempt_id(), 2u);
    EXPECT_EQ(completed.back().attempt_id(), 51u);
}

// ============================================================
// Phase 7：RequestCancel / IsCancelRequested
// ============================================================

TEST(AttemptTracker, IsCancelRequestedFalseByDefault)
{
    AttemptTracker tracker;
    EXPECT_FALSE(tracker.IsCancelRequested("task-1", 1));
}

TEST(AttemptTracker, RequestCancelMarksAttempt)
{
    AttemptTracker tracker;
    tracker.RequestCancel("task-1", 1);
    EXPECT_TRUE(tracker.IsCancelRequested("task-1", 1));
    EXPECT_FALSE(tracker.IsCancelRequested("task-1", 2)); // 不同 attemptID 不受影响
}

TEST(AttemptTracker, MarkCompletedClearsCancelFlag)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-1", 1);
    tracker.RequestCancel("task-1", 1);
    tracker.MarkCompleted("task-1", 1);
    EXPECT_FALSE(tracker.IsCancelRequested("task-1", 1));
}
