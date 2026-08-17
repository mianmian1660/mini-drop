// ============================================================
// agent/runners/BpfRunner.h — eBPF/bpftrace 采集器（profilerType=3）
// ============================================================

#pragma once

#include "agent/Runner.h"

#include <memory>

namespace drop_agent
{

    enum class BpfMode
    {
        CPU,
        IO_LATENCY,
        SCHED_LATENCY,
    };

    BpfMode ParseBpfMode(const std::string &event);

    class BpfRunner : public Runner
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
        BpfMode mode_ = BpfMode::CPU;
        std::string scriptPath_;
        std::string rawPath_;
        std::string outputPath_;
        std::unique_ptr<drop::TimedProcessPoller> poller_;
        int lastResultCode_ = 0;
    };

} // namespace drop_agent
