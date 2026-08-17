// ============================================================
// agent/RunnerRegistry.cpp — 实现
// ============================================================

#include "agent/RunnerRegistry.h"
#include "agent/runners/AsyncProfilerRunner.h"
#include "agent/runners/BpfRunner.h"
#include "agent/runners/PerfRunner.h"
#include "agent/runners/PprofRunner.h"

namespace drop_agent
{

    std::unique_ptr<Runner> CreateRunner(uint32_t profilerType)
    {
        switch (profilerType)
        {
        case 0:
            return std::unique_ptr<Runner>(new PerfRunner());
        case 1:
            return std::unique_ptr<Runner>(new AsyncProfilerRunner());
        case 2:
            return std::unique_ptr<Runner>(new PprofRunner());
        case 3:
            return std::unique_ptr<Runner>(new BpfRunner());
        default:
            return std::unique_ptr<Runner>(new PerfRunner());
        }
    }

} // namespace drop_agent
