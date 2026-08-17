// ============================================================
// agent/runners/PerfRunner.h — perf 采集器（profilerType=0）
// ============================================================

#pragma once

#include "agent/Runner.h"

#include <memory>

namespace drop_agent
{

    class PerfRunner : public Runner
    {
    public:
        ValidationResult Validate(const TaskContext &ctx) override;
        PrepareResult Prepare(TaskContext &ctx) override;
        StartResult Start(TaskContext &ctx) override;
        PollResult Poll(TaskContext &ctx) override;
        StopResult Stop(TaskContext &ctx, StopReason reason) override;
        CollectResult Collect(TaskContext &ctx) override;
        CleanupResult Cleanup(TaskContext &ctx) override;

    private:
        std::string outputPath_;
        std::unique_ptr<drop::TimedProcessPoller> poller_;
        int lastResultCode_ = 0;
    };

} // namespace drop_agent
