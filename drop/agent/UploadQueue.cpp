#include "agent/UploadQueue.h"

#include <chrono>

namespace drop_agent
{

    void UploadQueue::Push(UploadJob job)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            queue_.push_back(std::move(job));
        }
        cv_.notify_one();
    }

    bool UploadQueue::WaitPop(int timeoutMs, UploadJob *outJob)
    {
        std::unique_lock<std::mutex> lock(mutex_);
        cv_.wait_for(lock, std::chrono::milliseconds(timeoutMs),
                     [this] { return !queue_.empty() || shuttingDown_; });
        if (queue_.empty())
            return false;
        *outJob = std::move(queue_.front());
        queue_.pop_front();
        return true;
    }

    void UploadQueue::Shutdown()
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            shuttingDown_ = true;
        }
        cv_.notify_all();
    }

} // namespace drop_agent
