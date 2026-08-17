#include "agent/TaskQueue.h"

#include <chrono>

namespace drop_agent
{

    void TaskQueue::Push(const hotmethod::TaskDesc &task)
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            queue_.push_back(task);
        }
        cv_.notify_one();
    }

    bool TaskQueue::WaitPop(int timeoutMs, hotmethod::TaskDesc *outTask)
    {
        std::unique_lock<std::mutex> lock(mutex_);
        cv_.wait_for(lock, std::chrono::milliseconds(timeoutMs),
                     [this] { return !queue_.empty() || shuttingDown_; });
        if (queue_.empty())
            return false;
        *outTask = queue_.front();
        queue_.pop_front();
        return true;
    }

    void TaskQueue::Shutdown()
    {
        {
            std::lock_guard<std::mutex> lock(mutex_);
            shuttingDown_ = true;
        }
        cv_.notify_all();
    }

} // namespace drop_agent
