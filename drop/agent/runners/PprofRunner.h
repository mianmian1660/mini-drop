// ============================================================
// agent/runners/PprofRunner.h — pprof 采集器（profilerType=2，Go）
// ============================================================

#pragma once

#include "agent/Runner.h"

#include <memory>

namespace drop_agent
{

    class PprofRunner : public Runner
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
        std::string BuildUrl(const TaskContext &ctx) const;

        std::string outputPath_;
        std::string url_;
        std::unique_ptr<drop::TimedProcessPoller> poller_;
        int lastResultCode_ = 0;
    };

} // namespace drop_agent
