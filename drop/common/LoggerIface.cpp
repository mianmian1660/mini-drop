// ============================================================
// common/LoggerIface.cpp — 实现
// ============================================================

#include "common/LoggerIface.h"
#include "common/Log.h" // drop::log_event

namespace drop
{

    void RealLogger::Event(const std::string &event,
                            const std::vector<std::pair<std::string, std::string>> &fields)
    {
        log_event("drop_agent", event, fields);
    }

} // namespace drop
