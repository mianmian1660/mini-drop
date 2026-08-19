#include "agent/AttemptTracker.h"

#include <algorithm>

namespace drop_agent
{

    void AttemptTracker::MarkRunning(const std::string &taskID, uint64_t attemptID)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        common::AttemptStatus attempt;
        attempt.set_taskid(taskID);
        attempt.set_attempt_id(attemptID);
        running_.push_back(attempt);
    }

    void AttemptTracker::MarkCompleted(const std::string &taskID, uint64_t attemptID)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        running_.erase(std::remove_if(running_.begin(), running_.end(),
                                       [&](const common::AttemptStatus &attempt)
                                       {
                                           return attempt.taskid() == taskID &&
                                                  attempt.attempt_id() == attemptID;
                                       }),
                        running_.end());

        common::AttemptStatus attempt;
        attempt.set_taskid(taskID);
        attempt.set_attempt_id(attemptID);
        completed_.push_back(attempt);
        if (completed_.size() > 50)
            completed_.erase(completed_.begin());

        cancelRequested_.erase(Key(taskID, attemptID));
    }

    std::string AttemptTracker::Key(const std::string &taskID, uint64_t attemptID)
    {
        return taskID + "#" + std::to_string(attemptID);
    }

    void AttemptTracker::RequestCancel(const std::string &taskID, uint64_t attemptID)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        cancelRequested_.insert(Key(taskID, attemptID));
    }

    bool AttemptTracker::IsCancelRequested(const std::string &taskID, uint64_t attemptID) const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return cancelRequested_.find(Key(taskID, attemptID)) != cancelRequested_.end();
    }

    std::vector<common::AttemptStatus> AttemptTracker::RunningSnapshot() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return running_;
    }

    std::vector<common::AttemptStatus> AttemptTracker::CompletedSnapshot() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return completed_;
    }

} // namespace drop_agent
