#include "agent/AgentUtils.h"
#include "common/proto/common.pb.h"

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

TEST(AgentUtils, UploadedArtifactNotificationContainsMetadataOnly)
{
    common::File file;
    file.set_content(std::string(1024, 'x'));

    SetUploadedArtifactMetadata(file, "perf.data", 64LL * 1024 * 1024);

    EXPECT_EQ(file.name(), "perf.data");
    EXPECT_EQ(file.size(), 64LL * 1024 * 1024);
    EXPECT_TRUE(file.content().empty());
    EXPECT_LT(file.ByteSizeLong(), 256U);
}
