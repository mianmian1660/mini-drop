// ============================================================
// common/Clock.h — 时间源抽象
// ============================================================
// 指南 5.8 节要求 TaskContext 暴露 Clock，目的是让超时/取消状态机
// （见 ProcessExecutor.h 的 TimedProcessPoller）能在不真的 sleep 的情况下
// 用 FakeClock 做单元测试。生产代码统一用 RealClock。
// ============================================================

#pragma once

#include <chrono>

namespace drop
{

    class Clock
    {
    public:
        virtual ~Clock() = default;
        virtual std::chrono::steady_clock::time_point Now() const = 0;
    };

    class RealClock : public Clock
    {
    public:
        std::chrono::steady_clock::time_point Now() const override
        {
            return std::chrono::steady_clock::now();
        }
    };

} // namespace drop
