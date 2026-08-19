#include "agent/ResultDelivery.h"

#include <algorithm>
#include <thread>

namespace drop_agent
{

    bool DeliverResultAndMarkCompleted(const std::string &taskID,
                                       uint64_t attemptID,
                                       AttemptTracker &tracker,
                                       const NotifyResultAttempt &notify,
                                       const ResultDeliveryPolicy &policy,
                                       const RetrySleep &sleep,
                                       std::string *lastError)
    {
        if (!notify || policy.maxAttempts == 0)
        {
            if (lastError)
                *lastError = "result notifier is unavailable";
            return false;
        }

        uint32_t backoffMs = policy.initialBackoffMs;
        for (uint32_t attempt = 1; attempt <= policy.maxAttempts; ++attempt)
        {
            std::string error;
            if (notify(&error))
            {
                tracker.MarkCompleted(taskID, attemptID);
                if (lastError)
                    lastError->clear();
                return true;
            }
            if (lastError)
                *lastError = error;

            if (attempt == policy.maxAttempts)
                break;

            auto delay = std::chrono::milliseconds(backoffMs);
            if (sleep)
                sleep(delay);
            else
                std::this_thread::sleep_for(delay);

            if (backoffMs < policy.maxBackoffMs)
                backoffMs = std::min(policy.maxBackoffMs, backoffMs * 2);
        }
        return false;
    }

} // namespace drop_agent
