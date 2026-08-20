#include "agent/AgentUtils.h"

#include <gtest/gtest.h>

using namespace drop_agent;

TEST(AgentUtils, TaskAttemptDirectoriesAreIsolated)
{
    EXPECT_EQ(TaskAttemptDir("/tmp/drop_agent/tasks", "task-a", 1),
              "/tmp/drop_agent/tasks/task-a/1/");
    EXPECT_NE(TaskAttemptDir("/tmp/drop_agent/tasks/", "task-a", 1),
              TaskAttemptDir("/tmp/drop_agent/tasks", "task-a", 2));
    EXPECT_NE(TaskAttemptDir("/tmp/drop_agent/tasks", "task-a", 1),
              TaskAttemptDir("/tmp/drop_agent/tasks", "task-b", 1));
}
