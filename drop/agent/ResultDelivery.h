#pragma once

#include "agent/AttemptTracker.h"

#include <chrono>
#include <cstdint>
#include <functional>
#include <string>

namespace drop_agent
{

    struct ResultDeliveryPolicy
    {
        uint32_t maxAttempts = 3;
        uint32_t initialBackoffMs = 200;
        uint32_t maxBackoffMs = 1000;
    };

    using NotifyResultAttempt = std::function<bool(std::string *error)>;
    using RetrySleep = std::function<void(std::chrono::milliseconds)>;

    // A completed heartbeat is an acknowledgement that the result reached the
    // server. Keep the attempt running when every RPC attempt fails so the
    // scheduler cannot discard it as successfully acknowledged.
    bool DeliverResultAndMarkCompleted(const std::string &taskID,
                                       uint64_t attemptID,
                                       AttemptTracker &tracker,
                                       const NotifyResultAttempt &notify,
                                       const ResultDeliveryPolicy &policy = {},
                                       const RetrySleep &sleep = {},
                                       std::string *lastError = nullptr);

} // namespace drop_agent
