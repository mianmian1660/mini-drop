#include "agent/TaskQueue.h"

#include <algorithm>
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

    bool TaskQueue::CancelQueued(const std::string &taskID, uint64_t attemptID)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        auto it = std::find_if(queue_.begin(), queue_.end(),
                                [&](const hotmethod::TaskDesc &t)
                                { return t.taskid() == taskID && t.attempt_id() == attemptID; });
        if (it == queue_.end())
            return false;
        queue_.erase(it);
        return true;
    }

} // namespace drop_agent
