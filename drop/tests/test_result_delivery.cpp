#include "agent/ResultDelivery.h"

#include <gtest/gtest.h>
#include <vector>

using namespace drop_agent;

TEST(ResultDelivery, RetriesThenMarksCompletedOnce)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-retry", 3);
    int calls = 0;
    std::vector<int64_t> sleeps;
    ResultDeliveryPolicy policy{3, 10, 100};

    std::string error;
    EXPECT_TRUE(DeliverResultAndMarkCompleted(
        "task-retry", 3, tracker,
        [&](std::string *out)
        {
            ++calls;
            if (calls == 1)
            {
                *out = "temporary failure";
                return false;
            }
            return true;
        },
        policy,
        [&](std::chrono::milliseconds delay) { sleeps.push_back(delay.count()); },
        &error));

    EXPECT_EQ(calls, 2);
    EXPECT_EQ(sleeps, std::vector<int64_t>({10}));
    EXPECT_TRUE(error.empty());
    EXPECT_TRUE(tracker.RunningSnapshot().empty());
    ASSERT_EQ(tracker.CompletedSnapshot().size(), 1u);
}

TEST(ResultDelivery, ExhaustedRetriesRemainRunning)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-fail", 4);
    int calls = 0;
    std::vector<int64_t> sleeps;
    ResultDeliveryPolicy policy{3, 10, 15};

    std::string error;
    EXPECT_FALSE(DeliverResultAndMarkCompleted(
        "task-fail", 4, tracker,
        [&](std::string *out)
        {
            ++calls;
            *out = "server unavailable";
            return false;
        },
        policy,
        [&](std::chrono::milliseconds delay) { sleeps.push_back(delay.count()); },
        &error));

    EXPECT_EQ(calls, 3);
    EXPECT_EQ(sleeps, std::vector<int64_t>({10, 15}));
    EXPECT_EQ(error, "server unavailable");
    ASSERT_EQ(tracker.RunningSnapshot().size(), 1u);
    EXPECT_TRUE(tracker.CompletedSnapshot().empty());
}

TEST(ResultDelivery, RepeatedSuccessDoesNotDuplicateCompletion)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-once", 5);
    auto success = [](std::string *) { return true; };
    RetrySleep noSleep = [](std::chrono::milliseconds) {};

    EXPECT_TRUE(DeliverResultAndMarkCompleted("task-once", 5, tracker, success, {}, noSleep));
    EXPECT_TRUE(DeliverResultAndMarkCompleted("task-once", 5, tracker, success, {}, noSleep));
    ASSERT_EQ(tracker.CompletedSnapshot().size(), 1u);
}

TEST(ResultDelivery, ZeroAttemptsDoesNotAcknowledge)
{
    AttemptTracker tracker;
    tracker.MarkRunning("task-zero", 6);
    ResultDeliveryPolicy policy{0, 0, 0};
    std::string error;
    EXPECT_FALSE(DeliverResultAndMarkCompleted(
        "task-zero", 6, tracker, [](std::string *) { return true; }, policy, {}, &error));
    EXPECT_EQ(error, "result notifier is unavailable");
    EXPECT_TRUE(tracker.CompletedSnapshot().empty());
}
