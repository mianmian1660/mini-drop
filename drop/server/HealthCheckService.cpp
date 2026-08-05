// ============================================================
// server/HealthCheckService.cpp — 心跳服务 实现
// ============================================================

#include "server/HealthCheckService.h"
#include "server/AgentInfo.h"
#include "server/TaskQueue.h"

#include <iostream>
#include <mutex>

using namespace std;

namespace drop_server
{
    static int64_t system_now_unix_ms()
    {
        return chrono::duration_cast<chrono::milliseconds>(
                   chrono::system_clock::now().time_since_epoch())
            .count();
    }

    grpc::Status HealthCheckServiceImpl::Do(
        grpc::ServerContext * /*context*/,
        const healthcheck::HealthCheckRequest *request,
        healthcheck::HealthCheckResponse *response)
    {
        string uid = request->uid();
        string stableID = request->agent_id().empty() ? uid : request->agent_id();
        auto now = chrono::steady_clock::now();

        // 更新 Agent 信息
        {
            lock_guard<mutex> lock(agents_mutex);
            AgentInfo &info = agents_[stableID];
            bool wasOffline = !info.online;

            info.hostname = request->hostname();
            info.ipAddr = request->ipaddr();
            info.uid = uid;
            info.agentID = stableID;
            info.version = request->agentversion();
            info.platform = request->platform();
            info.capabilities.assign(request->capabilities().begin(), request->capabilities().end());
            info.labels.assign(request->labels().begin(), request->labels().end());
            info.resourceBudget = request->resource_budget();
            info.online = true;
            info.lastHeartbeat = now;
            info.lastSeenUnixMs = system_now_unix_ms();
            if (request->has_selfpstats())
            {
                info.lastSelfPstats.CopyFrom(request->selfpstats());
            }
            info.lastChildrenPstats.clear();
            if (request->has_childrenpstats())
            {
                info.lastChildrenPstats.push_back(request->childrenpstats());
            }
            info.runningAttempts.assign(request->running_attempts().begin(), request->running_attempts().end());
            info.completedAttempts.assign(request->completed_attempts().begin(), request->completed_attempts().end());

            if (wasOffline)
            {
                cout << "[server] Agent 恢复上线: agent_id=" << stableID
                     << " uid=" << uid
                     << " host=" << request->hostname()
                     << " ip=" << request->ipaddr() << endl;
            }
        }

        cout << "[server] 收到心跳: host=" << request->hostname()
             << " ip=" << request->ipaddr()
             << " uid=" << uid
             << " agent_id=" << stableID
             << " running_attempts=" << request->running_attempts_size()
             << " completed_attempts=" << request->completed_attempts_size();
        if (request->has_selfpstats())
        {
            cout << " CPU=" << request->selfpstats().cpupercent() << "%"
                 << " RSS=" << request->selfpstats().rsskb() << "KB";
        }
        cout << endl;

        // 回复 Agent，有任务就派发
        response->set_status(healthcheck::HealthCheckResponse::SERVING);
        response->set_pending(false);
        {
            lock_guard<mutex> lock(tasks_mutex);
            for (const auto &attempt : request->completed_attempts())
            {
                hotmethod::TaskResult completed;
                completed.set_taskid(attempt.taskid());
                completed.set_attempt_id(attempt.attempt_id());
                mark_attempt_completed_locked(completed);
            }
            hotmethod::TaskDesc task;
            if (pop_next_dispatchable_task_locked(request->ipaddr(), &task))
            {
                response->set_pending(true);
                response->mutable_taskdesc()->Swap(&task);
            }
        }

        return grpc::Status::OK;
    }

} // namespace drop_server
