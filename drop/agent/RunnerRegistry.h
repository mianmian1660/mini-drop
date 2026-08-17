// ============================================================
// agent/RunnerRegistry.h — profilerType -> Runner 工厂
// ============================================================

#pragma once

#include "agent/Runner.h"

#include <cstdint>
#include <memory>

namespace drop_agent
{

    /// 未知 profilerType 时的回退策略和旧 main.cpp 的 run_profiler() 一致：
    /// 回退到 PerfRunner（perf 是通用整机 CPU 采集，容错性最好）。
    std::unique_ptr<Runner> CreateRunner(uint32_t profilerType);

} // namespace drop_agent
