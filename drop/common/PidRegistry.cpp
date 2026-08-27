#include "common/PidRegistry.h"

namespace drop
{

    void PidRegistry::Register(pid_t pid)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        entries_[pid] = std::chrono::steady_clock::now();
    }

    void PidRegistry::Touch(pid_t pid)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        auto it = entries_.find(pid);
        if (it != entries_.end())
            it->second = std::chrono::steady_clock::now();
    }

    void PidRegistry::Unregister(pid_t pid)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        entries_.erase(pid);
    }

    std::vector<std::pair<pid_t, std::chrono::steady_clock::time_point>> PidRegistry::Snapshot() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        std::vector<std::pair<pid_t, std::chrono::steady_clock::time_point>> out;
        out.reserve(entries_.size());
        for (const auto &kv : entries_)
            out.push_back(kv);
        return out;
    }

} // namespace drop
