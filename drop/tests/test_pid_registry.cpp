// ============================================================
// tests/test_pid_registry.cpp — drop::PidRegistry 单元测试
// ============================================================
// 纯逻辑测试，不依赖真实进程：验证 Phase 5 拆出的 pid 登记表基本行为。
// ============================================================

#include <gtest/gtest.h>
#include "common/PidRegistry.h"

using drop::PidRegistry;

TEST(PidRegistry, SnapshotEmptyByDefault)
{
    PidRegistry reg;
    EXPECT_TRUE(reg.Snapshot().empty());
}

TEST(PidRegistry, RegisterAddsEntry)
{
    PidRegistry reg;
    reg.Register(1234);
    auto snap = reg.Snapshot();
    ASSERT_EQ(snap.size(), 1u);
    EXPECT_EQ(snap[0].first, 1234);
}

TEST(PidRegistry, UnregisterRemovesEntry)
{
    PidRegistry reg;
    reg.Register(1234);
    reg.Unregister(1234);
    EXPECT_TRUE(reg.Snapshot().empty());
}

TEST(PidRegistry, UnregisterUnknownPidIsNoop)
{
    PidRegistry reg;
    reg.Register(1);
    reg.Unregister(999); // 不存在的 pid，不应影响已有条目
    auto snap = reg.Snapshot();
    ASSERT_EQ(snap.size(), 1u);
    EXPECT_EQ(snap[0].first, 1);
}

TEST(PidRegistry, MultiplePidsTrackedIndependently)
{
    PidRegistry reg;
    reg.Register(1);
    reg.Register(2);
    reg.Register(3);
    reg.Unregister(2);
    auto snap = reg.Snapshot();
    ASSERT_EQ(snap.size(), 2u);
    bool has1 = false, has3 = false;
    for (const auto &e : snap)
    {
        if (e.first == 1)
            has1 = true;
        if (e.first == 3)
            has3 = true;
    }
    EXPECT_TRUE(has1);
    EXPECT_TRUE(has3);
}
